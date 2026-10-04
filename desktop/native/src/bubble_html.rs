//! The bubble document: the HTML each mail body is wrapped into before
//! `emlrender` lays it out (template + `ddm-` chrome CSS, attachment chips,
//! the text-only and source-viewer variants), and the template epoch that
//! keys the texture cache to it (contract §4б).

use super::*;

pub(crate) fn html_escape(s: &str) -> String {
    s.replace('&', "&amp;").replace('<', "&lt;").replace('>', "&gt;")
}

/// Bubble corner stamp: time-only for today, date+time within the year,
/// and a two-digit year for older mail. Empty string for a missing date.
/// `ts_secs` is MessageBody.date_ts — a Unix timestamp in SECONDS (the server
/// sends date_ts in seconds, unlike the messages table's millisecond `date`).
pub(crate) fn fmt_bubble_time(ts_secs: i64) -> String {
    if ts_secs <= 0 {
        return String::new();
    }
    use chrono::{DateTime, Datelike, Local, TimeZone, Timelike};
    let dt: DateTime<Local> = match Local.timestamp_opt(ts_secs, 0).single() {
        Some(d) => d,
        None => return String::new(),
    };
    let now = Local::now();
    if dt.year() == now.year() && dt.ordinal() == now.ordinal() {
        return format!("{:02}:{:02}", dt.hour(), dt.minute());
    }
    if dt.year() == now.year() {
        return format!("{:02}.{:02} {:02}:{:02}", dt.day(), dt.month(), dt.hour(), dt.minute());
    }
    format!(
        "{:02}.{:02}.{:02} {:02}:{:02}",
        dt.day(),
        dt.month(),
        dt.year() % 100,
        dt.hour(),
        dt.minute()
    )
}

/// Bubble wrapper + email-HTML normalization (tames ugly notification emails).
/// External-resource blocking is applied here per the current `Policy` —
/// images / stylesheets / `url(...)` references to non-allowlisted hosts
/// are replaced with empty `src=""` (kept as `data-blocked-src`). That is
/// presentation only; the loader's own gate is what keeps them off the wire.
pub(crate) fn build_body_html(b: &MessageBody, policy: &policy::Policy, caption: bool) -> String {
    let has_html = b.html.as_deref().map(|h| !h.trim().is_empty()).unwrap_or(false);
    let inner = match b.html.as_deref() {
        Some(h) if !h.trim().is_empty() => {
            let sanitized = sanitize::sanitize_email_html_for(h, policy, &b.from_addr);
            sanitize::block_external(&sanitized, policy, &b.from_addr)
        }
        _ => format!(
            "<div style=\"white-space:pre-wrap\">{}</div>",
            html_escape(b.text.as_deref().unwrap_or(""))
        ),
    };
    // Широкий пузырь — только ВХОДЯЩИМ. Выравнивание «своё справа» держится на
    // `margin-left: auto`, а он работает лишь пока блок уже строки: с
    // `max-width: 100%` пузырь занимает её целиком, и своё письмо уезжало
    // влево во всю ширину. Ради чего вводился широкий пузырь — рассыльные
    // шаблоны на 600 px — приходит только входящим; свои ответы такого не
    // содержат.
    let wide = has_html && !b.is_outgoing;
    bubble_template_wide(
        b.is_outgoing,
        &fmt_bubble_time(b.date_ts),
        &format!(
            "{}{inner}{}{}",
            subject_caption(b, caption),
            attachment_chips(b),
            empty_body_note(b)
        ),
        wide,
    )
}

/// Подпись темой над содержимым пузыря — только в склеенном диалоге
/// (контракт §4, «Склеенный диалог»): там рядом идут письма разных бесед, и
/// без темы не понять, к какой относится пузырь. Мелко, но читается.
pub(crate) fn subject_caption(b: &MessageBody, caption: bool) -> String {
    if !caption {
        return String::new();
    }
    let subject = b.subject.trim();
    let subject = if subject.is_empty() { "(без темы)" } else { subject };
    format!("<div class=\"{CSS_NS}-subj\">{}</div>", html_escape(subject))
}

/// Подсказка своего пузыря в склеенном диалоге: кому ушло письмо. Строка
/// на поле — «Кому» и, если есть, «Копия»; адреса как в заголовке письма.
pub(crate) fn recipients_tip(b: &MessageBody) -> String {
    let join = |v: &[String]| {
        v.iter().map(|a| a.trim()).filter(|a| !a.is_empty()).collect::<Vec<_>>().join(", ")
    };
    let to = join(&b.to);
    let cc = join(&b.cc);
    let mut out = format!("Кому: {}", if to.is_empty() { "—" } else { &to });
    if !cc.is_empty() {
        out.push_str("\nКопия: ");
        out.push_str(&cc);
    }
    out
}

/// Заметка «в письме нет ни текста, ни вложений» — единственный контент
/// пузыря для тела, у которого рисовать нечего. Без неё такой пузырь —
/// пустой прямоугольник с одним временем, визуально неотличимый от
/// «письмо вообще не отобразилось» (и именно так это и читается).
pub(crate) fn empty_body_note(b: &MessageBody) -> String {
    if !engine::body_is_blank(b) {
        return String::new();
    }
    format!("<div class=\"{CSS_NS}-nobody\">(в письме нет ни текста, ни вложений)</div>")
}

/// Text-only bubble — the fallback we render when the HTML body paints nothing
/// at all. Preserves linebreaks via `white-space: pre-wrap`, and still appends
/// attachment chips.
pub(crate) fn build_text_only_html(b: &MessageBody, caption: bool) -> String {
    let escaped = html_escape(b.text.as_deref().unwrap_or(""));
    let inner = format!("<div style=\"white-space:pre-wrap\">{escaped}</div>");
    bubble_template(
        b.is_outgoing,
        &fmt_bubble_time(b.date_ts),
        &format!(
            "{}{inner}{}{}",
            subject_caption(b, caption),
            attachment_chips(b),
            empty_body_note(b)
        ),
    )
}

/// Render width (CSS px) for the source/headers viewer bitmap. The Image is
/// displayed at exactly this width so pointer coords map 1:1 onto the word
/// rects extracted at render time.
pub(crate) const SOURCE_RENDER_W: u32 = 760;

/// HTML for the source viewer: the raw text in a monospace, wrapping <pre>.
/// Verbatim — only HTML-escaped so the markup can't be interpreted.
pub(crate) fn build_source_html(text: &str) -> String {
    format!(
        "<!doctype html><html><head><meta charset=\"utf-8\">\
         <style>html,body{{margin:0;padding:8px;background:#f7f8fa}}\
         pre{{margin:0;font-family:Consolas,'DejaVu Sans Mono',monospace;\
         font-size:12px;line-height:1.4;color:#2b3640;\
         white-space:pre-wrap;word-break:break-word}}</style></head>\
         <body><pre>{}</pre></body></html>",
        html_escape(text)
    )
}

/// Percent-кодирование частей `ddmail-attach:`-URL: кодируем сами,
/// детерминированно (всё, кроме ASCII-букв/цифр и `.-_~`), а att_url_decode
/// на выходе восстанавливает исходную строку байт-в-байт.
///
/// Потребовалось это из-за браузерных бэкендов, которых в сборке больше нет:
/// они отдавали хит-тесту НОРМАЛИЗОВАННЫЙ `a.href` и сами percent-кодировали
/// в нём всё не-ASCII, так что кириллическое имя вложения прилетало в Rust
/// изуродованным (%D0%9E…). Своя кодировка снимает вопрос независимо от того,
/// кто формирует href, поэтому остаётся.
pub(crate) fn att_url_encode(s: &str) -> String {
    let mut out = String::with_capacity(s.len() * 3);
    for b in s.bytes() {
        match b {
            b'0'..=b'9' | b'a'..=b'z' | b'A'..=b'Z' | b'.' | b'-' | b'_' | b'~' => {
                out.push(b as char)
            }
            _ => out.push_str(&format!("%{b:02X}")),
        }
    }
    out
}

/// Обратное к att_url_encode. Терпимо к легаси-строкам: сырые байты без `%`
/// проходят как есть (старые кэшированные рендеры со старым форматом href),
/// а %-последовательности от нормализации браузера декодируются так же.
pub(crate) fn att_url_decode(s: &str) -> String {
    fn hex(b: u8) -> Option<u8> {
        match b {
            b'0'..=b'9' => Some(b - b'0'),
            b'a'..=b'f' => Some(b - b'a' + 10),
            b'A'..=b'F' => Some(b - b'A' + 10),
            _ => None,
        }
    }
    let bytes = s.as_bytes();
    let mut out = Vec::with_capacity(bytes.len());
    let mut i = 0;
    while i < bytes.len() {
        if bytes[i] == b'%' && i + 2 < bytes.len() {
            if let (Some(hi), Some(lo)) = (hex(bytes[i + 1]), hex(bytes[i + 2])) {
                out.push(hi << 4 | lo);
                i += 3;
                continue;
            }
        }
        out.push(bytes[i]);
        i += 1;
    }
    String::from_utf8_lossy(&out).into_owned()
}

/// Attachment-chip HTML appended below every bubble's main body. Clickable via
/// the body link hit-test using an internal
/// `ddmail-attach:folder|uid|index|filename` scheme decoded on the UI thread
/// (handle_link → DownloadAttachment → AttachmentSaved → open_saved_file).
/// folder/filename идут через att_url_encode (см. выше — иначе браузерная
/// нормализация href ломает не-ASCII имена).
pub(crate) fn attachment_chips(b: &MessageBody) -> String {
    if b.attachments.is_empty() {
        return String::new();
    }
    let mut s = format!("<div class=\"{CSS_NS}-atts\">");
    for a in &b.attachments {
        let href = format!(
            "ddmail-attach:{}|{}|{}|{}",
            att_url_encode(&b.folder),
            b.uid,
            a.index,
            att_url_encode(&a.filename)
        );
        s.push_str(&format!(
            "<a class=\"{CSS_NS}-att\" href=\"{}\">\u{1F4CE} {} · {} \u{041A}\u{0411}</a>",
            html_escape(&href),
            html_escape(&a.filename),
            (a.size / 1024).max(1)
        ));
    }
    s.push_str("</div>");
    s
}

/// Bump whenever the bubble/render HTML template or its CSS changes: the
/// texture cache keys renders by fnv1a(body.html) only, so without this a
/// template/CSS edit would keep serving stale cached bitmaps (RAM + disk).
pub(crate) const RENDER_TEMPLATE_EPOCH: u64 = 9;

/// ВСЕ классы обвязки пузыря пишутся с этим префиксом. Документ пузыря —
/// общая песочница для нашей вёрстки и присланного HTML, а письма сплошь
/// используют «обычные» имена: `class="row"` (Альфа-Лизинг и любой сборщик
/// рассылок — 9–13 таких элементов в письме), `time`, `image`, `inner`,
/// `button`. Без префикса наши правила молча меняли чужую разметку: `.row`
/// добавлял письму 60 px отбивки внутри пузыря, `.time` разворачивал ячейку
/// вправо и снимал с неё выделение.
///
/// Инвариант держит тест `bubble_css_tests::chrome_classes_are_namespaced`.
/// Намеренно достаёт до письма только один селектор: `a` (ссылки в цвет
/// акцента). Сбросы `.ddm-bubble * {… !important}` из эпохи WebView убраны:
/// emlrender сам держит ширину, а с комбинаторами они стирали письму рамки,
/// градиенты и авторские max-width.
pub(crate) const CSS_NS: &str = "ddm";

pub(crate) fn bubble_template(is_outgoing: bool, time: &str, inner: &str) -> String {
    bubble_template_wide(is_outgoing, time, inner, false)
}

/// `wide` снимает чатное ограничение ширины пузыря (72%) — для писем с HTML.
/// Рассыльные шаблоны свёрстаны фиксированной таблицей на 600 px, и в узком
/// пузыре `emlrender` вынужден их ужимать или линеаризовать: вёрстка цела, но
/// читается как сплющенная. Короткое HTML-письмо всё равно остаётся узким —
/// `max-width` не растягивает, ширина = min(max-content, max-width).
pub(crate) fn bubble_template_wide(
    is_outgoing: bool,
    time: &str,
    inner: &str,
    wide: bool,
) -> String {
    let side = if is_outgoing { "out" } else { "in" };
    let wide_class = if wide { format!(" {CSS_NS}-wide") } else { String::new() };
    // Own messages in the app's green, not a blue from somewhere else: the
    // accent, links and the selection highlight are all #10b981.
    let bg = if is_outgoing { "#d7f0e3" } else { "#ffffff" };
    // Bottom-right timestamp inside the bubble (as in the old Tauri client).
    // Empty date_ts → no stamp.
    let time_html = if time.is_empty() {
        String::new()
    } else {
        format!("<div class=\"{CSS_NS}-time\">{}</div>", html_escape(time))
    };
    format!(
        r#"<!DOCTYPE html><html><head><meta charset="utf-8"><style>
        html, body {{ margin: 0; padding: 0; background: #e9eef5; }}
        body {{ font-family: 'Segoe UI', system-ui, sans-serif; }}
        /* Auto margins, not flex: the standard way to align a block, and it
           renders the same in a browser. Single-class selectors on the bubble
           itself rather than `.row.out .bubble` — by choice, not because
           emlrender can't (it matches combinators, !important and @media
           now): a selector that reaches into the bubble also reaches into
           the sender's markup, and flat specificity keeps our chrome from
           outranking the mail's own rules. */
        .{CSS_NS}-row {{ padding: 6px 60px; }}
        .{CSS_NS}-bubble-out {{ margin-left: auto; margin-right: 0; }}
        .{CSS_NS}-bubble-in  {{ margin-left: 0; margin-right: auto; }}
        .{CSS_NS}-bubble {{
            max-width: 72%; background: {bg}; border-radius: 16px; padding: 10px 14px;
            font-size: 15px; line-height: 1.4; color: #0f1419;
            box-shadow: 0 1px 2px rgba(0,0,0,0.12); overflow-wrap: anywhere;
        }}
        /* Одноклассовый и ПОСЛЕ .ddm-bubble: специфичность у них равная, так
           что переопределяет порядок в стилях. В `.ddm-bubble.ddm-wide` смысла
           нет — второй класс в селекторе матчер emlrender не понимает. */
        .{CSS_NS}-wide {{ max-width: 100%; }}
        .{CSS_NS}-bubble-out {{ border-bottom-right-radius: 4px; }}
        .{CSS_NS}-bubble-in  {{ border-bottom-left-radius: 4px; }}
        a {{ color: #10b981; }}
        .{CSS_NS}-atts {{ margin-top: 8px; }}
        .{CSS_NS}-att {{ display: inline-block; background: rgba(0,0,0,0.06); border-radius: 8px;
                padding: 4px 10px; margin: 2px 4px 2px 0; color: #10b981;
                text-decoration: none; font-size: 13px; }}
        .{CSS_NS}-nobody {{ color: #8a97a5; font-size: 13px; font-style: italic; }}
        .{CSS_NS}-subj {{ color: #6b7785; font-size: 12px; font-weight: 600; line-height: 1.3;
                 margin-bottom: 4px; }}
        .{CSS_NS}-time {{ text-align: right; font-size: 11px; color: #8a97a5;
                 margin-top: 4px; user-select: none; }}
        </style></head>
        <body><div class="{CSS_NS}-row"><div class="{CSS_NS}-bubble {CSS_NS}-bubble-{side}{wide_class}">{inner}{time_html}</div></div></body></html>"#
    )
}

#[cfg(test)]
mod bubble_css_tests {
    use super::{build_body_html, policy};
    use ddmail_core::types::MessageBody;

    fn html_body(html: &str) -> MessageBody {
        MessageBody {
            uid: 1,
            folder: "INBOX".into(),
            subject: "s".into(),
            from: "Альфа Лизинг <noreply@example.ru>".into(),
            from_addr: "noreply@example.ru".into(),
            to: vec![],
            cc: vec![],
            date: String::new(),
            date_ts: 1_785_000_000,
            html: Some(html.into()),
            text: None,
            attachments: vec![],
            is_outgoing: false,
            message_id: String::new(),
            in_reply_to: String::new(),
            references: vec![],
            raw_headers: String::new(),
        }
    }

    /// Классы обвязки пузыря обязаны быть в своём пространстве имён: письма
    /// сплошь используют `class="row"`/`time`/`image` (у Альфа-Лизинга 9–13
    /// таких элементов), а документ у нашей вёрстки и присланного HTML один.
    /// Голый `.row { padding: 6px 60px }` добавлял письму 60 px отбивки
    /// внутри пузыря, `.time` разворачивал его ячейку вправо.
    #[test]
    fn chrome_classes_are_namespaced() {
        let doc = build_body_html(
            &html_body(r#"<table class="row"><tr><td class="row">текст</td></tr></table>"#),
            &policy::Policy::default(),
            true,
        );
        let css = &doc[..doc.find("</style>").expect("есть <style>")];
        for bare in [".row", ".bubble", ".time", ".att", ".atts", ".subj"] {
            assert!(
                !css.contains(&format!("{bare} ")) && !css.contains(&format!("{bare}{{")),
                "селектор {bare} без префикса ddm- поймает разметку письма"
            );
        }
        assert!(css.contains(".ddm-row"), "своя вёрстка должна остаться стилизованной");
        // Разметка письма проходит как есть — её класс мы не переписываем.
        assert!(doc.contains(r#"class="row""#), "класс письма не должен переписываться");
    }

    /// Широкий пузырь — только ВХОДЯЩЕМУ письму с HTML: рассыльный шаблон на
    /// 600 px в 72%-пузыре пришлось бы ужимать. Текстовому ширину снимать
    /// незачем, а своему нельзя — на `max-width: 100%` ломается выравнивание
    /// «своё справа» (оно держится на `margin-left: auto`).
    #[test]
    fn wide_bubble_only_for_html() {
        let p = policy::Policy::default();
        // Смотреть надо РАЗМЕТКУ: правило `.ddm-wide` лежит в стилях любого
        // пузыря, поэтому поиск по всему документу всегда находил бы его.
        let markup = |doc: &str| doc[doc.find("</style>").expect("есть <style>")..].to_string();

        let html =
            build_body_html(&html_body("<table><tr><td>рассылка</td></tr></table>"), &p, false);
        assert!(markup(&html).contains("ddm-wide"), "HTML-письмо должно получить широкий пузырь");

        let mut plain = html_body("");
        plain.html = None;
        plain.text = Some("просто текст".into());
        let doc = build_body_html(&plain, &p, false);
        assert!(!markup(&doc).contains("ddm-wide"), "текстовое письмо остаётся узким");

        // Своё письмо с HTML: широким быть не должно, иначе оно уедет влево
        // во всю ширину вместо выравнивания вправо.
        let mut own = html_body("<table><tr><td>мой ответ</td></tr></table>");
        own.is_outgoing = true;
        let doc_own = build_body_html(&own, &p, false);
        assert!(
            !markup(&doc_own).contains("ddm-wide"),
            "своё письмо остаётся узким, иначе ломается выравнивание вправо"
        );
        assert!(markup(&doc_own).contains("ddm-bubble-out"), "и остаётся исходящим");
    }
}

#[cfg(test)]
mod blank_body_tests {
    use super::{build_body_html, engine, policy};
    use ddmail_core::types::{Attachment, MessageBody};

    fn body(html: Option<&str>, text: Option<&str>, atts: usize) -> MessageBody {
        MessageBody {
            uid: 1,
            folder: "INBOX".into(),
            subject: "тема".into(),
            from: "Кто-то <a@b.ru>".into(),
            from_addr: "a@b.ru".into(),
            to: vec![],
            cc: vec![],
            date: String::new(),
            date_ts: 1_700_000_000,
            html: html.map(String::from),
            text: text.map(String::from),
            attachments: (0..atts)
                .map(|i| Attachment {
                    filename: format!("f{i}.pdf"),
                    mime_type: "application/pdf".into(),
                    size: 1024,
                    index: i,
                })
                .collect(),
            is_outgoing: false,
            message_id: String::new(),
            in_reply_to: String::new(),
            references: vec![],
            raw_headers: String::new(),
        }
    }

    /// Письмо-«только вложение» (пустая text/plain-часть + PDF, типовая
    /// госпочта) — это НЕ пустое тело: чипы вложений и есть его содержимое,
    /// перезапрашивать его незачем.
    #[test]
    fn attachment_only_is_not_blank() {
        assert!(!engine::body_is_blank(&body(None, Some("\r\n"), 1)));
        assert!(!engine::body_is_blank(&body(None, None, 4)));
    }

    /// Ни HTML, ни текста, ни вложений — вот это пусто: такую строку кэша
    /// надо перезапрашивать, иначе пузырь останется пустым навсегда.
    #[test]
    fn nothing_at_all_is_blank() {
        assert!(engine::body_is_blank(&body(None, None, 0)));
        assert!(engine::body_is_blank(&body(Some("   "), Some("\n"), 0)));
    }

    /// Пузырь письма-«только вложение» содержит чип и НЕ содержит заметку
    /// о пустом теле; полностью пустое письмо — наоборот.
    #[test]
    fn empty_note_only_when_nothing_to_draw() {
        let p = policy::Policy::default();
        let atts = build_body_html(&body(None, None, 1), &p, false);
        assert!(atts.contains("class=\"ddm-att\""), "чип вложения обязателен");
        assert!(!atts.contains("class=\"ddm-nobody\""));

        let empty = build_body_html(&body(None, None, 0), &p, false);
        assert!(empty.contains("class=\"ddm-nobody\""), "пустое тело должно быть подписано");
    }
}

#[cfg(test)]
mod att_url_tests {
    use super::{att_url_decode, att_url_encode};

    /// Кириллица, пробелы и скобки — типовое «долбанутое» имя — должны
    /// пережить раунд-трип чип → href → парсер байт-в-байт.
    #[test]
    fn cyrillic_roundtrip() {
        let name = "Отчёт за март (итоговый) №7.pdf";
        assert_eq!(att_url_decode(&att_url_encode(name)), name);
    }

    /// Разделитель схемы `|`, `%`, `#` и `?` не должны ломать формат
    /// folder|uid|index|filename и не должны меняться при раунд-трипе.
    #[test]
    fn url_special_chars_roundtrip() {
        let name = "a|b%20c#d?e&f.txt";
        let enc = att_url_encode(name);
        assert!(!enc.contains('|') && !enc.contains('#') && !enc.contains('?'));
        assert_eq!(att_url_decode(&enc), name);
    }

    /// Браузерные бэкенды percent-кодировали не-ASCII в `a.href` сами —
    /// декодер обязан понимать их вывод для легаси-ссылок, оставшихся
    /// в кэшированных рендерах.
    #[test]
    fn decodes_webkit_normalized_legacy() {
        assert_eq!(att_url_decode("%D0%9E%D1%82%D1%87%D1%91%D1%82.pdf"), "Отчёт.pdf");
    }

    /// Сырые (некодированные) легаси-строки проходят без изменений,
    /// включая одинокий `%` без хекс-пары.
    #[test]
    fn raw_legacy_passthrough() {
        assert_eq!(att_url_decode("plain-name.txt"), "plain-name.txt");
        assert_eq!(att_url_decode("100% готово"), "100% готово");
    }
}
