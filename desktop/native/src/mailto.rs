//! Разбор `mailto:`-URL (RFC 6068) — для ссылок из писем и для запуска
//! клиента системой как обработчика схемы.
//!
//! Свой парсер, а не `split('?')`: адресов в пути бывает несколько через
//! запятую, `to=` в запросе их дополняет, значения percent-кодированы в UTF-8
//! (кириллическая тема приходит как `%D0%A1%D1%87…`), а `+` по RFC 6068 —
//! буквальный плюс, не пробел (`user+tag@…` ломался бы form-декодом).

/// Что содержит ссылка. Поля, которых в ссылке нет, пустые.
#[derive(Debug, Default, Clone, PartialEq, Eq)]
pub struct Mailto {
    pub to: Vec<String>,
    pub cc: Vec<String>,
    pub bcc: Vec<String>,
    pub subject: String,
    pub body: String,
}

/// `None` — не `mailto:` вовсе. Пустая ссылка `mailto:` валидна: это «новое
/// письмо» без адресата.
pub fn parse(url: &str) -> Option<Mailto> {
    let url = url.trim();
    let scheme = url.get(..7)?;
    if !scheme.eq_ignore_ascii_case("mailto:") {
        return None;
    }
    let rest = &url[7..];
    // Фрагмент к письму отношения не имеет.
    let rest = rest.split('#').next().unwrap_or("");
    let (path, query) = rest.split_once('?').unwrap_or((rest, ""));

    let mut m = Mailto::default();
    push_addrs(&mut m.to, &decode(path));
    for pair in query.split('&').filter(|p| !p.is_empty()) {
        let (k, v) = pair.split_once('=').unwrap_or((pair, ""));
        let v = decode(v);
        match decode(k).to_ascii_lowercase().as_str() {
            "to" => push_addrs(&mut m.to, &v),
            "cc" => push_addrs(&mut m.cc, &v),
            "bcc" => push_addrs(&mut m.bcc, &v),
            // Повтор поля — склеиваем, а не теряем первое значение.
            "subject" if m.subject.is_empty() => m.subject = v,
            "body" => {
                if !m.body.is_empty() {
                    m.body.push('\n');
                }
                m.body.push_str(&v);
            }
            // in-reply-to, keywords и прочие заголовки композер не задаёт.
            _ => {}
        }
    }
    // Тема — одна строка: перевод строки в заголовке сервер отбивает 400.
    m.subject = m.subject.replace(['\r', '\n'], " ").trim().to_string();
    m.body = m.body.replace("\r\n", "\n");
    Some(m)
}

/// Адреса через запятую; пустые куски и дубли выбрасываем, регистр не трогаем
/// (локальная часть формально регистрозависима).
fn push_addrs(out: &mut Vec<String>, list: &str) {
    for a in list.split(',') {
        let a = a.trim();
        // Перевод строки в адресе — попытка подсунуть заголовок.
        if a.is_empty() || a.contains(['\r', '\n']) {
            continue;
        }
        if !out.iter().any(|x| x.eq_ignore_ascii_case(a)) {
            out.push(a.to_string());
        }
    }
}

/// Percent-декод в UTF-8. Битая последовательность остаётся как была, а
/// невалидный UTF-8 заменяется на U+FFFD — ссылку открываем в любом случае.
fn decode(s: &str) -> String {
    let b = s.as_bytes();
    let mut out = Vec::with_capacity(b.len());
    let mut i = 0;
    while i < b.len() {
        if b[i] == b'%' && i + 2 < b.len() {
            if let (Some(h), Some(l)) = (hex(b[i + 1]), hex(b[i + 2])) {
                out.push(h << 4 | l);
                i += 3;
                continue;
            }
        }
        out.push(b[i]);
        i += 1;
    }
    String::from_utf8_lossy(&out).into_owned()
}

fn hex(c: u8) -> Option<u8> {
    match c {
        b'0'..=b'9' => Some(c - b'0'),
        b'a'..=b'f' => Some(c - b'a' + 10),
        b'A'..=b'F' => Some(c - b'A' + 10),
        _ => None,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn not_mailto() {
        assert_eq!(parse("https://example.invalid"), None);
        assert_eq!(parse("mail"), None);
    }

    #[test]
    fn plain_address_and_empty_link() {
        let m = parse("mailto:info@example.invalid").unwrap();
        assert_eq!(m.to, vec!["info@example.invalid"]);
        assert_eq!(parse("MAILTO:").unwrap(), Mailto::default());
    }

    /// Критерий из backlog: кириллические тема и текст доезжают целыми.
    #[test]
    fn cyrillic_subject_and_body() {
        let m = parse(
            "mailto:info@example.invalid?subject=%D0%A1%D1%87%D1%91%D1%82&body=%D0%94%D0%BE%D0%B1%D1%80%D1%8B%D0%B9%0D%0A%D0%B4%D0%B5%D0%BD%D1%8C",
        )
        .unwrap();
        assert_eq!(m.subject, "Счёт");
        assert_eq!(m.body, "Добрый\nдень");
    }

    #[test]
    fn several_recipients_from_path_and_query() {
        let m = parse("mailto:a@x.invalid,%20b@x.invalid?to=c@x.invalid&cc=d@x.invalid,e@x.invalid&bcc=f@x.invalid")
            .unwrap();
        assert_eq!(m.to, vec!["a@x.invalid", "b@x.invalid", "c@x.invalid"]);
        assert_eq!(m.cc, vec!["d@x.invalid", "e@x.invalid"]);
        assert_eq!(m.bcc, vec!["f@x.invalid"]);
    }

    /// `+` — буквальный плюс (RFC 6068 §5), а не пробел из form-кодирования.
    #[test]
    fn plus_is_literal() {
        let m = parse("mailto:user+tag@x.invalid?subject=a+b").unwrap();
        assert_eq!(m.to, vec!["user+tag@x.invalid"]);
        assert_eq!(m.subject, "a+b");
    }

    #[test]
    fn header_injection_is_neutralized() {
        let m = parse(
            "mailto:a@x.invalid%0D%0ABcc:evil@x.invalid?subject=hi%0D%0ABcc:%20evil@x.invalid",
        )
        .unwrap();
        assert!(m.to.is_empty());
        assert_eq!(m.subject, "hi  Bcc: evil@x.invalid");
    }

    #[test]
    fn broken_escapes_and_fragment() {
        let m = parse("mailto:a@x.invalid?subject=100%25%zz%4#frag").unwrap();
        assert_eq!(m.subject, "100%%zz%4");
        let m = parse("mailto:a@x.invalid?subject=%").unwrap();
        assert_eq!(m.subject, "%");
    }

    #[test]
    fn duplicate_addresses_collapse() {
        let m = parse("mailto:A@x.invalid?to=a@x.invalid").unwrap();
        assert_eq!(m.to, vec!["A@x.invalid"]);
    }
}
