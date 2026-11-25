// native 后端夹具（C++）：断点行由行尾标记定位，测试按标记所在行设置断点。
#include <iostream>
#include <string>
#include <vector>

struct point_t {
    int x;
    int y;
};

int main() {
    point_t origin{1, 2};
    int total = 0;
    std::vector<int> items{3, 4};
    std::string tag = "sum";
    for (int i = 0; i < 2; i++) {
        total += items[i]; /* BREAK_HERE */
    }
    std::cout << tag << "=" << total << " origin=" << origin.x + origin.y << std::endl;
    return 0;
}
