//! Автонастройка по адресу из командной строки — проверить цепочку
//! (`ddmail_core::discover`) на живых доменах без запуска клиента:
//! `cargo run --example discover -- user@example.org`.

use std::sync::atomic::AtomicBool;

#[tokio::main]
async fn main() {
    let email = std::env::args().nth(1).expect("адрес почты первым аргументом");
    let t = std::time::Instant::now();
    let cancel = AtomicBool::new(false);
    let found = ddmail_core::discover::discover(&email, None, &cancel, &|p| {
        eprintln!("{:>5.1} c [{:>2}/{}] {}", t.elapsed().as_secs_f32(), p.done, p.total, p.stage);
    })
    .await;
    println!("{found:#?}\n{:.1} c", t.elapsed().as_secs_f32());
}
