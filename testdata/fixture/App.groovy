/**
 * JVM 后端的 Groovy 夹具：编译为 App.class，用于校验四种 JVM 语言的
 * 调试信息判据（行号表 / 局部变量表）与语言相关的修复建议。
 */
class App {
    static int compute(List items, int base) {
        int total = 0
        for (item in items) {
            int price = item.price
            total += price * base // BREAK_HERE
            println "total=${total}"
        }
        return total
    }

    static void main(String[] args) {
        def items = [[price: 10], [price: 20]]
        println "done total=${compute(items, 2)}"
    }
}
