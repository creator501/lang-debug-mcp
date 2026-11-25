// 端到端测试用的最小 Node.js 目标。
// 断点行由测试按标记动态定位，避免行号硬编码。

function compute(items) {
  let total = 0;
  for (let i = 0; i < items.length; i++) {
    total += items[i].price * items[i].qty; // BREAK_HERE
  }
  return total;
}

function main() {
  const items = [
    { name: 'pen', price: 10, qty: 2 },
    { name: 'book', price: 30, qty: 1 },
  ];
  const total = compute(items);
  console.log('total=' + total);
}

main();
