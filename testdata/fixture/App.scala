/** JVM 后端的 Scala 端到端夹具：样例类字段、java 集合与循环累加。 */
case class Item(price: Int)

object App {
  def compute(items: java.util.List[Item], base: Int): Int = {
    var total = 0
    val it = items.iterator()
    while (it.hasNext) {
      val item = it.next()
      val price = item.price
      total += price * base // BREAK_HERE
      println(s"total=$total")
    }
    total
  }

  def main(args: Array[String]): Unit = {
    val items = new java.util.ArrayList[Item]()
    items.add(Item(10))
    items.add(Item(20))
    val total = compute(items, 2)
    println(s"done total=$total")
  }
}
