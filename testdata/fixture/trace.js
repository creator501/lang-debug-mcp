// 采集点夹具：固定次数循环，用于验证采样率、单点上限与聚合。
// 断点行由测试按标记定位；标记只能出现一次，否则会定位到注释行。
function work(rounds) {
  let total = 0;
  for (let i = 0; i < rounds; i++) {
    total += i; // TRACE_HERE
  }
  return total;
}

console.log('total=' + work(20));
