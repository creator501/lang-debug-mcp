// Package jvm 实现 JVM 系语言（Java / Kotlin / Scala / Groovy）的调试后端。
//
// 后端直接实现 JDWP（Java Debug Wire Protocol）客户端，而不是托管 java-debug
// 适配器：目标 JVM 由 -agentlib:jdwp 打开调试端口，本包用同一份协议完成断点、
// 栈帧与取值。四种 JVM 语言因此共享一套实现，调用方也不需要额外安装 Java 扩展。
//
// 与 DAP 后端的分工一致：本包只做协议翻译与能力边界，预算裁剪、错误码归一由
// model 与工具层负责。
package jvm

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/langdebug/lang-debug-mcp/internal/model"
)

// handshake 是 JDWP 的固定握手串。
const handshake = "JDWP-Handshake"

// JDWP 命令集编号。
const (
	csVirtualMachine       = 1
	csReferenceType        = 2
	csClassType            = 3
	csArrayType            = 4
	csMethod               = 6
	csObjectReference      = 9
	csStringReference      = 10
	csThreadReference      = 11
	csArrayReference       = 13
	csEventRequest         = 15
	csStackFrame           = 16
	csClassObjectReference = 17
)

// 事件种类。
const (
	evSingleStep   = 1
	evBreakpoint   = 2
	evException    = 5
	evThreadStart  = 6
	evThreadDeath  = 7
	evClassPrepare = 8
	evVMStart      = 90
	evVMDeath      = 99
)

// 挂起策略。
const (
	suspendNone        = 0
	suspendEventThread = 1
	suspendAll         = 2
)

// 单步的大小与深度。
const (
	stepLine = 1
	stepInto = 0
	stepOver = 1
	stepOut  = 2
)

// 值标签（JDWP 类型签名）。
const (
	tagArray   = '['
	tagByte    = 'B'
	tagChar    = 'C'
	tagObject  = 'L'
	tagFloat   = 'F'
	tagDouble  = 'D'
	tagInt     = 'I'
	tagLong    = 'J'
	tagShort   = 'S'
	tagVoid    = 'V'
	tagBoolean = 'Z'
	tagString  = 's'
)

// commandTimeout 是单条 JDWP 命令的默认超时。
const commandTimeout = 15 * time.Second

// maxPacket 是允许的最大包长，防止异常对端让本进程分配巨量内存。
const maxPacket = 64 << 20

// sizes 是各个标识符的字节宽度，由 VirtualMachine.IDSizes 给出。
type sizes struct {
	field  int
	method int
	object int
	refTyp int
	frame  int
}

// reply 是一条命令应答。
type reply struct {
	err  uint16
	data []byte
}

// eventPacket 是一条事件包。
type eventPacket struct {
	cmdSet byte
	cmd    byte
	data   []byte
}

// wire 是一条 JDWP 连接。
//
// 读循环独占读端：应答按序号交给等待者，事件投入带缓冲通道，任何一条慢命令都
// 不会阻塞后续事件与应答的投递。
type wire struct {
	rw   io.ReadWriteCloser
	ids  sizes
	wmu  sync.Mutex
	next atomic.Uint32

	mu     sync.Mutex
	wait   map[uint32]chan reply
	closed error

	events chan eventPacket
	done   chan struct{}
	once   sync.Once
}

// dial 连接调试端口并完成握手与标识宽度协商。
func dial(ctx context.Context, addr string) (*wire, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, model.New(model.CodeEnvAdapterMissing, "无法连接 JDWP 端口 %s", addr).
			WithFix("确认目标以 -agentlib:jdwp=...,server=y,suspend=y,address=" + addr + " 启动").
			Wrap(err)
	}

	// 握手阶段用短超时，避免连上了一个不说话的对端。
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write([]byte(handshake)); err != nil {
		_ = conn.Close()
		return nil, model.New(model.CodeInternalProtocol, "JDWP 握手写入失败").Wrap(err)
	}
	buf := make([]byte, len(handshake))
	if _, err := io.ReadFull(conn, buf); err != nil {
		_ = conn.Close()
		return nil, model.New(model.CodeInternalProtocol, "JDWP 握手响应读取失败").
			WithFix("确认端口上运行的是 JDWP 调试代理（而非普通服务）").Wrap(err)
	}
	if string(buf) != handshake {
		_ = conn.Close()
		return nil, model.New(model.CodeInternalProtocol, "JDWP 握手串不匹配: %q", string(buf))
	}
	_ = conn.SetDeadline(time.Time{})

	w := &wire{
		rw:     conn,
		wait:   make(map[uint32]chan reply),
		events: make(chan eventPacket, 256),
		done:   make(chan struct{}),
	}
	go w.readLoop()

	ids, err := w.idSizes(ctx)
	if err != nil {
		_ = w.Close()
		return nil, err
	}
	w.ids = ids
	return w, nil
}

// Events 返回事件通道。
func (w *wire) Events() <-chan eventPacket { return w.events }

// Done 在连接结束时关闭。
func (w *wire) Done() <-chan struct{} { return w.done }

// Close 关闭连接。
func (w *wire) Close() error {
	w.once.Do(func() {
		w.mu.Lock()
		if w.closed == nil {
			w.closed = errors.New("JDWP 连接已关闭")
		}
		w.mu.Unlock()
		close(w.done)
		_ = w.rw.Close()
	})
	return nil
}

// errClosed 返回连接结束原因。
func (w *wire) errClosed() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed == nil {
		return errors.New("JDWP 连接已关闭")
	}
	return w.closed
}

// fail 记录连接结束原因并唤醒等待者。
func (w *wire) fail(err error) {
	w.once.Do(func() {
		w.mu.Lock()
		w.closed = err
		w.mu.Unlock()
		close(w.done)
		_ = w.rw.Close()
	})
}

// readLoop 独占读取端。
func (w *wire) readLoop() {
	for {
		header := make([]byte, 11)
		if _, err := io.ReadFull(w.rw, header); err != nil {
			w.fail(model.New(model.CodeStateDeadSession, "调试连接已断开").
				WithFix("目标可能已退出；查看 debug_output 后重建会话").Wrap(err))
			return
		}
		length := int(binary.BigEndian.Uint32(header[0:4]))
		id := binary.BigEndian.Uint32(header[4:8])
		flags := header[8]
		if length < 11 || length > maxPacket {
			w.fail(model.New(model.CodeInternalProtocol, "JDWP 包长度异常: %d", length))
			return
		}
		body := make([]byte, length-11)
		if length > 11 {
			if _, err := io.ReadFull(w.rw, body); err != nil {
				w.fail(model.New(model.CodeStateDeadSession, "调试连接在读取包体时断开").Wrap(err))
				return
			}
		}
		// flags 的 0x80 位表示“应答”；反之为 VM 下发的事件（commandSet 64）。
		if flags&0x80 == 0 {
			select {
			case w.events <- eventPacket{cmdSet: header[9], cmd: header[10], data: body}:
			default:
				// 事件堆积说明调用方没在消费，丢弃由上层超时体现。
			}
			continue
		}
		errCode := binary.BigEndian.Uint16(header[9:11])
		w.mu.Lock()
		ch := w.wait[id]
		w.mu.Unlock()
		if ch == nil {
			continue
		}
		select {
		case ch <- reply{err: errCode, data: body}:
		default:
		}
	}
}

// request 发送命令并等待应答。
func (w *wire) request(ctx context.Context, cs, cmd byte, body []byte) ([]byte, error) {
	id := w.next.Add(1)
	packet := make([]byte, 0, 11+len(body))
	packet = appendU4(packet, uint32(11+len(body)))
	packet = appendU4(packet, id)
	packet = append(packet, 0, cs, cmd)
	packet = append(packet, body...)

	ch := make(chan reply, 1)
	w.mu.Lock()
	w.wait[id] = ch
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		delete(w.wait, id)
		w.mu.Unlock()
	}()

	w.wmu.Lock()
	_, err := w.rw.Write(packet)
	w.wmu.Unlock()
	if err != nil {
		return nil, model.New(model.CodeStateDeadSession, "JDWP 命令写入失败").Wrap(err)
	}

	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, commandTimeout)
		defer cancel()
	}
	select {
	case r := <-ch:
		if r.err != 0 {
			return nil, &jdwpError{code: r.err, command: commandName(cs, cmd)}
		}
		return r.data, nil
	case <-ctx.Done():
		return nil, model.New(model.CodeInternalProtocol, "JDWP 命令 %s 超时", commandName(cs, cmd)).
			WithFix("目标可能已卡死或被外部挂起；可用 debug_output 检查目标状态")
	case <-w.done:
		return nil, w.errClosed()
	}
}

// jdwpError 是 VM 返回的协议错误。
type jdwpError struct {
	code    uint16
	command string
}

func (e *jdwpError) Error() string {
	return fmt.Sprintf("JDWP %s 失败（错误码 %d: %s）", e.command, e.code, e.detail())
}

// Code 让工具层拿到稳定错误码。
func (e *jdwpError) Code() model.Code {
	switch e.code {
	case 13: // THREAD_NOT_SUSPENDED
		return model.CodeStateNotStopped
	case 14: // THREAD_SUSPENDED
		return model.CodeStateInvalidTransition
	case 20, 24, 30, 31, 32, 41: // INVALID_OBJECT / INVALID_LOCATION / INVALID_FRAMEID / NO_MORE_FRAMES ...
		return model.CodeInputNotFound
	case 21, 22, 23, 25: // INVALID_CLASS / CLASS_NOT_PREPARED / INVALID_METHODID / INVALID_FIELDID
		return model.CodeInputNotFound
	case 35: // INVALID_SLOT
		return model.CodeTargetOptimized
	case 90: // VM_DEAD
		return model.CodeTargetExited
	}
	return model.CodeInternalProtocol
}

// detail 返回错误码的可读说明。
func (e *jdwpError) detail() string {
	switch e.code {
	case 13:
		return "线程未挂起"
	case 14:
		return "线程已挂起"
	case 20:
		return "对象标识无效（可能已被回收）"
	case 21:
		return "类标识无效"
	case 22:
		return "类尚未准备"
	case 23:
		return "方法标识无效"
	case 24:
		return "位置无效"
	case 25:
		return "字段标识无效"
	case 30:
		return "栈帧标识无效"
	case 31:
		return "没有更多栈帧"
	case 32:
		return "栈帧不可见（非当前帧）"
	case 33:
		return "不是当前帧"
	case 35:
		return "变量槽无效（可能没有调试信息）"
	case 40:
		return "重复请求"
	case 41:
		return "未找到"
	case 50:
		return "监视器无效"
	case 52:
		return "线程被中断"
	case 68:
		return "协议版本不支持"
	case 90:
		return "虚拟机已退出"
	case 99:
		return "虚拟机内部错误"
	case 100:
		return "线程未附加"
	case 500:
		return "值标签无效"
	case 502:
		return "已有方法调用在执行"
	case 503:
		return "索引无效"
	case 504:
		return "长度无效"
	case 506:
		return "字符串标识无效"
	case 507:
		return "类加载器无效"
	case 508:
		return "数组标识无效"
	case 512:
		return "计数无效"
	}
	return "未知错误"
}

// fix 给出与错误码匹配的下一步建议。
func (e *jdwpError) fix() string {
	switch e.code {
	case 13:
		return "先让目标停止（设置断点后 continue，或调用 pause）"
	case 14:
		return "目标已处于挂起状态，直接读取快照即可"
	case 22:
		return "断点所属的类尚未加载；先运行到该类被加载，或改用 attach 到已启动的目标"
	case 35:
		return "该变量没有调试信息：用 -g（含 -g:vars）重新编译后再试"
	case 31:
		return "减小 frame 序号；当前帧以下没有更多调用帧"
	case 90:
		return "目标已退出，重建会话后重试"
	}
	return ""
}

// toModel 把协议错误转换为统一错误。
func (e *jdwpError) toModel() *model.Error {
	err := model.New(e.Code(), "%s", e.Error())
	if fix := e.fix(); fix != "" {
		err = err.WithFix(fix)
	}
	return err
}

// commandName 返回命令的可读名，用于错误信息。
func commandName(cs, cmd byte) string {
	names := map[uint16]string{
		0x0101: "VirtualMachine.Version",
		0x0102: "VirtualMachine.ClassesBySignature",
		0x0103: "VirtualMachine.AllClasses",
		0x0104: "VirtualMachine.AllThreads",
		0x0107: "VirtualMachine.IDSizes",
		0x0108: "VirtualMachine.Suspend",
		0x0109: "VirtualMachine.Resume",
		0x010B: "VirtualMachine.CreateString",
		0x0201: "ReferenceType.Signature",
		0x0204: "ReferenceType.Fields",
		0x0205: "ReferenceType.Methods",
		0x0206: "ReferenceType.GetValues",
		0x0207: "ReferenceType.SourceFile",
		0x0301: "ClassType.Superclass",
		0x0601: "Method.LineTable",
		0x0602: "Method.VariableTable",
		0x0901: "ObjectReference.ReferenceType",
		0x0902: "ObjectReference.GetValues",
		0x0903: "ObjectReference.SetValues",
		0x0A01: "StringReference.Value",
		0x0B01: "ThreadReference.Name",
		0x0B02: "ThreadReference.Suspend",
		0x0B03: "ThreadReference.Resume",
		0x0B04: "ThreadReference.Status",
		0x0B06: "ThreadReference.Frames",
		0x0D01: "ArrayReference.Length",
		0x0D02: "ArrayReference.GetValues",
		0x0D03: "ArrayReference.SetValues",
		0x0F01: "EventRequest.Set",
		0x0F02: "EventRequest.Clear",
		0x1001: "StackFrame.GetValues",
		0x1002: "StackFrame.SetValues",
		0x1003: "StackFrame.ThisObject",
	}
	if name, ok := names[uint16(cs)<<8|uint16(cmd)]; ok {
		return name
	}
	return "命令 " + strconv.Itoa(int(cs)) + "/" + strconv.Itoa(int(cmd))
}

// jdwpReader 按 JDWP 线格式读取。
type jdwpReader struct {
	buf []byte
	off int
	ids sizes
}

func newReader(buf []byte, ids sizes) *jdwpReader {
	return &jdwpReader{buf: buf, ids: ids}
}

func (r *jdwpReader) left() int { return len(r.buf) - r.off }

func (r *jdwpReader) u1() byte {
	if r.left() < 1 {
		r.off = len(r.buf)
		return 0
	}
	v := r.buf[r.off]
	r.off++
	return v
}

func (r *jdwpReader) u2() uint16 {
	if r.left() < 2 {
		r.off = len(r.buf)
		return 0
	}
	v := binary.BigEndian.Uint16(r.buf[r.off:])
	r.off += 2
	return v
}

func (r *jdwpReader) u4() uint32 {
	if r.left() < 4 {
		r.off = len(r.buf)
		return 0
	}
	v := binary.BigEndian.Uint32(r.buf[r.off:])
	r.off += 4
	return v
}

func (r *jdwpReader) u8() uint64 {
	if r.left() < 8 {
		r.off = len(r.buf)
		return 0
	}
	v := binary.BigEndian.Uint64(r.buf[r.off:])
	r.off += 8
	return v
}

// id 读取指定宽度的标识符。
func (r *jdwpReader) id(size int) uint64 {
	switch size {
	case 1:
		return uint64(r.u1())
	case 2:
		return uint64(r.u2())
	case 4:
		return uint64(r.u4())
	case 8:
		return r.u8()
	}
	r.skip(size)
	return 0
}

// str 读取带长度前缀的 UTF-8 字符串。
func (r *jdwpReader) str() string {
	n := int(r.u4())
	if n < 0 || r.left() < n {
		r.off = len(r.buf)
		return ""
	}
	s := string(r.buf[r.off : r.off+n])
	r.off += n
	return s
}

// skip 跳过 n 字节。
func (r *jdwpReader) skip(n int) {
	if r.left() < n {
		r.off = len(r.buf)
		return
	}
	r.off += n
}

// value 读取一个带标签的值。
func (r *jdwpReader) value() value {
	return r.untagged(r.u1())
}

// untagged 按给定标签读取值体（不含标签本身）。
func (r *jdwpReader) untagged(tag byte) value {
	v := value{tag: tag}
	switch v.tag {
	case tagBoolean, tagByte:
		v.num = int64(r.u1())
	case tagChar, tagShort:
		v.num = int64(r.u2())
	case tagInt, tagFloat:
		v.num = int64(int32(r.u4()))
	case tagLong, tagDouble:
		v.num = int64(r.u8())
	case tagObject, tagString, tagArray:
		v.ref = r.id(r.ids.object)
	default:
		// 未知标签：无法继续解析，交给上层按协议错误处理。
		r.off = len(r.buf)
	}
	return v
}

// jdwpWriter 按 JDWP 线格式写入。
type jdwpWriter struct {
	buf []byte
	ids sizes
}

func newWriter(ids sizes) *jdwpWriter { return &jdwpWriter{ids: ids} }

func (w *jdwpWriter) u1(v byte)   { w.buf = append(w.buf, v) }
func (w *jdwpWriter) u2(v uint16) { w.buf = appendU2(w.buf, v) }
func (w *jdwpWriter) u4(v uint32) { w.buf = appendU4(w.buf, v) }
func (w *jdwpWriter) u8(v uint64) { w.buf = appendU8(w.buf, v) }

// id 写入指定宽度的标识符。
func (w *jdwpWriter) id(size int, v uint64) {
	for shift := (size - 1) * 8; shift >= 0; shift -= 8 {
		w.buf = append(w.buf, byte(v>>uint(shift)))
	}
}

// str 写入带长度前缀的字符串。
func (w *jdwpWriter) str(s string) {
	w.u4(uint32(len(s)))
	w.buf = append(w.buf, s...)
}

// bytes 返回写入内容。
func (w *jdwpWriter) bytes() []byte { return w.buf }

func appendU2(b []byte, v uint16) []byte {
	return append(b, byte(v>>8), byte(v))
}

func appendU4(b []byte, v uint32) []byte {
	return append(b, byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

func appendU8(b []byte, v uint64) []byte {
	return append(b, byte(v>>56), byte(v>>48), byte(v>>40), byte(v>>32), byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

// value 是一个 JDWP 值：标量放在 num 里（浮点用位模式），引用放在 ref 里。
type value struct {
	tag byte
	num int64
	ref uint64
}

// f64 返回浮点值的数值。
func (v value) f64() float64 {
	if v.tag == tagFloat {
		return float64(math.Float32frombits(uint32(v.num)))
	}
	return math.Float64frombits(uint64(v.num))
}

// display 返回标量值的可读文本。
func (v value) display() string {
	switch v.tag {
	case tagBoolean:
		if v.num != 0 {
			return "true"
		}
		return "false"
	case tagByte, tagShort, tagInt, tagLong:
		return strconv.FormatInt(v.num, 10)
	case tagChar:
		return strconv.QuoteRune(rune(v.num))
	case tagFloat, tagDouble:
		return strconv.FormatFloat(v.f64(), 'g', -1, 64)
	}
	return ""
}

// isReference 报告该值是否为对象引用。
func (v value) isReference() bool {
	return v.tag == tagObject || v.tag == tagArray || v.tag == tagString
}

// zeroOf 返回与标签匹配的零值，用于占位。
func zeroOf(tag byte) value {
	return value{tag: tag}
}
