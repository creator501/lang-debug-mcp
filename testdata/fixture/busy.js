// 长跑夹具：用于中断采样，测试结束前由外部终止。
// 忙循环让采样能稳定取到栈，避免目标过早退出。
function spin() {
  let acc = 0;
  const deadline = Date.now() + 60000;
  while (Date.now() < deadline) {
    acc += Math.sqrt(acc + 1); // SPIN_HERE
  }
  return acc;
}

console.log('acc=' + spin());
