// 端到端测试用的最小 Go 目标。
// 断点行由测试按标记动态定位，避免行号硬编码。
package main

import "fmt"

// item 是参与计算的一条明细。
type item struct {
	price int
	qty   int
}

// compute 累加金额，循环体内是断点位置。
func compute(items []item) int {
	total := 0
	for _, it := range items {
		total += it.price * it.qty // BREAK_HERE
	}
	return total
}

func main() {
	fmt.Printf("total=%d\n", compute([]item{{price: 10, qty: 2}, {price: 30, qty: 1}}))
}
