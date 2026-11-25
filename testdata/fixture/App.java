import java.util.ArrayList;
import java.util.HashMap;
import java.util.List;
import java.util.Map;

/**
 * End-to-end fixture for the JVM backend: collections, maps and an accumulating
 * loop, so breakpoints and value reads can both be verified.
 */
public final class App {
    private static final int LIMIT = 3;
    private static int calls = 0;

    private App() {}

    static int compute(List<Map<String, Integer>> items, int base) {
        int total = 0;
        for (Map<String, Integer> item : items) {
            int price = item.get("price");
            calls++;
            total += price * base; // BREAK_HERE
            System.out.println("total=" + total);
        }
        return total;
    }

    public static void main(String[] args) {
        List<Map<String, Integer>> items = new ArrayList<>();
        Map<String, Integer> first = new HashMap<>();
        first.put("price", 10);
        first.put("name", 100);
        items.add(first);
        Map<String, Integer> second = new HashMap<>();
        second.put("price", 20);
        items.add(second);

        int total = compute(items, 2);
        System.out.println("done total=" + total + " limit=" + LIMIT);
    }
}
