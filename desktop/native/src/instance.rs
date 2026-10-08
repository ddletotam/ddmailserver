//! Связь второго запуска с уже работающим экземпляром.
//!
//! Single-instance guard (`main`) не пускает вторую копию, но одного «выйти»
//! мало: система запускает exe с `mailto:`-ссылкой, когда пользователь кликает
//! по адресу в браузере, и ссылка должна доехать до открытого окна, а не
//! пропасть вместе со второй копией. Да и простой повторный запуск из меню
//! должен поднять окно, а не молча ничего не сделать.
//!
//! Канал — loopback TCP: одинаково на Windows и Linux, без именованных каналов
//! и без новых зависимостей. Первый экземпляр слушает `127.0.0.1:<случайный
//! порт>` и кладёт порт, свой pid и случайный токен в `instance.json` в папке
//! конфига (0600 на Unix). Порт виден всем пользователям машины, поэтому
//! сообщение без токена отбрасывается: прочитать файл может только владелец.

use std::io::{BufRead, BufReader, Read, Write};
use std::net::{Ipv4Addr, SocketAddr, TcpListener, TcpStream};
use std::path::PathBuf;
use std::time::Duration;

/// Что второй запуск просит у первого.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Request {
    /// Просто показать окно (повторный запуск из меню/ярлыка).
    Activate,
    /// Открыть `mailto:`-ссылку в композере.
    Mailto(String),
}

/// Ссылки длиннее этого не бывают в природе, а читать без предела из сокета,
/// куда может постучаться кто угодно, нельзя.
const MAX_MSG: u64 = 64 * 1024;

#[derive(serde::Serialize, serde::Deserialize)]
struct Endpoint {
    port: u16,
    pid: u32,
    token: String,
}

fn endpoint_path() -> Option<PathBuf> {
    Some(crate::account_store::config_dir()?.join("instance.json"))
}

fn encode(req: &Request) -> String {
    match req {
        Request::Activate => "activate".to_string(),
        Request::Mailto(url) => format!("mailto {url}"),
    }
}

fn decode(line: &str) -> Option<Request> {
    match line.split_once(' ') {
        None if line == "activate" => Some(Request::Activate),
        Some(("mailto", url)) if crate::mailto::parse(url).is_some() => {
            Some(Request::Mailto(url.to_string()))
        }
        _ => None,
    }
}

/// Первый экземпляр: поднять приёмник и опубликовать адрес. `on_request`
/// зовётся из фонового потока — переброс на UI-поток на стороне вызывающего.
/// Ошибка не фатальна: без канала клиент работает, просто вторые запуски
/// ничего не передадут.
pub fn serve(on_request: impl Fn(Request) + Send + 'static) -> std::io::Result<()> {
    let listener = TcpListener::bind((Ipv4Addr::LOCALHOST, 0))?;
    let port = listener.local_addr()?.port();
    let mut raw = [0u8; 16];
    getrandom::fill(&mut raw).map_err(|e| std::io::Error::other(e.to_string()))?;
    let token: String = raw.iter().map(|b| format!("{b:02x}")).collect();
    let ep = Endpoint { port, pid: std::process::id(), token: token.clone() };
    let path = endpoint_path().ok_or_else(|| std::io::Error::other("нет папки конфига"))?;
    let json = serde_json::to_vec(&ep).map_err(std::io::Error::other)?;
    crate::account_store::write_atomic(&path, &json)?;

    std::thread::Builder::new().name("instance-ipc".into()).spawn(move || {
        for conn in listener.incoming() {
            let Ok(conn) = conn else { continue };
            // Чужой клиент, повисший на соединении, не должен держать приёмник.
            let _ = conn.set_read_timeout(Some(Duration::from_secs(2)));
            let mut lines = BufReader::new(conn.take(MAX_MSG)).lines();
            let (Some(Ok(got)), Some(Ok(msg))) = (lines.next(), lines.next()) else { continue };
            if got != token {
                eprintln!("instance: сообщение с неверным токеном отброшено");
                continue;
            }
            match decode(&msg) {
                Some(req) => on_request(req),
                None => eprintln!("instance: непонятный запрос отброшен"),
            }
        }
    })?;
    Ok(())
}

/// Второй экземпляр: передать запрос первому. `false` — передать не удалось.
///
/// Первый мог только что стартовать и ещё не записать `instance.json` (guard
/// он взял раньше), поэтому несколько попыток с паузой.
pub fn send(req: &Request) -> bool {
    for attempt in 0..15 {
        if attempt > 0 {
            std::thread::sleep(Duration::from_millis(200));
        }
        let Some(ep) = endpoint_path()
            .and_then(|p| std::fs::read(p).ok())
            .and_then(|b| serde_json::from_slice::<Endpoint>(&b).ok())
        else {
            continue;
        };
        let addr = SocketAddr::from((Ipv4Addr::LOCALHOST, ep.port));
        let Ok(mut s) = TcpStream::connect_timeout(&addr, Duration::from_secs(1)) else { continue };
        allow_foreground(ep.pid);
        if s.write_all(format!("{}\n{}\n", ep.token, encode(req)).as_bytes()).is_ok() {
            return true;
        }
    }
    false
}

/// Windows отдаёт фокус только процессу, который пользователь только что
/// трогал, — а это мы, второй запуск, а не первый экземпляр, которому надо
/// поднять окно. Без разрешения его `SetForegroundWindow` лишь мигнёт кнопкой
/// на панели задач.
#[cfg(windows)]
fn allow_foreground(pid: u32) {
    unsafe {
        let _ = windows::Win32::UI::WindowsAndMessaging::AllowSetForegroundWindow(pid);
    }
}

#[cfg(not(windows))]
fn allow_foreground(_pid: u32) {}

/// Запрос из аргументов командной строки: первая `mailto:`-ссылка. Система
/// передаёт её одним аргументом (`"%1"` в реестре, `%u` в `.desktop`).
pub fn request_from_args(args: impl IntoIterator<Item = String>) -> Request {
    args.into_iter()
        .find(|a| crate::mailto::parse(a).is_some())
        .map(Request::Mailto)
        .unwrap_or(Request::Activate)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn wire_roundtrip() {
        for req in [Request::Activate, Request::Mailto("mailto:a@x.invalid?subject=%D0%A1".into())]
        {
            assert_eq!(decode(&encode(&req)), Some(req));
        }
        assert_eq!(decode("mailto https://x.invalid"), None);
        assert_eq!(decode("exec rm"), None);
    }

    #[test]
    fn args_pick_mailto() {
        let args = |v: &[&str]| v.iter().map(|s| s.to_string()).collect::<Vec<_>>();
        assert_eq!(request_from_args(args(&["ddmail-native"])), Request::Activate);
        assert_eq!(
            request_from_args(args(&["ddmail-native", "MAILTO:a@x.invalid"])),
            Request::Mailto("MAILTO:a@x.invalid".into())
        );
    }
}
