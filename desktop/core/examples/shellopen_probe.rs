//! Проверка, что URL доезжает до обработчика целиком.
//!
//! `cargo run --example shellopen_probe`
//!
//! Слушатель на 127.0.0.1 играет роль сервера, на который ведёт кнопка из
//! письма, а URL несёт три параметра через `&` — ровно то, на чём ломался
//! `cmd /C start`: браузер получал только `?token=abc`, а сервер отвечал 4xx,
//! потому что хост и путь-то живые. Проба открывает настоящий системный
//! браузер (одна вкладка, её можно закрыть) и печатает, что реально пришло.
//!
//! Не тест: нужен живой браузер и работающий рабочий стол, в CI такому места
//! нет. Регресс на разбор ссылки живёт в `click_target_tests` (клиент).
use std::io::{BufRead, BufReader, Write};
use std::net::TcpListener;

fn main() {
    let listener = TcpListener::bind("127.0.0.1:0").expect("bind");
    let port = listener.local_addr().unwrap().port();
    let url = format!("http://127.0.0.1:{port}/p?token=abc&utm_source=mail&id=42");
    println!("отдаём обработчику: {url}");
    ddmail_core::shellopen::open_url(&url).expect("open_url");

    let (stream, _) = listener.accept().expect("accept");
    let mut line = String::new();
    BufReader::new(&stream).read_line(&mut line).expect("read request line");
    println!("сервер увидел:      {}", line.trim());
    let mut out = &stream;
    let _ = out.write_all(
        b"HTTP/1.1 200 OK\r\nContent-Type: text/plain; charset=utf-8\r\n\
          Connection: close\r\n\r\nproba proshla, vkladku mozhno zakryt\r\n",
    );

    let whole = ["token=abc", "utm_source=mail", "id=42"].iter().all(|p| line.contains(p));
    println!("все параметры целы: {whole}");
    assert!(whole, "URL доехал обрезанным — фикс потерян");
}
