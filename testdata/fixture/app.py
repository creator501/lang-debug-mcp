# 端到端测试用的最小 Python 目标。
# 断点行由测试按标记动态定位，避免行号硬编码。


def compute(items):
    total = 0
    for item in items:
        total += item["price"] * item["qty"]  # BREAK_HERE
    return total


def make_counter():
    calls = 0

    def bump():
        nonlocal calls
        calls += 1
        return calls

    return bump


def main():
    items = [
        {"name": "pen", "price": 10, "qty": 2},
        {"name": "book", "price": 30, "qty": 1},
    ]
    total = compute(items)
    bump = make_counter()
    bump()
    print(f"total={total}")


if __name__ == "__main__":
    main()
