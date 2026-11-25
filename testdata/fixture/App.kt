/**
 * JVM 后端的 Kotlin 端到端夹具：Kotlin 类字段、java 集合与循环累加，
 * 用于验证断点命中、局部变量读取与集合渲染。
 */
class Item(val price: Int)

fun compute(items: List<Item>, base: Int): Int {
    var total = 0
    for (item in items) {
        val price = item.price
        total += price * base // BREAK_HERE
        println("total=$total")
    }
    return total
}

fun main() {
    val items = listOf(Item(10), Item(20))
    val total = compute(items, 2)
    println("done total=$total")
}
