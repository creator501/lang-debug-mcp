package jvm

import (
	"context"
	"time"

	"github.com/langdebug/lang-debug-mcp/internal/backend"
)

// eventLoop 消费 JDWP 事件。
//
// 读循环只负责分拣，真正的栈帧收集与快照构建在本协程完成，避免阻塞应答投递。
func (s *Session) eventLoop() {
	for {
		select {
		case <-s.doneCh:
			return
		case <-s.wire.Done():
			s.mu.Lock()
			code := s.exitCode
			s.mu.Unlock()
			s.onTerminated(code)
			return
		case pkt := <-s.wire.Events():
			s.onPacket(pkt)
		}
	}
}

// onPacket 解析一条复合事件包。
//
// 包体为：挂起策略、事件个数，随后每个事件是「事件种类、请求标识、事件体」。
func (s *Session) onPacket(pkt eventPacket) {
	r := newReader(pkt.data, s.wire.ids)
	r.u1() // 挂起策略：由请求决定，解析时无需使用
	count := int(r.u4())
	for i := 0; i < count; i++ {
		if r.left() < 5 {
			return
		}
		kind := r.u1()
		reqID := requestID(r.u4())
		switch kind {
		case evBreakpoint:
			thread := r.id(s.wire.ids.object)
			r.skip(1) // typeTag
			r.skip(s.wire.ids.refTyp + s.wire.ids.method + 8)
			s.hit(reqID)
			stop := s.buildStop(context.Background(), thread, "breakpoint", reqID)
			s.publishStop(stop)
		case evSingleStep:
			thread := r.id(s.wire.ids.object)
			r.skip(1 + s.wire.ids.refTyp + s.wire.ids.method + 8)
			s.clearStepRequest(context.Background())
			stop := s.buildStop(context.Background(), thread, "step", reqID)
			s.publishStop(stop)
		case evVMStart:
			thread := r.id(s.wire.ids.object)
			stop := s.buildStop(context.Background(), thread, "entry", 0)
			s.publishStop(stop)
		case evClassPrepare:
			thread := r.id(s.wire.ids.object)
			r.skip(1 + s.wire.ids.refTyp) // refTypeTag + typeID
			r.str()                       // signature
			r.u4()                        // status
			s.resolvePending(context.Background())
			// 类准备事件按 EVENT_THREAD 挂起，解析完断点后只放行该线程，
			// 避免把其它原因的挂起（如断点停止）一起解除。
			_ = s.wire.resumeThread(context.Background(), thread)
		case evThreadStart, evThreadDeath:
			r.id(s.wire.ids.object)
		case evVMDeath:
			s.onTerminated(0)
			return
		default:
			// 无法安全跳过的事件（如异常事件带可变长结构），停止解析。
			return
		}
	}
}

// onTerminated 标记虚拟机结束并唤醒等待者。
func (s *Session) onTerminated(code int) {
	s.doneOnce.Do(func() {
		s.mu.Lock()
		s.terminated = true
		s.running = false
		s.stop = nil
		if code != 0 {
			s.exitCode = code
		}
		s.mu.Unlock()
		close(s.doneCh)
	})
}

// clearStepRequest 注销上一次单步请求。
func (s *Session) clearStepRequest(ctx context.Context) {
	s.mu.Lock()
	active, req := s.stepActive, s.stepReq
	s.stepActive = false
	s.mu.Unlock()
	if !active {
		return
	}
	cctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = s.wire.clearEvent(cctx, evSingleStep, req)
}

// waitAttached 等待入口事件或超时，用于启动阶段确认目标可调试。
func (s *Session) waitAttached(ctx context.Context, timeout time.Duration) *backend.Stop {
	select {
	case stop := <-s.stopCh:
		return stop
	case <-time.After(timeout):
		return nil
	case <-s.doneCh:
		return nil
	case <-ctx.Done():
		return nil
	}
}
