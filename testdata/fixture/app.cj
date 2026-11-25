// 端到端测试用的最小仓颉（Cangjie）目标。
// 需要 cjc 编译器：用 `cjc -g app.cj -o app` 编译后由 lldb-dap 调试。
// 断点行由测试按标记动态定位，避免行号硬编码。
package demo

import std.collection.*

class Item {
    public var price: Int64
    public var qty: Int64

    init(price: Int64, qty: Int64) {
        this.price = price
        this.qty = qty
    }
}

func compute(items: ArrayList<Item>): Int64 {
    var total: Int64 = 0
    for (item in items) {
        total += item.price * item.qty // BREAK_HERE
    }
    return total
}

main(): Int64 {
    let items = ArrayList<Item>()
    items.append(Item(10, 2))
    items.append(Item(30, 1))
    println("total=${compute(items)}")
    return 0
}
