// native 后端夹具（Rust）：断点行由行尾标记定位，测试按标记所在行设置断点。
fn main() {
    let mut total: i32 = 0;
    let items = [3, 4];
    let numbers: Vec<i32> = vec![5, 6, 7];
    let maybe: Option<i32> = Some(7);
    let name = String::from("sum");
    for i in 0..2 {
        total += items[i]; // BREAK_HERE
    }
    println!("{} {} {:?} {:?}", name, total, maybe, numbers);
}
