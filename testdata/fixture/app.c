/* native 后端夹具：断点行由行尾标记定位，测试按标记所在行设置断点。 */
#include <stdio.h>

typedef struct {
    int x;
    int y;
} point_t;

int main(void) {
    point_t origin = {1, 2};
    int total = 0;
    int items[2] = {3, 4};
    for (int i = 0; i < 2; i++) {
        total += items[i]; /* BREAK_HERE */
    }
    printf("origin=(%d,%d) total=%d\n", origin.x, origin.y, total);
    return 0;
}
