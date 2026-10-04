//! Links and files handed to the system: URL extraction for event cards,
//! the click-target rules (scheme allow-list, byte-exact HTML hrefs —
//! contract §4в), «Открыть с помощью…» handlers, and opening saved
//! attachments.

use super::*;

/// Pull every meeting link out of free text (calendar event fields keep them as
/// plain text more often than not) — both `http(s)://` URLs and bare hosts
/// without a scheme. Trailing punctuation is trimmed; order preserved,
/// duplicates dropped.
pub(crate) fn extract_urls(texts: &[&str]) -> Vec<String> {
    let mut seen = HashSet::new();
    let mut out = Vec::new();
    for t in texts {
        // Полноценные URL со схемой — и заодно их границы: голый хост внутри
        // уже найденного URL повторно ссылкой становиться не должен.
        let mut spans: Vec<(usize, usize)> = Vec::new();
        for m in full_url_re().find_iter(t) {
            spans.push((m.start(), m.end()));
            let url = trim_url_tail(m.as_str());
            if !url.is_empty() && seen.insert(url.to_string()) {
                out.push(url.to_string());
            }
        }
        let field = t.trim();
        for c in bare_host_re().captures_iter(t) {
            let Some(g) = c.get(1) else { continue };
            if spans.iter().any(|(s, e)| g.start() < *e && g.end() > *s) {
                continue;
            }
            // Символ перед кандидатом: `@` — это адрес почты, буква/цифра/
            // точка/дефис — обрезок более длинной строки, не хост.
            if let Some(prev) = t[..g.start()].chars().next_back() {
                if prev == '@'
                    || prev == '.'
                    || prev == '-'
                    || prev == '/'
                    || prev == ':'
                    || prev.is_alphanumeric()
                {
                    continue;
                }
            }
            let cand = trim_url_tail(g.as_str());
            let whole_field = cand == field;
            if let Some(url) = bare_host_to_url(cand, whole_field) {
                if seen.insert(url.clone()) {
                    out.push(url);
                }
            }
        }
    }
    out
}

pub(crate) fn full_url_re() -> &'static regex::Regex {
    use std::sync::OnceLock;
    static RE: OnceLock<regex::Regex> = OnceLock::new();
    RE.get_or_init(|| regex::Regex::new(r#"https?://[^\s<>"'\)\]]+"#).unwrap())
}

/// Голый хост (без схемы) с необязательным портом и путём. Ведущий
/// разделитель нужен вместо look-behind, которого нет в `regex`.
pub(crate) fn bare_host_re() -> &'static regex::Regex {
    use std::sync::OnceLock;
    static RE: OnceLock<regex::Regex> = OnceLock::new();
    RE.get_or_init(|| {
        regex::Regex::new(
            r#"(?i)(?:^|[\s,;(\[<«"'])((?:[a-z0-9](?:[a-z0-9\-]*[a-z0-9])?\.)+[a-z]{2,24}(?::\d{2,5})?(?:/[^\s<>"'\)\]»]*)?)"#,
        )
        .unwrap()
    })
}

pub(crate) fn trim_url_tail(s: &str) -> &str {
    s.trim_end_matches(['.', ',', ';', ':', '!', '?', '»'])
}

/// Признать голый хост ссылкой и достроить ему схему. Календарные ссылки
/// сплошь без схемы: Jitsi/«кабинет» кладут в LOCATION `meet.example.kz/room`,
/// CONFERENCE/X-…-CONFERENCE — такой же голый хост. Требовать `https?://`
/// значит показывать такую встречу просто текстом, кликать нечего.
///
/// Ложные срабатывания душим тремя правилами: TLD — буквы и не расширение
/// файла (`отчет.docx`), кандидат должен быть либо с путём/портом, либо
/// `www.`, либо занимать поле целиком (LOCATION, который И ЕСТЬ хост).
pub(crate) fn bare_host_to_url(cand: &str, whole_field: bool) -> Option<String> {
    let host = cand.split(['/', ':']).next().unwrap_or(cand);
    let tld = host.rsplit('.').next().unwrap_or("");
    if tld.len() < 2 || !tld.chars().all(|c| c.is_ascii_alphabetic()) {
        return None;
    }
    const FILE_EXT: &[&str] = &[
        "doc", "docx", "xls", "xlsx", "ppt", "pptx", "pdf", "txt", "csv", "rtf", "odt", "ods",
        "zip", "rar", "png", "jpg", "jpeg", "gif", "bmp", "svg", "mp3", "mp4", "avi", "mov", "exe",
        "msi", "dll", "ics", "eml", "msg", "sig", "json", "xml", "html", "htm",
    ];
    if FILE_EXT.contains(&tld.to_ascii_lowercase().as_str()) {
        return None;
    }
    let has_path_or_port = cand.contains('/') || cand.contains(':');
    if !has_path_or_port && !whole_field && !host.to_ascii_lowercase().starts_with("www.") {
        return None;
    }
    Some(format!("https://{cand}"))
}

/// Что открывать по клику в значении свойства события: полноценный URL —
/// как есть, голый хост — с достроенной схемой. None → это не ссылка.
pub(crate) fn link_target(value: &str) -> Option<String> {
    let v = value.trim();
    if v.starts_with("http://") || v.starts_with("https://") {
        return Some(trim_url_tail(v).to_string());
    }
    bare_host_to_url(trim_url_tail(v), true)
}

/// Откуда пришла ссылка — это решает, можно ли её править.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub(crate) enum LinkOrigin {
    /// `<a href>` из письма: строка уже готова и трогать её нельзя.
    Html,
    /// Свободный текст (LOCATION/CONFERENCE, свойства события, поля правки):
    /// схемы может не быть, а хвостовая пунктуация — часть фразы, не ссылки.
    Text,
}

/// Схема URL, если она есть: всё до первого `:`, начинается с буквы, дальше
/// буквы/цифры/`+`/`-`. Точку в схеме не допускаем намеренно — иначе
/// `meet.example.kz:8443/room` выглядел бы схемой `meet.example.kz`.
pub(crate) fn url_scheme(v: &str) -> Option<String> {
    let head = v.split(':').next()?;
    if head.len() == v.len() || head.is_empty() {
        return None;
    }
    let mut chars = head.chars();
    if !chars.next()?.is_ascii_alphabetic() {
        return None;
    }
    if !chars.all(|c| c.is_ascii_alphanumeric() || c == '+' || c == '-') {
        return None;
    }
    Some(head.to_ascii_lowercase())
}

/// Схемы, которые вообще передаём системе. Письмо — враждебный ввод, а
/// системный обработчик открывает не только браузер: `file:` тянет с сетевой
/// шары, `search-ms:`/`ms-*:` — целое семейство хэндлеров, которыми рассылки
/// запускают что попало. Всё, чего тут нет, до `ShellExecute`/`xdg-open`
/// не доходит.
pub(crate) const CLICKABLE_SCHEMES: &[&str] = &["http", "https", "mailto", "tel"];

/// Что открывать по клику по ссылке. Единственная точка, где решается судьба
/// клика — и в пузыре письма, и в карточке события.
///
/// href из письма уходит обработчику байт в байт: параметры кнопки бывают
/// одноразовыми, а хвостовой `?`/`.` в них — значимым, так что подчистка
/// пунктуации (`trim_url_tail`, она для плоского текста) сломала бы ссылку.
/// Голому хосту схему достраиваем в обоих случаях — календари кладут созвон в
/// LOCATION без схемы, да и в письмах `www.example.com/x` без `https://`
/// встречается.
pub(crate) fn click_target(value: &str, origin: LinkOrigin) -> Option<String> {
    let v = value.trim();
    match url_scheme(v) {
        Some(s) if !CLICKABLE_SCHEMES.contains(&s.as_str()) => {
            eprintln!("link: схему {s:?} не открываем — {v}");
            None
        }
        // `mailto:`/`tel:` через link_target гнать нельзя: он бы принял
        // `mailto` за хост и выдал `https://mailto:info@example.com`.
        Some(_) if origin == LinkOrigin::Html => Some(v.to_string()),
        Some(_) => Some(trim_url_tail(v).to_string()),
        None => link_target(v),
    }
}

#[cfg(all(test, target_os = "linux"))]
mod url_handler_tests {
    use super::url_handler_apps_in;

    fn write(dir: &std::path::Path, name: &str, body: &str) {
        std::fs::write(dir.join(name), body).unwrap();
    }

    /// Берём только обработчиков http(s), пропускаем NoDisplay/Terminal и
    /// схлопываем дубли по имени — один браузер обычно лежит и в пользовательском
    /// каталоге, и в системном.
    #[test]
    fn picks_url_handlers_and_dedupes() {
        // Каталог убирает `TempDir` в своём `Drop`, а не строка в конце теста:
        // уборка в конце не случается, когда падает assert выше, и каталог
        // остаётся в `/tmp` ровно в том прогоне, после которого в него полезут
        // разбираться.
        let base = tempfile::Builder::new().prefix("ddmail-urlh-").tempdir().expect("tempdir");
        let user = base.path().join("user");
        let sys = base.path().join("sys");
        std::fs::create_dir_all(&user).unwrap();
        std::fs::create_dir_all(&sys).unwrap();

        write(
            &user,
            "chrome.desktop",
            "[Desktop Entry]\nType=Application\nName=Chrome\nExec=chrome %U\nMimeType=text/html;x-scheme-handler/https;\n",
        );
        // Тот же браузер в системном каталоге — в меню должен попасть один раз.
        write(
            &sys,
            "chrome.desktop",
            "[Desktop Entry]\nType=Application\nName=Chrome\nExec=/usr/bin/chrome %U\nMimeType=x-scheme-handler/https;\n",
        );
        write(
            &sys,
            "yandex.desktop",
            "[Desktop Entry]\nType=Application\nName=Yandex\nName[ru]=Яндекс Браузер\nExec=yb %U\nMimeType=x-scheme-handler/http;\n",
        );
        // Не обработчик ссылок.
        write(
            &sys,
            "gimp.desktop",
            "[Desktop Entry]\nType=Application\nName=GIMP\nExec=gimp %F\nMimeType=image/png;\n",
        );
        // Скрытые и терминальные не показываем.
        write(
            &sys,
            "hidden.desktop",
            "[Desktop Entry]\nType=Application\nName=Hidden\nNoDisplay=true\nMimeType=x-scheme-handler/https;\n",
        );
        write(
            &sys,
            "lynx.desktop",
            "[Desktop Entry]\nType=Application\nName=Lynx\nTerminal=true\nMimeType=x-scheme-handler/http;\n",
        );
        // Name из секции [Desktop Action] не должен подменять основной.
        write(
            &sys,
            "acts.desktop",
            "[Desktop Entry]\nType=Application\nName=WithActions\nMimeType=x-scheme-handler/https;\n\n[Desktop Action new]\nName=Новое окно\n",
        );

        let dirs = [user.to_string_lossy().to_string(), sys.to_string_lossy().to_string()];
        let apps = url_handler_apps_in(&dirs);
        let names: Vec<&str> = apps.iter().map(|(n, _)| n.as_str()).collect();

        assert!(names.contains(&"Chrome"), "обработчик https обязателен: {names:?}");
        assert_eq!(names.iter().filter(|n| **n == "Chrome").count(), 1, "дубль по имени");
        assert!(names.contains(&"Яндекс Браузер"), "берём Name[ru]: {names:?}");
        assert!(names.contains(&"WithActions"), "Name основной секции: {names:?}");
        for skipped in ["GIMP", "Hidden", "Lynx", "Новое окно"] {
            assert!(!names.contains(&skipped), "{skipped} не должен попасть: {names:?}");
        }
        // Пользовательский каталог первым: у Chrome должен остаться его путь.
        let chrome = apps.iter().find(|(n, _)| n == "Chrome").unwrap();
        assert!(chrome.1.starts_with(&user), "пользовательский каталог приоритетнее");
    }
}

#[cfg(test)]
mod event_link_tests {
    use super::{extract_urls, link_target};

    /// Реальный кейс: LOCATION вида `meet.small.kz/dit_leads` — Jitsi-ссылка
    /// без схемы. Раньше регексп требовал `https?://`, и встреча показывалась
    /// просто текстом.
    #[test]
    fn schemeless_location_becomes_link() {
        assert_eq!(
            extract_urls(&["meet.small.kz/dit_leads", ""]),
            vec!["https://meet.small.kz/dit_leads".to_string()]
        );
    }

    /// Голый хост без пути — ссылка, когда он и есть всё поле (LOCATION) или
    /// начинается с www.; внутри фразы — нет (там это, скорее всего, не хост).
    #[test]
    fn bare_host_needs_path_www_or_whole_field() {
        assert_eq!(extract_urls(&["telemost.yandex.ru"]), vec!["https://telemost.yandex.ru"]);
        assert_eq!(
            extract_urls(&["см. www.example.com за деталями"]),
            vec!["https://www.example.com"]
        );
        assert!(extract_urls(&["созвон в 13.00, ответственный за подготовку — Иванов"]).is_empty());
    }

    /// Ложные срабатывания: имена файлов, адреса почты, числа.
    #[test]
    fn not_links() {
        assert!(extract_urls(&["во вложении report.docx и smeta.xlsx"]).is_empty());
        assert!(extract_urls(&["пишите на ivanov@small.kz"]).is_empty());
        assert!(extract_urls(&["версия 1.2.3"]).is_empty());
    }

    /// URL со схемой не должен продублироваться голым хостом из своей же
    /// середины.
    #[test]
    fn full_url_not_duplicated() {
        assert_eq!(
            extract_urls(&["ссылка: https://meet.small.kz/dit_leads"]),
            vec!["https://meet.small.kz/dit_leads".to_string()]
        );
    }

    /// Клик по свойству события открывает нормализованную ссылку, а не
    /// «относительный путь», который системный обработчик молча проглотит.
    #[test]
    fn link_target_normalizes() {
        assert_eq!(
            link_target("meet.small.kz/dit_leads").as_deref(),
            Some("https://meet.small.kz/dit_leads")
        );
        assert_eq!(
            link_target("https://telemost.yandex.ru/j/123").as_deref(),
            Some("https://telemost.yandex.ru/j/123")
        );
        assert_eq!(link_target("Переговорная 3"), None);
    }
}

#[cfg(test)]
mod click_target_tests {
    use super::{LinkOrigin, click_target};

    /// Тот самый баг: href кнопки уходил в браузер через `cmd /C start`, и `&`
    /// обрывал командную строку — сервер получал ссылку без токена и отвечал
    /// 4xx. Открывать обязаны ровно то, что стоит в href, до последнего
    /// символа. Менять эту проверку — только вместе с `shellopen`.
    #[test]
    fn html_href_survives_byte_for_byte() {
        let url = "https://example.invalid/c?token=abc&utm_source=mail&id=42";
        assert_eq!(click_target(url, LinkOrigin::Html).as_deref(), Some(url));
    }

    /// Хвостовую пунктуацию режем только у ссылок из текста: в href она бывает
    /// значимой (`?` пустого query, точка в пути), а во фразе «смотри
    /// https://x.invalid/a.» точка — часть фразы.
    #[test]
    fn tail_trimmed_only_in_plain_text() {
        assert_eq!(
            click_target("https://x.invalid/a?", LinkOrigin::Html).as_deref(),
            Some("https://x.invalid/a?")
        );
        assert_eq!(
            click_target("https://x.invalid/a.", LinkOrigin::Text).as_deref(),
            Some("https://x.invalid/a")
        );
    }

    /// `mailto:`/`tel:` отдаём системе как есть: через `link_target` они бы
    /// стали `https://mailto:info@example.invalid` — «хостом» он считал схему.
    #[test]
    fn mail_and_tel_pass_through() {
        assert_eq!(
            click_target("mailto:info@example.invalid", LinkOrigin::Html).as_deref(),
            Some("mailto:info@example.invalid")
        );
        assert_eq!(
            click_target("tel:+70000000000", LinkOrigin::Html).as_deref(),
            Some("tel:+70000000000")
        );
    }

    /// Письмо — враждебный ввод: системный обработчик открывает не только
    /// браузер, поэтому всё вне белого списка не доходит до ShellExecute.
    #[test]
    fn hostile_schemes_refused() {
        for url in [
            "file:///C:/Windows/System32/calc.exe",
            "search-ms:query=passwords",
            "ms-msdt:/id",
            "vbscript:msgbox",
        ] {
            assert_eq!(click_target(url, LinkOrigin::Html), None, "{url}");
        }
    }

    /// Голый хост остаётся ссылкой, и хост с портом не должен выглядеть
    /// схемой `meet.example.invalid` (в схеме точек не бывает).
    #[test]
    fn bare_host_still_gets_a_scheme() {
        assert_eq!(
            click_target("meet.example.invalid/room", LinkOrigin::Text).as_deref(),
            Some("https://meet.example.invalid/room")
        );
        assert_eq!(
            click_target("meet.example.invalid:8443/room", LinkOrigin::Html).as_deref(),
            Some("https://meet.example.invalid:8443/room")
        );
        assert_eq!(click_target("Переговорная 3", LinkOrigin::Text), None);
    }
}

/// Handle a clicked link from a bubble (UI thread). Internal `ddmail-attach:`
/// links trigger an attachment download via the engine; everything else opens
/// in the system browser. `origin` решает, можно ли ссылку нормализовать —
/// см. `click_target`.
pub(crate) fn handle_link(_ui: &MainWindow, url: String, origin: LinkOrigin) {
    if let Some(rest) = url.strip_prefix("ddmail-attach:") {
        // folder|uid|index|filename (folder/filename percent-кодированы)
        let parts: Vec<&str> = rest.splitn(4, '|').collect();
        if parts.len() == 4 {
            if let (Ok(uid), Ok(index)) = (parts[1].parse::<u32>(), parts[2].parse::<usize>()) {
                let folder = att_url_decode(parts[0]);
                let filename = att_url_decode(parts[3]);
                SHARED.with(|s| {
                    if let Some(sh) = s.borrow().as_ref() {
                        if let Some(etx) = sh.engine_tx.borrow().as_ref() {
                            println!("download attachment: {filename}");
                            let _ = etx.send(engine::EngineCmd::DownloadAttachment {
                                folder,
                                uid,
                                index,
                                filename,
                                account_key: sh.cur_account_key.borrow().clone(),
                                save_to: None,
                            });
                        } else {
                            eprintln!("attachment: no live engine");
                        }
                    }
                });
            }
        }
        return;
    }
    println!("link click -> {url}");
    // Значения событий (LOCATION/CONFERENCE) бывают без схемы: xdg-open/start
    // приняли бы «meet.example.kz/room» за относительный путь и молча ничего
    // не сделали. Достраиваем https:// на выходе — единая точка для всех
    // кликов (строки-ссылки карточки, свойства события, чипы пузыря).
    //
    // None — это не ссылка либо схема не из белого списка: открывать «как
    // есть» на всякий случай нельзя, ровно от этого белый список и защищает.
    match click_target(&url, origin) {
        Some(target) => open_external(&target),
        None => eprintln!("link click: нечего открывать — {url}"),
    }
}

/// Resolve a binary name to a full path via `PATH`. Avoids a `which` crate
/// dependency for the one place we need it.
#[cfg(target_os = "linux")]
pub(crate) fn which_bin(name: &str) -> Result<std::path::PathBuf, ()> {
    let path = std::env::var_os("PATH").ok_or(())?;
    for dir in std::env::split_paths(&path) {
        let cand = dir.join(name);
        if cand.is_file() {
            return Ok(cand);
        }
    }
    Err(())
}

/// Приложения, умеющие открыть http(s)-ссылку — для подменю «Открыть с
/// помощью…». Читаются один раз при старте: набор меняется установкой
/// программ, а не в течение сессии.
///
/// Смотрим `MimeType` у .desktop-файлов. Пользовательский каталог идёт первым,
/// и дубли по имени отбрасываются: один и тот же браузер обычно лежит и в
/// `~/.local/share/applications`, и в `/usr/share/applications` (Chrome у нас
/// втроём), а в меню он нужен один раз.
#[cfg(target_os = "linux")]
pub(crate) fn url_handler_apps() -> Vec<(String, std::path::PathBuf)> {
    let home = std::env::var("HOME").unwrap_or_default();
    url_handler_apps_in(&[
        format!("{home}/.local/share/applications"),
        "/usr/share/applications".to_string(),
    ])
}

/// Разбор каталогов .desktop — отдельно от путей, чтобы это можно было
/// проверить тестом на подставном каталоге.
#[cfg(target_os = "linux")]
pub(crate) fn url_handler_apps_in(dirs: &[String]) -> Vec<(String, std::path::PathBuf)> {
    let mut out: Vec<(String, std::path::PathBuf)> = Vec::new();
    let mut seen: HashSet<String> = HashSet::new();
    for dir in dirs {
        let Ok(rd) = std::fs::read_dir(&dir) else { continue };
        let mut paths: Vec<std::path::PathBuf> = rd
            .filter_map(Result::ok)
            .map(|e| e.path())
            .filter(|p| p.extension().is_some_and(|x| x == "desktop"))
            .collect();
        // Порядок каталога произволен — сортируем, чтобы меню не перетасовывалось
        // от запуска к запуску.
        paths.sort();
        for path in paths {
            let Ok(text) = std::fs::read_to_string(&path) else { continue };
            // Нас интересует только секция [Desktop Entry]: у Actions свои
            // Name= и они бы перебили основное.
            let entry = match text.split("\n[").next() {
                Some(first) if first.contains("[Desktop Entry]") => first,
                _ => continue,
            };
            let field = |key: &str| -> Option<&str> {
                entry.lines().find_map(|l| l.strip_prefix(key)?.strip_prefix('=')).map(str::trim)
            };
            if !field("MimeType").is_some_and(|m| {
                m.contains("x-scheme-handler/https") || m.contains("x-scheme-handler/http")
            }) {
                continue;
            }
            if field("NoDisplay").is_some_and(|v| v.eq_ignore_ascii_case("true"))
                || field("Hidden").is_some_and(|v| v.eq_ignore_ascii_case("true"))
                || field("Terminal").is_some_and(|v| v.eq_ignore_ascii_case("true"))
            {
                continue;
            }
            let name = field("Name[ru]").or_else(|| field("Name")).unwrap_or("");
            if name.is_empty() || !seen.insert(name.to_lowercase()) {
                continue;
            }
            out.push((name.to_string(), path));
        }
    }
    out
}

#[cfg(not(target_os = "linux"))]
pub(crate) fn url_handler_apps() -> Vec<(String, std::path::PathBuf)> {
    // На Windows выбор приложения для URL живёт в системных настройках, а не в
    // per-app списке: подменю там просто не показываем.
    Vec::new()
}

/// Открыть ссылку конкретным приложением из `url_handler_apps`.
#[cfg(target_os = "linux")]
pub(crate) fn open_with_app(desktop: &std::path::Path, url: &str) {
    // `gio launch` сам разбирает Exec с его %u/%U/%f и полями вроде
    // DBusActivatable — руками это воспроизводить незачем.
    match std::process::Command::new("gio").arg("launch").arg(desktop).arg(url).spawn() {
        Ok(_) => {}
        Err(e) => {
            eprintln!("open_with_app: gio launch failed ({e}) — открываю по умолчанию");
            open_external(url);
        }
    }
}

#[cfg(not(target_os = "linux"))]
pub(crate) fn open_with_app(_desktop: &std::path::Path, url: &str) {
    open_external(url);
}

/// Отдать URL системному браузеру. Никакого шелла в середине — `cmd /C start`
/// резал ссылку на первом `&`, и кнопка из письма приходила на сервер без
/// параметров (подробности в `ddmail_core::shellopen`). Работа уходит в свой
/// поток: `ShellExecuteW` может подождать shell-расширение, а мы на UI-потоке.
pub(crate) fn open_external(url: &str) {
    let url = url.to_string();
    std::thread::spawn(move || {
        if let Err(e) = ddmail_core::shellopen::open_url(&url) {
            eprintln!("open_external: не открылось {url}: {e}");
        }
    });
}

/// Открыть сохранённый файл системным обработчиком — в отличие от
/// open_external, ЗАБИРАЕМ вердикт в фоновом потоке: `xdg-open` молча
/// возвращает ненулевой код (а `ShellExecuteW` — код ≤ 32), когда обработчика
/// нет или путь его не устроил, и без проверки клик по вложению «просто ничего
/// не делает». Провал показывается тостом (та же янтарная плашка, что и у
/// ошибок отправки). Поток нужен из-за Unix-ветки: там вердикт — exit-код
/// обработчика, и его приходится ждать.
pub(crate) fn open_saved_file(path: &str) {
    let path = path.to_string();
    std::thread::spawn(move || {
        let verdict = ddmail_core::shellopen::open_path_checked(std::path::Path::new(&path));
        let Err(reason) = verdict else { return };
        eprintln!("open_saved_file: {path}: {reason}");
        // Тостовые окна — только с UI-потока.
        if let Some(weak) = UI_WEAK.get() {
            let _ = weak.clone().upgrade_in_event_loop(move |_ui| {
                toast_window::show(
                    2, // amber
                    0,
                    "Не удалось открыть вложение",
                    &format!("Файл сохранён: {path}\nПричина: {reason}"),
                    false,
                    600,
                    || {},
                    || {},
                    || {},
                );
            });
        }
    });
}

/// Links and attachment chips inside bubbles: click, hover cursor, and the
/// context-menu entries (open / copy / open with… / open or save attachment).
pub(crate) fn wire_bubble_links(ui: &MainWindow, shared: &Rc<Shared>) {
    // Link click — resolved right here against the UI-thread copy of the
    // link rects (the same ones the hover cursor reads), not through the
    // render worker: a click must not wait behind a conversation's layout.
    let ui_weak_hit = ui.as_weak();
    let sh_hit = shared.clone();
    ui.on_hit_test(move |row, x, y| {
        // A click that ends a drag-selection is not a link click.
        if sh_hit.sel_suppress_click.replace(false) {
            return;
        }
        let hit = sh_hit
            .row_links
            .borrow()
            .get(row as usize)
            .and_then(|links| links.iter().find(|l| l.contains(x, y)))
            .map(|l| l.href.clone());
        match hit {
            // Resolved URLs (incl. internal ddmail-attach:* schemes) go to
            // handle_link from the event loop, as before — not from inside
            // the pointer callback.
            Some(url) => {
                let _ = ui_weak_hit
                    .upgrade_in_event_loop(move |ui| handle_link(&ui, url, LinkOrigin::Html));
            }
            None => println!("click row {row} @({x:.0},{y:.0}) — no link"),
        }
    });

    // Pointer-cursor hover query — pure point-in-rect against the UI-thread
    // copy of the link rects, re-evaluated by the binding on every move.
    let sh_hover = shared.clone();
    ui.on_hover_link(move |row, x, y| {
        sh_hover
            .row_links
            .borrow()
            .get(row as usize)
            .map(|links| links.iter().any(|l| l.contains(x, y)))
            .unwrap_or(false)
    });

    // ── Вложения: контекст правого клика ──
    // Перед показом меню пузыря Slint зовёт probe: если под курсором чип
    // вложения (ddmail-attach:-ссылка), меню получает пункты
    // «Открыть/Сохранить»; цель откладывается в sh.ctx_attach.
    let ui_weak_probe = ui.as_weak();
    let sh_probe = shared.clone();
    ui.on_ctx_menu_probe(move |row, x, y| {
        let Some(ui) = ui_weak_probe.upgrade() else { return };
        let href = sh_probe
            .row_links
            .borrow()
            .get(row as usize)
            .and_then(|links| links.iter().find(|l| l.contains(x, y)).map(|l| l.href.clone()));
        let att = href.as_deref().and_then(|u| u.strip_prefix("ddmail-attach:")).and_then(|rest| {
            let p: Vec<&str> = rest.splitn(4, '|').collect();
            if p.len() == 4 {
                if let (Ok(uid), Ok(index)) = (p[1].parse::<u32>(), p[2].parse::<usize>()) {
                    // folder/filename percent-кодированы (att_url_encode) —
                    // в меню и в диалог сохранения идёт человеческое имя.
                    return Some((att_url_decode(p[0]), uid, index, att_url_decode(p[3])));
                }
            }
            None
        });
        ui.set_ctx_attach_name(att.as_ref().map(|a| a.3.clone()).unwrap_or_default().into());
        // Внешняя ссылка под курсором. `ddmail-attach:` сюда не попадает — это
        // вложение, у него свои пункты выше; всё остальное отдаёт
        // `click_target`, он же достраивает схему голому хосту и отсеивает
        // схемы не из белого списка. Origin::Html — «Копировать ссылку» обязано
        // дать ту же строку, что откроет «Открыть ссылку», байт в байт.
        let link = href
            .as_deref()
            .filter(|u| !u.starts_with("ddmail-attach:"))
            .and_then(|u| click_target(u, LinkOrigin::Html));
        ui.set_ctx_link_url(link.clone().unwrap_or_default().into());
        *sh_probe.ctx_link.borrow_mut() = link;
        *sh_probe.ctx_attach.borrow_mut() = att;
    });

    // «Открыть ссылку» / «Копировать ссылку» / «Открыть с помощью…».
    let sh_ol = shared.clone();
    ui.on_open_link(move || {
        if let Some(url) = sh_ol.ctx_link.borrow().clone() {
            println!("ctx open link -> {url}");
            open_external(&url);
        }
    });
    let sh_cl = shared.clone();
    ui.on_copy_link(move || {
        if let Some(url) = sh_cl.ctx_link.borrow().clone() {
            clipboard_set(&url);
        }
    });
    // Клик по адресу в шапке диалога: адрес — в буфер, подтверждение плашкой.
    let ui_weak_ca = ui.as_weak();
    ui.on_copy_address(move |addr| {
        if addr.is_empty() {
            return;
        }
        clipboard_set(&addr);
        if let Some(ui) = ui_weak_ca.upgrade() {
            flash_confirm(&ui, &format!("✓ Скопировано: {addr}"));
        }
    });
    let sh_lw = shared.clone();
    ui.on_open_link_with(move |idx| {
        let Some(url) = sh_lw.ctx_link.borrow().clone() else { return };
        // Индекс приходит из того же списка, которым заполнено подменю, но
        // проверяем: модель и обработчик живут в разных потоках событий.
        if let Some((name, desktop)) = sh_lw.link_apps.get(idx.max(0) as usize) {
            println!("ctx open link with {name} -> {url}");
            open_with_app(desktop, &url);
        }
    });

    // «Открыть …» — тот же путь, что левый клик по чипу: Downloads + запуск.
    let sh_oa = shared.clone();
    ui.on_open_attachment(move || {
        let Some((folder, uid, index, filename)) = sh_oa.ctx_attach.borrow().clone() else {
            return;
        };
        if let Some(etx) = sh_oa.engine_tx.borrow().as_ref() {
            let _ = etx.send(engine::EngineCmd::DownloadAttachment {
                folder,
                uid,
                index,
                filename,
                account_key: sh_oa.cur_account_key.borrow().clone(),
                save_to: None,
            });
        }
    });

    // «Сохранить … как…» — системный диалог сохранения; файл пишется по
    // выбранному пути и НЕ открывается (подтверждение — плашка «✓ Сохранено»).
    let ui_weak_sa = ui.as_weak();
    let sh_sa = shared.clone();
    ui.on_save_attachment(move || {
        let Some(ui) = ui_weak_sa.upgrade() else { return };
        let Some((folder, uid, index, filename)) = sh_sa.ctx_attach.borrow().clone() else {
            return;
        };
        let Some(path) = pick_save_path(&ui, &filename) else { return };
        if let Some(etx) = sh_sa.engine_tx.borrow().as_ref() {
            println!("save attachment: {filename} -> {}", path.display());
            let _ = etx.send(engine::EngineCmd::DownloadAttachment {
                folder,
                uid,
                index,
                filename,
                account_key: sh_sa.cur_account_key.borrow().clone(),
                save_to: Some(path.to_string_lossy().into_owned()),
            });
        }
    });
}
