//! Своя аватарка диалога (двойной клик по аватарке в шапке, контракт §4,
//! «Шапка диалога»): из файла или по ссылке.
//!
//! Как и пользовательское имя, живёт только в клиенте: запись в
//! `merges.json` (ключ — тот же, что у переименования) плюс файл
//! `avatars/<хэш ключа>.png` рядом. Картинка проверяется декодированием —
//! расширению и Content-Type не верим, — обрезается до квадрата по центру и
//! сохраняется PNG 256×256: в шапке она 40px, в списке 48px, оригинал в
//! мегабайты незачем ни хранить, ни декодировать на каждом старте.

use std::collections::HashMap;
use std::io::Read as _;
use std::path::PathBuf;
use std::time::Duration;

use slint::{Image, Rgba8Pixel, SharedPixelBuffer};

use crate::merges::{MergeKey, Merges};

/// Сторона сохранённой картинки.
const SIDE: u32 = 256;
/// Больше — не аватарка, а случайно выбранный не тот файл.
pub const MAX_BYTES: usize = 10 * 1024 * 1024;
const URL_TIMEOUT: Duration = Duration::from_secs(20);

fn dir() -> Option<PathBuf> {
    crate::account_store::config_dir().map(|d| d.join("avatars"))
}

/// Имя файла по ключу диалога: FNV-1a от `account|id` — id диалога это
/// набор адресов, в имя файла его не положить.
pub fn file_name(key: &MergeKey) -> String {
    let mut h: u64 = 0xcbf29ce484222325;
    for b in key.account.bytes().chain(std::iter::once(b'|')).chain(key.id.bytes()) {
        h ^= b as u64;
        h = h.wrapping_mul(0x100000001b3);
    }
    format!("{h:016x}.png")
}

/// Декодировать, обрезать до квадрата по центру, уменьшить до 256 и
/// закодировать PNG. Ошибка — человеческим текстом для панели.
pub fn normalize(bytes: &[u8]) -> Result<Vec<u8>, String> {
    if bytes.is_empty() {
        return Err("Файл пустой.".into());
    }
    if bytes.len() > MAX_BYTES {
        return Err(format!(
            "Слишком большой файл: {} МБ, можно до 10 МБ.",
            bytes.len() / (1024 * 1024)
        ));
    }
    let img = image::load_from_memory(bytes).map_err(|_| {
        "Это не картинка или формат не поддерживается (нужен PNG, JPEG, GIF, WebP, BMP или ICO).".to_string()
    })?;
    let (w, h) = (img.width(), img.height());
    if w < 16 || h < 16 {
        return Err(format!("Картинка слишком маленькая: {w}×{h}."));
    }
    let side = w.min(h);
    let square = img.crop_imm((w - side) / 2, (h - side) / 2, side, side);
    let out = if side > SIDE {
        square.resize_exact(SIDE, SIDE, image::imageops::FilterType::Lanczos3)
    } else {
        square
    };
    let mut png = Vec::new();
    out.to_rgba8()
        .write_to(&mut std::io::Cursor::new(&mut png), image::ImageFormat::Png)
        .map_err(|e| format!("Не удалось сохранить картинку: {e}"))?;
    Ok(png)
}

/// Скачать картинку по ссылке: только http(s), ответ 200, не больше 10 МБ.
/// Формат проверяет `normalize`. Блокирующий — звать из фонового потока.
pub fn fetch_url(url: &str) -> Result<Vec<u8>, String> {
    let url = url.trim();
    let lower = url.to_ascii_lowercase();
    if !(lower.starts_with("https://") || lower.starts_with("http://")) {
        return Err("Нужна ссылка, начинающаяся с https:// или http://.".into());
    }
    let client = reqwest::blocking::Client::builder()
        .timeout(URL_TIMEOUT)
        .user_agent("ddmail-desktop")
        .build()
        .map_err(|e| format!("Не удалось подготовить запрос: {e}"))?;
    let resp = client.get(url).send().map_err(|e| {
        if e.is_timeout() {
            "Сервер не ответил за 20 секунд.".to_string()
        } else if e.is_connect() {
            "Не удалось подключиться к серверу по ссылке.".to_string()
        } else {
            format!("Ссылка недоступна: {e}")
        }
    })?;
    let status = resp.status();
    if !status.is_success() {
        return Err(format!("Ссылка недоступна: сервер ответил {status}."));
    }
    if resp.content_length().is_some_and(|n| n as usize > MAX_BYTES) {
        return Err("По ссылке слишком большой файл — можно до 10 МБ.".into());
    }
    let mut bytes = Vec::new();
    resp.take(MAX_BYTES as u64 + 1)
        .read_to_end(&mut bytes)
        .map_err(|e| format!("Не удалось дочитать ответ: {e}"))?;
    Ok(bytes)
}

/// Записать нормализованную картинку для диалога; возвращает имя файла.
pub fn store(key: &MergeKey, png: &[u8]) -> Result<String, String> {
    let dir = dir().ok_or("Нет папки настроек.")?;
    std::fs::create_dir_all(&dir).map_err(|e| format!("Не удалось создать папку аватарок: {e}"))?;
    let name = file_name(key);
    crate::account_store::write_atomic(&dir.join(&name), png)
        .map_err(|e| format!("Не удалось сохранить: {e}"))?;
    Ok(name)
}

pub fn remove_file(name: &str) {
    if let Some(dir) = dir() {
        let _ = std::fs::remove_file(dir.join(name));
    }
}

/// PNG → картинка Slint.
pub fn to_image(png: &[u8]) -> Option<Image> {
    let rgba = image::load_from_memory(png).ok()?.to_rgba8();
    let (w, h) = rgba.dimensions();
    Some(Image::from_rgba8(SharedPixelBuffer::<Rgba8Pixel>::clone_from_slice(rgba.as_raw(), w, h)))
}

/// Все свои аватарки: id диалога → картинка. По id, без учётки: так их
/// находит и сайдбар, где строки знают только свой диалог.
pub fn load_all(m: &Merges) -> HashMap<String, Image> {
    let Some(dir) = dir() else { return HashMap::new() };
    m.avatars
        .iter()
        .filter_map(|a| {
            let bytes = std::fs::read(dir.join(&a.file)).ok()?;
            Some((a.key.id.clone(), to_image(&bytes)?))
        })
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    fn png(w: u32, h: u32) -> Vec<u8> {
        let img = image::RgbaImage::from_pixel(w, h, image::Rgba([10, 200, 30, 255]));
        let mut out = Vec::new();
        img.write_to(&mut std::io::Cursor::new(&mut out), image::ImageFormat::Png).unwrap();
        out
    }

    #[test]
    fn normalize_crops_square_and_downscales() {
        let out = normalize(&png(800, 400)).unwrap();
        let img = image::load_from_memory(&out).unwrap();
        assert_eq!((img.width(), img.height()), (256, 256));
        let small = image::load_from_memory(&normalize(&png(64, 100)).unwrap()).unwrap();
        assert_eq!((small.width(), small.height()), (64, 64));
    }

    #[test]
    fn normalize_rejects_non_images_and_tiny() {
        assert!(normalize(b"<html>not an image</html>").is_err());
        assert!(normalize(&[]).is_err());
        assert!(normalize(&png(8, 8)).is_err());
    }

    #[test]
    fn url_must_be_http() {
        assert!(fetch_url("ftp://x/y.png").is_err());
        assert!(fetch_url("file:///c:/a.png").is_err());
    }

    #[test]
    fn file_name_is_stable_and_distinct() {
        let a = MergeKey { account: "lucky@mail.letotam.ru".into(), id: "me@a|bob@x".into() };
        let b = MergeKey { account: "lucky@mail.letotam.ru".into(), id: "me@a|carol@x".into() };
        assert_eq!(file_name(&a), file_name(&a));
        assert_ne!(file_name(&a), file_name(&b));
        assert!(file_name(&a).ends_with(".png"));
    }
}
