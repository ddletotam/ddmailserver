//! ddmail-native — Slint shell + Ultralight (WebKit) body rendering.
//! Sidebar = real conversations from the desktop cache; selecting one renders
//! its real message bodies as Ultralight bitmaps composited as Slint images.

slint::include_modules!();

mod calendar_settings;
mod engine;
mod notify;
mod policy;
mod recurrence;
mod reminders;
mod render_common;
#[cfg(any(windows, target_os = "linux"))]
mod tray;
#[cfg(target_os = "linux")]
#[path = "render_webkit.rs"]
mod render;
#[cfg(windows)]
#[path = "render_webview2.rs"]
mod render;
mod sanitize;
mod texture_cache;
mod toast;
mod toast_window;
mod window_state;

use std::cell::{Cell, RefCell};
use std::collections::{HashMap, HashSet};
use std::rc::Rc;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{mpsc, Arc, Mutex};
use std::time::Instant;

use slint::{Image, ModelRc, Rgba8Pixel, SharedPixelBuffer, VecModel};

use ddmail_core::cache::Cache;
use ddmail_core::types::{Contact, Conversation, MessageBody, MessageEnvelope, MessageRef};

const NAMES: [&str; 25] = [
    "Анна Соколова", "Команда AppSec", "Дмитрий П.", "Поддержка letotam",
    "Ольга Кузнецова", "DevSecOps канал", "Игорь Лебедев", "Мария В.",
    "Никита Орлов", "Рассылки", "Светлана Г.", "Павел Морозов",
    "QA дайджест", "Елена Фомина", "Артём Зайцев", "Релизы 4.x",
    "Юлия Беляева", "Сергей Котов", "HR отдел", "Григорий Н.",
    "Вера Полякова", "Алексей Тимофеев", "Финансы", "Дарья Жукова", "Roadmap",
];
const PALETTE: [&str; 6] = ["#2f80ed", "#27ae60", "#eb5757", "#9b51e0", "#f2994a", "#11998e"];

/// How many messages the server may scan when building the conversation list.
/// Mirrors the server-side default (handlers_desktop.go). A small cap combined
/// with the server's `ORDER BY uid ASC` fetch means it returns the OLDEST N
/// messages — so a multi-account aggregated INBOX with thousands of messages
/// drops recent conversations off the list entirely (ancient threads linger,
/// mail from a few days ago never appears). 5000 covers realistic inboxes;
/// deltas keep every subsequent sync cheap regardless.
const CONV_FETCH_LIMIT: u32 = 5000;

/// Calendar palette, modelled on Google Calendar's event colours — the
/// reference design for "distinct, calm, and readable with white text on
/// event blocks". Their Banana yellow is swapped for a darker amber (white
/// text drowns on yellow).
const CAL_PALETTE: [&str; 12] = [
    "#D50000", // tomato
    "#E67C73", // flamingo
    "#F4511E", // tangerine
    "#F09300", // amber
    "#33B679", // sage
    "#0B8043", // basil
    "#039BE5", // peacock
    "#3F51B5", // blueberry
    "#7986CB", // lavender
    "#8E24AA", // grape
    "#616161", // graphite
    "#009688", // teal
];

/// Stable default colour for a calendar that the server gave no colour for.
/// Keyed on the calendar id via a multiplicative hash so the mapping is
/// deterministic across sessions and spreads ids across the palette.
fn default_cal_color(id: i64) -> &'static str {
    let idx = ((id.unsigned_abs().wrapping_mul(2_654_435_761)) >> 16) as usize % CAL_PALETTE.len();
    CAL_PALETTE[idx]
}

/// Colour to actually paint a calendar with. The CalDAV import stamps most
/// calendars with the generic placeholder `#3788d8`, so we treat that (and
/// an empty value) as "no real colour" and fall back to our distinct
/// per-calendar palette; a genuinely customised colour is kept.
fn cal_color(id: i64, server_color: &str) -> String {
    if server_color.is_empty() || server_color.eq_ignore_ascii_case("#3788d8") {
        default_cal_color(id).to_string()
    } else {
        server_color.to_string()
    }
}

const DEFAULT_WIDTH: u32 = 740;

/// Default workday bounds (local hours). The calendar's work-hours view
/// shows one hour either side of these.

fn initials(name: &str) -> String {
    name.split_whitespace()
        .filter_map(|w| w.chars().next())
        .take(2)
        .collect::<String>()
        .to_uppercase()
}

fn hex(s: &str) -> slint::Color {
    let s = s.trim_start_matches('#');
    let r = u8::from_str_radix(&s[0..2], 16).unwrap_or(0);
    let g = u8::from_str_radix(&s[2..4], 16).unwrap_or(0);
    let b = u8::from_str_radix(&s[4..6], 16).unwrap_or(0);
    slint::Color::from_rgb_u8(r, g, b)
}

#[derive(Clone)]
struct Disp {
    name: String,
    initials: String,
    color: String,
    preview: String,
    email: String,
    /// Sidebar row tint — colour of the identity that received the
    /// conversation (см. identity_color_map). Empty = no tint.
    ident_color: String,
    /// Unread badge value (0 = no badge).
    unread: u32,
}

/// Pastel palette for identities lacking a server-side colour. Mirrors the
/// old Tauri identityStore + imap.rs::fetch_identities_impl so both
/// connection modes look identical.
const IDENT_PASTEL: [&str; 15] = [
    "#FFE4E1", "#E8F5E9", "#E3F2FD", "#FFF9C4", "#F3E5F5",
    "#E0F7FA", "#FBE9E7", "#F1F8E9", "#EDE7F6", "#E8EAF6",
    "#FCE4EC", "#E0F2F1", "#FFF3E0", "#F9FBE7", "#EFEBE9",
];
/// «Ugly gray» for conversations received by an unknown alias.
const IDENT_UNKNOWN: &str = "#d5d5d0";

/// email(lowercase) → row tint for every known identity.
fn identity_color_map(cache: &Cache, key: &str) -> HashMap<String, String> {
    cache
        .load_identities(key)
        .unwrap_or_default()
        .into_iter()
        .enumerate()
        .map(|(i, id)| {
            let color = if id.color.trim().is_empty() {
                IDENT_PASTEL[i % IDENT_PASTEL.len()].to_string()
            } else {
                id.color
            };
            (id.email.to_lowercase(), color)
        })
        .collect()
}

/// (Re)fill the composer from-picker: the Slint model for drawing and the
/// parallel email list in Shared for send-time resolution. Keeps the current
/// selection if its email survives the refresh; otherwise re-aims at the
/// default identity (falling back to the primary account email).
fn refresh_composer_identities(ui: &MainWindow, sh: &Shared) {
    let Some(cache) = &sh.cache else { return };
    let idents = cache.load_identities(&sh.key).unwrap_or_default();
    let prev_email = {
        let list = sh.composer_identities.borrow();
        let idx = ui.get_composer_identity_index();
        list.get(idx.max(0) as usize).cloned()
    };
    let mut items: Vec<IdentityItem> = Vec::with_capacity(idents.len());
    let mut emails: Vec<String> = Vec::with_capacity(idents.len());
    let mut selected: i32 = -1;
    let mut default_idx: i32 = 0;
    for (i, id) in idents.iter().enumerate() {
        let color = if id.color.trim().is_empty() {
            IDENT_PASTEL[i % IDENT_PASTEL.len()]
        } else {
            id.color.as_str()
        };
        let label = if id.name.trim().is_empty() {
            id.email.clone()
        } else {
            format!("{} <{}>", id.name.trim(), id.email)
        };
        items.push(IdentityItem {
            label: label.into(),
            email: id.email.clone().into(),
            tint: parse_hex_color(color),
        });
        emails.push(id.email.to_lowercase());
        if id.is_default {
            default_idx = i as i32;
        }
        if Some(&id.email.to_lowercase()) == prev_email.as_ref() {
            selected = i as i32;
        }
    }
    // No identities synced yet — the picker hides (length < 2), sends fall
    // back to the account email engine-side.
    ui.set_composer_identities(ModelRc::new(VecModel::from(items)));
    ui.set_composer_identity_index(if selected >= 0 { selected } else { default_idx });
    *sh.composer_identities.borrow_mut() = emails;
}

/// Aim the from-picker at a specific identity email (case-insensitive).
/// No-op when the email isn't one of ours — the previous selection stays.
fn aim_composer_identity(ui: &MainWindow, sh: &Shared, email: &str) {
    if email.is_empty() {
        return;
    }
    let lc = email.to_lowercase();
    if let Some(i) = sh.composer_identities.borrow().iter().position(|e| *e == lc) {
        ui.set_composer_identity_index(i as i32);
    }
}

/// Ask the engine for the address book. Empty query = full book, otherwise a
/// search. Answers are guarded UI-side by the echoed query (see the
/// EngineResult::Contacts handler), so typing fast just drops stale results.
fn fetch_contacts(sh: &Shared, query: &str) {
    if let Some(etx) = sh.engine_tx.borrow().as_ref() {
        let limit = if query.trim().is_empty() { 500 } else { 50 };
        let _ = etx.send(engine::EngineCmd::FetchContacts {
            query: query.to_string(),
            limit,
        });
    }
}

/// Two-letter initials for the avatar bubble: first letters of the first two
/// whitespace-separated words, else the first char, uppercased.
fn contact_initials(name: &str) -> String {
    let words: Vec<&str> = name.split_whitespace().collect();
    let s: String = match words.as_slice() {
        [] => String::new(),
        [one] => one.chars().take(1).collect(),
        [a, b, ..] => a.chars().take(1).chain(b.chars().take(1)).collect(),
    };
    s.to_uppercase()
}

/// Stable pastel pick for a contact bubble, hashed off a seed (email or name).
fn contact_pastel(seed: &str) -> &'static str {
    let h = seed.bytes().fold(0u32, |acc, b| acc.wrapping_mul(31).wrapping_add(b as u32));
    IDENT_PASTEL[(h as usize) % IDENT_PASTEL.len()]
}

/// Build the Slint address-book rows from the engine's contact DTOs.
/// Build a contact-write body from the editor fields (one email/phone slot in
/// the v1 form; the server/model accept arrays).
fn contact_body_from_ui(ui: &MainWindow) -> serde_json::Value {
    let email = ui.get_ce_email().trim().to_string();
    let phone = ui.get_ce_phone().trim().to_string();
    let emails: Vec<String> = if email.is_empty() { vec![] } else { vec![email] };
    let phones: Vec<String> = if phone.is_empty() { vec![] } else { vec![phone] };
    serde_json::json!({
        "full_name": ui.get_ce_name().trim().to_string(),
        "emails": emails,
        "phones": phones,
        "organization": ui.get_ce_org().trim().to_string(),
    })
}

fn address_book_rows(list: &[ddmail_core::types::DesktopContact]) -> Vec<AddrBookRow> {
    list.iter()
        .map(|c| {
            let email = c.emails.first().cloned().unwrap_or_default();
            let has_name = !c.full_name.trim().is_empty();
            // With a real name, the second line is the email. Without one, the
            // email becomes the title and the second line falls back to the
            // organization (usually empty) — never repeat the email twice.
            let name = if has_name {
                c.full_name.clone()
            } else if !email.is_empty() {
                email.clone()
            } else {
                c.organization.clone()
            };
            let detail = if has_name { email.clone() } else { c.organization.clone() };
            let seed = if email.is_empty() { name.clone() } else { email.clone() };
            AddrBookRow {
                name: name.clone().into(),
                detail: detail.into(),
                initials: contact_initials(&name).into(),
                color: parse_hex_color(contact_pastel(&seed)).into(),
                email: email.into(),
            }
        })
        .collect()
}

/// "#rrggbb" → slint Color; anything unparsable → neutral grey.
fn parse_hex_color(s: &str) -> slint::Color {
    let h = s.trim().trim_start_matches('#');
    if h.len() == 6 {
        if let Ok(v) = u32::from_str_radix(h, 16) {
            return slint::Color::from_rgb_u8((v >> 16) as u8, (v >> 8) as u8, v as u8);
        }
    }
    slint::Color::from_rgb_u8(0x8b, 0x95, 0xa1)
}

fn cache_db_path() -> Option<std::path::PathBuf> {
    // Pick the per-OS location of the existing cache dir so the client
    // reads the same cache.db the user already has.
    //   * Windows: %APPDATA%\ru.letotam.ddmail\cache.db
    //   * macOS:   ~/Library/Application Support/ru.letotam.ddmail/cache.db
    //   * Linux:   $XDG_DATA_HOME/ru.letotam.ddmail/cache.db,
    //              or ~/.local/share/ru.letotam.ddmail/cache.db
    #[cfg(target_os = "windows")]
    {
        let appdata = std::env::var("APPDATA").ok()?;
        return Some(std::path::PathBuf::from(appdata).join("ru.letotam.ddmail").join("cache.db"));
    }
    #[cfg(target_os = "macos")]
    {
        let home = std::env::var("HOME").ok()?;
        return Some(
            std::path::PathBuf::from(home)
                .join("Library/Application Support/ru.letotam.ddmail/cache.db"),
        );
    }
    #[cfg(target_os = "linux")]
    {
        let base = std::env::var("XDG_DATA_HOME").ok()
            .map(std::path::PathBuf::from)
            .or_else(|| std::env::var("HOME").ok().map(|h|
                std::path::PathBuf::from(h).join(".local/share")
            ))?;
        return Some(base.join("ru.letotam.ddmail").join("cache.db"));
    }
    #[allow(unreachable_code)]
    None
}

fn open_cache() -> Option<Arc<Cache>> {
    let path = cache_db_path()?;
    let dir = path.parent()?.to_path_buf();
    Cache::new(dir).ok().map(Arc::new)
}

fn open_account() -> Option<(Arc<Cache>, String, Vec<Conversation>)> {
    let path = cache_db_path()?;
    if !path.exists() {
        println!("cache.db not found at {}", path.display());
        return None;
    }
    let cache = open_cache()?;
    let key = cache.account_keys().ok()?.into_iter().next()?;
    let convs = cache.load_conversations(&key).ok()?;
    if convs.is_empty() {
        return None;
    }
    println!("loaded {} real conversations (account {key})", convs.len());
    Some((cache, key, convs))
}

fn conv_name(c: &Conversation) -> String {
    if !c.label.is_empty() {
        c.label.clone()
    } else {
        c.counterparts
            .first()
            .map(|cp| if cp.name.is_empty() { cp.addr.clone() } else { cp.name.clone() })
            .unwrap_or_default()
    }
}

fn displays_from(convs: &[Conversation], ident_colors: &HashMap<String, String>) -> Vec<Disp> {
    convs
        .iter()
        .enumerate()
        .map(|(i, c)| {
            let name = conv_name(c);
            let ident_color = ident_colors
                .get(&c.received_by.to_lowercase())
                .cloned()
                .unwrap_or_else(|| IDENT_UNKNOWN.to_string());
            Disp {
                initials: initials(&name),
                name,
                color: PALETTE[i % PALETTE.len()].to_string(),
                preview: if c.last_subject.is_empty() {
                    "(без темы)".to_string()
                } else {
                    c.last_subject.clone()
                },
                email: c.counterparts.first().map(|cp| cp.addr.clone()).unwrap_or_default(),
                ident_color,
                unread: c.unread_count,
            }
        })
        .collect()
}

fn synthetic_displays() -> Vec<Disp> {
    (0..25)
        .map(|i| Disp {
            name: NAMES[i].to_string(),
            initials: initials(NAMES[i]),
            color: PALETTE[i % PALETTE.len()].to_string(),
            preview: "Последнее сообщение в диалоге…".to_string(),
            email: String::new(),
            ident_color: String::new(),
            unread: 0,
        })
        .collect()
}

fn html_escape(s: &str) -> String {
    s.replace('&', "&amp;").replace('<', "&lt;").replace('>', "&gt;")
}

/// "Looks like an email" check for the live-dropdown compose-new row.
/// Matches the spec from svelte's SearchDropdown (EMAIL_RE): something
/// before @, something between @ and the last dot, something after.
/// Pulled into Rust so the Slint side stays declarative.
fn parse_email_like(q: &str) -> Option<String> {
    let s = q.trim();
    if s.contains(|c: char| c.is_whitespace() || c == '<' || c == '>' || c == '"' || c == ',') {
        return None;
    }
    let at = s.find('@')?;
    let local = &s[..at];
    let domain = &s[at + 1..];
    if local.is_empty() || domain.is_empty() { return None; }
    let dot = domain.find('.')?;
    if dot == 0 || dot == domain.len() - 1 { return None; }
    Some(s.to_lowercase())
}

/// Short "HH:MM" / "DD.MM" / "DD.MM.YY" formatter for the dropdown
/// message rows — matches svelte's formatDateShort behaviour closely
/// enough for the right-aligned date hint.
fn fmt_short_date(ts_ms: i64) -> String {
    if ts_ms <= 0 { return String::new(); }
    use chrono::{DateTime, Datelike, Local, TimeZone, Timelike};
    let dt: DateTime<Local> = match Local.timestamp_millis_opt(ts_ms).single() {
        Some(d) => d,
        None => return String::new(),
    };
    let now = Local::now();
    if dt.year() == now.year() && dt.ordinal() == now.ordinal() {
        return format!("{:02}:{:02}", dt.hour(), dt.minute());
    }
    if dt.year() == now.year() {
        return format!("{:02}.{:02}", dt.day(), dt.month());
    }
    format!("{:02}.{:02}.{:02}", dt.day(), dt.month(), dt.year() % 100)
}

/// Bubble corner stamp: time-only for today, date+time within the year,
/// and a two-digit year for older mail. Empty string for a missing date.
/// `ts_secs` is MessageBody.date_ts — a Unix timestamp in SECONDS (the server
/// sends date_ts in seconds, unlike the messages table's millisecond `date`).
fn fmt_bubble_time(ts_secs: i64) -> String {
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
        dt.day(), dt.month(), dt.year() % 100, dt.hour(), dt.minute()
    )
}

fn contact_items(contacts: &[Contact]) -> Vec<ContactItem> {
    contacts
        .iter()
        .map(|c| ContactItem {
            name: c.name.clone().into(),
            email: c.email.clone().into(),
        })
        .collect()
}

/// Pull a short single-line preview out of a message body — first the
/// plain-text part, then collapsing HTML tags out of html when text is
/// missing. Whitespace squashed, capped at ~80 chars for the ribbon.
fn body_preview(body: &MessageBody) -> String {
    let raw = body
        .text
        .as_deref()
        .filter(|s| !s.trim().is_empty())
        .map(|s| s.to_string())
        .or_else(|| {
            body.html.as_deref().map(|h| {
                // Cheap tag strip — the ribbon needs at most a sentence.
                let mut out = String::with_capacity(h.len());
                let mut in_tag = false;
                for ch in h.chars() {
                    match ch {
                        '<' => in_tag = true,
                        '>' => in_tag = false,
                        _ if !in_tag => out.push(ch),
                        _ => {}
                    }
                }
                out
            })
        })
        .unwrap_or_default();
    let collapsed = raw.split_whitespace().collect::<Vec<_>>().join(" ");
    if collapsed.chars().count() <= 80 {
        collapsed
    } else {
        let cut: String = collapsed.chars().take(80).collect();
        format!("{cut}…")
    }
}

fn enter_reply_mode(sh: &Shared, ui: &MainWindow, body: MessageBody) {
    let display_from = if body.from.is_empty() {
        body.from_addr.clone()
    } else {
        body.from.clone()
    };
    let preview = body_preview(&body);
    *sh.pending_reply.borrow_mut() = Some(body);
    ui.set_reply_ribbon_from(display_from.into());
    ui.set_reply_ribbon_preview(preview.into());
    ui.set_reply_ribbon_visible(true);
    ui.invoke_focus_composer();
}

fn exit_reply_mode(sh: &Shared, ui: &MainWindow) {
    *sh.pending_reply.borrow_mut() = None;
    *sh.pending_forward.borrow_mut() = None;
    ui.set_reply_ribbon_visible(false);
    ui.set_reply_ribbon_from("".into());
    ui.set_reply_ribbon_preview("".into());
    ui.set_composer_to("".into());
}

/// «Переслать» — Telegram-style: the original is pinned above the input as
/// a non-editable ribbon, the recipients panel unfolds with EMPTY Кому/Cc
/// and focus lands in «Кому». The composer text stays free for the user's
/// covering note; at send time the original's text goes below it after a
/// separator and its attachments are re-attached as-is (engine-side).
fn enter_forward_mode(sh: &Shared, ui: &MainWindow, body: MessageBody) {
    exit_reply_mode(sh, ui); // a forward replaces any staged reply
    let display_from = if body.from.is_empty() {
        body.from_addr.clone()
    } else {
        body.from.clone()
    };
    let subj_lc = body.subject.to_lowercase();
    let subject = if subj_lc.starts_with("fwd:") || subj_lc.starts_with("fw:") {
        body.subject.clone()
    } else {
        format!("Fwd: {}", body.subject)
    };
    let preview = body_preview(&body);
    *sh.pending_forward.borrow_mut() = Some(body);
    ui.set_reply_ribbon_from(format!("Переслать: {display_from}").into());
    ui.set_reply_ribbon_preview(preview.into());
    ui.set_reply_ribbon_visible(true);
    ui.set_composer_subject(subject.into());
    ui.set_composer_to("".into());
    ui.set_composer_cc("".into());
    ui.set_composer_expanded(true);
    ui.set_focus_to_seq(ui.get_focus_to_seq() + 1);
}

/// Shared logic for "enter transient compose mode". Pins the chat header
/// to the new recipient, blanks the bubble list, deselects the sidebar
/// row (none of the existing conversations match), and stashes the
/// target email on `Shared.pending_compose` for `on_send` to pick up.
fn enter_compose_mode(sh: &Shared, ui: &MainWindow, email: &str) {
    let email = email.trim().to_lowercase();
    *sh.pending_compose.borrow_mut() = Some(email.clone());
    // Any staged explicit-reply target is invalidated by jumping into
    // a fresh compose: the new conversation has no bubble to quote.
    exit_reply_mode(sh, ui);
    sh.current_msgs.borrow_mut().clear();
    let initial = email
        .chars()
        .next()
        .map(|c| c.to_uppercase().to_string())
        .unwrap_or_default();
    ui.set_active_name(email.clone().into());
    ui.set_active_initials(initial.into());
    ui.set_active_color(slint::Brush::SolidColor(hex("#2f80ed")));
    ui.set_active_meta("".into());
    ui.set_active_ident_color(slint::Brush::SolidColor(hex("#ffffff")));
    ui.set_messages(ModelRc::new(VecModel::from(Vec::<RowItem>::new())));
    ui.set_search_open(false);
    ui.set_search_query("".into());
    ui.set_search_selected_row(-1);
    ui.set_search_compose_email("".into());
    ui.set_search_contacts(ModelRc::new(VecModel::from(Vec::<ContactItem>::new())));
    ui.set_search_messages(ModelRc::new(VecModel::from(Vec::<MessageHit>::new())));
    // Prepend the synthetic "new chat" row and pull focus to the input.
    refresh_sidebar(sh, ui);
    ui.invoke_focus_composer();
}

/// Mirror the staged attachment basenames into the composer's chip model.
fn refresh_attachment_chips(ui: &MainWindow, sh: &Shared) {
    let chips: Vec<AttachChip> = sh
        .compose_attachments
        .borrow()
        .iter()
        .map(|p| AttachChip {
            name: p
                .file_name()
                .map(|n| n.to_string_lossy().into_owned())
                .unwrap_or_else(|| p.to_string_lossy().into_owned())
                .into(),
        })
        .collect();
    ui.set_composer_attachments(slint::ModelRc::new(slint::VecModel::from(chips)));
}

fn message_hits(envs: &[MessageEnvelope]) -> Vec<MessageHit> {
    envs.iter()
        .map(|e| MessageHit {
            from: if e.from.is_empty() { e.from_addr.clone() } else { e.from.clone() }.into(),
            subject: e.subject.clone().into(),
            date: fmt_short_date(e.date_ts).into(),
        })
        .collect()
}


/// Bubble wrapper + email-HTML normalization (tames ugly notification emails).
/// External-resource blocking is applied here per the current `Policy` —
/// images / stylesheets / `url(...)` references to non-allowlisted hosts
/// are replaced with empty `src=""` (kept as `data-blocked-src` for the
/// future "show domain X" UX).
fn build_body_html(b: &MessageBody, policy: &policy::Policy) -> String {
    let inner = match b.html.as_deref() {
        Some(h) if !h.trim().is_empty() => {
            let sanitized = sanitize::sanitize_email_html_for(h, policy, &b.from_addr);
            sanitize::block_external(&sanitized, policy, &b.from_addr).html
        }
        _ => format!(
            "<div style=\"white-space:pre-wrap\">{}</div>",
            html_escape(b.text.as_deref().unwrap_or(""))
        ),
    };
    bubble_template(b.is_outgoing, &fmt_bubble_time(b.date_ts), &format!("{inner}{}", attachment_chips(b)))
}

/// Text-only bubble — the fallback we render when WebKit chokes on an
/// HTML body (timeout or empty paint). Preserves linebreaks via
/// `white-space: pre-wrap`, and still appends attachment chips.
fn build_text_only_html(b: &MessageBody) -> String {
    let escaped = html_escape(b.text.as_deref().unwrap_or(""));
    let inner = format!("<div style=\"white-space:pre-wrap\">{escaped}</div>");
    bubble_template(b.is_outgoing, &fmt_bubble_time(b.date_ts), &format!("{inner}{}", attachment_chips(b)))
}

/// Render width (CSS px) for the source/headers viewer bitmap. The Image is
/// displayed at exactly this width so pointer coords map 1:1 onto the word
/// rects extracted at render time.
const SOURCE_RENDER_W: u32 = 760;

/// HTML for the source viewer: the raw text in a monospace, wrapping <pre>.
/// Verbatim — only HTML-escaped so the markup can't be interpreted.
fn build_source_html(text: &str) -> String {
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

/// Attachment-chip HTML appended below every bubble's main body. Clickable via
/// the body link hit-test using an internal
/// `ddmail-attach:folder|uid|index|filename` scheme decoded on the UI thread
/// (handle_link → DownloadAttachment → AttachmentSaved → open_external).
fn attachment_chips(b: &MessageBody) -> String {
    if b.attachments.is_empty() {
        return String::new();
    }
    let mut s = String::from("<div class=\"atts\">");
    for a in &b.attachments {
        let href = format!(
            "ddmail-attach:{}|{}|{}|{}",
            b.folder, b.uid, a.index, a.filename
        );
        s.push_str(&format!(
            "<a class=\"att\" href=\"{}\">\u{1F4CE} {} · {} \u{041A}\u{0411}</a>",
            html_escape(&href),
            html_escape(&a.filename),
            (a.size / 1024).max(1)
        ));
    }
    s.push_str("</div>");
    s
}

fn bubble_template(is_outgoing: bool, time: &str, inner: &str) -> String {
    let side = if is_outgoing { "out" } else { "in" };
    let bg = if is_outgoing { "#cfe6ff" } else { "#ffffff" };
    // Bottom-right timestamp inside the bubble (as in the old Tauri client).
    // Empty date_ts → no stamp.
    let time_html = if time.is_empty() {
        String::new()
    } else {
        format!("<div class=\"time\">{}</div>", html_escape(time))
    };
    format!(
        r#"<!DOCTYPE html><html><head><meta charset="utf-8"><style>
        html, body {{ margin: 0; padding: 0; background: #e9eef5; }}
        body {{ font-family: 'Segoe UI', system-ui, sans-serif; }}
        .row {{ display: flex; padding: 6px 60px; }}
        .row.out {{ justify-content: flex-end; }}
        .row.in  {{ justify-content: flex-start; }}
        .bubble {{
            max-width: 72%; background: {bg}; border-radius: 16px; padding: 10px 14px;
            font-size: 15px; line-height: 1.4; color: #0f1419;
            box-shadow: 0 1px 2px rgba(0,0,0,0.12); overflow-wrap: anywhere;
        }}
        .row.out .bubble {{ border-bottom-right-radius: 4px; }}
        .row.in  .bubble {{ border-bottom-left-radius: 4px; }}
        .bubble * {{ max-width: 100% !important; border: 0 !important; background-image: none !important; }}
        .bubble table, .bubble td, .bubble th {{ border-collapse: collapse !important; }}
        .bubble img {{ max-width: 100% !important; height: auto !important; }}
        a {{ color: #2f80ed; }}
        .atts {{ margin-top: 8px; }}
        .att {{ display: inline-block; background: rgba(0,0,0,0.06); border-radius: 8px;
                padding: 4px 10px; margin: 2px 4px 2px 0; color: #2f80ed;
                text-decoration: none; font-size: 13px; }}
        .time {{ text-align: right; font-size: 11px; color: #8a97a5;
                 margin-top: 4px; user-select: none; }}
        </style></head>
        <body><div class="row {side}"><div class="bubble">{inner}{time_html}</div></div></body></html>"#
    )
}

enum Job {
    SetConversation {
        bodies: Vec<MessageBody>,
        width: u32,
        policy: policy::Policy,
        /// Bumped whenever the policy mutates so the body_cache knows
        /// to miss for entries rendered under a stale policy.
        policy_gen: u64,
        /// Monotonic job sequence (latest wins). The render worker skips
        /// any job older than the newest one enqueued, and aborts
        /// mid-render when a newer one arrives — so a drag-resize or a
        /// fast conversation switch never renders bubbles nobody will see.
        seq: u64,
        /// After the rows land: None = keep scroll position (resize,
        /// policy toggle), Some(-1) = scroll to the end, Some(r) =
        /// scroll so row r (first unread) is at the top.
        scroll_to: Option<i32>,
        /// Per-body render mode, parallel to `bodies`: 0 = auto
        /// (HTML when present), 1 = force the text-only bubble.
        modes: Vec<u8>,
    },
    HitTest { row: usize, x: f32, y: f32 },
    /// Render the source/headers viewer text to a bitmap + word rects, so the
    /// modal reuses the fast bubble selection layer instead of Slint's
    /// (slow-on-large-text) TextInput.
    RenderSource { text: String, width: u32 },
}

/// Everything the render worker knows about a row besides its bitmap —
/// shipped to the UI thread to fill RowItem (context-menu data included).
struct RowMeta {
    h: f32,
    has_html: bool,
    has_text: bool,
    viewing_html: bool,
    sender: String,
    media_host: String,
    script_host: String,
    m_sender_on: bool,
    s_sender_on: bool,
    m_host_on: bool,
    s_host_on: bool,
}

/// UI-thread state shared by the select/resize/engine-result paths. All mail
/// state is interior-mutable so the live engine refresh can replace it.
struct Shared {
    cache: Option<Arc<Cache>>,
    key: String,
    convs: RefCell<Vec<Conversation>>,
    displays: RefCell<Vec<Disp>>,
    avatars: RefCell<HashMap<String, Image>>,
    /// account_key of the currently open conversation. Addressed engine
    /// commands (body/flags/delete/source/attachment/send) carry it so they
    /// route to the right server. Empty falls back to the primary account.
    cur_account_key: RefCell<String>,
    /// All account keys (the indicator's denominator) and their last-known
    /// connection state ("connecting" | "connected" | "error"). Drives the
    /// aggregate green/yellow/red status light.
    account_keys: RefCell<Vec<String>>,
    account_states: RefCell<HashMap<String, String>>,
    /// Message refs for the currently rendered rows (row index → message).
    current_msgs: RefCell<Vec<MessageRef>>,
    /// Bodies of the open conversation, kept in memory (parallel to
    /// `current_msgs`) so resize / reply / forward / policy-toggle /
    /// send-subject paths never re-read SQLite on the UI thread.
    current_bodies: RefCell<Vec<MessageBody>>,
    /// Conversation-open generation. Bumped on every open_conversation;
    /// FetchMessages echoes it back so an answer for a conversation the
    /// user already left is dropped instead of overwriting the screen
    /// (same pattern as `search_query_inflight`).
    open_gen: Cell<u64>,
    /// (folder, uid) of the messages that were UNREAD when the current
    /// conversation was opened — the scroll anchor survives the
    /// mark-as-read that fires right after open.
    open_unread: RefCell<HashSet<(String, u32)>>,
    /// True until the first render after open has applied its scroll;
    /// lets the network Messages path scroll when the cache had nothing.
    scroll_pending: Cell<bool>,
    /// Per-message render-view override: present = force the text-only
    /// bubble even when an HTML part exists («Показать → Текстовую
    /// версию»). Session-scoped on purpose.
    body_view_text: RefCell<HashSet<(String, u32)>>,
    /// Forward target — set by «Переслать»; on Send the original's text
    /// goes below the typed text and its attachments are re-attached.
    pending_forward: RefCell<Option<MessageBody>>,
    /// Which source view a pending FetchSource should open:
    /// 1 = заголовки, 2 = полный исходник.
    pending_source_view: Cell<u8>,
    /// Full, untruncated text currently behind the source viewer (the widget
    /// shows only a capped slice — see SOURCE_VIEW_MAX). «Копировать всё» reads
    /// this so the clipboard always gets the complete source.
    source_view_full: RefCell<String>,
    /// Word rects of the rendered source bitmap + its selection state. Mirrors
    /// the bubble selection layer (row_text_runs/sel_*) but for the modal.
    src_runs: RefCell<Vec<render_common::TextRun>>,
    src_sel_anchor: Cell<usize>,
    src_sel_head: Cell<usize>,
    src_sel_moved: Cell<bool>,
    src_sel_dragging: Cell<bool>,
    /// email(lowercase) → пастельный цвет айдентики (подкраска строк
    /// сайдбара по received_by). Обновляется при каждом списке диалогов.
    identity_colors: RefCell<HashMap<String, String>>,
    /// From-picker дропдауна композера: e-mail'ы в том же порядке, что и
    /// Slint-модель composer-identities. on_send резолвит выбранный индекс
    /// через этот список (Slint-модель — источник только для отрисовки).
    composer_identities: RefCell<Vec<String>>,
    /// UI-thread copy of the per-row link rects (CSS px, bubble-relative) —
    /// drives the pointer cursor over links. The render worker keeps its
    /// own copy for click hit-testing; this one answers synchronous
    /// hover-binding queries without a worker round-trip.
    row_links: RefCell<Vec<Vec<render_common::LinkRect>>>,
    /// What the shared confirmation modal confirms: 1 = удалить диалог,
    /// 2 = спам (blacklist + purge отправителя).
    confirm_mode: Cell<u8>,
    /// Per-row text layers (word rects, bubble-relative CSS px) — mouse
    /// selection. Parallel to the rendered rows, like row_links.
    row_text_runs: RefCell<Vec<Vec<render_common::TextRun>>>,
    /// Mouse selection: row index (-1 none) and the anchor/head word
    /// indices within that row's text layer (inclusive, unordered).
    sel_row: Cell<i32>,
    sel_anchor: Cell<usize>,
    sel_head: Cell<usize>,
    sel_dragging: Cell<bool>,
    sel_moved: Cell<bool>,
    /// Set when a drag-selection just ended — the click that Slint fires
    /// on release must NOT open a link.
    sel_suppress_click: Cell<bool>,
    /// Toast-click navigation: scroll to this (folder, uid) once its body
    /// is rendered. Takes priority over the unread-anchor logic.
    pending_open_ref: RefCell<Option<(String, u32)>>,
    /// Render-job sequence shared with the render worker (see Job::seq).
    render_seq: Arc<AtomicU64>,
    current: Cell<usize>,
    width: Cell<u32>,
    tx: mpsc::Sender<Job>,
    engine_tx: RefCell<Option<mpsc::Sender<engine::EngineCmd>>>,
    /// Last search query we asked the engine for. Engine echoes the
    /// query back in `SearchDropdown`; we drop results that don't match
    /// — handles the race where typing outruns the engine.
    search_query_inflight: RefCell<String>,
    /// Latest rows in the dropdown (parallel to the Slint model order),
    /// so callbacks can resolve `search-select-contact(idx)` and
    /// `search-select-message(idx)` back to their domain objects.
    search_contacts: RefCell<Vec<Contact>>,
    search_messages: RefCell<Vec<MessageEnvelope>>,
    /// "Transient compose" target — set when the user picks a fresh
    /// recipient via the search dropdown ("Написать xxx@yyy" or a
    /// contact with no existing conversation). While Some, the chat
    /// pane shows an empty bubble list with the recipient pinned in
    /// the header; `on_send` routes the outgoing message to this
    /// address instead of the (irrelevant) sidebar-selected
    /// conversation. Cleared by EngineResult::Sent.
    pending_compose: RefCell<Option<String>>,
    /// Explicit-reply target — set when the user hits "Ответить" on a
    /// specific bubble. Drives the quote ribbon above the input and,
    /// at send time, the Re: subject + In-Reply-To / References
    /// threading. Cleared by Send or by the ribbon's × button.
    pending_reply: RefCell<Option<MessageBody>>,
    /// Content-permission policy (per-sender media/scripts, per-domain
    /// allowlist) — port of the svelte permissionStore. Persisted to
    /// disk on every toggle.
    policy: RefCell<policy::Policy>,
    /// Monotonic generation counter, bumped each time the policy
    /// mutates. Render worker uses it as part of the bitmap cache key
    /// so toggling a permission invalidates exactly the relevant
    /// cached rows.
    policy_gen: Cell<u64>,
    /// Calendars list as the engine last reported it; we hold them so
    /// the visibility map can resolve names/colors when the user
    /// toggles checkboxes.
    calendars: RefCell<Vec<ddmail_core::types::DesktopCalendar>>,
    /// Per-calendar visibility, keyed by id. Defaults to true the first
    /// time a calendar shows up.
    calendar_visible: RefCell<HashMap<i64, bool>>,
    /// User-picked colour overrides (id → "#rrggbb"); wins over the server
    /// colour and the palette default. Persisted in calendar.json.
    calendar_colors: RefCell<HashMap<i64, String>>,
    /// Latest events from the engine, kept so toggling visibility /
    /// changing hour-range can re-layout without a server round-trip.
    calendar_events: RefCell<Vec<ddmail_core::types::DesktopCalendarEvent>>,
    /// First day of the currently displayed week (Monday) in local
    /// time, as days since the unix epoch. Stored as i64 so the
    /// timezone-conversion math is straightforward.
    calendar_week_start_days: Cell<i64>,
    /// Live size of the calendar grid body (px), mirrored from Slint so the
    /// layout math can decide day-count / hour-height / what to hide.
    grid_canvas_w: Cell<f32>,
    grid_canvas_h: Cell<f32>,
    /// Working-day window (local hours) — the band kept visible when 0–24
    /// can't fit; outside it is shaded. Configurable in settings.
    work_start: Cell<i32>,
    work_end: Cell<i32>,
    /// Manual zoom (px); 0 = automatic fit. Set on ctrl / ctrl-alt scroll,
    /// after which manual zoom wins over autofit (per spec).
    manual_hour_h: Cell<f32>,
    manual_col_w: Cell<f32>,
    /// Event being edited (0 in create mode).
    editing_event_id: Cell<i64>,
    /// Writable calendar ids, parallel to the edit-form's ComboBox model.
    edit_cal_ids: RefCell<Vec<i64>>,
    /// Last-fetched address book (parallel to the `address-book` Slint model),
    /// so the contact editor can read a row's full data by index.
    address_book: RefCell<Vec<ddmail_core::types::DesktopContact>>,
    /// Contact being edited (0 in create mode).
    editing_contact_id: Cell<i64>,
    /// Files staged for the next outgoing message, picked via the composer's
    /// attach button. Parallel to the `composer-attachments` Slint model
    /// (which holds just the basenames). Cleared once a message is staged.
    compose_attachments: RefCell<Vec<std::path::PathBuf>>,
    /// Event a reminder toast asked to open (0 = none); consumed once the
    /// calendar events for its week arrive from the engine.
    pending_open_event: Cell<i64>,
    /// (event_id, occurrence_start_ms, occurrence_end_ms, toast_id, summary)
    /// the snooze modal is acting on. toast_id lets a committed choice close
    /// the originating toast and a cancel resume its paused timer.
    snooze_ctx: RefCell<(i64, i64, i64, u64, String)>,
    /// Per-render map of on-screen occurrences for drag-move: (event_id, day)
    /// → (occurrence_start_ms, occurrence_end_ms, recurring). Lets the drag
    /// handler recover the EXACT instance start (recurrence_id for scope=single)
    /// — Slint `int` is i32 and can't carry epoch-ms. Rebuilt each layout.
    cal_occ: RefCell<HashMap<(i32, i32), (i64, i64, bool)>>,
}

thread_local! {
    /// Set once on the UI thread so engine-result closures (posted via
    /// invoke_from_event_loop, which must be Send + 'static and can't capture
    /// the Rc) can reach the shared state.
    static SHARED: RefCell<Option<Rc<Shared>>> = const { RefCell::new(None) };
}

#[cfg(any(windows, target_os = "linux"))]
thread_local! {
    /// The tray handle — kept here so the new-mail path can flip the
    /// unread dot from engine-result handlers.
    static TRAY: RefCell<Option<tray::Tray>> = const { RefCell::new(None) };
}

/// Show + un-minimize + bring to foreground. Plain `ui.show()` is a no-op
/// for a window that is minimized or buried under others — tray and toast
/// clicks must actually surface it. SetForegroundWindow is allowed to
/// succeed here because the click that got us called counts as user input.
fn raise_window(ui: &MainWindow) {
    let _ = ui.show();
    #[cfg(windows)]
    {
        use raw_window_handle::{HasWindowHandle, RawWindowHandle};
        use windows::Win32::UI::WindowsAndMessaging::{
            IsIconic, SetForegroundWindow, ShowWindow, SW_RESTORE,
        };
        let handle = ui.window().window_handle();
        if let Ok(wh) = handle.window_handle() {
            if let RawWindowHandle::Win32(h) = wh.as_raw() {
                let hwnd =
                    windows::Win32::Foundation::HWND(h.hwnd.get() as *mut core::ffi::c_void);
                unsafe {
                    if IsIconic(hwnd).as_bool() {
                        let _ = ShowWindow(hwnd, SW_RESTORE);
                    }
                    let _ = SetForegroundWindow(hwnd);
                }
            }
        }
    }
    #[cfg(target_os = "linux")]
    {
        use raw_window_handle::{HasWindowHandle, RawWindowHandle};
        let handle = ui.window().window_handle();
        if let Ok(wh) = handle.window_handle() {
            if let RawWindowHandle::Xlib(h) = wh.as_raw() {
                activate_window_x11(h.window);
            }
        }
    }
}

/// Raise + focus an X11 window the WM-friendly way: send `_NET_ACTIVE_WINDOW`
/// (EWMH) to the root window. Uses a throwaway display connection so we never
/// race the winit/Slint event loop's own Xlib connection. Best-effort — failures
/// (e.g. a Wayland session or a WM ignoring the hint) are silently no-ops.
#[cfg(target_os = "linux")]
fn activate_window_x11(window: std::os::raw::c_ulong) {
    use std::ptr;
    use x11::xlib;
    unsafe {
        let dpy = xlib::XOpenDisplay(ptr::null());
        if dpy.is_null() {
            return;
        }
        let net_active =
            xlib::XInternAtom(dpy, c"_NET_ACTIVE_WINDOW".as_ptr(), xlib::False);
        let root = xlib::XDefaultRootWindow(dpy);

        let mut data = xlib::ClientMessageData::new();
        // Source indication 2 (pager): the activation is a direct user action
        // via the tray, so the WM should honor it. Source 1 (application) is
        // subject to focus-stealing prevention — KWin would only flag the
        // window as "demands attention" instead of raising + focusing it.
        data.set_long(0, 2);
        data.set_long(1, xlib::CurrentTime as std::os::raw::c_long);
        data.set_long(2, 0); // no "requestor's currently active window"

        let mut ev = xlib::XEvent {
            client_message: xlib::XClientMessageEvent {
                type_: xlib::ClientMessage,
                serial: 0,
                send_event: xlib::True,
                display: dpy,
                window,
                message_type: net_active,
                format: 32,
                data,
            },
        };
        xlib::XSendEvent(
            dpy,
            root,
            xlib::False,
            xlib::SubstructureRedirectMask | xlib::SubstructureNotifyMask,
            &mut ev,
        );
        xlib::XRaiseWindow(dpy, window);
        xlib::XFlush(dpy);
        xlib::XCloseDisplay(dpy);
    }
}

/// Flip the tray unread dot (no-op where there's no tray backend).
fn tray_set_dot(on: bool) {
    #[cfg(any(windows, target_os = "linux"))]
    TRAY.with(|t| {
        if let Some(tr) = t.borrow().as_ref() {
            tr.set_unread_dot(on);
        }
    });
    #[cfg(not(any(windows, target_os = "linux")))]
    let _ = on;
}

/// UI weak handle reachable from non-UI threads (toast click callbacks hop
/// to the event loop through it).
static UI_WEAK: std::sync::OnceLock<slint::Weak<MainWindow>> = std::sync::OnceLock::new();

/// Open a conversation by index: show cached bodies immediately, and (if a live
/// engine is running) fire a background fetch to refresh them.
fn open_conversation(ui: &MainWindow, sh: &Shared, idx: usize) {
    let t0 = Instant::now();
    sh.current.set(idx);
    // New conversation generation: any in-flight FetchMessages answer for
    // the previously open conversation will be dropped on arrival.
    let generation = sh.open_gen.get() + 1;
    sh.open_gen.set(generation);
    let convs = sh.convs.borrow();
    let Some(c) = convs.get(idx) else { return };
    let conv_label = c.label.clone();
    let msg_count = c.messages.len();
    // Which account this conversation belongs to (empty → primary). Drives the
    // cache namespace and every addressed command issued while it's open.
    let akey = if c.account_key.is_empty() {
        sh.key.clone()
    } else {
        c.account_key.clone()
    };
    sh.cur_account_key.replace(akey.clone());
    // Replies default to the identity that received this conversation; the
    // from-picker shows it and the user can still override before sending.
    aim_composer_identity(ui, sh, &c.received_by);
    ui.set_identity_menu_open(false);

    // The right pane clears IMMEDIATELY: stale bubbles from the previous
    // conversation must never linger while this one loads. The progress
    // bar appears right away (seeded with the ref count; the render job
    // re-seeds it with the real body count when it starts).
    ui.set_messages(ModelRc::new(VecModel::from(Vec::<RowItem>::new())));
    ui.set_render_total(msg_count.max(1) as i32);
    ui.set_render_progress(0);
    sh.current_msgs.borrow_mut().clear();
    sh.current_bodies.borrow_mut().clear();

    // Unread snapshot BEFORE we mark anything read — it anchors the
    // scroll (first unread at top; none unread → scroll to the end).
    let unread: Vec<MessageRef> = c.messages.iter().filter(|m| !m.seen).cloned().collect();
    let had_unread = !unread.is_empty();
    *sh.open_unread.borrow_mut() =
        unread.iter().map(|m| (m.folder.clone(), m.uid)).collect();
    sh.scroll_pending.set(true);

    if let Some(cache) = &sh.cache {
        let key = &akey;
        let t_load_start = Instant::now();
        let bodies = cache.load_message_bodies(key, &c.messages).unwrap_or_default();
        let load_ms = t_load_start.elapsed().as_millis();
        if !bodies.is_empty() {
            *sh.current_msgs.borrow_mut() =
                bodies.iter().map(|b| MessageRef { folder: b.folder.clone(), uid: b.uid, message_id: b.message_id.clone(), seen: true }).collect();
            *sh.current_bodies.borrow_mut() = bodies.clone();
            println!(
                "[perf] open_conversation idx={idx} label={conv_label:?} \
                 messages={msg_count} bodies={} cache_load={load_ms}ms enqueue@{:?}",
                bodies.len(),
                t0.elapsed()
            );
            // Peek, don't consume: the FetchMessages answer will re-render
            // and must carry the same anchor (it aborts this render).
            let scroll = take_scroll_target(sh, &bodies, false);
            send_render_job(sh, bodies, scroll);
        } else {
            println!(
                "[perf] open_conversation idx={idx} label={conv_label:?} \
                 messages={msg_count} cache_miss (no cached bodies) cache_load={load_ms}ms"
            );
        }
    }
    if let Some(etx) = sh.engine_tx.borrow().as_ref() {
        // A toast-click target may be newer than the cached conversation
        // refs — make sure the fetch includes it.
        let mut fetch_refs = c.messages.clone();
        if let Some((f, u)) = sh.pending_open_ref.borrow().clone() {
            if !fetch_refs.iter().any(|m| m.uid == u) {
                // Toast target identified only by (folder, uid); no RFC
                // Message-ID here — server falls back to uid for this one.
                fetch_refs.push(MessageRef { folder: f, uid: u, message_id: String::new(), seen: false });
            }
        }
        let _ = etx.send(engine::EngineCmd::FetchMessages {
            messages: fetch_refs,
            generation,
            account_key: akey.clone(),
        });
        // Opening a conversation reads it: push \Seen for everything that
        // was unread. The scroll anchor above is already snapshotted.
        if !unread.is_empty() {
            let _ = etx.send(engine::EngineCmd::SetFlags {
                messages: unread,
                flags: "\\Seen".into(),
                add: true,
                account_key: akey.clone(),
            });
        }
    }
    drop(convs);
    // Optimistic badge clear: the server-side mark-read lands via the
    // delta refetch a second later, but the sidebar must not keep showing
    // an unread pill for the conversation the user is literally reading.
    if had_unread {
        if let Some(c) = sh.convs.borrow_mut().get_mut(idx) {
            c.unread_count = 0;
            for m in c.messages.iter_mut() {
                m.seen = true;
            }
        }
        let displays = displays_from(&sh.convs.borrow(), &sh.identity_colors.borrow());
        *sh.displays.borrow_mut() = displays;
        refresh_sidebar(sh, ui);
    }
}

/// Pull every http(s) URL out of free text (calendar event fields keep
/// meeting links as plain text more often than not). Trailing punctuation
/// is trimmed; order preserved, duplicates dropped.
fn extract_urls(texts: &[&str]) -> Vec<String> {
    use std::sync::OnceLock;
    static URL_RE: OnceLock<regex::Regex> = OnceLock::new();
    let re = URL_RE.get_or_init(|| {
        regex::Regex::new(r#"https?://[^\s<>"'\)\]]+"#).unwrap()
    });
    let mut seen = HashSet::new();
    let mut out = Vec::new();
    for t in texts {
        for m in re.find_iter(t) {
            let url = m.as_str().trim_end_matches(['.', ',', ';', ':', '!', '?', '»']);
            if !url.is_empty() && seen.insert(url.to_string()) {
                out.push(url.to_string());
            }
        }
    }
    out
}

/// Index of the text run nearest to (x, y): the containing run when there
/// is one, otherwise the run with the closest centre. None for empty layers.
fn nearest_run(runs: &[render_common::TextRun], x: f32, y: f32) -> Option<usize> {
    if let Some(i) = runs.iter().position(|r| r.contains(x, y)) {
        return Some(i);
    }
    runs.iter()
        .enumerate()
        .min_by(|(_, a), (_, b)| {
            let da = (a.x + a.w / 2.0 - x).powi(2) + (a.y + a.h / 2.0 - y).powi(2);
            let db = (b.x + b.w / 2.0 - x).powi(2) + (b.y + b.h / 2.0 - y).powi(2);
            da.partial_cmp(&db).unwrap_or(std::cmp::Ordering::Equal)
        })
        .map(|(i, _)| i)
}

/// Merged highlight rects for a run range: consecutive selected words on the
/// same visual line merge into one rect. Pure — shared by the bubble rows and
/// the source-viewer modal.
fn selection_rects_for(runs: &[render_common::TextRun], anchor: usize, head: usize) -> Vec<SelRect> {
    let mut rects: Vec<SelRect> = Vec::new();
    if runs.is_empty() {
        return rects;
    }
    let (lo, hi) = (anchor.min(head), anchor.max(head).min(runs.len() - 1));
    for r in &runs[lo..=hi] {
        match rects.last_mut() {
            // Same visual line → extend the previous rect.
            Some(last) if (last.y - r.y).abs() < r.h * 0.6 => {
                let right = (r.x + r.w).max(last.x + last.w);
                last.x = last.x.min(r.x);
                last.w = right - last.x;
                last.h = last.h.max(r.h);
            }
            _ => rects.push(SelRect { x: r.x, y: r.y, w: r.w, h: r.h }),
        }
    }
    rects
}

/// Selected words joined back into text: spaces within a line, a newline when
/// the next word starts a new visual line. Pure — shared by rows and modal.
fn selection_text_for(runs: &[render_common::TextRun], anchor: usize, head: usize) -> Option<String> {
    if runs.is_empty() {
        return None;
    }
    let (lo, hi) = (anchor.min(head), anchor.max(head).min(runs.len() - 1));
    let mut out = String::new();
    let mut prev: Option<&render_common::TextRun> = None;
    for r in &runs[lo..=hi] {
        if let Some(p) = prev {
            if (r.y - p.y).abs() > p.h * 0.6 {
                out.push('\n');
            } else {
                out.push(' ');
            }
        }
        out.push_str(&r.text);
        prev = Some(r);
    }
    (!out.is_empty()).then_some(out)
}

/// Rebuild the bubble-row highlight rects for the current selection.
fn refresh_selection_rects(ui: &MainWindow, sh: &Shared) {
    let row = sh.sel_row.get();
    if row < 0 || !sh.sel_moved.get() {
        ui.set_selection_row(-1);
        ui.set_selection_rects(ModelRc::new(VecModel::from(Vec::<SelRect>::new())));
        return;
    }
    let runs_all = sh.row_text_runs.borrow();
    let Some(runs) = runs_all.get(row as usize) else { return };
    let rects = selection_rects_for(runs, sh.sel_anchor.get(), sh.sel_head.get());
    ui.set_selection_rects(ModelRc::new(VecModel::from(rects)));
    ui.set_selection_row(row);
}

/// Selected bubble-row text (legacy entry point for the row selection).
fn selection_text(sh: &Shared) -> Option<String> {
    let row = sh.sel_row.get();
    if row < 0 || !sh.sel_moved.get() {
        return None;
    }
    let runs_all = sh.row_text_runs.borrow();
    let runs = runs_all.get(row as usize)?;
    selection_text_for(runs, sh.sel_anchor.get(), sh.sel_head.get())
}

/// Put text on the system clipboard (best-effort).
thread_local! {
    // Held for the whole session. On X11 the clipboard is served live by the
    // owning process, so a Clipboard created per-call and dropped immediately
    // loses ownership the instant it returns — paste then comes back empty.
    // Keeping one instance alive keeps us the owner so paste actually works.
    static CLIPBOARD: RefCell<Option<arboard::Clipboard>> = RefCell::new(None);
}

fn clipboard_set(text: &str) {
    CLIPBOARD.with(|c| {
        let mut slot = c.borrow_mut();
        if slot.is_none() {
            match arboard::Clipboard::new() {
                Ok(cb) => *slot = Some(cb),
                Err(e) => {
                    eprintln!("clipboard init: {e}");
                    return;
                }
            }
        }
        if let Some(cb) = slot.as_mut() {
            if let Err(e) = cb.set_text(text.to_string()) {
                eprintln!("clipboard set: {e}");
            }
        }
    });
}

/// Tauri-era header meta line: counterpart address (1:1) or participants
/// (group), plus " → receiving identity".
fn conv_meta(c: &Conversation) -> String {
    let mut m = if c.is_group {
        c.counterparts
            .iter()
            .map(|cp| if cp.name.is_empty() { cp.addr.clone() } else { cp.name.clone() })
            .collect::<Vec<_>>()
            .join(", ")
    } else {
        c.counterparts.first().map(|cp| cp.addr.clone()).unwrap_or_default()
    };
    if !c.received_by.is_empty() {
        m.push_str(" → ");
        m.push_str(&c.received_by);
    }
    m
}

/// Set every header property for conversation `idx` in one place: name,
/// initials, avatar colour, identity tint and the meta line. Replaces the
/// four hand-synced copies the review flagged.
fn apply_active_header(ui: &MainWindow, sh: &Shared, idx: usize) {
    if let Some(d) = sh.displays.borrow().get(idx) {
        ui.set_active_name(d.name.clone().into());
        ui.set_active_initials(d.initials.clone().into());
        ui.set_active_color(slint::Brush::SolidColor(hex(&d.color)));
        ui.set_active_ident_color(if d.ident_color.is_empty() {
            slint::Brush::SolidColor(hex("#ffffff"))
        } else {
            slint::Brush::SolidColor(hex(&d.ident_color))
        });
    }
    let meta = sh.convs.borrow().get(idx).map(conv_meta).unwrap_or_default();
    ui.set_active_meta(meta.into());
}

/// Mirror the policy's global «Медиа…» switches into root properties so
/// the menu's checkmarks and enabled-states stay live.
fn sync_media_globals(ui: &MainWindow, p: &policy::Policy) {
    ui.set_media_allow_all_on(p.allow_all);
    ui.set_media_all_images_on(p.allow_all_media);
    ui.set_media_all_scripts_on(p.allow_all_scripts);
}

/// Consume the pending post-open scroll: row index of the LAST unread
/// body (top-aligned), or -1 for "scroll to the end" when everything was
/// already read. None when this render isn't the first one after open.
/// Post-open scroll anchor: the FIRST unread row (the whole unread run then
/// reads top-to-bottom), or -1 (= scroll to end) when nothing is unread.
/// `consume` clears the pending flag; peek mode
/// (`consume=false`) is for the optimistic cached render — the anchor must
/// survive until the FetchMessages answer re-renders the conversation,
/// because that render ABORTS the cached one (seq bump) and would otherwise
/// arrive with no scroll target, losing the jump entirely.
fn take_scroll_target(sh: &Shared, bodies: &[MessageBody], consume: bool) -> Option<i32> {
    if !sh.scroll_pending.get() {
        return None;
    }
    if consume {
        sh.scroll_pending.set(false);
    }
    // Toast-click target wins: jump straight to the clicked message. Peek
    // mode must not take() the ref — open_conversation still needs it for
    // fetch_refs, and the consuming render needs it to re-anchor.
    let toast_uid = if consume {
        sh.pending_open_ref.borrow_mut().take().map(|(_, u)| u)
    } else {
        sh.pending_open_ref.borrow().as_ref().map(|(_, u)| *u)
    };
    if let Some(uid) = toast_uid {
        if let Some(r) = bodies.iter().position(|b| b.uid == uid) {
            return Some(r as i32);
        }
    }
    let unread = sh.open_unread.borrow();
    Some(
        bodies
            .iter()
            .position(|b| unread.contains(&(b.folder.clone(), b.uid)))
            .map(|r| r as i32)
            .unwrap_or(-1),
    )
}

/// Enqueue a (re)render of `bodies` at the current width/policy. Bumps the
/// shared render sequence so any older queued job becomes a no-op and a
/// mid-render older job aborts (latest wins).
fn send_render_job(sh: &Shared, bodies: Vec<MessageBody>, scroll_to: Option<i32>) {
    let seq = sh.render_seq.fetch_add(1, Ordering::SeqCst) + 1;
    let overrides = sh.body_view_text.borrow();
    let modes: Vec<u8> = bodies
        .iter()
        .map(|b| u8::from(overrides.contains(&(b.folder.clone(), b.uid))))
        .collect();
    drop(overrides);
    let _ = sh.tx.send(Job::SetConversation {
        bodies,
        width: sh.width.get(),
        policy: sh.policy.borrow().clone(),
        policy_gen: sh.policy_gen.get(),
        seq,
        scroll_to,
        modes,
    });
}

/// Rebuild the sidebar ConvItem list from displays + the avatar map.
fn sidebar_items(displays: &[Disp], avatars: &HashMap<String, Image>) -> Vec<ConvItem> {
    displays
        .iter()
        .map(|d| {
            let avatar = avatars.get(&d.email).cloned();
            ConvItem {
                name: d.name.clone().into(),
                preview: d.preview.clone().into(),
                initials: d.initials.clone().into(),
                color: slint::Brush::SolidColor(hex(&d.color)),
                time: "".into(),
                has_avatar: avatar.is_some(),
                avatar: avatar.unwrap_or_default(),
                ident_color: if d.ident_color.is_empty() {
                    slint::Brush::SolidColor(slint::Color::from_argb_u8(0, 0, 0, 0))
                } else {
                    slint::Brush::SolidColor(hex(&d.ident_color))
                },
                unread: d.unread as i32,
                highlight: false,
            }
        })
        .collect()
}

/// One row representing the transient-compose target — rendered at the
/// very top of the sidebar so the user sees "a chat" with the new
/// recipient before any message has been sent. Telegram does the same.
fn pending_compose_item(target: &str) -> ConvItem {
    let initials = target
        .chars()
        .next()
        .map(|c| c.to_uppercase().to_string())
        .unwrap_or_default();
    ConvItem {
        name: target.to_string().into(),
        preview: "Новое сообщение".into(),
        initials: initials.into(),
        color: slint::Brush::SolidColor(hex("#2f80ed")),
        time: "".into(),
        has_avatar: false,
        avatar: Image::default(),
        ident_color: slint::Brush::SolidColor(slint::Color::from_argb_u8(0, 0, 0, 0)),
        unread: 0,
        highlight: false,
    }
}

/// One-second attention flash on a sidebar row (model index): flips the
/// row's highlight on, then off after 150 ms — the Slint side fades the
/// overlay out over ~a second.
fn flash_sidebar_row(ui: &MainWindow, model_idx: usize) {
    use slint::Model;
    let model = ui.get_conversations();
    let Some(mut item) = model.row_data(model_idx) else { return };
    item.highlight = true;
    model.set_row_data(model_idx, item.clone());
    let ui_weak = ui.as_weak();
    slint::Timer::single_shot(std::time::Duration::from_millis(150), move || {
        if let Some(ui) = ui_weak.upgrade() {
            let model = ui.get_conversations();
            if let Some(mut item) = model.row_data(model_idx) {
                item.highlight = false;
                model.set_row_data(model_idx, item);
            }
        }
    });
}

/// Extract a bare lowercase address from a "Name <addr>" header value.
fn header_addr(raw: &str) -> String {
    if let (Some(i), Some(j)) = (raw.rfind('<'), raw.rfind('>')) {
        if i < j {
            return raw[i + 1..j].trim().to_lowercase();
        }
    }
    raw.trim().to_lowercase()
}

/// Push the latest displays + pending-compose state into the Slint
/// sidebar model. When `pending_compose` is Some, prepend a synthetic
/// "new chat" row at index 0 and select it.
fn refresh_sidebar(sh: &Shared, ui: &MainWindow) {
    let displays = sh.displays.borrow();
    let avatars = sh.avatars.borrow();
    let pending = sh.pending_compose.borrow().clone();

    let mut items = Vec::with_capacity(displays.len() + 1);
    if let Some(target) = pending.as_ref() {
        items.push(pending_compose_item(target));
    }
    items.extend(sidebar_items(&displays, &avatars));
    ui.set_conversations(ModelRc::new(VecModel::from(items)));
    if pending.is_some() {
        ui.set_selected(0);
    }
}

/// Days-since-epoch for the Monday of the calendar week containing
/// "today" (local time).
fn week_start_days_today() -> i64 {
    use chrono::{Datelike, Duration, Local};
    let today = Local::now().date_naive();
    let from_mon = today.weekday().num_days_from_monday() as i64;
    let monday = today - Duration::days(from_mon);
    monday
        .signed_duration_since(chrono::NaiveDate::from_ymd_opt(1970, 1, 1).unwrap())
        .num_days()
}

/// Same, but for the week containing an arbitrary timestamp — used to
/// navigate the calendar to a reminder's occurrence.
fn week_start_days_for_ms(ms: i64) -> i64 {
    use chrono::{Datelike, Duration, Local, TimeZone};
    let date = Local
        .timestamp_millis_opt(ms)
        .single()
        .map(|t| t.date_naive())
        .unwrap_or_else(|| Local::now().date_naive());
    let monday = date - Duration::days(date.weekday().num_days_from_monday() as i64);
    monday
        .signed_duration_since(chrono::NaiveDate::from_ymd_opt(1970, 1, 1).unwrap())
        .num_days()
}

/// Route a reminder-toast action onto the UI loop. Toast callbacks fire on the
/// UI thread already, but hopping via the event loop keeps us clear of any
/// borrow that might be live while a toast window dispatches.
fn reminder_dispatch(action: &'static str, eid: i64, occ: i64, seq: i64, summary: String) {
    if let Some(weak) = UI_WEAK.get() {
        let _ = weak.upgrade_in_event_loop(move |ui| {
            SHARED.with(|s| {
                if let Some(sh) = s.borrow().as_ref() {
                    handle_reminder_action(&ui, sh, action, eid, occ, seq, &summary);
                }
            });
        });
    }
}

/// Smart-step «напомнить через …» options: only steps that land BEFORE the
/// event starts (`mins_until` = minutes from now to the occurrence). Returns
/// (value, label) pairs — value is minutes as a string for the snooze-choice
/// callback. «В момент начала» is a separate fixed button, so it's not here.
fn snooze_steps(mins_until: i64) -> Vec<(String, String)> {
    const STEPS: &[(i64, &str)] = &[
        (1, "Через 1 минуту"),
        (5, "Через 5 минут"),
        (10, "Через 10 минут"),
        (15, "Через 15 минут"),
        (30, "Через 30 минут"),
        (60, "Через 1 час"),
        (120, "Через 2 часа"),
        (180, "Через 3 часа"),
        (360, "Через 6 часов"),
        (720, "Через 12 часов"),
        (1440, "Через 1 день"),
    ];
    STEPS
        .iter()
        .filter(|(m, _)| *m < mins_until)
        .map(|(m, l)| (m.to_string(), l.to_string()))
        .collect()
}

/// Single dispatch point for what a calendar-reminder toast can ask for
/// (spec 2026-07-11):
///   cancel-occ    → «✕»: kill the whole cascade of this occurrence, forever.
///   timeout       → toast expired untouched: retire the row + arm the next
///                   cascade link (event-defined secondary alarm).
///   open-stay     → body of a «скоро» toast: navigate to the event, STOP the
///                   toast's timer, leave it open.
///   open-close    → body of an at-start / running toast: navigate + close.
///   snooze-window → «Напомнить позже»: pause the toast timer + open the
///                   in-app snooze dialog (choice committed in on_snooze_choice).
fn handle_reminder_action(
    ui: &MainWindow,
    sh: &Rc<Shared>,
    action: &str,
    event_id: i64,
    occ_ms: i64,
    seq: i64,
    summary: &str,
) {
    match action {
        "cancel-occ" => {
            if let Some(c) = sh.cache.as_ref() {
                let _ = c.cancel_occurrence_reminders(event_id, occ_ms);
            }
            toast_window::close_for_event(event_id);
        }
        "timeout" => {
            // The toast is already gone (tick removed it). Advance the
            // cascade so a secondary alarm can arm.
            if let Some(c) = sh.cache.as_ref() {
                let _ = c.reminder_timeout(event_id, occ_ms, seq);
            }
        }
        "snooze-window" => {
            let toast_id = toast_window::id_for_event(event_id);
            toast_window::pause_timer(toast_id);
            let now_ms = chrono::Utc::now().timestamp_millis();
            let mins_until = (occ_ms - now_ms) / 60_000;
            let opts: Vec<SnoozeOpt> = snooze_steps(mins_until)
                .into_iter()
                .map(|(value, label)| SnoozeOpt {
                    value: value.into(),
                    label: label.into(),
                })
                .collect();
            // occ_end recovered from the current calendar view (0 if unknown;
            // user_choice_reminder tolerates it).
            let occ_end = sh
                .calendar_events
                .borrow()
                .iter()
                .find(|e| e.id == event_id)
                .and_then(|e| e.dtend)
                .unwrap_or(0);
            sh.snooze_ctx
                .replace((event_id, occ_ms, occ_end, toast_id, summary.to_string()));
            ui.set_snooze_summary(summary.into());
            ui.set_snooze_options(slint::ModelRc::new(slint::VecModel::from(opts)));
            ui.set_snooze_visible(true);
            raise_window(ui);
        }
        "open-stay" | "open-close" => {
            if action == "open-close" {
                toast_window::close_for_event(event_id);
            } else {
                // Body click on «скоро»: freeze the toast, it stays until the
                // user dismisses it (spec: таймер закрытия останавливается).
                toast_window::stop_timer(toast_window::id_for_event(event_id));
            }
            raise_window(ui);
            sh.calendar_week_start_days.set(week_start_days_for_ms(occ_ms));
            sh.pending_open_event.set(event_id);
            ui.set_view_mode(1);
            apply_calendar_view(ui, sh);
            if let Some(etx) = sh.engine_tx.borrow().as_ref() {
                let _ = etx.send(engine::EngineCmd::FetchCalendars);
            }
            refetch_calendar_events(ui, sh);
        }
        other => eprintln!("reminders: unknown action {other:?}"),
    }
}

/// Side-by-side lanes for overlapping blocks within a day column: events
/// that share time split the column into equal-width lanes, like every
/// desktop calendar. Without this, same-time events from different
/// calendars paint over each other and only the topmost stays visible.
fn assign_overlap_lanes(blocks: &mut [EventBlock], day_count: i32) {
    // Assign greedy lanes within one cluster of transitively-overlapping
    // blocks, then split the column between the lanes used.
    fn flush(blocks: &mut [EventBlock], cluster: &mut Vec<usize>) {
        if cluster.is_empty() {
            return;
        }
        let mut lane_ends: Vec<f32> = Vec::new();
        let mut lane_of: Vec<usize> = Vec::with_capacity(cluster.len());
        for &i in cluster.iter() {
            let top = blocks[i].top;
            let lane = match lane_ends.iter().position(|&e| e <= top) {
                Some(l) => l,
                None => {
                    lane_ends.push(f32::MIN);
                    lane_ends.len() - 1
                }
            };
            lane_ends[lane] = top + blocks[i].h;
            lane_of.push(lane);
        }
        let n = lane_ends.len() as f32;
        for (k, &i) in cluster.iter().enumerate() {
            blocks[i].xf = lane_of[k] as f32 / n;
            blocks[i].wf = 1.0 / n;
        }
        cluster.clear();
    }

    for day in 0..day_count {
        let mut idx: Vec<usize> = (0..blocks.len()).filter(|&i| blocks[i].day == day).collect();
        idx.sort_by(|&a, &b| {
            blocks[a]
                .top
                .partial_cmp(&blocks[b].top)
                .unwrap_or(std::cmp::Ordering::Equal)
        });
        let mut cluster: Vec<usize> = Vec::new();
        let mut cluster_end = f32::MIN;
        for i in idx {
            if !cluster.is_empty() && blocks[i].top >= cluster_end {
                flush(blocks, &mut cluster);
                cluster_end = f32::MIN;
            }
            cluster_end = cluster_end.max(blocks[i].top + blocks[i].h);
            cluster.push(i);
        }
        flush(blocks, &mut cluster);
    }
}

/// Absolute ms of local midnight for the given calendar date. None only if
/// the local timezone genuinely has no midnight that day (DST gap).
fn local_midnight_ms(date: chrono::NaiveDate) -> Option<i64> {
    use chrono::{Local, TimeZone};
    Local
        .from_local_datetime(&date.and_hms_opt(0, 0, 0)?)
        .single()
        .map(|d| d.timestamp_millis())
}

/// Compute the [from_ms, to_ms) range covering the displayed week
/// (5 or 7 days, full 24h regardless of hour-toggle), anchored to LOCAL
/// Monday midnight — must match apply_calendar_view's window.
fn week_range_ms(week_start_days: i64, day_count: i32) -> (i64, i64) {
    use chrono::Duration;
    let day_ms: i64 = 24 * 60 * 60 * 1000;
    let monday = chrono::NaiveDate::from_ymd_opt(1970, 1, 1).unwrap()
        + Duration::days(week_start_days);
    let from = local_midnight_ms(monday).unwrap_or(week_start_days * day_ms);
    let to = from + day_count as i64 * day_ms;
    (from, to)
}

/// Snapshot the current calendar-view preferences into calendar.json.
/// Called immediately after every change (visibility / colour / panel /
/// day- and hour-range toggles) — never deferred to exit.
fn save_calendar_settings(ui: &MainWindow, sh: &Shared) {
    let hidden: Vec<i64> = sh
        .calendar_visible
        .borrow()
        .iter()
        .filter(|(_, visible)| !**visible)
        .map(|(id, _)| *id)
        .collect();
    calendar_settings::save(&calendar_settings::CalendarSettings {
        hidden,
        colors: sh.calendar_colors.borrow().clone(),
        notify_sound: ui.get_notify_sound_on(),
        panel_collapsed: ui.get_calendar_panel_collapsed(),
        work_start_hour: sh.work_start.get(),
        work_end_hour: sh.work_end.get(),
        manual_hour_height: sh.manual_hour_h.get(),
        manual_col_width: sh.manual_col_w.get(),
    });
}

/// Hard floors from the spec: an hour cell is never shorter than 52px nor a
/// day column narrower than 300px. The 48px gutter holds the time labels.
const MIN_HOUR_H: f32 = 52.0;
const MIN_COL_W: f32 = 300.0;
const GUTTER_W: f32 = 48.0;

/// Choose day-count + column width for the available width.
///   - manual day-zoom → 7-day content at the chosen width (horizontal scroll)
///   - else 7 days if they fit at ≥300px (filling the width)
///   - else 5 days (Mon–Fri) if they fit
///   - else 5 days at the 300px floor (horizontal scroll)
fn compute_horizontal(canvas_w: f32, manual_col_w: f32) -> (i32, f32) {
    let avail = (canvas_w - GUTTER_W).max(MIN_COL_W);
    if manual_col_w > 0.0 {
        return (7, manual_col_w.clamp(MIN_COL_W, avail));
    }
    if avail >= 7.0 * MIN_COL_W {
        (7, avail / 7.0)
    } else if avail >= 5.0 * MIN_COL_W {
        (5, avail / 5.0)
    } else {
        (5, MIN_COL_W)
    }
}

/// Choose the visible hour band + hour height for the available height.
/// Returns (vis_start, vis_end, hour_height) where the band is [vis_start,
/// vis_end) local hours.
///   - manual hour-zoom → full 0–24 at the chosen height (vertical scroll)
///   - else if 24h fits at ≥52px → fill the canvas with all 24h
///   - else if there are events outside work hours → full 0–24 scroll at 52px
///   - else hide non-work symmetrically: the work window plus as many equal
///     padding hours above/below as fit, filling the canvas (no scroll)
fn compute_vertical(
    canvas_h: f32,
    work_start: i32,
    work_end: i32,
    has_out_of_work: bool,
    manual_hour_h: f32,
) -> (i32, i32, f32) {
    let ws = work_start.clamp(0, 23);
    let we = work_end.clamp(ws + 1, 24);
    let h = canvas_h.max(MIN_HOUR_H);

    if manual_hour_h > 0.0 {
        return (0, 24, manual_hour_h.clamp(MIN_HOUR_H, h));
    }
    if h >= 24.0 * MIN_HOUR_H {
        return (0, 24, h / 24.0); // all day fits — fill
    }
    if has_out_of_work {
        return (0, 24, MIN_HOUR_H); // must show everything — scroll
    }
    // Hide non-work, keep work window + symmetric padding that still fits.
    let work_hours = (we - ws).max(1);
    let fit_rows = (h / MIN_HOUR_H).floor() as i32;
    if fit_rows <= work_hours {
        return (ws, we, MIN_HOUR_H); // even the work window must scroll
    }
    let extra = fit_rows - work_hours;
    let pad = (extra / 2).min(ws).min(24 - we);
    let top = ws - pad;
    let bottom = we + pad;
    let rows = (bottom - top).max(1);
    (top, bottom, h / rows as f32)
}

#[cfg(test)]
mod grid_layout_tests {
    use super::{compute_horizontal, compute_vertical, MIN_COL_W, MIN_HOUR_H};

    #[test]
    fn horizontal_seven_then_five_then_scroll() {
        // Wide enough for 7 columns → 7, filling the width.
        let (d, w) = compute_horizontal(48.0 + 7.0 * 320.0, 0.0);
        assert_eq!(d, 7);
        assert!((w - 320.0).abs() < 0.1);
        // Fits 5 but not 7 → 5 days, filled.
        let (d, w) = compute_horizontal(48.0 + 5.0 * 320.0, 0.0);
        assert_eq!(d, 5);
        assert!(w >= MIN_COL_W);
        // Too narrow even for 5 at the floor → 5 days at the 300px floor.
        let (d, w) = compute_horizontal(48.0 + 3.0 * MIN_COL_W, 0.0);
        assert_eq!(d, 5);
        assert!((w - MIN_COL_W).abs() < 0.1);
    }

    #[test]
    fn horizontal_manual_zoom_is_seven_days_clamped() {
        let (d, w) = compute_horizontal(2000.0, 9999.0);
        assert_eq!(d, 7);
        assert!(w <= 2000.0 - 48.0 + 0.1); // clamped to available width
        let (_, w) = compute_horizontal(2000.0, 100.0);
        assert!((w - MIN_COL_W).abs() < 0.1); // clamped up to the floor
    }

    #[test]
    fn vertical_fills_when_all_day_fits() {
        // Plenty of height → all 24h, filled (hour height > floor).
        let (s, e, hh) = compute_vertical(24.0 * 80.0, 8, 19, false, 0.0);
        assert_eq!((s, e), (0, 24));
        assert!((hh - 80.0).abs() < 0.1);
    }

    #[test]
    fn vertical_scrolls_full_day_when_out_of_work_events() {
        // Can't fit 24h and there ARE out-of-work events → 0–24 at the floor.
        let (s, e, hh) = compute_vertical(10.0 * MIN_HOUR_H, 8, 19, true, 0.0);
        assert_eq!((s, e), (0, 24));
        assert!((hh - MIN_HOUR_H).abs() < 0.1);
    }

    #[test]
    fn vertical_hides_non_work_symmetrically() {
        // Work window 8–19 (11h). Room for ~15 rows → 4 extra → 2 above/below.
        let h = 15.0 * MIN_HOUR_H;
        let (s, e, hh) = compute_vertical(h, 8, 19, false, 0.0);
        assert_eq!(s, 6);
        assert_eq!(e, 21);
        assert!(hh >= MIN_HOUR_H); // fills, no scroll
        assert!((hh - h / 15.0).abs() < 0.1);
    }

    #[test]
    fn vertical_manual_zoom_full_day_scroll() {
        let (s, e, hh) = compute_vertical(600.0, 8, 19, false, 120.0);
        assert_eq!((s, e), (0, 24));
        assert!((hh - 120.0).abs() < 0.1);
    }
}

fn apply_calendar_view(ui: &MainWindow, sh: &Shared) {
    use chrono::{Datelike, Duration, NaiveDate};
    let (day_count, col_width) =
        compute_horizontal(sh.grid_canvas_w.get(), sh.manual_col_w.get());
    ui.set_col_width(col_width);
    // Dash segments per quarter-hour line (24px period), capped so a very
    // wide manual zoom can't spawn an absurd number of rects.
    let dash_count = ((day_count as f32 * col_width) / 24.0)
        .floor()
        .clamp(0.0, 160.0) as i32;
    ui.set_dash_count(dash_count);
    let week_days = sh.calendar_week_start_days.get();
    let monday = NaiveDate::from_ymd_opt(1970, 1, 1).unwrap()
        + Duration::days(week_days);
    let headers: Vec<slint::SharedString> = (0..day_count as i64)
        .map(|i| {
            let d = monday + Duration::days(i);
            const NAMES: [&str; 7] = ["Пн", "Вт", "Ср", "Чт", "Пт", "Сб", "Вс"];
            let n = NAMES[d.weekday().num_days_from_monday() as usize];
            format!("{n}, {:02}.{:02}", d.day(), d.month()).into()
        })
        .collect();
    ui.set_day_headers(slint::ModelRc::new(slint::VecModel::from(headers)));
    ui.set_day_count(day_count);
    // Mark "today" when it falls inside the displayed week: its column
    // index drives the header highlight + column tint, and the current
    // local time drives the now-line.
    {
        let now = chrono::Local::now();
        let today_days = (now.date_naive() - NaiveDate::from_ymd_opt(1970, 1, 1).unwrap()).num_days();
        let col = today_days - week_days;
        let in_view = (0..day_count as i64).contains(&col);
        ui.set_today_col(if in_view { col as i32 } else { -1 });
        use chrono::Timelike;
        ui.set_now_hour(now.hour() as f32 + now.minute() as f32 / 60.0);
    }
    let title = {
        const MONTHS: [&str; 12] = [
            "Январь", "Февраль", "Март", "Апрель", "Май", "Июнь",
            "Июль", "Август", "Сентябрь", "Октябрь", "Ноябрь", "Декабрь",
        ];
        format!("{} {}", MONTHS[(monday.month() - 1) as usize], monday.year())
    };
    ui.set_week_title(title.into());

    // Sidebar — calendar list. Sorted by name for stability. User-picked
    // colour overrides win over server colour / palette default.
    let cal_items: Vec<CalendarItem> = {
        let cals = sh.calendars.borrow();
        let visibility = sh.calendar_visible.borrow();
        let overrides = sh.calendar_colors.borrow();
        let mut v: Vec<&ddmail_core::types::DesktopCalendar> = cals.iter().collect();
        v.sort_by(|a, b| a.name.to_lowercase().cmp(&b.name.to_lowercase()));
        v.into_iter()
            .map(|c| CalendarItem {
                id: c.id as i32,
                name: c.name.clone().into(),
                color: hex(&overrides
                    .get(&c.id)
                    .cloned()
                    .unwrap_or_else(|| cal_color(c.id, &c.color)))
                .into(),
                visible: *visibility.get(&c.id).unwrap_or(&true),
            })
            .collect()
    };
    ui.set_calendars(slint::ModelRc::new(slint::VecModel::from(cal_items)));

    // Place event blocks in two passes. Pass 1 expands every event into
    // per-day timed segments + all-day chips, recording whether anything
    // falls outside the work window (drives the vertical layout choice).
    // Pass 2 — after the vertical band is known — turns segments into
    // positioned blocks.
    let day_ms: i64 = 24 * 60 * 60 * 1000;
    // Week window in LOCAL time: the grid's day columns are local days, so
    // the window must start at local Monday midnight, not UTC midnight.
    let week_start_ms = local_midnight_ms(monday).unwrap_or(week_days * day_ms);
    // Always expand a full 7 days so dropping to a 5-day view doesn't lose
    // data and the out-of-work scan stays stable; layout clamps to day_count.
    let week_end_ms = week_start_ms + 7 * day_ms;

    /// One timed segment confined to a single day column.
    struct Seg {
        id: i32,
        day: i32,
        start_in_day: i64, // ms from that day's local midnight
        end_in_day: i64,
        color: slint::Color,
        title: slint::SharedString,
        time: slint::SharedString,
        count: i32,
        tentative: bool,
        writable: bool, // calendar can_write → drag/resize allowed
    }

    let (segs, all_day_blocks, all_day_rows, has_out_of_work, occ_map) = {
        let events = sh.calendar_events.borrow();
        let visibility = sh.calendar_visible.borrow();
        let cals = sh.calendars.borrow();
        let overrides = sh.calendar_colors.borrow();
        let color_for = |cal_id: i64| -> slint::Color {
            let raw = overrides.get(&cal_id).cloned().unwrap_or_else(|| {
                cals.iter()
                    .find(|c| c.id == cal_id)
                    .map(|c| cal_color(c.id, &c.color))
                    .unwrap_or_else(|| "#3788d8".to_string())
            });
            hex(&raw)
        };
        let fmt_hm = |abs_ms: i64| -> String {
            use chrono::{Local, TimeZone, Timelike};
            match Local.timestamp_millis_opt(abs_ms).single() {
                Some(d) => format!("{:02}:{:02}", d.hour(), d.minute()),
                None => "??:??".into(),
            }
        };

        let mut segs: Vec<Seg> = Vec::new();
        let mut occ_map: HashMap<(i32, i32), (i64, i64, bool)> = HashMap::new();
        let mut all_day_blocks: Vec<AllDayBlock> = Vec::new();
        let mut all_day_fill = vec![0i32; day_count.max(0) as usize];
        let idents = sh.identity_colors.borrow();
        let me_key = sh.key.to_lowercase();
        let ws_ms = sh.work_start.get() as i64 * 3_600_000;
        let we_ms = sh.work_end.get() as i64 * 3_600_000;
        let mut has_out_of_work = false;

        for e in events.iter() {
            if !*visibility.get(&e.calendar_id).unwrap_or(&true) {
                continue;
            }
            let color = color_for(e.calendar_id);
            let writable = cals
                .iter()
                .find(|c| c.id == e.calendar_id)
                .map(|c| c.can_write)
                .unwrap_or(false);
            let att_count = e.attendees.len() as i32;
            let tentative = att_count >= 2
                && e
                    .attendees
                    .iter()
                    .find(|a| {
                        let lc = a.email.to_lowercase();
                        lc == me_key || idents.contains_key(&lc)
                    })
                    .map(|a| {
                        let ps = a.partstat.to_uppercase();
                        ps.is_empty() || ps == "NEEDS-ACTION"
                    })
                    .unwrap_or(false);
            let occ = recurrence::expand(
                e.dtstart,
                e.dtend,
                &e.rrule,
                &e.exdates,
                week_start_ms,
                week_end_ms,
            );
            if occ.is_empty() && !e.rrule.is_empty() {
                println!(
                    "[cal] no-occurrence: id={} dtstart={} rrule={:?} exdates={} {:?}",
                    e.id,
                    e.dtstart,
                    e.rrule,
                    e.exdates.len(),
                    e.summary.chars().take(30).collect::<String>()
                );
            }
            for o in occ {
                let first = ((o.start_ms - week_start_ms) / day_ms) as i32;
                let last = (((o.end_ms - 1) - week_start_ms) / day_ms) as i32;
                if e.all_day {
                    for day in first.max(0)..=last.min(day_count - 1) {
                        let idx = day as usize;
                        let row = all_day_fill[idx];
                        all_day_fill[idx] += 1;
                        all_day_blocks.push(AllDayBlock {
                            id: e.id as i32,
                            day,
                            row,
                            color,
                            title: e.summary.clone().into(),
                        });
                    }
                    continue;
                }
                let title: slint::SharedString = if e.summary.is_empty() {
                    "(без названия)".into()
                } else {
                    e.summary.clone().into()
                };
                let time: slint::SharedString =
                    format!("{} – {}", fmt_hm(o.start_ms), fmt_hm(o.end_ms)).into();
                for day in first.max(0)..=last.min(day_count - 1) {
                    let day_start_ms = week_start_ms + day as i64 * day_ms;
                    let start_in_day = (o.start_ms - day_start_ms).max(0);
                    let end_in_day = (o.end_ms - day_start_ms).min(day_ms);
                    if end_in_day <= start_in_day {
                        continue;
                    }
                    if start_in_day < ws_ms || end_in_day > we_ms {
                        has_out_of_work = true;
                    }
                    segs.push(Seg {
                        id: e.id as i32,
                        day,
                        start_in_day,
                        end_in_day,
                        color,
                        title: title.clone(),
                        time: time.clone(),
                        count: att_count,
                        tentative,
                        writable,
                    });
                    // Exact instance bounds for drag-move (recurrence_id +
                    // duration). Recurring → only a scope=single override moves
                    // the day (an "all" dtstart shift keeps BYDAY's weekday).
                    // An override row (non-empty recurrence_id, empty rrule)
                    // must ALSO stay scope=single: patching it as "all" would
                    // rewrite the master series' times with one occurrence's.
                    occ_map.insert(
                        (e.id as i32, day),
                        (o.start_ms, o.end_ms, !e.rrule.is_empty() || !e.recurrence_id.is_empty()),
                    );
                }
            }
        }
        let rows = *all_day_fill.iter().max().unwrap_or(&0);
        (segs, all_day_blocks, rows, has_out_of_work, occ_map)
    };
    *sh.cal_occ.borrow_mut() = occ_map;

    // Vertical band now that we know whether anything sits outside work hours.
    let (vis_start, vis_end, hour_height) = compute_vertical(
        sh.grid_canvas_h.get(),
        sh.work_start.get(),
        sh.work_end.get(),
        has_out_of_work,
        sh.manual_hour_h.get(),
    );
    ui.set_hour_height(hour_height);
    ui.set_hour_start(vis_start);
    ui.set_hour_end(vis_end);
    ui.set_work_start(sh.work_start.get());
    ui.set_work_end(sh.work_end.get());

    let visible_top_ms = vis_start as i64 * 3_600_000;
    let visible_bottom_ms = vis_end as i64 * 3_600_000;
    let to_px = |ms: i64| -> f32 { (ms - visible_top_ms) as f32 / 3_600_000.0 * hour_height };
    let mut blocks: Vec<EventBlock> = segs
        .iter()
        .filter_map(|s| {
            let top_ms = s.start_in_day.max(visible_top_ms);
            let bot_ms = s.end_in_day.min(visible_bottom_ms);
            if bot_ms <= top_ms {
                return None;
            }
            let top = to_px(top_ms);
            let h = (to_px(bot_ms) - top).max(18.0);
            Some(EventBlock {
                id: s.id,
                day: s.day,
                top,
                h,
                color: s.color,
                title: s.title.clone(),
                time: s.time.clone(),
                all_day: false,
                xf: 0.0,
                wf: 1.0,
                count: s.count,
                tentative: s.tentative,
                writable: s.writable,
            })
        })
        .collect();
    println!(
        "[cal] layout: blocks={} all_day={} days={} col_w={:.0} vis=[{}..{}) hh={:.0} oow={}",
        blocks.len(),
        all_day_blocks.len(),
        day_count,
        col_width,
        vis_start,
        vis_end,
        hour_height,
        has_out_of_work
    );
    assign_overlap_lanes(&mut blocks, day_count);
    ui.set_events(slint::ModelRc::new(slint::VecModel::from(blocks)));
    ui.set_all_day_events(slint::ModelRc::new(slint::VecModel::from(all_day_blocks)));
    ui.set_all_day_rows(all_day_rows);
}

/// Re-fire FetchCalendarEvents for the currently displayed week. Also
/// flips `calendar-loading` on so the topbar shows a "Загрузка…" pill
/// until the result lands.
fn refetch_calendar_events(ui: &MainWindow, sh: &Shared) {
    // Always fetch the full 7-day week so toggling to a 5-day view (or
    // horizontal scroll) never needs a refetch.
    let (from_ms, to_ms) = week_range_ms(sh.calendar_week_start_days.get(), 7);
    if let Some(etx) = sh.engine_tx.borrow().as_ref() {
        ui.set_calendar_loading(true);
        let _ = etx.send(engine::EngineCmd::FetchCalendarEvents {
            from_ms,
            to_ms,
            calendar_ids: Vec::new(),
        });
    }
}

/// Parse a form date/time string ("YYYY-MM-DD HH:MM", or "YYYY-MM-DD" when
/// all-day) as LOCAL time → ms since epoch. None on parse failure.
fn parse_form_ms(s: &str, all_day: bool) -> Option<i64> {
    use chrono::{Local, NaiveDate, NaiveDateTime, TimeZone};
    let s = s.trim();
    let naive = if all_day {
        NaiveDate::parse_from_str(s, "%Y-%m-%d").ok()?.and_hms_opt(0, 0, 0)?
    } else {
        NaiveDateTime::parse_from_str(s, "%Y-%m-%d %H:%M").ok()?
    };
    Local.from_local_datetime(&naive).single().map(|d| d.timestamp_millis())
}

/// Format ms → form string (date-only when all-day).
fn fmt_form(ms: i64, all_day: bool) -> String {
    use chrono::{Datelike, Local, TimeZone, Timelike};
    match chrono::Local.timestamp_millis_opt(ms).single() {
        Some(d) if all_day => format!("{:04}-{:02}-{:02}", d.year(), d.month(), d.day()),
        Some(d) => format!(
            "{:04}-{:02}-{:02} {:02}:{:02}",
            d.year(), d.month(), d.day(), d.hour(), d.minute()
        ),
        None => String::new(),
    }
}

/// Populate the edit-form's writable-calendar ComboBox; returns the ids
/// parallel to the model so the save step can map index → calendar_id.
///
/// `only_visible` restricts the list to calendars currently ticked active
/// in the sidebar — used when creating (you can only file a new event into
/// a calendar you're actually looking at). Editing passes `false` so an
/// event already living in a hidden calendar stays selectable.
fn fill_writable_calendars(ui: &MainWindow, sh: &Shared, only_visible: bool) -> Vec<i64> {
    let cals = sh.calendars.borrow();
    let visibility = sh.calendar_visible.borrow();
    let mut ids = Vec::new();
    let mut names: Vec<slint::SharedString> = Vec::new();
    for c in cals.iter().filter(|c| c.can_write) {
        if only_visible && !*visibility.get(&c.id).unwrap_or(&true) {
            continue;
        }
        ids.push(c.id);
        names.push(c.name.clone().into());
    }
    ui.set_edit_calendars(slint::ModelRc::new(slint::VecModel::from(names)));
    ids
}

/// Open the create form (blank, default now → +1h, first writable calendar).
fn open_create_form(ui: &MainWindow, sh: &Shared) {
    let now = chrono::Local::now().timestamp_millis();
    open_create_form_at(ui, sh, now);
}

/// Open the create form anchored at a specific start time (e.g. the slot a
/// double-click landed on), running one hour by default.
fn open_create_form_at(ui: &MainWindow, sh: &Shared, start_ms: i64) {
    // Active (visible) writable calendars only. If every writable calendar
    // is hidden, fall back to the full set so creation isn't a dead end.
    let mut ids = fill_writable_calendars(ui, sh, true);
    if ids.is_empty() {
        ids = fill_writable_calendars(ui, sh, false);
    }
    *sh.edit_cal_ids.borrow_mut() = ids;
    sh.editing_event_id.set(0);
    ui.set_edit_is_create(true);
    ui.set_edit_title("".into());
    ui.set_edit_all_day(false);
    ui.set_edit_start(fmt_form(start_ms, false).into());
    ui.set_edit_end(fmt_form(start_ms + 3_600_000, false).into());
    ui.set_edit_location("".into());
    ui.set_edit_description("".into());
    ui.set_edit_calendar_idx(0);
    ui.set_edit_organizer("".into());
    ui.set_edit_attendees(ModelRc::new(VecModel::from(Vec::<AttendeeItem>::new())));
    ui.set_edit_meta("".into());
    ui.set_edit_extras(ModelRc::new(VecModel::from(Vec::<EventExtraItem>::new())));
    ui.set_edit_visible(true);
}

/// Russian display label for an extra VEVENT property; unknown names pass
/// through as-is (they're already uppercased server-side).
fn extra_label(name: &str, value: &str) -> (String, String) {
    match name {
        "CONFERENCE" | "X-TELEMOST-CONFERENCE" | "X-GOOGLE-CONFERENCE" => {
            ("Видеовстреча".into(), value.into())
        }
        "URL" => ("Ссылка".into(), value.into()),
        "CATEGORIES" => ("Категории".into(), value.into()),
        "CLASS" => (
            "Доступ".into(),
            match value.to_uppercase().as_str() {
                "PRIVATE" => "Приватное".into(),
                "CONFIDENTIAL" => "Конфиденциальное".into(),
                _ => value.into(),
            },
        ),
        "TRANSP" => (
            "Занятость".into(),
            if value.eq_ignore_ascii_case("TRANSPARENT") {
                "Свободен".into()
            } else {
                value.into()
            },
        ),
        "PRIORITY" => ("Приоритет".into(), value.into()),
        "COMMENT" => ("Комментарий".into(), value.into()),
        "CONTACT" => ("Контакт".into(), value.into()),
        "ATTACH" => ("Вложение".into(), value.into()),
        _ => (name.into(), value.into()),
    }
}

/// PARTSTAT → status dot colour (green accepted / red declined / orange
/// tentative / grey no answer yet).
fn partstat_dot(partstat: &str) -> slint::Color {
    match partstat.to_uppercase().as_str() {
        "ACCEPTED" => slint::Color::from_rgb_u8(0x34, 0xa8, 0x53),
        "DECLINED" => slint::Color::from_rgb_u8(0xe2, 0x3b, 0x3b),
        "TENTATIVE" => slint::Color::from_rgb_u8(0xf5, 0xa6, 0x23),
        _ => slint::Color::from_rgb_u8(0xb5, 0xbc, 0xc6),
    }
}

/// "Имя <email>" when a display name exists, plain email otherwise.
fn person_label(name: &str, email: &str) -> String {
    if name.trim().is_empty() {
        email.to_string()
    } else {
        format!("{} <{}>", name.trim(), email)
    }
}

/// RRULE → короткая русская метка. Only FREQ/INTERVAL are surfaced — the
/// point is "это повторяющееся событие", not a full RFC 5545 rendering.
fn humanize_rrule(rrule: &str) -> String {
    let up = rrule.to_uppercase();
    let get = |k: &str| {
        up.split(&[';', ':'][..])
            .find_map(|p| p.strip_prefix(k).map(|v| v.to_string()))
    };
    let interval: u32 = get("INTERVAL=").and_then(|v| v.parse().ok()).unwrap_or(1);
    let (each, unit) = match get("FREQ=").as_deref() {
        Some("DAILY") => ("Ежедневно", "дн."),
        Some("WEEKLY") => ("Еженедельно", "нед."),
        Some("MONTHLY") => ("Ежемесячно", "мес."),
        Some("YEARLY") => ("Ежегодно", "г."),
        _ => return "Повторяется".to_string(),
    };
    if interval > 1 {
        format!("Каждые {interval} {unit}")
    } else {
        each.to_string()
    }
}

/// Minutes-before-start → "N мин" / "N ч" / "N дн".
fn humanize_lead(min: i32) -> String {
    if min % 1440 == 0 && min >= 1440 {
        format!("{} дн", min / 1440)
    } else if min % 60 == 0 && min >= 60 {
        format!("{} ч", min / 60)
    } else {
        format!("{min} мин")
    }
}

/// Open the edit form populated from an existing event.
fn open_edit_form(ui: &MainWindow, sh: &Shared, ev: &ddmail_core::types::DesktopCalendarEvent) {
    let ids = fill_writable_calendars(ui, sh, false);
    let idx = ids.iter().position(|&id| id == ev.calendar_id).unwrap_or(0) as i32;
    *sh.edit_cal_ids.borrow_mut() = ids;
    sh.editing_event_id.set(ev.id);
    ui.set_edit_is_create(false);
    ui.set_edit_title(ev.summary.clone().into());
    ui.set_edit_all_day(ev.all_day);
    ui.set_edit_start(fmt_form(ev.dtstart, ev.all_day).into());
    ui.set_edit_end(fmt_form(ev.dtend.unwrap_or(ev.dtstart), ev.all_day).into());
    ui.set_edit_location(ev.location.clone().into());
    ui.set_edit_description(ev.description.clone().into());
    ui.set_edit_calendar_idx(idx);

    // Read-only meeting details (see the card's edit-organizer block).
    ui.set_edit_organizer(
        if ev.organizer_email.is_empty() {
            String::new()
        } else {
            person_label(&ev.organizer_name, &ev.organizer_email)
        }
        .into(),
    );
    let attendees: Vec<AttendeeItem> = ev
        .attendees
        .iter()
        .map(|a| AttendeeItem {
            label: person_label(&a.name, &a.email).into(),
            dot: partstat_dot(&a.partstat),
        })
        .collect();
    ui.set_edit_attendees(ModelRc::new(VecModel::from(attendees)));
    let extras: Vec<EventExtraItem> = ev
        .extras
        .iter()
        .map(|x| {
            let (label, value) = extra_label(&x.name, &x.value);
            let is_link = value.starts_with("http://") || value.starts_with("https://");
            EventExtraItem {
                label: label.into(),
                value: value.into(),
                is_link,
            }
        })
        .collect();
    ui.set_edit_extras(ModelRc::new(VecModel::from(extras)));
    let mut meta: Vec<String> = Vec::new();
    match ev.status.to_uppercase().as_str() {
        "CANCELLED" => meta.push("Отменено".to_string()),
        "TENTATIVE" => meta.push("Предварительно".to_string()),
        _ => {}
    }
    if !ev.rrule.is_empty() {
        meta.push(humanize_rrule(&ev.rrule));
    }
    if ev.alarm_lead_min > 0 {
        meta.push(format!("Напоминание за {}", humanize_lead(ev.alarm_lead_min)));
    }
    ui.set_edit_meta(meta.join(" · ").into());

    ui.set_edit_visible(true);
}

/// Validate the form and dispatch Create or Patch to the engine.
fn save_edit_form(ui: &MainWindow, sh: &Shared) {
    let all_day = ui.get_edit_all_day();
    let Some(start) = parse_form_ms(&ui.get_edit_start(), all_day) else {
        eprintln!("edit: bad start time");
        return;
    };
    let end = parse_form_ms(&ui.get_edit_end(), all_day);
    let title = ui.get_edit_title().to_string();
    let location = ui.get_edit_location().to_string();
    let description = ui.get_edit_description().to_string();
    let Some(etx) = sh.engine_tx.borrow().clone() else { return };

    let editing = sh.editing_event_id.get();
    if editing == 0 {
        let idx = ui.get_edit_calendar_idx() as usize;
        let Some(&cal_id) = sh.edit_cal_ids.borrow().get(idx) else {
            eprintln!("edit: no writable calendar selected");
            return;
        };
        let mut body = serde_json::json!({
            "calendar_id": cal_id,
            "summary": title,
            "description": description,
            "location": location,
            "all_day": all_day,
            "dtstart": start,
        });
        if let Some(e) = end {
            body["dtend"] = e.into();
        }
        let _ = etx.send(engine::EngineCmd::CreateEvent { body });
    } else {
        let mut body = serde_json::json!({
            "scope": "all",
            "summary": title,
            "description": description,
            "location": location,
            "all_day": all_day,
            "dtstart": start,
        });
        body["dtend"] = end.unwrap_or(0).into(); // explicit 0 ⇒ clear on server
        // Edit drops all prior reminders for this event; the refetch's seed()
        // recreates them from the new settings (incl. wiping a manual snooze).
        if let Some(c) = sh.cache.as_ref() {
            let _ = c.purge_event_reminders(editing);
        }
        let _ = etx.send(engine::EngineCmd::PatchEvent { event_id: editing, body });
    }
    ui.set_edit_visible(false);
}

/// Seed the calendar view's read-only state before the engine produces
/// events: day labels for the current week + a sane initial vertical band
/// so the grid isn't blank. Real layout is computed in `apply_calendar_view`
/// once the grid's on-screen size is known.
fn apply_calendar_defaults(ui: &MainWindow) {
    use chrono::{Datelike, Duration, Local};
    let day_count = 7;
    let now = Local::now();
    // Week starts on Monday (ISO).
    let weekday_from_mon = now.weekday().num_days_from_monday() as i64;
    let monday = now.date_naive() - Duration::days(weekday_from_mon);
    let headers: Vec<slint::SharedString> = (0..day_count as i64)
        .map(|i| {
            let d = monday + Duration::days(i);
            const NAMES: [&str; 7] = ["Пн", "Вт", "Ср", "Чт", "Пт", "Сб", "Вс"];
            let n = NAMES[d.weekday().num_days_from_monday() as usize];
            format!("{n}, {:02}.{:02}", d.day(), d.month()).into()
        })
        .collect();
    ui.set_day_headers(slint::ModelRc::new(slint::VecModel::from(headers)));
    ui.set_day_count(day_count);
    ui.set_col_width(MIN_COL_W);
    let title = {
        use chrono::Datelike as _;
        const MONTHS: [&str; 12] = [
            "Январь", "Февраль", "Март", "Апрель", "Май", "Июнь",
            "Июль", "Август", "Сентябрь", "Октябрь", "Ноябрь", "Декабрь",
        ];
        format!("{} {}", MONTHS[(monday.month() - 1) as usize], monday.year())
    };
    ui.set_week_title(title.into());
    ui.set_hour_height(MIN_HOUR_H);
    if ui.get_hour_end() == 0 {
        ui.set_hour_start(8);
        ui.set_hour_end(19);
    }
    // Empty models so the for-loops don't trip on undefined.
    ui.set_calendars(slint::ModelRc::new(slint::VecModel::from(Vec::<CalendarItem>::new())));
    ui.set_events(slint::ModelRc::new(slint::VecModel::from(Vec::<EventBlock>::new())));
}

/// Minimal stdout logger: ddmail-core (NativeProvider, engine) reports
/// through the `log` crate, and without an installed logger those records
/// vanish — the WebSocket watcher's connect/refresh diagnostics were
/// invisible exactly when they were needed.
struct StdoutLogger;

impl log::Log for StdoutLogger {
    fn enabled(&self, m: &log::Metadata) -> bool {
        m.level() <= log::Level::Info
    }
    fn log(&self, r: &log::Record) {
        if self.enabled(r.metadata()) {
            println!("[{}] {}", r.level(), r.args());
        }
    }
    fn flush(&self) {}
}

static STDOUT_LOGGER: StdoutLogger = StdoutLogger;

/// `https://mail.letotam.ru:8443/x` → `mail.letotam.ru` — the host part only,
/// used as the account-key host for native-mode accounts.
fn host_from_url(url: &str) -> String {
    let s = url.trim();
    let s = s.strip_prefix("https://").or_else(|| s.strip_prefix("http://")).unwrap_or(s);
    s.split(['/', ':']).next().unwrap_or(s).to_string()
}

/// First-run login: modal window → POST /auth/login → accounts.json.
/// Returns false when the user closed the window without logging in
/// (the caller exits — there is nothing to show without an account).
fn run_login_window() -> bool {
    let Ok(lw) = LoginWindow::new() else { return false };
    let done = Arc::new(AtomicBool::new(false));

    let weak = lw.as_weak();
    let done_cb = done.clone();
    lw.on_submit(move || {
        let Some(lw) = weak.upgrade() else { return };
        if lw.get_busy() {
            return;
        }
        let username = lw.get_username().trim().to_string();
        let password = lw.get_password().to_string();

        // ── Standalone (IMAP + optional CalDAV/CardDAV): no server login,
        // just persist the account directly. ──
        if lw.get_mode() == 1 {
            let email = lw.get_email().trim().to_string();
            let host = lw.get_imap_host().trim().to_string();
            if email.is_empty() || host.is_empty() || username.is_empty() || password.is_empty() {
                lw.set_error("Заполните email, IMAP-сервер, логин и пароль".into());
                return;
            }
            let port: u16 = lw.get_imap_port().trim().parse().unwrap_or(993);
            let opt = |s: String| {
                let t = s.trim().to_string();
                if t.is_empty() { None } else { Some(t) }
            };
            let cfg = engine::AccountConfig {
                host: host.clone(),
                port,
                username,
                password,
                use_tls: true,
                email,
                smtp_host: host,
                smtp_port: 465,
                native_url: None,
                native_token: None,
                carddav_url: opt(lw.get_carddav_url().to_string()),
                caldav_url: opt(lw.get_caldav_url().to_string()),
                oauth_refresh_token: None,
            };
            engine::AccountConfig::save_all(std::slice::from_ref(&cfg));
            done_cb.store(true, Ordering::Relaxed);
            let _ = lw.hide();
            return;
        }

        // ── Native (our server): username+password → JWT. ──
        let server = lw.get_server_url().trim().trim_end_matches('/').to_string();
        if server.is_empty() || username.is_empty() || password.is_empty() {
            lw.set_error("Заполните все поля".into());
            return;
        }
        // The scheme is implied for the common case; typing it still works.
        let server = if server.starts_with("http://") || server.starts_with("https://") {
            server
        } else {
            format!("https://{server}")
        };
        lw.set_error("".into());
        lw.set_busy(true);

        let weak = lw.as_weak();
        let done = done_cb.clone();
        std::thread::spawn(move || {
            let result = tokio::runtime::Runtime::new()
                .map_err(|e| format!("tokio: {e}"))
                .and_then(|rt| {
                    rt.block_on(ddmail_core::auth::login(&server, &username, &password))
                });
            match result {
                Ok(login) => {
                    let cfg = engine::AccountConfig {
                        host: host_from_url(&server),
                        port: 993,
                        username: login.username.clone(),
                        password: String::new(),
                        use_tls: true,
                        email: login.email.clone(),
                        smtp_host: host_from_url(&server),
                        smtp_port: 465,
                        native_url: Some(server.clone()),
                        native_token: Some(login.token.clone()),
                        carddav_url: None,
                        caldav_url: None,
                        oauth_refresh_token: None,
                    };
                    engine::AccountConfig::save_all(std::slice::from_ref(&cfg));
                    done.store(true, Ordering::Relaxed);
                    let _ = weak.upgrade_in_event_loop(|lw| {
                        let _ = lw.hide();
                    });
                }
                Err(e) => {
                    let _ = weak.upgrade_in_event_loop(move |lw| {
                        lw.set_busy(false);
                        lw.set_error(e.into());
                    });
                }
            }
        });
    });

    // Offer Google sign-in only when client creds are present on this machine.
    lw.set_google_available(ddmail_core::oauth::load_client_creds().is_some());
    let gweak = lw.as_weak();
    let gdone = done.clone();
    lw.on_google_login(move || {
        let Some(lw) = gweak.upgrade() else { return };
        if lw.get_busy() {
            return;
        }
        lw.set_error("".into());
        lw.set_busy(true);
        let weak = lw.as_weak();
        let done = gdone.clone();
        std::thread::spawn(move || {
            let result = tokio::runtime::Runtime::new()
                .map_err(|e| format!("tokio: {e}"))
                .and_then(|rt| {
                    rt.block_on(async {
                        let creds = ddmail_core::oauth::load_client_creds()
                            .ok_or("google_oauth.json missing")?;
                        let now = chrono::Local::now().timestamp();
                        let tokens = ddmail_core::oauth::google_login(
                            &creds.client_id,
                            &creds.client_secret,
                            now,
                        )
                        .await?;
                        let email = ddmail_core::oauth::fetch_email(&tokens.access_token).await?;
                        Ok::<_, String>((tokens, email))
                    })
                });
            match result {
                Ok((tokens, email)) => {
                    let cfg = engine::AccountConfig {
                        host: "imap.gmail.com".into(),
                        port: 993,
                        username: email.clone(),
                        password: String::new(),
                        use_tls: true,
                        email: email.clone(),
                        smtp_host: "smtp.gmail.com".into(),
                        smtp_port: 465,
                        native_url: None,
                        native_token: None,
                        carddav_url: Some("https://www.googleapis.com/.well-known/carddav".into()),
                        caldav_url: Some(format!(
                            "https://apidata.googleusercontent.com/caldav/v2/{email}/events"
                        )),
                        oauth_refresh_token: Some(tokens.refresh_token),
                    };
                    engine::AccountConfig::save_all(std::slice::from_ref(&cfg));
                    done.store(true, Ordering::Relaxed);
                    let _ = weak.upgrade_in_event_loop(|lw| {
                        let _ = lw.hide();
                    });
                }
                Err(e) => {
                    let _ = weak.upgrade_in_event_loop(move |lw| {
                        lw.set_busy(false);
                        lw.set_error(e.into());
                    });
                }
            }
        });
    });

    if lw.run().is_err() {
        return false;
    }
    done.load(Ordering::Relaxed)
}

fn main() {
    let _ = log::set_logger(&STDOUT_LOGGER)
        .map(|()| log::set_max_level(log::LevelFilter::Info));
    // Single-instance guard: a second launch exits instead of opening a
    // duplicate window. (Focusing the existing window needs IPC — TODO.)
    let _instance = single_instance::SingleInstance::new("ddmail-native-single").ok();
    if let Some(inst) = &_instance {
        if !inst.is_single() {
            eprintln!("ddmail is already running");
            return;
        }
    }

    // First run (no env override, no accounts.json): login screen first.
    // Closing it without logging in exits — the app is useless without an
    // account and an empty cache.
    if engine::AccountConfig::load_all().is_empty() && !run_login_window() {
        return;
    }

    let ui = MainWindow::new().unwrap();

    // Restore the persisted window geometry + sidebar width before the
    // first paint, so the UI opens exactly where the user left it instead
    // of at the hard-coded defaults.
    let saved = window_state::load();
    // Best-effort before the first paint (reduces the open-then-resize
    // flicker on backends that honor it).
    ui.window().set_size(slint::LogicalSize::new(saved.width, saved.height));
    if saved.has_position() {
        ui.window().set_position(slint::PhysicalPosition::new(saved.x, saved.y));
    }
    ui.set_sidebar_width(saved.sidebar_width);

    // Re-apply geometry once the window is actually shown. set_size BEFORE the
    // first paint is unreliable — width falls back to the component's
    // preferred-width (1100px) while height is honored — but a resize request
    // on a live window sticks. `restore_done` gates the saver so it can't
    // persist the transient pre-restore size and clobber the saved geometry.
    let restore_done = Arc::new(AtomicBool::new(false));
    {
        let w = ui.as_weak();
        let restore_done = restore_done.clone();
        let _ = slint::invoke_from_event_loop(move || {
            if let Some(ui) = w.upgrade() {
                ui.window()
                    .set_size(slint::LogicalSize::new(saved.width, saved.height));
                if saved.has_position() {
                    ui.window()
                        .set_position(slint::PhysicalPosition::new(saved.x, saved.y));
                }
                if saved.maximized {
                    ui.window().set_maximized(true);
                }
            }
            restore_done.store(true, Ordering::Relaxed);
        });
    }

    // Restore persisted calendar-view preferences (panel state + view
    // toggles now; the per-calendar maps are seeded into Shared below).
    let cal_set = calendar_settings::load();
    ui.set_calendar_panel_collapsed(cal_set.panel_collapsed);
    ui.set_notify_sound_on(cal_set.notify_sound);
    ui.set_work_start(cal_set.work_start_hour.clamp(0, 23));
    ui.set_work_end(cal_set.work_end_hour.clamp(1, 24));
    // Palette for the colour-picker popup, mirroring CAL_PALETTE.
    ui.set_cal_palette(ModelRc::new(VecModel::from(
        CAL_PALETTE.iter().map(|c| hex(c)).collect::<Vec<slint::Color>>(),
    )));

    // Seed calendar view with sane defaults so the grid lays itself out
    // even before the engine produces any real data. Real `events` and
    // `calendars` arrive via FetchCalendars / FetchEvents.
    apply_calendar_defaults(&ui);

    ui.window().on_close_requested(move || slint::CloseRequestResponse::HideWindow);

    // Persist geometry continuously: Slint exposes no moved/resized
    // callbacks, so a UI-thread timer polls twice a second and writes the
    // state whenever position / size / sidebar changed. This survives a
    // hard kill (saving only on close used to lose the last state) and
    // skips while maximized so the file always holds the last NORMAL
    // geometry — the app must never reopen maximized.
    let ui_weak_geom = ui.as_weak();
    // Last *normal* (un-maximized) geometry — seeded from the restored state so
    // that, while maximized, we keep persisting a sane un-maximize target.
    let last_normal = std::cell::Cell::new(saved);
    let last_written =
        std::cell::Cell::new(None::<(i32, i32, u32, u32, f32, bool)>);
    let restore_done_saver = restore_done.clone();
    // Advance the calendar now-line every minute. The full calendar re-render
    // sets now-hour too (and today-col), so this only nudges the vertical
    // position between renders — kept simple (no today-col recompute; the
    // midnight column shift rides the next render/navigation).
    let ui_weak_now = ui.as_weak();
    let now_line_timer = slint::Timer::default();
    now_line_timer.start(
        slint::TimerMode::Repeated,
        std::time::Duration::from_secs(60),
        move || {
            if let Some(ui) = ui_weak_now.upgrade() {
                use chrono::Timelike;
                let now = chrono::Local::now();
                ui.set_now_hour(now.hour() as f32 + now.minute() as f32 / 60.0);
            }
        },
    );

    let geometry_saver = slint::Timer::default();
    geometry_saver.start(
        slint::TimerMode::Repeated,
        std::time::Duration::from_millis(300),
        move || {
            let Some(ui) = ui_weak_geom.upgrade() else { return };
            // Don't persist anything until the post-show restore has run, or
            // we'd save the transient pre-restore size and lose the real one.
            if !restore_done_saver.load(Ordering::Relaxed) {
                return;
            }
            let win = ui.window();
            if win.is_minimized() {
                return;
            }
            let sidebar = ui.get_sidebar_width();
            let state = if win.is_maximized() {
                // Keep the stored normal geometry; only flag maximized.
                let mut s = last_normal.get();
                s.sidebar_width = sidebar;
                s.maximized = true;
                s
            } else {
                let pos = win.position();
                let size = win.size();
                let scale = win.scale_factor().max(0.1);
                let s = window_state::WindowState {
                    width: size.width as f32 / scale,
                    height: size.height as f32 / scale,
                    sidebar_width: sidebar,
                    x: pos.x,
                    y: pos.y,
                    maximized: false,
                };
                last_normal.set(s);
                s
            };
            let snapshot = (
                state.x,
                state.y,
                state.width as u32,
                state.height as u32,
                state.sidebar_width,
                state.maximized,
            );
            if last_written.get() == Some(snapshot) {
                return;
            }
            last_written.set(Some(snapshot));
            window_state::save(&state);
        },
    );

    let account = open_account();
    let startup_ident_colors = match &account {
        Some((c, k, _)) => identity_color_map(c, k),
        None => HashMap::new(),
    };
    let displays = match &account {
        Some((_, _, convs)) => displays_from(convs, &startup_ident_colors),
        None => synthetic_displays(),
    };

    ui.set_conversations(ModelRc::new(VecModel::from(sidebar_items(&displays, &HashMap::new()))));
    if let Some(d0) = displays.first() {
        ui.set_active_name(d0.name.clone().into());
        ui.set_active_initials(d0.initials.clone().into());
        ui.set_active_color(slint::Brush::SolidColor(hex(&d0.color)));
    }

    // ----- Ultralight render worker -----
    //
    // Holds two pieces of state across jobs:
    //   * `body_cache` — finished Slint `RowItem`s keyed by
    //     (folder, uid, width). Re-opening a conversation reuses the
    //     bitmaps instead of re-rendering them through Ultralight, which
    //     is what dominates the latency budget (Notion 21 msgs: ~6.4 s
    //     cold vs ~0 ms warm). Pack-to-Image (memcpy of RGBA into a
    //     `SharedPixelBuffer`) runs on THIS thread now, so the UI thread
    //     stays responsive — previously it spent ~1 s per heavy
    //     conversation just packing.
    //   * `row_view_indices` — parallel to the rendered rows, mapping
    //     a row index to its Ultralight `View` inside the engine for
    //     hit-testing. `None` means the row was served from cache and
    //     has no live view; link clicks on those rows are ignored until
    //     a future iteration that caches views too.
    let (tx, rx) = mpsc::channel::<Job>();
    let rx = Arc::new(Mutex::new(rx));
    // Shared with Job::SetConversation senders (send_render_job): holds the
    // seq of the newest enqueued job so the worker can skip/abort stale ones.
    let render_seq = Arc::new(AtomicU64::new(0));
    // Disk layer under the RAM texture cache — survives restarts, so warm
    // conversations skip the WebView entirely after a relaunch.
    let tex_disk = cache_db_path()
        .and_then(|p| p.parent().map(|d| d.to_path_buf()))
        .and_then(texture_cache::TextureDiskCache::open);
    let ui_weak = ui.as_weak();
    {
        let rx = Arc::clone(&rx);
        let latest_seq = Arc::clone(&render_seq);
        let ui_weak = ui_weak.clone();
        std::thread::spawn(move || {
            let mut engine = render::Engine::new();
            // Cache holds the (already-packed) RGBA pixel buffer + height
            // per (folder, uid, width). `SharedPixelBuffer<Rgba8Pixel>` is
            // Send + Sync (unlike Slint's `Image`, which is UI-thread-only),
            // so we can build it here on the render thread and ship the
            // result across to the UI without doing any more memcpy work
            // on the hot path. UI-thread cost shrinks to just wrapping
            // each buffer in an `Image`.
            let mut body_cache: HashMap<(String, u32, u32, u64, u8, u64),
                (SharedPixelBuffer<Rgba8Pixel>, f32, Vec<render_common::LinkRect>, Vec<render_common::TextRun>)> = HashMap::new();
            // FIFO insertion order for the RAM cache: bitmaps are megabytes
            // each, so cap the entry count and drop the oldest (the disk
            // layer below still has them — eviction only costs a PNG decode).
            const RAM_CAP: usize = 400;
            let mut ram_order: Vec<(String, u32, u32, u64, u8, u64)> = Vec::new();
            // Per-row clickable link rects (CSS px), parallel to the rendered
            // rows. Renderer-agnostic; the click is a pure point-in-rect test.
            let mut row_links: Vec<Vec<render_common::LinkRect>> = Vec::new();
            // Per-row text layer (word rects) — mouse selection support.
            let mut row_runs: Vec<Vec<render_common::TextRun>> = Vec::new();
            // Set when a render panics mid-job: the WebView/COM state may be
            // wedged, so we throw the engine away and build a fresh one before
            // the next job rather than risk every later render failing too.
            let mut engine_needs_rebuild = false;
            loop {
                let job = {
                    let lock = rx.lock().unwrap();
                    lock.recv()
                };
                let Ok(job) = job else { break };
                if engine_needs_rebuild {
                    engine = render::Engine::new();
                    engine_needs_rebuild = false;
                }
                match job {
                    Job::SetConversation { bodies, width, policy, policy_gen, seq, scroll_to, modes } => {
                        // Latest-wins: a newer conversation/relayout job is
                        // already queued behind this one — rendering it would
                        // produce frames nobody will ever see.
                        if seq < latest_seq.load(Ordering::SeqCst) {
                            println!("[perf] render job seq={seq} superseded — skipped");
                            continue;
                        }
                        let t_wall = Instant::now();
                        let n = bodies.len();
                        engine.clear_views();
                        row_links.clear();
                        row_runs.clear();

                        // Tell the UI to show the progress bar.
                        let n_total = n as i32;
                        let _ = ui_weak.upgrade_in_event_loop(move |ui| {
                            ui.set_render_total(n_total);
                            ui.set_render_progress(0);
                        });

                        let mut packs: Vec<(SharedPixelBuffer<Rgba8Pixel>, RowMeta)> =
                            Vec::with_capacity(n);
                        let mut cache_hits = 0usize;
                        let mut disk_hits = 0usize;
                        let mut fallback_used = 0usize;
                        let mut render_ms_total = 0u128;
                        let mut pack_ms_total = 0u128;
                        let mut aborted = false;
                        for (i, body) in bodies.iter().enumerate() {
                            // Cheap mid-render cancellation: a newer job
                            // arrived (conversation switch, next resize step)
                            // — stop burning WebView time on this one.
                            if seq < latest_seq.load(Ordering::SeqCst) {
                                aborted = true;
                                break;
                            }
                            // Mode 1 = «Текстовая версия» override for this body.
                            let mode = modes.get(i).copied().unwrap_or(0);
                            let has_html =
                                body.html.as_deref().map(|s| !s.trim().is_empty()).unwrap_or(false);
                            let has_text =
                                body.text.as_deref().map(|s| !s.trim().is_empty()).unwrap_or(false);
                            let force_text = mode == 1 && has_html;
                            // Content fingerprint: bodies are mostly immutable,
                            // but cid:→data: healing rewrites the HTML — the
                            // texture must miss when the content changed.
                            let fp = texture_cache::fnv1a(body.html.as_deref().unwrap_or(""));
                            let key = (body.folder.clone(), body.uid, width, policy_gen, mode, fp);
                            let mut remember =
                                |key: &(String, u32, u32, u64, u8, u64),
                                 entry: &(SharedPixelBuffer<Rgba8Pixel>, f32, Vec<render_common::LinkRect>, Vec<render_common::TextRun>),
                                 body_cache: &mut HashMap<_, _>,
                                 ram_order: &mut Vec<(String, u32, u32, u64, u8, u64)>| {
                                    body_cache.insert(key.clone(), entry.clone());
                                    ram_order.push(key.clone());
                                    if ram_order.len() > RAM_CAP {
                                        let oldest = ram_order.remove(0);
                                        body_cache.remove(&oldest);
                                    }
                                };
                            let (buf, h, links, runs) = if let Some(cached) = body_cache.get(&key) {
                                cache_hits += 1;
                                cached.clone()
                            } else if let Some(de) = tex_disk.as_ref().and_then(|t| {
                                t.load(&body.folder, body.uid, width, policy_gen, mode, fp)
                            }) {
                                // Disk layer: rendered in a previous session —
                                // a PNG decode instead of a WebView pass.
                                disk_hits += 1;
                                let buf = SharedPixelBuffer::<Rgba8Pixel>::clone_from_slice(
                                    &de.rgba, de.width, de.height,
                                );
                                let entry = (buf, de.h, de.links, de.runs);
                                remember(&key, &entry, &mut body_cache, &mut ram_order);
                                entry
                            } else {
                                // First render: try the full HTML (unless the
                                // text view is forced). If WebKit doesn't manage
                                // to paint anything we retry with the text-only
                                // bubble — keeps "missing bubble" failures from
                                // being silent.
                                let html = if force_text {
                                    build_text_only_html(body)
                                } else {
                                    build_body_html(body, &policy)
                                };
                                let t_r = Instant::now();
                                let (mut result, panicked) = engine.render_one_guarded(&html, width);
                                if panicked {
                                    engine_needs_rebuild = true;
                                }
                                let text_available = body
                                    .text
                                    .as_deref()
                                    .map(|s| !s.trim().is_empty())
                                    .unwrap_or(false);
                                // Don't poke a just-panicked engine again this
                                // job — the text fallback would likely panic too.
                                if !result.successful() && text_available && !force_text && !panicked {
                                    fallback_used += 1;
                                    let text_html = build_text_only_html(body);
                                    let (r2, p2) = engine.render_one_guarded(&text_html, width);
                                    result = r2;
                                    if p2 {
                                        engine_needs_rebuild = true;
                                    }
                                }
                                render_ms_total += t_r.elapsed().as_millis();
                                // A failed paint (load timed out, DOM never
                                // parsed, empty document) must NOT be cached —
                                // otherwise the degenerate 1px bitmap sticks on
                                // disk and every later open serves that instead
                                // of retrying. We still return it for this pass
                                // (an empty bubble beats a missing one), just
                                // don't persist it.
                                let succeeded = result.successful();
                                let bitmap = result.bitmap;
                                let t_p = Instant::now();
                                let buf = SharedPixelBuffer::<Rgba8Pixel>::clone_from_slice(
                                    &bitmap.rgba, bitmap.width, bitmap.height,
                                );
                                pack_ms_total += t_p.elapsed().as_millis();
                                let entry = (buf, bitmap.height as f32, result.links, result.runs);
                                println!(
                                    "[perf]   body uid={} h={}px painted={} ready={} links={} runs={} cached={}",
                                    body.uid, bitmap.height, result.painted_height,
                                    result.view_ready, entry.2.len(), entry.3.len(), succeeded
                                );
                                if succeeded {
                                    if let Some(t) = tex_disk.as_ref() {
                                        t.store(
                                            &body.folder, body.uid, width, policy_gen, mode, fp,
                                            &bitmap.rgba, bitmap.width, bitmap.height,
                                            entry.1, &entry.2, &entry.3,
                                        );
                                    }
                                    remember(&key, &entry, &mut body_cache, &mut ram_order);
                                }
                                entry
                            };
                            // Context-menu data: per-sender / per-host
                            // checkbox states reflect the policy this job
                            // rendered under (a toggle re-renders anyway).
                            let sender_lc = body.from_addr.to_lowercase();
                            let (media_host, script_host) = sanitize::first_external_hosts(
                                body.html.as_deref().unwrap_or(""),
                            );
                            row_runs.push(runs);
                            packs.push((buf, RowMeta {
                                h,
                                has_html,
                                has_text,
                                viewing_html: has_html && !force_text,
                                m_sender_on: policy.allow_media.contains(&sender_lc),
                                s_sender_on: policy.allow_scripts.contains(&sender_lc),
                                m_host_on: !media_host.is_empty()
                                    && (policy.media_hosts.contains(&media_host)
                                        || policy.allow_domains.contains(&media_host)),
                                s_host_on: !script_host.is_empty()
                                    && policy.script_hosts.contains(&script_host),
                                sender: body.from_addr.clone(),
                                media_host,
                                script_host,
                            }));
                            row_links.push(links);
                            // Push progress to the UI — one event per body.
                            let done = (i + 1) as i32;
                            let _ = ui_weak.upgrade_in_event_loop(move |ui| {
                                ui.set_render_progress(done);
                            });
                        }
                        if aborted {
                            println!(
                                "[perf] render job seq={seq} aborted mid-render \
                                 ({}/{n} done) — newer job queued",
                                packs.len()
                            );
                            // Hide the progress bar; the superseding job
                            // re-seeds it with its own totals.
                            let _ = ui_weak.upgrade_in_event_loop(move |ui| {
                                ui.set_render_total(0);
                                ui.set_render_progress(0);
                            });
                            continue;
                        }
                        println!(
                            "[perf] render N={n} width={width}px cache_hits={cache_hits} \
                             disk_hits={disk_hits} fallback={fallback_used} \
                             ultralight={render_ms_total}ms pack={pack_ms_total}ms total_job={}ms",
                            t_wall.elapsed().as_millis()
                        );
                        // UI-thread link/text rects for the pointer cursor
                        // and mouse selection (the worker keeps its own
                        // copies for click hit-testing).
                        let links_for_ui = row_links.clone();
                        let runs_for_ui = row_runs.clone();
                        let _ = ui_weak.upgrade_in_event_loop(move |ui| {
                            // Wrap each SharedPixelBuffer in an Image —
                            // this is cheap (refcount bump, no memcpy)
                            // and is the only step that has to run on
                            // the UI thread.
                            SHARED.with(|s| {
                                if let Some(sh) = s.borrow().as_ref() {
                                    *sh.row_links.borrow_mut() = links_for_ui;
                                    *sh.row_text_runs.borrow_mut() = runs_for_ui;
                                    // Rows are being replaced — any active
                                    // selection now points at stale indices.
                                    sh.sel_row.set(-1);
                                    ui.set_selection_row(-1);
                                }
                            });
                            let rows: Vec<RowItem> = packs
                                .into_iter()
                                .map(|(buf, m)| RowItem {
                                    img: Image::from_rgba8(buf),
                                    h: m.h,
                                    has_html: m.has_html,
                                    has_text: m.has_text,
                                    viewing_html: m.viewing_html,
                                    sender: m.sender.into(),
                                    media_host: m.media_host.into(),
                                    script_host: m.script_host.into(),
                                    m_sender_on: m.m_sender_on,
                                    s_sender_on: m.s_sender_on,
                                    m_host_on: m.m_host_on,
                                    s_host_on: m.s_host_on,
                                })
                                .collect();
                            // Post-open scroll target: y offset of the first
                            // unread row, or "very far down" for scroll-to-end
                            // (the Slint bridge clamps to the content range).
                            let scroll_y: Option<f32> = scroll_to.map(|sr| {
                                if sr < 0 {
                                    1.0e9
                                } else {
                                    rows.iter().take(sr as usize).map(|r| r.h).sum()
                                }
                            });
                            ui.set_messages(ModelRc::new(VecModel::from(rows)));
                            // Hide the progress bar.
                            ui.set_render_total(0);
                            ui.set_render_progress(0);
                            if let Some(y) = scroll_y {
                                ui.set_chat_scroll_y(y);
                                ui.set_chat_scroll_seq(ui.get_chat_scroll_seq() + 1);
                            } else {
                                // Scroll-less render (width change, new mail,
                                // policy toggle): stop re-anchoring on future
                                // viewport-height changes.
                                ui.set_chat_scroll_pending(false);
                            }
                        });
                    }
                    Job::HitTest { row, x, y } => {
                        // Renderer-agnostic hit-test: pure point-in-rect against
                        // the link rects extracted at render time (works for
                        // both cached and freshly-rendered rows). Resolved URLs
                        // (incl. internal ddmail-attach:* schemes) go to
                        // handle_link on the UI thread.
                        let hit = row_links
                            .get(row)
                            .and_then(|links| links.iter().find(|l| l.contains(x, y)))
                            .map(|l| l.href.clone());
                        match hit {
                            Some(url) => {
                                let _ = ui_weak.upgrade_in_event_loop(move |ui| handle_link(&ui, url));
                            }
                            None => println!("click row {row} @({x:.0},{y:.0}) — no link"),
                        }
                    }
                    Job::RenderSource { text, width } => {
                        // Render the viewer text the same way as a bubble: a
                        // bitmap + word rects. The modal then selects via the
                        // fast Rust text-run layer, not Slint's TextInput.
                        let html = build_source_html(&text);
                        let (result, panicked) = engine.render_one_guarded(&html, width);
                        if panicked {
                            engine_needs_rebuild = true;
                        }
                        let bmp = result.bitmap;
                        let buf = SharedPixelBuffer::<Rgba8Pixel>::clone_from_slice(
                            &bmp.rgba, bmp.width, bmp.height,
                        );
                        let h = bmp.height as f32;
                        let runs = result.runs;
                        println!(
                            "[perf] source render {}x{} runs={}",
                            bmp.width, bmp.height, runs.len()
                        );
                        let _ = ui_weak.upgrade_in_event_loop(move |ui| {
                            SHARED.with(|s| {
                                if let Some(sh) = s.borrow().as_ref() {
                                    *sh.src_runs.borrow_mut() = runs;
                                    sh.src_sel_moved.set(false);
                                    sh.src_sel_dragging.set(false);
                                }
                            });
                            ui.set_source_img(Image::from_rgba8(buf));
                            ui.set_source_img_h(h);
                            ui.set_source_selection_rects(ModelRc::new(VecModel::from(
                                Vec::<SelRect>::new(),
                            )));
                        });
                    }
                }
            }
        });
    }

    // ----- Shared state -----
    let (cache, key, init_convs) = match account {
        Some((c, k, convs)) => (Some(c), k, convs),
        None => {
            // Empty cache at startup (first run or right after a cache reset):
            // still attach the cache and adopt the live account's key. The old
            // behaviour left sh.cache = None and sh.key = "", so the
            // Conversations handler skipped identity_color_map entirely and
            // every sidebar row stayed grey (and cached-body reads were cold)
            // until a restart happened to find a populated DB.
            let key = engine::AccountConfig::load_all()
                .first()
                .map(|a| a.account_key())
                .unwrap_or_default();
            (open_cache(), key, Vec::new())
        }
    };
    let loaded_policy = policy::load();
    let shared = Rc::new(Shared {
        cache,
        key,
        convs: RefCell::new(init_convs),
        displays: RefCell::new(displays.clone()),
        avatars: RefCell::new(HashMap::new()),
        cur_account_key: RefCell::new(String::new()),
        account_keys: RefCell::new(Vec::new()),
        account_states: RefCell::new(HashMap::new()),
        current_msgs: RefCell::new(Vec::new()),
        current_bodies: RefCell::new(Vec::new()),
        open_gen: Cell::new(0),
        open_unread: RefCell::new(HashSet::new()),
        scroll_pending: Cell::new(false),
        body_view_text: RefCell::new(HashSet::new()),
        pending_forward: RefCell::new(None),
        pending_source_view: Cell::new(0),
        source_view_full: RefCell::new(String::new()),
        src_runs: RefCell::new(Vec::new()),
        src_sel_anchor: Cell::new(0),
        src_sel_head: Cell::new(0),
        src_sel_moved: Cell::new(false),
        src_sel_dragging: Cell::new(false),
        identity_colors: RefCell::new(startup_ident_colors),
        composer_identities: RefCell::new(Vec::new()),
        row_links: RefCell::new(Vec::new()),
        confirm_mode: Cell::new(0),
        row_text_runs: RefCell::new(Vec::new()),
        sel_row: Cell::new(-1),
        sel_anchor: Cell::new(0),
        sel_head: Cell::new(0),
        sel_dragging: Cell::new(false),
        sel_moved: Cell::new(false),
        sel_suppress_click: Cell::new(false),
        pending_open_ref: RefCell::new(None),
        render_seq,
        current: Cell::new(0),
        width: Cell::new(DEFAULT_WIDTH),
        tx,
        engine_tx: RefCell::new(None),
        search_query_inflight: RefCell::new(String::new()),
        search_contacts: RefCell::new(Vec::new()),
        search_messages: RefCell::new(Vec::new()),
        pending_compose: RefCell::new(None),
        pending_reply: RefCell::new(None),
        policy_gen: Cell::new(loaded_policy.generation),
        policy: RefCell::new(loaded_policy),
        calendars: RefCell::new(Vec::new()),
        calendar_visible: RefCell::new(HashMap::new()),
        calendar_colors: RefCell::new(HashMap::new()),
        calendar_events: RefCell::new(Vec::new()),
        calendar_week_start_days: Cell::new(week_start_days_today()),
        grid_canvas_w: Cell::new(1000.0),
        grid_canvas_h: Cell::new(680.0),
        work_start: Cell::new(cal_set.work_start_hour.clamp(0, 23)),
        work_end: Cell::new(cal_set.work_end_hour.clamp(1, 24)),
        manual_hour_h: Cell::new(cal_set.manual_hour_height),
        manual_col_w: Cell::new(cal_set.manual_col_width),
        editing_event_id: Cell::new(0),
        edit_cal_ids: RefCell::new(Vec::new()),
        address_book: RefCell::new(Vec::new()),
        editing_contact_id: Cell::new(0),
        compose_attachments: RefCell::new(Vec::new()),
        pending_open_event: Cell::new(0),
        snooze_ctx: RefCell::new((0, 0, 0, 0, String::new())),
        cal_occ: RefCell::new(HashMap::new()),
    });
    SHARED.with(|s| *s.borrow_mut() = Some(shared.clone()));
    sync_media_globals(&ui, &shared.policy.borrow());
    // Seed the composer from-picker from cached identities; refreshed again
    // whenever the engine resyncs identities.
    refresh_composer_identities(&ui, &shared);

    // Seed the persisted per-calendar maps (visibility deny-list + colour
    // overrides) now that Shared exists.
    {
        let mut vis = shared.calendar_visible.borrow_mut();
        for id in &cal_set.hidden {
            vis.insert(*id, false);
        }
        *shared.calendar_colors.borrow_mut() = cal_set.colors.clone();
    }

    // Open the first conversation that has cached bodies.
    {
        let convs = shared.convs.borrow();
        for (i, c) in convs.iter().enumerate() {
            let bodies = shared
                .cache
                .as_ref()
                .and_then(|cache| cache.load_message_bodies(&shared.key, &c.messages).ok())
                .unwrap_or_default();
            if bodies.is_empty() {
                continue;
            }
            shared.current.set(i);
            ui.set_selected(i as i32);
            apply_active_header(&ui, &shared, i);
            // Seed the row refs/bodies too — context-menu actions on the
            // startup conversation resolve rows through these.
            *shared.current_msgs.borrow_mut() = bodies
                .iter()
                .map(|b| MessageRef { folder: b.folder.clone(), uid: b.uid, message_id: b.message_id.clone(), seen: true })
                .collect();
            *shared.current_bodies.borrow_mut() = bodies.clone();
            // Startup scroll: same first-unread/end anchoring as a click.
            *shared.open_unread.borrow_mut() = c
                .messages
                .iter()
                .filter(|m| !m.seen)
                .map(|m| (m.folder.clone(), m.uid))
                .collect();
            shared.scroll_pending.set(true);
            // Consume: no fetch follows at startup — a leftover pending flag
            // would let a much later background refresh yank the viewport.
            let scroll = take_scroll_target(&shared, &bodies, true);
            send_render_job(&shared, bodies, scroll);
            break;
        }
    }

    // ----- Live engine (config from env, with cache-only fallback) -----
    //
    // When DDMAIL_IMAP_* env vars are set we spawn a fully live engine and
    // fire the initial FetchConversations + StartWatching. When they're
    // not, we still spawn an engine with a placeholder config so that
    // `SearchDropdown` (the live-dropdown lookup) keeps working — it
    // only needs the cache for contacts; the provider call is wrapped in
    // unwrap_or_default and silently returns empty messages.
    if let Some(cache) = open_cache() {
        let mut accounts = engine::AccountConfig::load_all();
        let live = !accounts.is_empty();
        if accounts.is_empty() {
            // No live config: reconstruct just enough so that the engine's
            // `key = cfg.account_key()` matches what's already in the cache
            // (`{username}@{host}` format). Anything provider-touching
            // stays empty / unreachable; only cache-backed reads make sense.
            let key = shared.key.clone();
            let (username, host) = key
                .rsplit_once('@')
                .map(|(u, h)| (u.to_string(), h.to_string()))
                .unwrap_or_else(|| (key.clone(), String::new()));
            accounts.push(engine::AccountConfig {
                host: host.clone(),
                port: 993,
                username,
                password: String::new(),
                use_tls: true,
                email: key,
                smtp_host: host,
                smtp_port: 465,
                native_url: None,
                native_token: None,
                carddav_url: None,
                caldav_url: None,
                oauth_refresh_token: None,
            });
        }
        // Seed the connection indicator: every account starts "connecting"
        // until its watcher reports in.
        {
            let keys: Vec<String> = accounts.iter().map(|a| a.account_key()).collect();
            let mut st = shared.account_states.borrow_mut();
            for k in &keys {
                st.insert(k.clone(), "connecting".into());
            }
            drop(st);
            *shared.account_keys.borrow_mut() = keys;
        }
        let ui_weak_eng = ui.as_weak();
        let etx = engine::spawn(accounts, cache, move |res| {
            let _ = ui_weak_eng.upgrade_in_event_loop(move |ui| handle_engine_result(&ui, res));
        });
        if live {
            let _ = etx.send(engine::EngineCmd::FetchConversations { limit: CONV_FETCH_LIMIT });
            let _ = etx.send(engine::EngineCmd::StartWatching);
        } else {
            println!("engine: DDMAIL_IMAP_* not set — live IMAP disabled, cache-only mode");
        }
        *shared.engine_tx.borrow_mut() = Some(etx);
    }

    // ----- Callbacks -----
    let ui_weak2 = ui.as_weak();
    let sh_sel = shared.clone();
    ui.on_select(move |idx| {
        let Some(ui) = ui_weak2.upgrade() else { return };
        let model_idx = idx as usize;
        // While in transient-compose mode the first row is the synthetic
        // "new chat" — clicking it is a no-op (we're already there).
        let pending = sh_sel.pending_compose.borrow().is_some();
        if pending && model_idx == 0 {
            return;
        }
        // Real-conversation rows: when a transient row is present we
        // need to subtract one to map model index → displays index.
        let real_idx = if pending { model_idx - 1 } else { model_idx };
        // Picking any real conversation leaves transient-compose mode AND
        // drops any staged explicit-reply target — both are tied to the
        // previous context.
        let was_pending = sh_sel.pending_compose.borrow_mut().take().is_some();
        exit_reply_mode(&sh_sel, &ui);
        apply_active_header(&ui, &sh_sel, real_idx);
        if was_pending {
            refresh_sidebar(&sh_sel, &ui);
        }
        // Highlight the real row at its post-refresh model index.
        ui.set_selected(real_idx as i32);
        // Re-grab the window-level key sink so Delete works right after a
        // click (typing in the composer moves focus there as usual).
        ui.invoke_grab_key_focus();
        open_conversation(&ui, &sh_sel, real_idx);
    });

    // ↑/↓ over the conversation list: select + open the neighbour and keep
    // its row visible. Pairs with Delete for sweeping unwanted dialogs.
    let ui_weak_nav = ui.as_weak();
    let sh_nav = shared.clone();
    ui.on_nav_conversation(move |delta| {
        let Some(ui) = ui_weak_nav.upgrade() else { return };
        if sh_nav.pending_compose.borrow().is_some() {
            return;
        }
        let len = sh_nav.convs.borrow().len() as i32;
        if len == 0 {
            return;
        }
        let cur = sh_nav.current.get() as i32;
        let new = (cur + delta).clamp(0, len - 1);
        if new == cur {
            return;
        }
        exit_reply_mode(&sh_nav, &ui);
        ui.set_selected(new);
        apply_active_header(&ui, &sh_nav, new as usize);
        open_conversation(&ui, &sh_nav, new as usize);
        ui.set_sidebar_row_y(new as f32 * 64.0);
        ui.set_sidebar_scroll_seq(ui.get_sidebar_scroll_seq() + 1);
    });

    // Delete key → confirm modal → delete the whole conversation (every
    // message incl. the user's own replies from Sent). The server handler
    // soft-deletes locally AND queues flag-sync deleted=true, so the worker
    // pushes STORE \Deleted + UID EXPUNGE to the source IMAP server.
    let ui_weak_delc = ui.as_weak();
    let sh_delc = shared.clone();
    ui.on_delete_conversation(move || {
        let Some(ui) = ui_weak_delc.upgrade() else { return };
        if sh_delc.pending_compose.borrow().is_some() {
            return; // transient compose has no conversation to delete
        }
        let convs = sh_delc.convs.borrow();
        let Some(c) = convs.get(sh_delc.current.get()) else { return };
        sh_delc.confirm_mode.set(1);
        ui.set_confirm_delete_title("Удалить диалог?".into());
        ui.set_confirm_delete_text(
            format!(
                "«{}» — сообщений: {}. Все письма диалога, включая ваши ответы, \
                 будут удалены и на сервере.",
                c.label,
                c.messages.len()
            )
            .into(),
        );
        ui.set_confirm_delete_visible(true);
    });

    // «Спам» in the chat header: blacklist the counterpart's domain and
    // purge every message from them (Tauri-era behaviour), confirmed
    // through the same modal as conversation deletion.
    let ui_weak_spam = ui.as_weak();
    let sh_spam = shared.clone();
    ui.on_spam_conversation(move || {
        let Some(ui) = ui_weak_spam.upgrade() else { return };
        if sh_spam.pending_compose.borrow().is_some() {
            return;
        }
        let convs = sh_spam.convs.borrow();
        let Some(c) = convs.get(sh_spam.current.get()) else { return };
        let Some(cp) = c.counterparts.first().filter(|cp| !cp.addr.is_empty()) else { return };
        let label = if cp.name.is_empty() { cp.addr.clone() } else { cp.name.clone() };
        sh_spam.confirm_mode.set(2);
        ui.set_confirm_delete_title("В спам?".into());
        ui.set_confirm_delete_text(
            format!("Удалить все письма от «{label}» и добавить отправителя в чёрный список?")
                .into(),
        );
        ui.set_confirm_delete_visible(true);
    });
    let ui_weak_delk = ui.as_weak();
    let sh_delk = shared.clone();
    ui.on_delete_conversation_confirmed(move || {
        let Some(ui) = ui_weak_delk.upgrade() else { return };
        let cur = sh_delk.current.get();
        let mode = sh_delk.confirm_mode.get();
        sh_delk.confirm_mode.set(0);
        let (conv_id, refs, cp_addr) = {
            let convs = sh_delk.convs.borrow();
            let Some(c) = convs.get(cur) else { return };
            (
                c.id.clone(),
                c.messages.clone(),
                c.counterparts.first().map(|cp| cp.addr.to_lowercase()).unwrap_or_default(),
            )
        };
        if let Some(etx) = sh_delk.engine_tx.borrow().as_ref() {
            if mode == 2 {
                // Spam: domain blacklist + purge everything from the sender;
                // the conversation's own rows go by id so outgoing-from-us
                // threads disappear too.
                if cp_addr.is_empty() {
                    return;
                }
                let domain = cp_addr.split('@').nth(1).unwrap_or("").to_string();
                let ids: Vec<i64> = refs.iter().map(|m| m.uid as i64).collect();
                println!("spam purge {cp_addr} (domain {domain}, {} rows)", ids.len());
                let _ = etx.send(engine::EngineCmd::BlacklistAndPurge {
                    domain,
                    address: cp_addr,
                    message_ids: ids,
                    account_key: sh_delk.cur_account_key.borrow().clone(),
                });
            } else {
                println!("delete conversation {conv_id} ({} messages)", refs.len());
                let _ = etx.send(engine::EngineCmd::Delete {
                    messages: refs,
                    account_key: sh_delk.cur_account_key.borrow().clone(),
                });
            }
        }
        // Optimistic local removal; the engine resets the full-sync stamp on
        // success, so the Done-triggered refetch reconciles with the server.
        if let Some(cache) = &sh_delk.cache {
            cache.delete_conversation(&sh_delk.key, &conv_id).ok();
        }
        {
            let mut convs = sh_delk.convs.borrow_mut();
            if cur < convs.len() {
                convs.remove(cur);
            }
            let displays = displays_from(&convs, &sh_delk.identity_colors.borrow());
            let items = sidebar_items(&displays, &sh_delk.avatars.borrow());
            ui.set_conversations(ModelRc::new(VecModel::from(items)));
            *sh_delk.displays.borrow_mut() = displays;
        }
        let len = sh_delk.convs.borrow().len();
        if len == 0 {
            ui.set_messages(ModelRc::new(VecModel::from(Vec::<RowItem>::new())));
            ui.set_render_total(0);
            sh_delk.current_msgs.borrow_mut().clear();
            sh_delk.current_bodies.borrow_mut().clear();
            return;
        }
        // Show the neighbour (same index now points at the next conversation).
        let next = cur.min(len - 1);
        ui.set_selected(next as i32);
        apply_active_header(&ui, &sh_delk, next);
        open_conversation(&ui, &sh_delk, next);
        ui.set_sidebar_row_y(next as f32 * 64.0);
        ui.set_sidebar_scroll_seq(ui.get_sidebar_scroll_seq() + 1);
    });

    // Resize = pure relayout: re-render the in-memory bodies at the new
    // width after the drag settles. No SQLite, no network — the contents
    // didn't change, only the pixels. Debounce coalesces the drag stream;
    // the render seq additionally kills any still-queued older job.
    let sh_rs = shared.clone();
    let resize_debounce = Rc::new(slint::Timer::default());
    ui.on_viewport_resized(move |w| {
        let neww = w as u32;
        // Sub-minimum widths are layout transients (chat pane hidden in
        // calendar mode, first frame) — rendering at them would be junk.
        if neww < 240 || neww == sh_rs.width.get() {
            return;
        }
        sh_rs.width.set(neww);
        let sh2 = sh_rs.clone();
        resize_debounce.start(
            slint::TimerMode::SingleShot,
            std::time::Duration::from_millis(150),
            move || {
                let bodies = sh2.current_bodies.borrow().clone();
                if !bodies.is_empty() {
                    send_render_job(&sh2, bodies, None);
                }
            },
        );
    });

    // Slint's `changed width` does NOT fire for the initial layout pass, so
    // after a restart the render width silently stayed at DEFAULT_WIDTH and
    // every bubble stretched to the real (wider) column. This watcher feeds
    // the actual chat-column width through the same resize path — covering
    // the first frame and any future missed events (DPI change, etc.).
    let ui_weak_ww = ui.as_weak();
    let width_watcher = slint::Timer::default();
    width_watcher.start(
        slint::TimerMode::Repeated,
        std::time::Duration::from_millis(500),
        move || {
            if let Some(ui) = ui_weak_ww.upgrade() {
                let w = ui.get_chat_width();
                if w > 0.0 {
                    ui.invoke_viewport_resized(w);
                }
            }
        },
    );

    let tx_hit = shared.tx.clone();
    let sh_hit = shared.clone();
    ui.on_hit_test(move |row, x, y| {
        // A click that ends a drag-selection is not a link click.
        if sh_hit.sel_suppress_click.replace(false) {
            return;
        }
        let _ = tx_hit.send(Job::HitTest { row: row as usize, x, y });
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

    // ── Mouse text selection over bubbles ──
    let ui_weak_ss = ui.as_weak();
    let sh_ss = shared.clone();
    ui.on_sel_start(move |row, x, y| {
        let Some(ui) = ui_weak_ss.upgrade() else { return };
        sh_ss.sel_dragging.set(false);
        sh_ss.sel_moved.set(false);
        sh_ss.sel_row.set(-1);
        if let Some(runs) = sh_ss.row_text_runs.borrow().get(row as usize) {
            if let Some(i) = nearest_run(runs, x, y) {
                sh_ss.sel_row.set(row);
                sh_ss.sel_anchor.set(i);
                sh_ss.sel_head.set(i);
                sh_ss.sel_dragging.set(true);
            }
        }
        // Clear any previous highlight; Ctrl+C must reach the key sink.
        refresh_selection_rects(&ui, &sh_ss);
        ui.invoke_grab_key_focus();
    });
    let ui_weak_sm = ui.as_weak();
    let sh_sm = shared.clone();
    ui.on_sel_move(move |row, x, y| {
        if !sh_sm.sel_dragging.get() || sh_sm.sel_row.get() != row {
            return;
        }
        let Some(ui) = ui_weak_sm.upgrade() else { return };
        let head = sh_sm
            .row_text_runs
            .borrow()
            .get(row as usize)
            .and_then(|runs| nearest_run(runs, x, y));
        if let Some(i) = head {
            if !sh_sm.sel_moved.get() && i == sh_sm.sel_anchor.get() {
                return; // not an actual drag yet
            }
            sh_sm.sel_moved.set(true);
            sh_sm.sel_head.set(i);
            refresh_selection_rects(&ui, &sh_sm);
        }
    });
    let sh_se = shared.clone();
    ui.on_sel_end(move || {
        sh_se.sel_dragging.set(false);
        if sh_se.sel_moved.get() {
            // The release also fires `clicked` — it must not open a link.
            sh_se.sel_suppress_click.set(true);
        }
    });
    let sh_cs = shared.clone();
    ui.on_copy_selection(move || {
        if let Some(text) = selection_text(&sh_cs) {
            println!("copy selection: {} chars", text.len());
            clipboard_set(&text);
        }
    });

    // Composer → three branches depending on staged intent:
    //   1. Transient compose target (search dropdown).
    //   2. Explicit reply to a specific bubble (quote ribbon).
    //   3. Implicit reply to the currently open conversation.
    let ui_weak_send = ui.as_weak();
    let sh_send = shared.clone();
    ui.on_send(move |text| {
        let text = text.to_string();
        if text.trim().is_empty() {
            return;
        }
        // Read chevron-panel overrides up front. Non-empty subject
        // override wins over the per-branch auto-derivation; cc is
        // parsed once and passed through to the engine in every
        // branch.
        let ui_now = ui_weak_send.upgrade();
        let subject_override = ui_now
            .as_ref()
            .map(|u| u.get_composer_subject().to_string().trim().to_string())
            .unwrap_or_default();
        let cc: Vec<String> = ui_now
            .as_ref()
            .map(|u| u.get_composer_cc().to_string())
            .unwrap_or_default()
            .split(|c: char| c == ',' || c == ';')
            .map(|s| s.trim().to_string())
            .filter(|s| !s.is_empty())
            .collect();
        // Sending identity from the from-picker (None until identities sync;
        // the engine then falls back to the account email). Resolved through
        // the Shared email list — the Slint model is draw-only.
        let from_identity: Option<String> = ui_now.as_ref().and_then(|u| {
            let idx = u.get_composer_identity_index();
            sh_send
                .composer_identities
                .borrow()
                .get(idx.max(0) as usize)
                .cloned()
        });
        // Staged attachment paths for this send, snapshotted up front so the
        // per-branch Send commands all carry the same list.
        let attachments: Vec<String> = sh_send
            .compose_attachments
            .borrow()
            .iter()
            .map(|p| p.to_string_lossy().into_owned())
            .collect();
        // After a successful staging the override fields + attachments reset
        // so the next message starts blank again. Keeps the chevron panel
        // from silently inheriting last message's headers.
        let clear_overrides = || {
            sh_send.compose_attachments.borrow_mut().clear();
            if let Some(u) = ui_weak_send.upgrade() {
                u.set_composer_subject("".into());
                u.set_composer_cc("".into());
                refresh_attachment_chips(&u, &sh_send);
            }
        };

        // Branch 0: forward — explicit recipients from the «Кому» field;
        // the typed text is the covering note, the original's text goes
        // below it after a separator, attachments re-attach engine-side.
        if let Some(orig) = sh_send.pending_forward.borrow().clone() {
            let to: Vec<String> = ui_now
                .as_ref()
                .map(|u| u.get_composer_to().to_string())
                .unwrap_or_default()
                .split(|c: char| c == ',' || c == ';')
                .map(|s| s.trim().to_string())
                .filter(|s| !s.is_empty())
                .collect();
            if to.is_empty() {
                eprintln!("forward: адресат не указан — заполните «Кому»");
                if let Some(u) = ui_now.as_ref() {
                    u.set_composer_expanded(true);
                    u.set_focus_to_seq(u.get_focus_to_seq() + 1);
                }
                return;
            }
            // enter_forward_mode pre-filled composer-subject with «Fwd: …»,
            // so the override carries it; fall back defensively anyway.
            let subject = if !subject_override.is_empty() {
                subject_override.clone()
            } else {
                format!("Fwd: {}", orig.subject)
            };
            let from_line = if orig.from.is_empty() {
                orig.from_addr.clone()
            } else {
                orig.from.clone()
            };
            let orig_text = orig.text.clone().unwrap_or_default();
            let body_text = format!(
                "{text}\n\n---------- Пересланное сообщение ----------\n\
                 От: {from_line}\nДата: {}\nТема: {}\n\n{orig_text}",
                orig.date, orig.subject
            );
            if let Some(etx) = sh_send.engine_tx.borrow().as_ref() {
                println!("forwarding {}/{} to {to:?}", orig.folder, orig.uid);
                let _ = etx.send(engine::EngineCmd::Send {
                    to,
                    cc: cc.clone(),
                    subject,
                    body: body_text,
                    in_reply_to: None,
                    references: None,
                    from: from_identity.clone(),
                    attachments: attachments.clone(),
                    forward_attachments: Some(MessageRef {
                        folder: orig.folder.clone(),
                        uid: orig.uid,
                        message_id: orig.message_id.clone(),
                        seen: true,
                    }),
                    account_key: sh_send.cur_account_key.borrow().clone(),
                });
                clear_overrides();
                if let Some(u) = ui_now.as_ref() {
                    exit_reply_mode(&sh_send, u);
                    u.set_composer_expanded(false);
                }
            } else {
                eprintln!("send: no live engine (set DDMAIL_* env)");
            }
            return;
        }
        // Branch 1: transient compose target set via the search dropdown.
        if let Some(target) = sh_send.pending_compose.borrow().clone() {
            let subject = if !subject_override.is_empty() {
                subject_override.clone()
            } else {
                "Новое сообщение".to_string()
            };
            if let Some(etx) = sh_send.engine_tx.borrow().as_ref() {
                println!("sending new message to {target}");
                let _ = etx.send(engine::EngineCmd::Send {
                    to: vec![target],
                    cc: cc.clone(),
                    subject,
                    body: text,
                    in_reply_to: None,
                    references: None,
                    from: from_identity.clone(),
                    attachments: attachments.clone(),
                    forward_attachments: None,
                    account_key: sh_send.cur_account_key.borrow().clone(),
                });
                clear_overrides();
            } else {
                eprintln!("send: no live engine (set DDMAIL_* env)");
            }
            return;
        }
        // Branch 2: explicit reply via quote ribbon.
        if let Some(reply_body) = sh_send.pending_reply.borrow().clone() {
            // Reply-all in groups: the current convs entry tells us
            // group-ness; in 1:1 conversations the counterpart is the
            // sender anyway. The recipients are the source's from + to
            // + cc minus our identities (mirrored from svelte's
            // ChatView.svelte:478-501).
            let our_lc: std::collections::HashSet<String> =
                std::iter::once(sh_send.key.to_lowercase()).collect();
            let extract_addr = |raw: &str| -> String {
                let lt = raw.find('<');
                let gt = lt.and_then(|i| raw[i..].find('>').map(|j| i + j));
                if let (Some(i), Some(j)) = (lt, gt) {
                    raw[i + 1..j].trim().to_lowercase()
                } else {
                    raw.trim().to_lowercase()
                }
            };
            let mut to: Vec<String> = Vec::new();
            let mut seen: std::collections::HashSet<String> = std::collections::HashSet::new();
            let mut push = |a: String| {
                if a.is_empty() || our_lc.contains(&a) || !seen.insert(a.clone()) {
                    return;
                }
                to.push(a);
            };
            push(reply_body.from_addr.to_lowercase());
            let is_group = sh_send
                .convs
                .borrow()
                .get(sh_send.current.get())
                .map(|c| c.is_group)
                .unwrap_or(false);
            if is_group {
                for a in reply_body.to.iter().chain(reply_body.cc.iter()) {
                    push(extract_addr(a));
                }
            }
            if to.is_empty() {
                eprintln!("reply: no recipient resolved");
                return;
            }
            let subject = if !subject_override.is_empty() {
                subject_override.clone()
            } else if reply_body.subject.to_lowercase().starts_with("re:") {
                reply_body.subject.clone()
            } else {
                format!("Re: {}", reply_body.subject)
            };
            let in_reply_to = (!reply_body.message_id.is_empty())
                .then(|| reply_body.message_id.clone());
            let mut refs = reply_body.references.clone();
            if !reply_body.message_id.is_empty() {
                refs.push(reply_body.message_id.clone());
            }
            let references = (!refs.is_empty()).then(|| refs.join(" "));
            if let Some(etx) = sh_send.engine_tx.borrow().as_ref() {
                println!("sending explicit reply to {to:?}");
                let _ = etx.send(engine::EngineCmd::Send {
                    to, cc: cc.clone(), subject, body: text, in_reply_to, references,
                    from: from_identity.clone(),
                    attachments: attachments.clone(),
                    forward_attachments: None,
                    account_key: sh_send.cur_account_key.borrow().clone(),
                });
                clear_overrides();
            } else {
                eprintln!("send: no live engine (set DDMAIL_* env)");
            }
            // Quote ribbon goes away once the message is staged for send.
            if let Some(ui) = ui_weak_send.upgrade() {
                exit_reply_mode(&sh_send, &ui);
            }
            return;
        }
        // Branch 3: implicit reply within the currently selected conversation.
        let convs = sh_send.convs.borrow();
        let Some(c) = convs.get(sh_send.current.get()) else { return };
        let to: Vec<String> = c
            .counterparts
            .iter()
            .map(|cp| cp.addr.clone())
            .filter(|a| !a.is_empty())
            .collect();
        if to.is_empty() {
            eprintln!("send: no recipient for this conversation");
            return;
        }
        // Subject mirrors the *last incoming* message per the spec — that's
        // the one the user is replying to, even if our own outgoing came
        // after it. Bodies of the open conversation are already in memory;
        // fall back to conversation last_subject when there are none.
        let cached = sh_send.current_bodies.borrow();
        let last_incoming = cached.iter().rev().find(|b| !b.is_outgoing);
        let base_subject = last_incoming
            .map(|b| b.subject.clone())
            .unwrap_or_else(|| c.last_subject.clone());
        let subject = if !subject_override.is_empty() {
            subject_override.clone()
        } else if base_subject.to_lowercase().starts_with("re:") {
            base_subject
        } else {
            format!("Re: {base_subject}")
        };
        // Threading headers from the same last-incoming we used for the subject.
        let (in_reply_to, references) = last_incoming
            .or_else(|| cached.last())
            .map(|b| {
                let irt = (!b.message_id.is_empty()).then(|| b.message_id.clone());
                let mut refs = b.references.clone();
                if !b.message_id.is_empty() {
                    refs.push(b.message_id.clone());
                }
                let refs = (!refs.is_empty()).then(|| refs.join(" "));
                (irt, refs)
            })
            .unwrap_or((None, None));
        if let Some(etx) = sh_send.engine_tx.borrow().as_ref() {
            println!("sending reply to {to:?}");
            let _ = etx.send(engine::EngineCmd::Send {
                to, cc, subject, body: text, in_reply_to, references,
                from: from_identity.clone(),
                attachments,
                forward_attachments: None,
                account_key: sh_send.cur_account_key.borrow().clone(),
            });
            clear_overrides();
        } else {
            eprintln!("send: no live engine (set DDMAIL_* env)");
        }
    });

    // ── Composer attachments ──
    //
    // The attach button opens the native file picker (blocking — the OS
    // dialog is modal, so the event loop has nothing to do meanwhile) and
    // appends the chosen paths to the staged set. `on_send` snapshots that
    // set into each outgoing message and clears it afterwards.
    let ui_weak_att = ui.as_weak();
    let sh_att = shared.clone();
    ui.on_attach_files(move || {
        let Some(u) = ui_weak_att.upgrade() else { return };
        // Parent the dialog to our window so it opens in front and is truly
        // modal — without an owner Win32 can surface it behind the app. The
        // raw-window-handle-06 feature gives us window_handle(); bind it so it
        // outlives pick_files().
        let handle = u.window().window_handle();
        let picked = rfd::FileDialog::new().set_parent(&handle).pick_files();
        if let Some(paths) = picked {
            if paths.is_empty() {
                return;
            }
            sh_att.compose_attachments.borrow_mut().extend(paths);
            refresh_attachment_chips(&u, &sh_att);
        }
    });
    let ui_weak_rm = ui.as_weak();
    let sh_rm = shared.clone();
    ui.on_remove_attachment(move |idx| {
        {
            let mut atts = sh_rm.compose_attachments.borrow_mut();
            let i = idx as usize;
            if i < atts.len() {
                atts.remove(i);
            }
        }
        if let Some(u) = ui_weak_rm.upgrade() {
            refresh_attachment_chips(&u, &sh_rm);
        }
    });

    // ── Search-as-compose dropdown wiring ──
    //
    // Each keystroke fires `search-typed` → we cache the latest query on
    // Shared, kick the engine for both contacts+messages in one call, and
    // immediately update the "Написать xxx@yyy" compose-row from the
    // client-side email regex. Debouncing is unnecessary here: the
    // engine result is keyed by the query string and the UI drops stale
    // answers in `handle_engine_result`.
    let ui_weak_st = ui.as_weak();
    let sh_typed = shared.clone();
    ui.on_search_typed(move |query| {
        let q = query.to_string();
        let trimmed = q.trim().to_string();
        *sh_typed.search_query_inflight.borrow_mut() = trimmed.clone();
        // Compose-row visibility is local to the UI thread — no engine
        // round-trip needed.
        if let Some(ui) = ui_weak_st.upgrade() {
            ui.set_search_compose_email(parse_email_like(&trimmed).unwrap_or_default().into());
            ui.set_search_loading(true);
        }
        if let Some(etx) = sh_typed.engine_tx.borrow().as_ref() {
            let _ = etx.send(engine::EngineCmd::SearchDropdown { query: trimmed, limit: 12 });
        }
    });

    let ui_weak_sc = ui.as_weak();
    let sh_clr = shared.clone();
    ui.on_search_cleared(move || {
        *sh_clr.search_query_inflight.borrow_mut() = String::new();
        sh_clr.search_contacts.borrow_mut().clear();
        sh_clr.search_messages.borrow_mut().clear();
        if let Some(ui) = ui_weak_sc.upgrade() {
            ui.set_search_contacts(ModelRc::new(VecModel::from(Vec::<ContactItem>::new())));
            ui.set_search_messages(ModelRc::new(VecModel::from(Vec::<MessageHit>::new())));
            ui.set_search_compose_email("".into());
            ui.set_search_loading(false);
        }
    });

    let ui_weak_cn = ui.as_weak();
    let sh_cn = shared.clone();
    ui.on_search_compose_new(move |email| {
        let Some(ui) = ui_weak_cn.upgrade() else { return };
        enter_compose_mode(&sh_cn, &ui, email.as_str());
    });

    let ui_weak_sel_c = ui.as_weak();
    let sh_sel_c = shared.clone();
    ui.on_search_select_contact(move |idx| {
        let i = idx as usize;
        let contact = sh_sel_c.search_contacts.borrow().get(i).cloned();
        let Some(contact) = contact else { return };
        // Find any conversation with this counterpart; prefer the most recent.
        let convs = sh_sel_c.convs.borrow();
        let target_lc = contact.email.to_lowercase();
        let best = convs.iter().enumerate().filter(|(_, c)| {
            c.counterparts.first()
                .map(|cp| cp.addr.to_lowercase() == target_lc)
                .unwrap_or(false)
        }).max_by_key(|(_, c)| c.last_date_ts);
        if let Some((conv_idx, _)) = best {
            drop(convs);
            let _ = sh_sel_c.search_query_inflight.borrow_mut().clear();
            if let Some(ui) = ui_weak_sel_c.upgrade() {
                ui.set_search_open(false);
                ui.set_search_query("".into());
                ui.set_selected(conv_idx as i32);
                apply_active_header(&ui, &sh_sel_c, conv_idx);
                open_conversation(&ui, &sh_sel_c, conv_idx);
            }
        } else {
            // No existing conv with this counterpart → enter transient
            // compose mode pointed at this contact's email.
            drop(convs);
            if let Some(ui) = ui_weak_sel_c.upgrade() {
                enter_compose_mode(&sh_sel_c, &ui, &contact.email);
            }
        }
    });

    let ui_weak_sel_m = ui.as_weak();
    let sh_sel_m = shared.clone();
    ui.on_search_select_message(move |idx| {
        let i = idx as usize;
        let env = sh_sel_m.search_messages.borrow().get(i).cloned();
        let Some(env) = env else { return };
        // The conversation that owns this message is the one whose
        // messages list contains the (folder, uid) pair.
        let convs = sh_sel_m.convs.borrow();
        let conv_idx = convs.iter().position(|c| {
            c.messages.iter().any(|m| m.folder == env.folder && m.uid == env.uid)
        });
        if let Some(conv_idx) = conv_idx {
            drop(convs);
            if let Some(ui) = ui_weak_sel_m.upgrade() {
                ui.set_search_open(false);
                ui.set_search_query("".into());
                ui.set_selected(conv_idx as i32);
                apply_active_header(&ui, &sh_sel_m, conv_idx);
                open_conversation(&ui, &sh_sel_m, conv_idx);
            }
        } else {
            // Message is on the server but not in any local conversation —
            // out of scope for v1 of the dropdown.
            println!("search-select-message: no local conv contains {}/{}", env.folder, env.uid);
            if let Some(ui) = ui_weak_sel_m.upgrade() {
                ui.set_search_open(false);
            }
        }
    });

    // Context-menu actions on a message row.
    let ui_weak_act = ui.as_weak();
    let sh_act = shared.clone();
    {
        let ui_weak = ui.as_weak();
        ui.on_source_view_copy(move || {
            use slint::Model;
            let Some(ui) = ui_weak.upgrade() else { return };
            if ui.get_source_view_is_headers() {
                let mut out = String::new();
                for h in ui.get_source_view_headers().iter() {
                    out.push_str(h.name.as_str());
                    out.push_str(": ");
                    out.push_str(h.value.as_str());
                    out.push('\n');
                }
                clipboard_set(&out);
            } else {
                // Full, untruncated source — not the capped slice in the widget.
                SHARED.with(|s| {
                    if let Some(sh) = s.borrow().as_ref() {
                        clipboard_set(&sh.source_view_full.borrow());
                    }
                });
            }
        });
    }

    // ── Mouse text selection over the rendered source bitmap (modal) ──
    {
        let ui_weak = ui.as_weak();
        let sh1 = shared.clone();
        ui.on_src_sel_start(move |x, y| {
            let Some(ui) = ui_weak.upgrade() else { return };
            sh1.src_sel_dragging.set(false);
            sh1.src_sel_moved.set(false);
            {
                let runs = sh1.src_runs.borrow();
                if let Some(i) = nearest_run(&runs, x, y) {
                    sh1.src_sel_anchor.set(i);
                    sh1.src_sel_head.set(i);
                    sh1.src_sel_dragging.set(true);
                }
            }
            ui.set_source_selection_rects(ModelRc::new(VecModel::from(Vec::<SelRect>::new())));
            // Keep the key sink focused so Ctrl+C lands in kb's modal branch.
            ui.invoke_grab_key_focus();
        });
        let ui_weak2 = ui.as_weak();
        let sh2 = shared.clone();
        ui.on_src_sel_move(move |x, y| {
            if !sh2.src_sel_dragging.get() {
                return;
            }
            let Some(ui) = ui_weak2.upgrade() else { return };
            let runs = sh2.src_runs.borrow();
            if let Some(i) = nearest_run(&runs, x, y) {
                if !sh2.src_sel_moved.get() && i == sh2.src_sel_anchor.get() {
                    return; // not an actual drag yet
                }
                sh2.src_sel_moved.set(true);
                sh2.src_sel_head.set(i);
                let rects = selection_rects_for(&runs, sh2.src_sel_anchor.get(), sh2.src_sel_head.get());
                ui.set_source_selection_rects(ModelRc::new(VecModel::from(rects)));
            }
        });
        let sh3 = shared.clone();
        ui.on_src_sel_end(move || {
            sh3.src_sel_dragging.set(false);
        });
        let sh4 = shared.clone();
        ui.on_src_copy_selection(move || {
            let runs = sh4.src_runs.borrow();
            if sh4.src_sel_moved.get() {
                if let Some(t) =
                    selection_text_for(&runs, sh4.src_sel_anchor.get(), sh4.src_sel_head.get())
                {
                    println!("copy source selection: {} chars", t.len());
                    clipboard_set(&t);
                }
            }
        });
    }

    ui.on_msg_action(move |row, action| {
        let row = row as usize;
        let action = action.to_string();
        let msg = sh_act.current_msgs.borrow().get(row).cloned();
        let Some(msg) = msg else { return };
        // Toggle per-sender media/scripts allowance. Cache-aware: bumps
        // policy_gen so the body_cache misses for entries rendered
        // under the old policy, and re-fires SetConversation so the
        // bubbles repaint immediately.
        // «Медиа…» menu: every item toggles one policy switch, persists it
        // immediately, and repaints (the policy generation is part of the
        // texture cache key, so the re-render is guaranteed to miss).
        if action.starts_with("media-") {
            let body_opt = sh_act.current_bodies.borrow().get(row).cloned();
            let Some(b) = body_opt else { return };
            let sender = b.from_addr.clone();
            let (media_host, script_host) =
                sanitize::first_external_hosts(b.html.as_deref().unwrap_or(""));
            {
                let mut p = sh_act.policy.borrow_mut();
                match action.as_str() {
                    "media-allow-all" => p.allow_all = !p.allow_all,
                    "media-scripts-all" => p.allow_all_scripts = !p.allow_all_scripts,
                    "media-scripts-sender" => {
                        p.toggle_scripts(&sender);
                    }
                    "media-scripts-host" => {
                        if script_host.is_empty() {
                            return;
                        }
                        p.toggle_script_host(&script_host);
                    }
                    "media-images-all" => p.allow_all_media = !p.allow_all_media,
                    "media-images-sender" => {
                        p.toggle_media(&sender);
                    }
                    "media-images-host" => {
                        if media_host.is_empty() {
                            return;
                        }
                        p.toggle_media_host(&media_host);
                    }
                    other => {
                        println!("media action {other} — not wired");
                        return;
                    }
                }
                println!("[policy] {action} (sender={sender}, img={media_host}, js={script_host})");
                // Bump the persisted generation BEFORE saving: the texture
                // cache key must change atomically with the policy.
                p.generation += 1;
                let gen_now = p.generation;
                policy::save(&p);
                sh_act.policy_gen.set(gen_now);
            }
            if let Some(ui) = ui_weak_act.upgrade() {
                sync_media_globals(&ui, &sh_act.policy.borrow());
            }
            // Repaint the in-memory bodies under the new policy — no SQLite
            // reload and no network refetch for a permission toggle.
            let bodies = sh_act.current_bodies.borrow().clone();
            send_render_job(&sh_act, bodies, None);
            return;
        }

        // Reply doesn't need the live engine — we just stage the bubble's
        // body into the quote ribbon and let the next Send pick up the
        // subject + threading headers.
        if action == "reply" {
            let body_opt = sh_act.current_bodies.borrow().get(row).cloned();
            let Some(body) = body_opt else {
                eprintln!("reply: body not in memory for {msg:?}");
                return;
            };
            if let Some(ui) = ui_weak_act.upgrade() {
                enter_reply_mode(&sh_act, &ui, body);
            }
            return;
        }
        // Forward: prefill the composer with the quoted original + a "Fwd:"
        // subject, then let the user pick a recipient via search (same path
        // as any new message). Attachments aren't carried yet — noted inline.
        if action == "forward" {
            let body_opt = sh_act.current_bodies.borrow().get(row).cloned();
            let Some(body) = body_opt else {
                eprintln!("forward: body not in memory for {msg:?}");
                return;
            };
            if let Some(ui) = ui_weak_act.upgrade() {
                enter_forward_mode(&sh_act, &ui, body);
            }
            return;
        }
        // «Копировать текст» — the whole message's plain-text part.
        if action == "copy" {
            let body_opt = sh_act.current_bodies.borrow().get(row).cloned();
            let Some(body) = body_opt else { return };
            let text = body
                .text
                .filter(|t| !t.trim().is_empty())
                .unwrap_or_else(|| "(письмо без текстовой версии)".to_string());
            println!("copy message text: {} chars", text.len());
            clipboard_set(&text);
            return;
        }
        // «Показать → Заголовки / Исходник сообщения» — fetch the raw
        // RFC-822 source; the result handler opens the viewer with the
        // requested slice.
        if action == "show-headers" || action == "show-source" {
            sh_act
                .pending_source_view
                .set(if action == "show-headers" { 1 } else { 2 });
            if let Some(etx) = sh_act.engine_tx.borrow().as_ref() {
                let _ = etx.send(engine::EngineCmd::FetchSource {
                    folder: msg.folder.clone(),
                    uid: msg.uid,
                    account_key: sh_act.cur_account_key.borrow().clone(),
                });
            }
            return;
        }
        // «Исходник тела» — the HTML part is already in memory.
        if action == "show-body-source" {
            let body_opt = sh_act.current_bodies.borrow().get(row).cloned();
            let Some(body) = body_opt else { return };
            if let Some(ui) = ui_weak_act.upgrade() {
                set_source_text(
                    &ui,
                    &sh_act,
                    format!("Исходник тела — {}", body.subject),
                    body.html.unwrap_or_default(),
                );
            }
            return;
        }
        // Per-message text/HTML view toggle. The render-mode is part of the
        // texture cache key, so this is a guaranteed re-render of that row.
        if action == "view-text" || action == "view-html" {
            let key = (msg.folder.clone(), msg.uid);
            {
                let mut ov = sh_act.body_view_text.borrow_mut();
                if action == "view-text" {
                    ov.insert(key);
                } else {
                    ov.remove(&key);
                }
            }
            let bodies = sh_act.current_bodies.borrow().clone();
            send_render_job(&sh_act, bodies, None);
            return;
        }
        // Everything else (delete / read / unread) goes through the engine.
        let Some(etx) = sh_act.engine_tx.borrow().clone() else {
            eprintln!("msg-action: no live engine");
            return;
        };
        match action.as_str() {
            "delete" => {
                let _ = etx.send(engine::EngineCmd::Delete {
                    messages: vec![msg],
                    account_key: sh_act.cur_account_key.borrow().clone(),
                });
            }
            "read" => {
                let _ = etx.send(engine::EngineCmd::SetFlags {
                    messages: vec![msg],
                    flags: "\\Seen".into(),
                    add: true,
                    account_key: sh_act.cur_account_key.borrow().clone(),
                });
            }
            "unread" => {
                let _ = etx.send(engine::EngineCmd::SetFlags {
                    messages: vec![msg],
                    flags: "\\Seen".into(),
                    add: false,
                    account_key: sh_act.cur_account_key.borrow().clone(),
                });
            }
            other => println!("msg-action {other} (not wired yet)"),
        }
    });

    // × on the reply ribbon — drop the staged reply target without sending.
    let ui_weak_rc = ui.as_weak();
    let sh_rc = shared.clone();
    ui.on_reply_ribbon_cancel(move || {
        if let Some(ui) = ui_weak_rc.upgrade() {
            exit_reply_mode(&sh_rc, &ui);
        }
    });

    // ── Calendar callbacks ──
    //
    // Switching into the calendar view triggers the initial fetch of
    // both calendars + this-week events. Navigation buttons (prev /
    // today / next) and the workdays/non-work-hours toggles all push
    // the week-start forward/backward and re-fetch.
    let ui_weak_view = ui.as_weak();
    let sh_view = shared.clone();
    ui.on_view_changed(move |mode| {
        if let Some(ui) = ui_weak_view.upgrade() {
            if mode == 1 {
                apply_calendar_view(&ui, &sh_view);
                if let Some(etx) = sh_view.engine_tx.borrow().as_ref() {
                    let _ = etx.send(engine::EngineCmd::FetchCalendars);
                }
                refetch_calendar_events(&ui, &sh_view);
            } else if mode == 2 {
                // Enter the address book: load the full book (empty query).
                ui.set_contacts_query("".into());
                fetch_contacts(&sh_view, "");
            }
        }
    });

    // Address-book search box: fire the lookup on every edit (engine answers
    // are guarded by the echoed query, so stale results are dropped).
    let sh_cs = shared.clone();
    ui.on_contacts_search(move |q| {
        fetch_contacts(&sh_cs, q.as_str());
    });

    // Click a contact row → jump to a compose addressed to them.
    let ui_weak_ca = ui.as_weak();
    ui.on_contact_activated(move |email| {
        let Some(ui) = ui_weak_ca.upgrade() else { return };
        if email.is_empty() {
            return;
        }
        ui.set_view_mode(0);
        ui.invoke_search_compose_new(email);
    });

    // Contact editor: open blank (create).
    let ui_weak_cadd = ui.as_weak();
    let sh_cadd = shared.clone();
    ui.on_contact_add(move || {
        let Some(ui) = ui_weak_cadd.upgrade() else { return };
        sh_cadd.editing_contact_id.set(0);
        ui.set_ce_is_edit(false);
        ui.set_ce_name("".into());
        ui.set_ce_email("".into());
        ui.set_ce_phone("".into());
        ui.set_ce_org("".into());
        ui.set_contact_editor_open(true);
    });

    // Contact editor: open populated for a row (edit).
    let ui_weak_ced = ui.as_weak();
    let sh_ced = shared.clone();
    ui.on_contact_edit(move |idx| {
        let Some(ui) = ui_weak_ced.upgrade() else { return };
        let book = sh_ced.address_book.borrow();
        let Some(c) = book.get(idx.max(0) as usize) else { return };
        sh_ced.editing_contact_id.set(c.id);
        ui.set_ce_is_edit(true);
        ui.set_ce_name(c.full_name.clone().into());
        ui.set_ce_email(c.emails.first().cloned().unwrap_or_default().into());
        ui.set_ce_phone(c.phones.first().cloned().unwrap_or_default().into());
        ui.set_ce_org(c.organization.clone().into());
        ui.set_contact_editor_open(true);
    });

    let ui_weak_ccancel = ui.as_weak();
    ui.on_contact_editor_cancel(move || {
        if let Some(ui) = ui_weak_ccancel.upgrade() {
            ui.set_contact_editor_open(false);
        }
    });

    // Save: create or update, then close and refresh the book.
    let ui_weak_csave = ui.as_weak();
    let sh_csave = shared.clone();
    ui.on_contact_save(move || {
        let Some(ui) = ui_weak_csave.upgrade() else { return };
        let body = contact_body_from_ui(&ui);
        let Some(etx) = sh_csave.engine_tx.borrow().clone() else { return };
        let id = sh_csave.editing_contact_id.get();
        if id == 0 {
            let _ = etx.send(engine::EngineCmd::CreateContact { body });
        } else {
            let _ = etx.send(engine::EngineCmd::UpdateContact { id, body });
        }
        ui.set_contact_editor_open(false);
    });

    // Delete the contact being edited.
    let ui_weak_cdel = ui.as_weak();
    let sh_cdel = shared.clone();
    ui.on_contact_delete(move || {
        let Some(ui) = ui_weak_cdel.upgrade() else { return };
        let id = sh_cdel.editing_contact_id.get();
        if id != 0 {
            if let Some(etx) = sh_cdel.engine_tx.borrow().as_ref() {
                let _ = etx.send(engine::EngineCmd::DeleteContact { id });
            }
        }
        ui.set_contact_editor_open(false);
    });
    // If we start straight in calendar mode (e.g. saved state), kick
    // off the same fetch. (Not yet persisted, but trivial when it is.)

    let nav = |delta_days: i64| {
        let ui_weak = ui.as_weak();
        let sh = shared.clone();
        move || {
            let Some(ui) = ui_weak.upgrade() else { return };
            let new_start = if delta_days == 0 {
                week_start_days_today()
            } else {
                sh.calendar_week_start_days.get() + delta_days
            };
            sh.calendar_week_start_days.set(new_start);
            apply_calendar_view(&ui, &sh);
            refetch_calendar_events(&ui, &sh);
        }
    };
    ui.on_calendar_prev(nav(-7));
    ui.on_calendar_next(nav(7));
    ui.on_calendar_today(nav(0));

    // Grid body size mirror: layout depends on the on-screen canvas, so
    // recompute whenever it changes (and once on init — `changed` doesn't
    // fire for the first layout pass).
    let ui_weak_gr = ui.as_weak();
    let sh_gr = shared.clone();
    ui.on_grid_area_resized(move |w, h| {
        let Some(ui) = ui_weak_gr.upgrade() else { return };
        if w <= 0.0 || h <= 0.0 {
            return;
        }
        let changed = (sh_gr.grid_canvas_w.get() - w).abs() > 0.5
            || (sh_gr.grid_canvas_h.get() - h).abs() > 0.5;
        sh_gr.grid_canvas_w.set(w);
        sh_gr.grid_canvas_h.set(h);
        if changed {
            apply_calendar_view(&ui, &sh_gr);
        }
    });
    // Ctrl-wheel = zoom hours; Ctrl-Alt-wheel = zoom day width. Manual zoom
    // wins over autofit (the layout then scrolls). delta>0 = zoom in.
    let ui_weak_zh = ui.as_weak();
    let sh_zh = shared.clone();
    ui.on_calendar_zoom_hours(move |delta| {
        let Some(ui) = ui_weak_zh.upgrade() else { return };
        let canvas_h = sh_zh.grid_canvas_h.get().max(MIN_HOUR_H);
        let cur = if sh_zh.manual_hour_h.get() > 0.0 {
            sh_zh.manual_hour_h.get()
        } else {
            ui.get_hour_height()
        };
        let factor = if delta > 0.0 { 1.1 } else { 1.0 / 1.1 };
        let next = (cur * factor).clamp(MIN_HOUR_H, canvas_h);
        sh_zh.manual_hour_h.set(next);
        apply_calendar_view(&ui, &sh_zh);
        save_calendar_settings(&ui, &sh_zh);
    });
    let ui_weak_zd = ui.as_weak();
    let sh_zd = shared.clone();
    ui.on_calendar_zoom_days(move |delta| {
        let Some(ui) = ui_weak_zd.upgrade() else { return };
        let avail = (sh_zd.grid_canvas_w.get() - GUTTER_W).max(MIN_COL_W);
        let cur = if sh_zd.manual_col_w.get() > 0.0 {
            sh_zd.manual_col_w.get()
        } else {
            ui.get_col_width()
        };
        let factor = if delta > 0.0 { 1.1 } else { 1.0 / 1.1 };
        let next = (cur * factor).clamp(MIN_COL_W, avail);
        sh_zd.manual_col_w.set(next);
        apply_calendar_view(&ui, &sh_zd);
        save_calendar_settings(&ui, &sh_zd);
    });
    // Working-day start/end from the settings «Календарь» tab.
    let ui_weak_ws = ui.as_weak();
    let sh_ws = shared.clone();
    ui.on_set_work_hours(move |start, end| {
        let Some(ui) = ui_weak_ws.upgrade() else { return };
        let s = start.clamp(0, 23);
        let e = end.clamp(s + 1, 24);
        sh_ws.work_start.set(s);
        sh_ws.work_end.set(e);
        ui.set_work_start(s);
        ui.set_work_end(e);
        apply_calendar_view(&ui, &sh_ws);
        save_calendar_settings(&ui, &sh_ws);
    });
    let ui_weak_vis = ui.as_weak();
    let sh_vis = shared.clone();
    ui.on_calendar_toggle_visibility(move |cal_id| {
        if let Some(ui) = ui_weak_vis.upgrade() {
            let id = cal_id as i64;
            let cur = *sh_vis.calendar_visible.borrow().get(&id).unwrap_or(&true);
            sh_vis.calendar_visible.borrow_mut().insert(id, !cur);
            apply_calendar_view(&ui, &sh_vis);
            save_calendar_settings(&ui, &sh_vis);
        }
    });
    // Panel collapse/expand — the property flips on the Slint side; this
    // callback just persists the new state immediately.
    let ui_weak_pt = ui.as_weak();
    let sh_pt = shared.clone();
    ui.on_calendar_panel_toggled(move || {
        if let Some(ui) = ui_weak_pt.upgrade() {
            save_calendar_settings(&ui, &sh_pt);
        }
    });
    // Notification-sound toggle (burger menu) — persisted immediately.
    let ui_weak_snd = ui.as_weak();
    let sh_snd = shared.clone();
    ui.on_toggle_notify_sound(move || {
        if let Some(ui) = ui_weak_snd.upgrade() {
            ui.set_notify_sound_on(!ui.get_notify_sound_on());
            save_calendar_settings(&ui, &sh_snd);
        }
    });
    // Settings modal: populate the read-only connection section from the
    // live config (env first, then on-disk profile) and show it.
    let ui_weak_set = ui.as_weak();
    let sh_set = shared.clone();
    ui.on_open_settings(move || {
        let Some(ui) = ui_weak_set.upgrade() else { return };
        let cfg = engine::AccountConfig::load_all().into_iter().next();
        match &cfg {
            Some(c) => {
                let account = if c.email.is_empty() {
                    format!("{}@{}", c.username, c.host)
                } else {
                    c.email.clone()
                };
                ui.set_conn_account(account.into());
                ui.set_conn_mode("Онлайн — IMAP/SMTP".into());
                ui.set_conn_imap(
                    format!("{}:{} · {}", c.host, c.port, if c.use_tls { "TLS" } else { "без TLS" })
                        .into(),
                );
                ui.set_conn_smtp(format!("{}:{}", c.smtp_host, c.smtp_port).into());
                ui.set_conn_native(c.native_url.clone().unwrap_or_default().into());
            }
            None => {
                ui.set_conn_account(sh_set.key.clone().into());
                ui.set_conn_mode("Только локальный кэш (IMAP не настроен)".into());
                ui.set_conn_imap("".into());
                ui.set_conn_smtp("".into());
                ui.set_conn_native("".into());
            }
        }
        ui.set_settings_tab(0);
        ui.set_settings_visible(true);
    });
    // Global media-policy toggles from the settings «Контент» tab. Same
    // effect as the per-message «Медиа…» allow-alls, minus the row context,
    // so no body is needed.
    let ui_weak_mg = ui.as_weak();
    let sh_mg = shared.clone();
    ui.on_set_media_global(move |which| {
        let gen_now = {
            let mut p = sh_mg.policy.borrow_mut();
            match which.as_str() {
                "allow-all" => p.allow_all = !p.allow_all,
                "scripts-all" => p.allow_all_scripts = !p.allow_all_scripts,
                "images-all" => p.allow_all_media = !p.allow_all_media,
                other => {
                    println!("media global {other} — not wired");
                    return;
                }
            }
            // Generation must change atomically with the policy so the
            // texture cache key invalidates exactly the affected rows.
            p.generation += 1;
            let g = p.generation;
            policy::save(&p);
            g
        };
        sh_mg.policy_gen.set(gen_now);
        if let Some(ui) = ui_weak_mg.upgrade() {
            sync_media_globals(&ui, &sh_mg.policy.borrow());
        }
        // Repaint the open conversation under the new policy — no refetch.
        let bodies = sh_mg.current_bodies.borrow().clone();
        send_render_job(&sh_mg, bodies, None);
    });
    // Snooze modal choice → commit through the same action machine the
    // toast buttons use ("snz:5" … "snz:atstart").
    let ui_weak_snz = ui.as_weak();
    let sh_snz = shared.clone();
    ui.on_snooze_choice(move |choice| {
        if let Some(ui) = ui_weak_snz.upgrade() {
            let (eid, occ, occ_end, toast_id, summary) = sh_snz.snooze_ctx.borrow().clone();
            if eid != 0 {
                let now_ms = chrono::Utc::now().timestamp_millis();
                let at_start = choice == "atstart";
                let fire_at = if at_start {
                    occ
                } else {
                    now_ms + choice.parse::<i64>().unwrap_or(5) * 60_000
                };
                // User made a choice: cascade → one reminder; toast closes
                // immediately and silently (no cascade-advancing timeout).
                if let Some(c) = sh_snz.cache.as_ref() {
                    if let Err(e) =
                        c.user_choice_reminder(eid, occ, occ_end, fire_at, at_start, &summary)
                    {
                        eprintln!("reminders: user choice failed for {eid}: {e}");
                    }
                }
                toast_window::stop_timer(toast_id); // disarm the timeout hook
                toast_window::close(toast_id);
            }
            sh_snz.snooze_ctx.replace((0, 0, 0, 0, String::new()));
        }
    });
    // Snooze dialog dismissed WITHOUT a choice: the toast behaves as if the
    // button was never pressed — resume its paused countdown.
    let sh_snc = shared.clone();
    ui.on_snooze_cancel(move || {
        let (_, _, _, toast_id, _) = sh_snc.snooze_ctx.borrow().clone();
        if toast_id != 0 {
            toast_window::resume_timer(toast_id);
        }
        sh_snc.snooze_ctx.replace((0, 0, 0, 0, String::new()));
    });
    // Colour picked in the per-calendar palette popup.
    let ui_weak_cc = ui.as_weak();
    let sh_cc = shared.clone();
    ui.on_calendar_set_color(move |cal_id, palette_idx| {
        let Some(ui) = ui_weak_cc.upgrade() else { return };
        if let Some(hex_color) = CAL_PALETTE.get(palette_idx as usize) {
            sh_cc
                .calendar_colors
                .borrow_mut()
                .insert(cal_id as i64, (*hex_color).to_string());
            apply_calendar_view(&ui, &sh_cc);
            save_calendar_settings(&ui, &sh_cc);
        }
    });
    // Event click → populate + show the detail popup (Phase B, read-only).
    let ui_weak_ev = ui.as_weak();
    let sh_ev = shared.clone();
    ui.on_event_clicked(move |id| {
        use chrono::{Datelike, Local, TimeZone, Timelike};
        let Some(ui) = ui_weak_ev.upgrade() else { return };
        let events = sh_ev.calendar_events.borrow();
        let Some(ev) = events.iter().find(|e| e.id as i32 == id) else { return };

        // Humanized date: «чт, 12 декабря · 14:30 – 15:30» — a bare digit
        // train («12.12 14:30») read as a hyperlink-ish blur.
        const WD: [&str; 7] = ["пн", "вт", "ср", "чт", "пт", "сб", "вс"];
        const MON: [&str; 12] = [
            "января", "февраля", "марта", "апреля", "мая", "июня",
            "июля", "августа", "сентября", "октября", "ноября", "декабря",
        ];
        let date_of = |ms: i64| {
            Local
                .timestamp_millis_opt(ms)
                .single()
                .map(|d| {
                    format!(
                        "{}, {} {}",
                        WD[d.weekday().num_days_from_monday() as usize],
                        d.day(),
                        MON[(d.month() - 1) as usize]
                    )
                })
                .unwrap_or_default()
        };
        let tm = |ms: i64| {
            Local
                .timestamp_millis_opt(ms)
                .single()
                .map(|d| format!("{:02}:{:02}", d.hour(), d.minute()))
                .unwrap_or_default()
        };
        let when = if ev.all_day {
            format!("{} · весь день", date_of(ev.dtstart))
        } else if let Some(end) = ev.dtend {
            format!("{} · {} – {}", date_of(ev.dtstart), tm(ev.dtstart), tm(end))
        } else {
            format!("{} · {}", date_of(ev.dtstart), tm(ev.dtstart))
        };

        let organizer = match (ev.organizer_name.is_empty(), ev.organizer_email.is_empty()) {
            (true, true) => String::new(),
            (true, false) => ev.organizer_email.clone(),
            (false, true) => ev.organizer_name.clone(),
            (false, false) => format!("{} <{}>", ev.organizer_name, ev.organizer_email),
        };
        // Attendees table: localized status + colour per row; also resolve
        // MY participation so the pressed RSVP button is obvious.
        let status_of = |ps: &str| -> (&'static str, &'static str) {
            match ps.to_uppercase().as_str() {
                "ACCEPTED" => ("Принял", "#27ae60"),
                "DECLINED" => ("Отклонил", "#eb5757"),
                "TENTATIVE" => ("Возможно", "#f2994a"),
                _ => ("Не ответил", "#8b95a1"),
            }
        };
        let att_rows: Vec<AttRow> = ev
            .attendees
            .iter()
            .map(|a| {
                let n = if a.name.is_empty() { a.email.clone() } else { a.name.clone() };
                let (st, col) = status_of(&a.partstat);
                AttRow {
                    name: n.into(),
                    status: st.into(),
                    color: hex(col),
                }
            })
            .collect();
        let my_partstat = {
            let idents = sh_ev.identity_colors.borrow();
            let me_key = sh_ev.key.to_lowercase();
            ev.attendees
                .iter()
                .find(|a| {
                    let lc = a.email.to_lowercase();
                    lc == me_key || idents.contains_key(&lc)
                })
                .map(|a| a.partstat.to_uppercase())
                .unwrap_or_default()
        };

        ui.set_detail_title(
            if ev.summary.is_empty() { "(без названия)".into() } else { ev.summary.clone() }.into(),
        );
        ui.set_detail_when(when.into());
        ui.set_detail_location(ev.location.clone().into());
        ui.set_detail_organizer(organizer.into());
        ui.set_detail_attendee_rows(ModelRc::new(VecModel::from(att_rows)));
        ui.set_detail_my_partstat(my_partstat.into());
        ui.set_detail_description(ev.description.clone().into());
        // Status/recurrence/reminder digest — same shape as the edit form.
        let mut meta: Vec<String> = Vec::new();
        match ev.status.to_uppercase().as_str() {
            "CANCELLED" => meta.push("Отменено".to_string()),
            "TENTATIVE" => meta.push("Предварительно".to_string()),
            _ => {}
        }
        if !ev.rrule.is_empty() {
            meta.push(humanize_rrule(&ev.rrule));
        }
        if ev.alarm_lead_min > 0 {
            meta.push(format!("Напоминание за {}", humanize_lead(ev.alarm_lead_min)));
        }
        ui.set_detail_meta(meta.join(" · ").into());
        // Every non-default VEVENT property the server extracted.
        let extras: Vec<EventExtraItem> = ev
            .extras
            .iter()
            .map(|x| {
                let (label, value) = extra_label(&x.name, &x.value);
                let is_link = value.starts_with("http://") || value.starts_with("https://");
                EventExtraItem { label: label.into(), value: value.into(), is_link }
            })
            .collect();
        let extra_urls: std::collections::HashSet<String> = extras
            .iter()
            .filter(|x| x.is_link)
            .map(|x| x.value.to_string())
            .collect();
        ui.set_detail_extras(ModelRc::new(VecModel::from(extras)));
        // Meeting links live as plain text in location/description more
        // often than not — surface every URL as a clickable row, minus the
        // ones already shown as first-class extras (CONFERENCE/URL).
        let links: Vec<slint::SharedString> =
            extract_urls(&[ev.location.as_str(), ev.description.as_str()])
                .into_iter()
                .filter(|u| !extra_urls.contains(u))
                .map(Into::into)
                .collect();
        ui.set_detail_links(ModelRc::new(VecModel::from(links)));
        ui.set_detail_event_id(ev.id as i32);
        ui.set_detail_visible(true);
    });
    let ui_weak_dol = ui.as_weak();
    ui.on_detail_open_link(move |url| {
        if let Some(ui) = ui_weak_dol.upgrade() {
            handle_link(&ui, url.to_string());
        }
    });
    let ui_weak_dc = ui.as_weak();
    ui.on_detail_close(move || {
        if let Some(ui) = ui_weak_dc.upgrade() {
            ui.set_detail_visible(false);
        }
    });
    // RSVP from the detail popup → set PARTSTAT on the server, then refresh.
    let sh_rsvp = shared.clone();
    ui.on_rsvp(move |id, partstat| {
        if let Some(etx) = sh_rsvp.engine_tx.borrow().as_ref() {
            println!("rsvp event {id} -> {partstat}");
            let _ = etx.send(engine::EngineCmd::Rsvp {
                event_id: id as i64,
                partstat: partstat.to_string(),
            });
        }
    });

    // Delete event from the detail popup.
    let ui_weak_del = ui.as_weak();
    let sh_del = shared.clone();
    ui.on_detail_delete(move || {
        let Some(ui) = ui_weak_del.upgrade() else { return };
        let id = ui.get_detail_event_id() as i64;
        // Delete wipes the event's reminders too (and the toast, if showing).
        if let Some(c) = sh_del.cache.as_ref() {
            let _ = c.purge_event_reminders(id);
        }
        toast_window::close_for_event(id);
        if let Some(etx) = sh_del.engine_tx.borrow().as_ref() {
            println!("delete event {id}");
            let _ = etx.send(engine::EngineCmd::DeleteEvent { event_id: id });
        }
        ui.set_detail_visible(false);
    });

    // Create / edit event form.
    let ui_weak_new = ui.as_weak();
    let sh_new = shared.clone();
    ui.on_new_event(move || {
        if let Some(ui) = ui_weak_new.upgrade() {
            open_create_form(&ui, &sh_new);
        }
    });
    // Double-click on empty grid space → create form prefilled with that
    // day/time. x/y are viewport-content px; view_w is the viewport width.
    let ui_weak_gc = ui.as_weak();
    let sh_gc = shared.clone();
    // Manual double-click detection (the Flickable eats TouchArea::double-clicked):
    // (last_ms, last_x, last_y). A create fires only on the second click within
    // 450 ms and ~12 px of the first.
    let gc_last = std::cell::Cell::new((0i64, 0f32, 0f32));
    ui.on_grid_create_at(move |x, y, view_w| {
        let Some(ui) = ui_weak_gc.upgrade() else { return };
        let now = chrono::Local::now().timestamp_millis();
        let (last_ms, last_x, last_y) = gc_last.get();
        let is_double =
            now - last_ms < 450 && (x - last_x).abs() < 12.0 && (y - last_y).abs() < 12.0;
        if !is_double {
            // First click — arm and wait for the second.
            gc_last.set((now, x, y));
            return;
        }
        gc_last.set((0, 0.0, 0.0)); // consume, so a triple-click doesn't re-fire
        const GUTTER: f32 = 48.0;
        if x < GUTTER {
            return; // clicked in the time-label gutter
        }
        let day_count = ui.get_day_count();
        if day_count <= 0 {
            return;
        }
        let col_w = (view_w - GUTTER) / day_count as f32;
        if col_w <= 0.0 {
            return;
        }
        let day = ((x - GUTTER) / col_w).floor() as i64;
        if day < 0 || day >= day_count as i64 {
            return;
        }
        // y px → hour-of-day, then snap the start to the nearest 15 minutes.
        let hour_height = ui.get_hour_height();
        let hour_start = ui.get_hour_start();
        let minutes = hour_start as f32 * 60.0 + (y / hour_height) * 60.0;
        let snapped = ((minutes / 15.0).round() as i64) * 15;
        let day_ms: i64 = 24 * 60 * 60 * 1000;
        let (week_start_ms, _) =
            week_range_ms(sh_gc.calendar_week_start_days.get(), day_count);
        let start_ms = week_start_ms + day * day_ms + snapped * 60_000;
        open_create_form_at(&ui, &sh_gc, start_ms);
    });

    // Drag-to-move a block to a new day/time (writable calendars only — the
    // block's TouchArea won't even start a drag otherwise). The ghost's final
    // top-left (grid px) → nearest day column + 15-min-snapped start; duration
    // and all other fields are preserved.
    let ui_weak_gm = ui.as_weak();
    let sh_gm = shared.clone();
    ui.on_grid_event_moved(move |id, orig_x, orig_y, new_x, new_y| {
        let Some(ui) = ui_weak_gm.upgrade() else { return };
        const GUTTER: f32 = 48.0;
        let day_count = ui.get_day_count();
        let col_w = ui.get_col_width();
        if day_count <= 0 || col_w <= 0.0 {
            return;
        }
        let hour_height = ui.get_hour_height();
        let hour_start = ui.get_hour_start();
        let day_ms: i64 = 24 * 60 * 60 * 1000;
        let (week_start_ms, _) = week_range_ms(sh_gm.calendar_week_start_days.get(), day_count);

        // Block x = GUTTER + (day + lane_xf)*col_w + 2px, lane_xf ∈ [0,1) for
        // overlap lanes — floor recovers the day column. round() broke every
        // block in lane xf >= 0.5: the lookup jumped to the NEXT day, the
        // cal_occ probe missed, and the drag silently did nothing.
        let px_to_day = |x: f32| -> i64 {
            (((x - GUTTER - 2.0) / col_w).floor() as i64).clamp(0, day_count as i64 - 1)
        };
        let px_to_min = |y: f32| -> i64 {
            let minutes = hour_start as f32 * 60.0 + (y / hour_height) * 60.0;
            ((minutes / 15.0).round().max(0.0) as i64) * 15
        };

        let orig_day = px_to_day(orig_x);
        let new_day = px_to_day(new_x);
        let new_start = week_start_ms + new_day * day_ms + px_to_min(new_y) * 60_000;

        // Exact instance grabbed (gives recurrence_id + duration + whether the
        // event recurs). Keyed (event_id, original day column).
        let (occ_start, occ_end, recurring) =
            match sh_gm.cal_occ.borrow().get(&(id, orig_day as i32)).copied() {
                Some(v) => v,
                None => {
                    eprintln!("[cal] move: no occurrence for id={id} day={orig_day} — drop ignored");
                    return;
                }
            };
        if new_start == occ_start {
            return; // dropped back where it was
        }
        let new_end = new_start + (occ_end - occ_start).max(0);

        // Preserve the event's display fields.
        let (summary, description, location, all_day) = {
            let events = sh_gm.calendar_events.borrow();
            match events.iter().find(|e| e.id as i32 == id) {
                Some(e) => (
                    e.summary.clone(),
                    e.description.clone(),
                    e.location.clone(),
                    e.all_day,
                ),
                None => return,
            }
        };

        let mut body = serde_json::json!({
            "summary": summary,
            "description": description,
            "location": location,
            "all_day": all_day,
            "dtstart": new_start,
            "dtend": new_end,
        });
        if recurring {
            // Move just THIS occurrence — an "all" dtstart shift keeps BYDAY's
            // weekday, so only scope=single (an override) actually re-days it.
            body["scope"] = "single".into();
            body["recurrence_id"] = occ_start.into();
            // No optimistic redraw: the override can't be reflected by local
            // RRULE expansion; the refetch after PatchEvent shows it.
        } else {
            body["scope"] = "all".into();
            // Optimistic shift so the block lands immediately; refetch reconciles.
            {
                let mut events = sh_gm.calendar_events.borrow_mut();
                if let Some(e) = events.iter_mut().find(|e| e.id as i32 == id) {
                    if e.dtend.is_some() {
                        e.dtend = Some(new_end);
                    }
                    e.dtstart = new_start;
                }
            }
            apply_calendar_view(&ui, &sh_gm);
        }

        if let Some(c) = sh_gm.cache.as_ref() {
            let _ = c.purge_event_reminders(id as i64);
        }
        if let Some(etx) = sh_gm.engine_tx.borrow().as_ref() {
            let _ = etx.send(engine::EngineCmd::PatchEvent {
                event_id: id as i64,
                body,
            });
        }
    });

    // Resize a block by its top/bottom edge (writable only). Day is unchanged
    // (taken from the original x); new start = top edge, new end = bottom edge.
    let ui_weak_gr = ui.as_weak();
    let sh_gr = shared.clone();
    ui.on_grid_event_resized(move |id, orig_x, orig_y, new_top_y, new_bottom_y| {
        let _ = orig_y;
        let Some(ui) = ui_weak_gr.upgrade() else { return };
        const GUTTER: f32 = 48.0;
        let day_count = ui.get_day_count();
        let col_w = ui.get_col_width();
        if day_count <= 0 || col_w <= 0.0 {
            return;
        }
        let hour_height = ui.get_hour_height();
        let hour_start = ui.get_hour_start();
        let day_ms: i64 = 24 * 60 * 60 * 1000;
        let (week_start_ms, _) = week_range_ms(sh_gr.calendar_week_start_days.get(), day_count);
        // floor, not round: orig_x carries the overlap-lane fraction (xf) —
        // see px_to_day in the move handler above.
        let day = (((orig_x - GUTTER - 2.0) / col_w).floor() as i64).clamp(0, day_count as i64 - 1);
        let to_min = |y: f32| -> i64 {
            let m = hour_start as f32 * 60.0 + (y / hour_height) * 60.0;
            ((m / 15.0).round().max(0.0) as i64) * 15
        };
        let new_start = week_start_ms + day * day_ms + to_min(new_top_y) * 60_000;
        let mut new_end = week_start_ms + day * day_ms + to_min(new_bottom_y) * 60_000;
        if new_end <= new_start {
            new_end = new_start + 15 * 60_000;
        }

        let (occ_start, occ_end, recurring) =
            match sh_gr.cal_occ.borrow().get(&(id, day as i32)).copied() {
                Some(v) => v,
                None => {
                    eprintln!("[cal] resize: no occurrence for id={id} day={day} — ignored");
                    return;
                }
            };
        if new_start == occ_start && new_end == occ_end {
            return; // no change
        }
        let (summary, description, location, all_day) = {
            let events = sh_gr.calendar_events.borrow();
            match events.iter().find(|e| e.id as i32 == id) {
                Some(e) => (
                    e.summary.clone(),
                    e.description.clone(),
                    e.location.clone(),
                    e.all_day,
                ),
                None => return,
            }
        };
        let mut body = serde_json::json!({
            "summary": summary,
            "description": description,
            "location": location,
            "all_day": all_day,
            "dtstart": new_start,
            "dtend": new_end,
        });
        if recurring {
            body["scope"] = "single".into();
            body["recurrence_id"] = occ_start.into();
        } else {
            body["scope"] = "all".into();
            {
                let mut events = sh_gr.calendar_events.borrow_mut();
                if let Some(e) = events.iter_mut().find(|e| e.id as i32 == id) {
                    e.dtstart = new_start;
                    e.dtend = Some(new_end);
                }
            }
            apply_calendar_view(&ui, &sh_gr);
        }
        if let Some(c) = sh_gr.cache.as_ref() {
            let _ = c.purge_event_reminders(id as i64);
        }
        if let Some(etx) = sh_gr.engine_tx.borrow().as_ref() {
            let _ = etx.send(engine::EngineCmd::PatchEvent {
                event_id: id as i64,
                body,
            });
        }
    });

    let ui_weak_ee = ui.as_weak();
    let sh_ee = shared.clone();
    ui.on_detail_edit(move || {
        let Some(ui) = ui_weak_ee.upgrade() else { return };
        let id = ui.get_detail_event_id();
        let events = sh_ee.calendar_events.borrow();
        if let Some(ev) = events.iter().find(|e| e.id as i32 == id) {
            ui.set_detail_visible(false);
            open_edit_form(&ui, &sh_ee, ev);
        }
    });
    let ui_weak_es = ui.as_weak();
    let sh_es = shared.clone();
    ui.on_edit_save(move || {
        if let Some(ui) = ui_weak_es.upgrade() {
            save_edit_form(&ui, &sh_es);
        }
    });
    let ui_weak_ec = ui.as_weak();
    ui.on_edit_cancel(move || {
        if let Some(ui) = ui_weak_ec.upgrade() {
            ui.set_edit_visible(false);
        }
    });
    ui.on_edit_open_url(move |url| {
        open_external(url.as_str());
    });

    // Calendar reminders: a UI-thread timer scans the persisted reminder
    // table every interval and toasts whatever just came due. Runs on the
    // Slint event loop, which keeps ticking while hidden to tray — so we
    // don't need the background Tokio task the old build relied on. Bound
    // to a name (not bare `_`) so it lives for the loop's lifetime.
    let _reminder_timer = slint::Timer::default();
    _reminder_timer.start(
        slint::TimerMode::Repeated,
        std::time::Duration::from_secs(reminders::SCAN_INTERVAL_SECS),
        || {
            let now_ms = chrono::Utc::now().timestamp_millis();
            let due = SHARED.with(|s| {
                s.borrow()
                    .as_ref()
                    .and_then(|sh| sh.cache.as_ref().map(|c| reminders::scan(c, now_ms)))
                    .unwrap_or_default()
            });
            for t in due {
                // One toast per event on screen at a time (dedup a burst).
                if toast_window::has_for_event(t.row.event_id) {
                    continue;
                }
                let title = reminders::title_for(&t);
                let body = reminders::body_for(&t, now_ms);
                let eid = t.row.event_id;
                let occ = t.row.occurrence_start_ms;
                let seq = t.row.seq;
                let summary = t.row.summary.clone();

                match t.mode {
                    reminders::ToastMode::AtStart | reminders::ToastMode::AlreadyRunning => {
                        // ✕ = close only; body = open card + close. No snooze,
                        // no cascade advance (this is the terminal alarm).
                        let s_body = summary.clone();
                        let id = toast_window::show(
                            2,
                            eid,
                            &title,
                            &body,
                            false,
                            reminders::AT_START_TIMEOUT_SECS,
                            move || reminder_dispatch("cancel-occ", eid, occ, seq, String::new()),
                            move || reminder_dispatch("open-close", eid, occ, seq, s_body.clone()),
                            || {},
                        );
                        // Timeout = silent expiry; still retire the row so the
                        // cascade can't resurrect it.
                        toast_window::set_on_timeout(id, move || {
                            reminder_dispatch("timeout", eid, occ, seq, String::new())
                        });
                    }
                    reminders::ToastMode::Soon => {
                        // ✕ = kill the whole cascade of this occurrence.
                        // Body = open card, STOP the timer (toast stays).
                        // «Напомнить позже» = snooze dialog (pauses timer).
                        // Timeout = advance the cascade to the next alarm.
                        let s_body = summary.clone();
                        let s_act = summary.clone();
                        let id = toast_window::show(
                            1,
                            eid,
                            &title,
                            &body,
                            true,
                            reminders::SOON_TIMEOUT_SECS,
                            move || reminder_dispatch("cancel-occ", eid, occ, seq, String::new()),
                            move || reminder_dispatch("open-stay", eid, occ, seq, s_body.clone()),
                            move || reminder_dispatch("snooze-window", eid, occ, seq, s_act.clone()),
                        );
                        toast_window::set_on_timeout(id, move || {
                            reminder_dispatch("timeout", eid, occ, seq, String::new())
                        });
                    }
                }
            }
        },
    );

    // System tray (Windows): left-click / "Открыть" re-shows the window,
    // "Выход" quits. Kept alive until the event loop ends.
    // Toast click callbacks (non-UI threads) reach the event loop through
    // this weak handle.
    let _ = UI_WEAK.set(ui.as_weak());

    #[cfg(windows)]
    {
        let ui_open = ui.as_weak();
        let tray = tray::setup(
            move || {
                println!("tray: open requested");
                if let Some(ui) = ui_open.upgrade() {
                    raise_window(&ui);
                }
                // Showing the window acknowledges the unread dot.
                tray_set_dot(false);
            },
            || slint::quit_event_loop().unwrap(),
        );
        TRAY.with(|t| *t.borrow_mut() = tray);
    }

    // System tray (Linux / ksni): same behaviour, but callbacks arrive on the
    // ksni service thread, so each one marshals its UI work back to the Slint
    // event loop via invoke_from_event_loop.
    #[cfg(target_os = "linux")]
    {
        let ui_open = ui.as_weak();
        let tray = tray::setup(
            move || {
                println!("tray: open requested");
                let ui_open = ui_open.clone();
                let _ = slint::invoke_from_event_loop(move || {
                    if let Some(ui) = ui_open.upgrade() {
                        raise_window(&ui);
                    }
                    // Showing the window acknowledges the unread dot.
                    tray_set_dot(false);
                });
            },
            || {
                let _ = slint::invoke_from_event_loop(|| {
                    let _ = slint::quit_event_loop();
                });
            },
        );
        TRAY.with(|t| *t.borrow_mut() = tray);
    }

    ui.run().unwrap();
}

/// Apply an engine result on the UI thread (reaches Shared via the thread-local).
/// Recompute the aggregate connection light from per-account states:
/// 2 = green (all connected), 1 = yellow (some down), 0 = red (none connected).
fn apply_conn_status(ui: &MainWindow, sh: &Shared) {
    let states = sh.account_states.borrow();
    let keys = sh.account_keys.borrow();
    let total = keys.len();
    let connected = keys
        .iter()
        .filter(|k| states.get(*k).map(|s| s == "connected").unwrap_or(false))
        .count();
    let status = if total == 0 || connected == total {
        2
    } else if connected > 0 {
        1
    } else {
        0
    };
    ui.set_conn_status(status);
}

fn handle_engine_result(ui: &MainWindow, res: engine::EngineResult) {
    match res {
        engine::EngineResult::Conversations { mut list, partial } => {
            SHARED.with(|s| {
                if let Some(sh) = s.borrow().as_ref() {
                    if partial && list.is_empty() {
                        println!("engine: conversations delta — no changes");
                        return;
                    }
                    // Remember the open conversation's identity: merging or a
                    // refetch can reorder the list and shift its index.
                    let current_id = sh.convs.borrow().get(sh.current.get()).map(|c| c.id.clone());
                    let merged: Vec<Conversation> = if partial {
                        let mut all = sh.convs.borrow().clone();
                        for nc in list.drain(..) {
                            match all.iter_mut().find(|c| c.id == nc.id) {
                                Some(slot) => *slot = nc,
                                None => all.push(nc),
                            }
                        }
                        all.sort_by(|a, b| b.last_date_ts.cmp(&a.last_date_ts));
                        all
                    } else {
                        list
                    };
                    println!(
                        "engine: {} conversations (delta={})",
                        merged.len(),
                        if partial { "merge" } else { "full" }
                    );
                    // Identities may have been (re)synced alongside the
                    // conversations — refresh the row-tint map and the
                    // composer from-picker from cache.
                    if let Some(cache) = &sh.cache {
                        *sh.identity_colors.borrow_mut() = identity_color_map(cache, &sh.key);
                        refresh_composer_identities(ui, sh);
                    }
                    let displays = displays_from(&merged, &sh.identity_colors.borrow());
                    let items = sidebar_items(&displays, &sh.avatars.borrow());
                    ui.set_conversations(ModelRc::new(VecModel::from(items)));
                    // Request avatars for unique counterpart emails not yet cached.
                    if let Some(etx) = sh.engine_tx.borrow().as_ref() {
                        let mut seen = std::collections::HashSet::new();
                        for d in &displays {
                            if d.email.is_empty() || sh.avatars.borrow().contains_key(&d.email) {
                                continue;
                            }
                            if seen.insert(d.email.clone()) {
                                let _ = etx.send(engine::EngineCmd::FetchAvatar { email: d.email.clone() });
                            }
                        }
                    }
                    *sh.convs.borrow_mut() = merged;
                    *sh.displays.borrow_mut() = displays;
                    // Re-locate the selection by id (skip in transient-compose
                    // mode, where the sidebar has a synthetic first row).
                    if sh.pending_compose.borrow().is_none() {
                        if let Some(id) = current_id {
                            if let Some(idx) = sh.convs.borrow().iter().position(|c| c.id == id) {
                                if idx != sh.current.get() {
                                    sh.current.set(idx);
                                    ui.set_selected(idx as i32);
                                }
                            }
                        }
                        // The dialog on screen can never show an unread pill —
                        // the user is reading it. Push \Seen for whatever the
                        // server still reports unseen (covers letters whose
                        // uid never reached us, e.g. two in one sync batch).
                        if ui.window().is_visible() && ui.get_view_mode() == 0 {
                            let cur = sh.current.get();
                            let mut to_mark: Vec<MessageRef> = Vec::new();
                            let had_unread = {
                                let mut convs = sh.convs.borrow_mut();
                                match convs.get_mut(cur) {
                                    Some(c) if c.unread_count > 0 => {
                                        for m in c.messages.iter_mut().filter(|m| !m.seen) {
                                            to_mark.push(m.clone());
                                            m.seen = true;
                                        }
                                        c.unread_count = 0;
                                        true
                                    }
                                    _ => false,
                                }
                            };
                            if had_unread {
                                let displays = displays_from(
                                    &sh.convs.borrow(),
                                    &sh.identity_colors.borrow(),
                                );
                                *sh.displays.borrow_mut() = displays;
                                refresh_sidebar(sh, ui);
                            }
                            if !to_mark.is_empty() {
                                if let Some(etx) = sh.engine_tx.borrow().as_ref() {
                                    let _ = etx.send(engine::EngineCmd::SetFlags {
                                        messages: to_mark,
                                        flags: "\\Seen".into(),
                                        add: true,
                                        account_key: sh.cur_account_key.borrow().clone(),
                                    });
                                }
                            }
                        }
                    }
                }
            });
        }
        engine::EngineResult::Avatar { email, rgba, w, h } => {
            let buf = SharedPixelBuffer::<Rgba8Pixel>::clone_from_slice(&rgba, w, h);
            let img = Image::from_rgba8(buf);
            SHARED.with(|s| {
                if let Some(sh) = s.borrow().as_ref() {
                    sh.avatars.borrow_mut().insert(email, img);
                    let items = sidebar_items(&sh.displays.borrow(), &sh.avatars.borrow());
                    ui.set_conversations(ModelRc::new(VecModel::from(items)));
                }
            });
        }
        engine::EngineResult::Messages { bodies, generation } => {
            SHARED.with(|s| {
                if let Some(sh) = s.borrow().as_ref() {
                    // Stale-fetch guard: the user may have switched
                    // conversations while this answer was in flight — an
                    // old answer must not overwrite the new screen (same
                    // pattern as the search dropdown's query echo).
                    if generation != sh.open_gen.get() {
                        println!(
                            "engine: dropping stale Messages (gen {generation} != {})",
                            sh.open_gen.get()
                        );
                        return;
                    }
                    *sh.current_msgs.borrow_mut() = bodies
                        .iter()
                        .map(|b| MessageRef { folder: b.folder.clone(), uid: b.uid, message_id: b.message_id.clone(), seen: true })
                        .collect();
                    *sh.current_bodies.borrow_mut() = bodies.clone();
                    // Last render after open is THIS one (it aborts the
                    // optimistic cached render) — consume the pending scroll.
                    let scroll = take_scroll_target(sh, &bodies, true);
                    send_render_job(sh, bodies, scroll);
                }
            });
        }
        engine::EngineResult::Done(what) => {
            println!("engine: {what} done — refreshing");
            if what == "rsvp" {
                // Calendar mutation → refresh the visible week's events.
                SHARED.with(|s| {
                    if let Some(sh) = s.borrow().as_ref() {
                        refetch_calendar_events(ui, sh);
                    }
                });
                return;
            }
            if what == "contact" {
                // Address-book mutation → refresh the current view/search.
                let q = ui.get_contacts_query().to_string();
                SHARED.with(|s| {
                    if let Some(sh) = s.borrow().as_ref() {
                        fetch_contacts(sh, &q);
                    }
                });
                return;
            }
            SHARED.with(|s| {
                if let Some(sh) = s.borrow().as_ref() {
                    // A flag change (seen/unseen) doesn't alter the conversation
                    // list — the optimistic local update already cleared the
                    // pill. Refetching here would re-detect a still-"unread"
                    // server state (e.g. an unfetchable/stale message whose
                    // \Seen never lands) and re-mark it forever. Only structural
                    // changes (delete / spam purge) need a refetch.
                    if what == "flags" {
                        return;
                    }
                    if let Some(etx) = sh.engine_tx.borrow().as_ref() {
                        let _ = etx.send(engine::EngineCmd::FetchConversations { limit: CONV_FETCH_LIMIT });
                        // Reopen current conversation to refresh its bodies.
                        let cur = sh.current.get();
                        if let Some(c) = sh.convs.borrow().get(cur) {
                            let _ = etx.send(engine::EngineCmd::FetchMessages {
                                messages: c.messages.clone(),
                                generation: sh.open_gen.get(),
                                account_key: sh.cur_account_key.borrow().clone(),
                            });
                        }
                    }
                }
            });
        }
        engine::EngineResult::AccountState { account_key, state } => {
            println!("account {account_key}: {state}");
            SHARED.with(|s| {
                if let Some(sh) = s.borrow().as_ref() {
                    // Catch-up refetch on every (re)connect: the client is
                    // push-driven, and any event published while the socket
                    // was dead (silent TCP death, server restart, watchdog
                    // gap) is gone for good — the hub doesn't replay. One
                    // conversations fetch per reconnect closes that window.
                    if state == "connected" {
                        if let Some(etx) = sh.engine_tx.borrow().as_ref() {
                            let _ = etx.send(engine::EngineCmd::FetchConversations {
                                limit: CONV_FETCH_LIMIT,
                            });
                        }
                    }
                    sh.account_states
                        .borrow_mut()
                        .insert(account_key, state);
                    apply_conn_status(ui, sh);
                }
            });
        }
        engine::EngineResult::Event(ev) => {
            use ddmail_core::event::EngineEvent;
            match ev {
                EngineEvent::NewMail { folder, count: _, new_count, from, subject, message_id } => {
                    println!("engine event: new mail in {folder} (+{new_count}) from {from:?}");
                    SHARED.with(|s| {
                        let Some(sh) = s.borrow().as_ref().cloned() else { return };
                        handle_new_mail(ui, &sh, folder, new_count, from, subject, message_id);
                    });
                }
                EngineEvent::ConnectionState { state, message } => {
                    println!("engine connection: {state} {}", message.unwrap_or_default());
                }
                EngineEvent::Expunged { folder } => {
                    println!("engine event: expunge in {folder} — forcing full resync");
                    SHARED.with(|s| {
                        if let Some(sh) = s.borrow().as_ref() {
                            // Force a FULL conversation resync so deleted threads
                            // drop off: the delta only reports CHANGED convs, not
                            // removals. Clearing each account's full-sync stamp
                            // makes the next FetchConversations run with since=0
                            // (full), which replaces the cache and prunes the
                            // gone conversations.
                            if let Some(cache) = &sh.cache {
                                for k in sh.account_keys.borrow().iter() {
                                    cache.set_meta(&format!("conv_full_ts:{k}"), "0").ok();
                                }
                            }
                            if let Some(etx) = sh.engine_tx.borrow().as_ref() {
                                let _ = etx.send(engine::EngineCmd::FetchConversations { limit: CONV_FETCH_LIMIT });
                            }
                        }
                    });
                }
                EngineEvent::CalendarUpdated { calendar_id } => {
                    println!("engine event: calendar {calendar_id} updated");
                }
                EngineEvent::TokenRefreshed { account_id, token } => {
                    // Persist the rotated JWT — otherwise the next launch
                    // starts from the stale token and, past the 30-day
                    // refresh window, would silently fall back to cache-only.
                    engine::AccountConfig::persist_native_token(&account_id, &token);
                    println!("engine event: token refreshed for {account_id} (persisted)");
                }
            }
        }
        engine::EngineResult::Sent(id) => {
            println!("engine: message sent ({id}) — refetching conversations");
            SHARED.with(|s| {
                if let Some(sh) = s.borrow().as_ref() {
                    // Leaving transient-compose mode now that the message
                    // landed; the new conversation row will appear on the
                    // next FetchConversations and the user can pick it up
                    // from the sidebar.
                    let was_pending = sh.pending_compose.borrow_mut().take().is_some();
                    if was_pending {
                        refresh_sidebar(sh, ui);
                    }
                    if let Some(etx) = sh.engine_tx.borrow().as_ref() {
                        let _ = etx.send(engine::EngineCmd::FetchConversations { limit: CONV_FETCH_LIMIT });
                    }
                }
            });
        }
        engine::EngineResult::SearchDropdown { query, contacts, messages } => {
            // Drop stale answers: the user has typed past this query, no
            // point updating the dropdown with results for a string they
            // no longer see.
            SHARED.with(|s| {
                if let Some(sh) = s.borrow().as_ref() {
                    if *sh.search_query_inflight.borrow() != query {
                        return;
                    }
                    let c_items = contact_items(&contacts);
                    let m_items = message_hits(&messages);
                    *sh.search_contacts.borrow_mut() = contacts;
                    *sh.search_messages.borrow_mut() = messages;
                    ui.set_search_contacts(ModelRc::new(VecModel::from(c_items)));
                    ui.set_search_messages(ModelRc::new(VecModel::from(m_items)));
                    ui.set_search_loading(false);
                }
            });
        }
        engine::EngineResult::AttachmentSaved(path) => {
            println!("attachment saved: {path} — opening");
            open_external(&path);
        }
        engine::EngineResult::Source { uid, raw } => {
            SHARED.with(|s| {
                if let Some(sh) = s.borrow().as_ref() {
                    let what = sh.pending_source_view.get();
                    sh.pending_source_view.set(0);
                    if what == 1 {
                        // Headers: a name/value table, in message order.
                        let rows: Vec<HeaderRow> = parse_headers(&raw)
                            .into_iter()
                            .map(|(name, value)| HeaderRow {
                                name: name.into(),
                                value: value.into(),
                            })
                            .collect();
                        ui.set_source_view_title(format!("Заголовки (id {uid})").into());
                        ui.set_source_view_headers(ModelRc::new(VecModel::from(rows)));
                        ui.set_source_view_is_headers(true);
                        ui.set_source_view_visible(true);
                    } else {
                        // Source: the raw RFC-822 bytes, verbatim, no processing.
                        set_source_text(ui, sh, format!("Исходник сообщения (id {uid})"), raw);
                    }
                }
            });
        }
        engine::EngineResult::Calendars(cals) => {
            println!("engine: {} calendars", cals.len());
            SHARED.with(|s| {
                if let Some(sh) = s.borrow().as_ref() {
                    {
                        let mut vis = sh.calendar_visible.borrow_mut();
                        for c in &cals {
                            vis.entry(c.id).or_insert(true);
                        }
                    }
                    *sh.calendars.borrow_mut() = cals;
                    apply_calendar_view(ui, sh);
                }
            });
        }
        engine::EngineResult::Contacts { query, list } => {
            // Drop stale answers: the search box has moved on since this
            // request went out.
            if ui.get_contacts_query().as_str() != query {
                return;
            }
            let rows = address_book_rows(&list);
            ui.set_address_book(ModelRc::new(VecModel::from(rows)));
            SHARED.with(|s| {
                if let Some(sh) = s.borrow().as_ref() {
                    *sh.address_book.borrow_mut() = list;
                }
            });
        }
        engine::EngineResult::CalendarEvents(events) => {
            println!("engine: {} calendar events", events.len());
            SHARED.with(|s| {
                if let Some(sh) = s.borrow().as_ref() {
                    let now_ms = chrono::Utc::now().timestamp_millis();
                    if let Some(c) = sh.cache.as_ref() {
                        // Hidden calendars don't get reminders (spec #9).
                        let vis = sh.calendar_visible.borrow();
                        let hidden = |cal_id: i64| !*vis.get(&cal_id).unwrap_or(&true);
                        reminders::seed(c, &events, &hidden, now_ms);
                    }
                    *sh.calendar_events.borrow_mut() = events;
                    apply_calendar_view(ui, sh);
                    // A reminder toast asked to open this event — its week
                    // is loaded now, so pop the detail card.
                    let pend = sh.pending_open_event.get();
                    if pend != 0 {
                        sh.pending_open_event.set(0);
                        if sh.calendar_events.borrow().iter().any(|e| e.id == pend) {
                            ui.invoke_event_clicked(pend as i32);
                        }
                    }
                }
            });
            ui.set_calendar_loading(false);
        }
        engine::EngineResult::SendFailed(e) => {
            eprintln!("engine: send failed: {e}");
            // Make the failure visible — the composer optimistically looked
            // like it sent, so without this the user only finds out by asking.
            // Translate the two common causes into plain Russian.
            let body = if e.contains("413") || e.to_lowercase().contains("too large") {
                "Вложения слишком большие. Уменьшите размер или пришлите ссылкой.".to_string()
            } else {
                format!("Причина: {}", e.chars().take(160).collect::<String>())
            };
            toast_window::show(
                2, // amber
                0, // not tied to a calendar event
                "Не удалось отправить письмо",
                &body,
                false,
                600,
                || {},
                || {},
                || {},
            );
        }
        engine::EngineResult::Error(e) => eprintln!("engine error: {e}"),
    }
}

/// Display name from a "Name <addr>" header (falls back to the address).
fn display_from(raw: &str) -> String {
    let r = raw.trim();
    if r.is_empty() {
        return "Новое письмо".into();
    }
    if let Some(i) = r.rfind('<') {
        let name = r[..i].trim().trim_matches('"').trim();
        if !name.is_empty() {
            return name.to_string();
        }
    }
    header_addr(r)
}

/// displays-index → sidebar model index (the transient compose row shifts
/// everything by one).
fn model_index(sh: &Shared, idx: usize) -> usize {
    if sh.pending_compose.borrow().is_some() {
        idx + 1
    } else {
        idx
    }
}

/// New-mail behaviour per the notification spec:
///   * reading that very dialog → silent append (autoscroll only when the
///     user is already at the bottom; otherwise just a row flash);
///   * anywhere else → tray dot + sound (per settings), badge bump + row
///     flash when the window shows the mail view;
///   * window hidden → clickable toast (sender + subject) that raises the
///     window, opens the dialog and scrolls to the message.
fn handle_new_mail(
    ui: &MainWindow,
    sh: &Rc<Shared>,
    folder: String,
    new_count: u32,
    from: String,
    subject: String,
    message_id: i64,
) {
    let from_addr = header_addr(&from);
    // Spec #1: a toast only for a real letter to read. Spam and iTIP/ics are
    // already dropped server-side; «своё» (from one of our own identities) is
    // filtered here — our outgoing mail syncing back is not a notification.
    if !from_addr.is_empty()
        && sh.identity_colors.borrow().contains_key(&from_addr.to_lowercase())
    {
        return;
    }
    let conv_idx = if from_addr.is_empty() {
        None
    } else {
        sh.convs.borrow().iter().position(|c| {
            c.counterparts.iter().any(|cp| cp.addr.eq_ignore_ascii_case(&from_addr))
        })
    };

    let visible = ui.window().is_visible();
    let in_mail_view = ui.get_view_mode() == 0;
    // «Я в этом диалоге?» — compare the sender against the OPEN conversation,
    // not the first list hit: pair-grouping can hold the same counterpart in
    // several rows, and list re-sorts make index equality a lottery.
    let is_current = visible
        && in_mail_view
        && !from_addr.is_empty()
        && sh.convs.borrow().get(sh.current.get()).is_some_and(|c| {
            c.counterparts.iter().any(|cp| cp.addr.eq_ignore_ascii_case(&from_addr))
        });

    if is_current {
        let at_bottom = {
            let vp_y = ui.get_chat_vp_y(); // negative when scrolled down
            let vp_h = ui.get_chat_vp_h();
            let view_h = ui.get_chat_view_h();
            vp_h <= view_h || (-vp_y) + view_h >= vp_h - 60.0
        };
        if at_bottom {
            // Follow the conversation: append + scroll to the end.
            sh.open_unread.borrow_mut().clear();
            sh.scroll_pending.set(true);
        } else {
            // Reading history above: don't yank the viewport — just flash.
            flash_sidebar_row(ui, model_index(sh, sh.current.get()));
        }
        if let Some(etx) = sh.engine_tx.borrow().as_ref() {
            // In the dialog = read: push \Seen right away, so the delta
            // refetch can't resurrect an unread pill for this row.
            // The stable RFC Message-ID for this row, if the conversation refs
            // carry it — prefer it over the volatile uid (db id) so the flag
            // lands even if the server reinserted the row.
            let row_mid = sh
                .convs
                .borrow()
                .get(sh.current.get())
                .and_then(|c| {
                    c.messages
                        .iter()
                        .find(|m| m.uid == message_id as u32)
                        .map(|m| m.message_id.clone())
                })
                .unwrap_or_default();
            if message_id > 0 {
                let _ = etx.send(engine::EngineCmd::SetFlags {
                    messages: vec![MessageRef {
                        folder: folder.clone(),
                        uid: message_id as u32,
                        message_id: row_mid.clone(),
                        seen: false,
                    }],
                    flags: "\\Seen".into(),
                    add: true,
                    account_key: sh.cur_account_key.borrow().clone(),
                });
            }
            let mut refs = sh
                .convs
                .borrow()
                .get(sh.current.get())
                .map(|c| c.messages.clone())
                .unwrap_or_default();
            if message_id > 0 && !refs.iter().any(|m| m.uid == message_id as u32) {
                refs.push(MessageRef { folder, uid: message_id as u32, message_id: row_mid, seen: true });
            }
            let _ = etx.send(engine::EngineCmd::FetchMessages {
                messages: refs,
                generation: sh.open_gen.get(),
                account_key: sh.cur_account_key.borrow().clone(),
            });
            let _ = etx.send(engine::EngineCmd::FetchConversations { limit: CONV_FETCH_LIMIT });
        }
        return;
    }

    // Not looking at that dialog: tray dot + sound.
    tray_set_dot(true);
    if ui.get_notify_sound_on() {
        toast::beep();
    }

    // Optimistic badge bump (data updates even when the calendar view
    // hides the sidebar); flash only when the row is actually on screen.
    if let Some(idx) = conv_idx {
        if let Some(c) = sh.convs.borrow_mut().get_mut(idx) {
            c.unread_count += new_count.max(1);
        }
        let displays = displays_from(&sh.convs.borrow(), &sh.identity_colors.borrow());
        *sh.displays.borrow_mut() = displays;
        refresh_sidebar(sh, ui);
        if visible && in_mail_view {
            flash_sidebar_row(ui, model_index(sh, idx));
        }
    }

    // Mail toast fires when the user can't see the new mail in-window: the
    // window is hidden (tray), OR the calendar view is up (mail sidebar not
    // visible). When the mail view is open and visible, the sidebar flash +
    // unread bump above is the notification — no toast.
    if !visible || !in_mail_view {
        // Spec #1: a batch collapses to a single «N новых» toast; a lone
        // message shows sender + subject.
        let (title, body) = if new_count > 1 {
            (format!("{new_count} новых"), String::new())
        } else {
            let b = if subject.is_empty() { "(без темы)".to_string() } else { subject.clone() };
            (display_from(&from), b)
        };
        let click_folder = folder.clone();
        let click_uid = message_id.max(0) as u32;
        let click_addr = from_addr.clone();
        toast::mail_toast(&title, &body, move || {
            let Some(weak) = UI_WEAK.get() else { return };
            let folder = click_folder.clone();
            let addr = click_addr.clone();
            let _ = weak.clone().upgrade_in_event_loop(move |ui| {
                raise_window(&ui);
                tray_set_dot(false);
                SHARED.with(|s| {
                    if let Some(sh) = s.borrow().as_ref() {
                        open_message_from_toast(&ui, sh, &folder, click_uid, &addr);
                    }
                });
            });
        });
    }

    if let Some(etx) = sh.engine_tx.borrow().as_ref() {
        let _ = etx.send(engine::EngineCmd::FetchConversations { limit: CONV_FETCH_LIMIT });
    }
}

/// Toast click: jump to the conversation (by the message id when we know
/// it, else by sender) and scroll to the message once its body renders.
fn open_message_from_toast(ui: &MainWindow, sh: &Shared, folder: &str, uid: u32, addr: &str) {
    if uid > 0 {
        *sh.pending_open_ref.borrow_mut() = Some((folder.to_string(), uid));
    }
    let idx = {
        let convs = sh.convs.borrow();
        convs
            .iter()
            .position(|c| uid > 0 && c.messages.iter().any(|m| m.uid == uid))
            .or_else(|| {
                if addr.is_empty() {
                    None
                } else {
                    convs.iter().position(|c| {
                        c.counterparts.iter().any(|cp| cp.addr.eq_ignore_ascii_case(addr))
                    })
                }
            })
    };
    ui.set_view_mode(0);
    if let Some(idx) = idx {
        ui.set_selected(idx as i32);
        apply_active_header(ui, sh, idx);
        open_conversation(ui, sh, idx);
        ui.set_sidebar_row_y(idx as f32 * 64.0);
        ui.set_sidebar_scroll_seq(ui.get_sidebar_scroll_seq() + 1);
    }
}

/// Handle a clicked link from a bubble (UI thread). Internal `ddmail-attach:`
/// links trigger an attachment download via the engine; everything else opens
/// in the system browser.
fn handle_link(_ui: &MainWindow, url: String) {
    if let Some(rest) = url.strip_prefix("ddmail-attach:") {
        // folder|uid|index|filename
        let parts: Vec<&str> = rest.splitn(4, '|').collect();
        if parts.len() == 4 {
            if let (Ok(uid), Ok(index)) = (parts[1].parse::<u32>(), parts[2].parse::<usize>()) {
                let folder = parts[0].to_string();
                let filename = parts[3].to_string();
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
    open_external(&url);
}

/// Max raw text fed to the source-viewer widget. Slint lays out the entire
/// string (no text virtualization), so a multi-MB source — a big message is
/// mostly base64 — would freeze rendering and selection. We show the first
/// chunk; «Копировать всё» still copies the full text from `source_view_full`.
const SOURCE_VIEW_MAX: usize = 128 * 1024;

/// Stash the full text and push a capped, render-safe slice into the viewer.
fn set_source_text(ui: &MainWindow, sh: &Shared, title: String, full: String) {
    let display = if full.len() > SOURCE_VIEW_MAX {
        let mut cut = SOURCE_VIEW_MAX;
        while cut > 0 && !full.is_char_boundary(cut) {
            cut -= 1;
        }
        format!(
            "{}\n\n[… показаны первые {} КБ из {} КБ — «Копировать всё» даёт полный исходник …]",
            &full[..cut],
            SOURCE_VIEW_MAX / 1024,
            full.len() / 1024
        )
    } else {
        full.clone()
    };
    sh.source_view_full.replace(full);
    // Reset the modal selection; the render job repopulates src_runs.
    sh.src_sel_moved.set(false);
    sh.src_sel_dragging.set(false);
    sh.src_runs.borrow_mut().clear();
    ui.set_source_view_title(title.into());
    ui.set_source_view_is_headers(false);
    ui.set_source_img_h(0.0);
    ui.set_source_selection_rects(ModelRc::new(VecModel::from(Vec::<SelRect>::new())));
    ui.set_source_view_visible(true);
    // The right-click menu may have taken focus; pull it back to the main key
    // sink so the modal's Ctrl+C/Escape (handled in kb) reach us.
    ui.invoke_grab_key_focus();
    // Render the (capped) text to a bitmap + word rects on the worker thread;
    // the modal then selects via the fast text-run layer.
    let _ = sh.tx.send(Job::RenderSource { text: display, width: SOURCE_RENDER_W });
}

/// Parse the RFC-822 header block into ordered (name, value) pairs.
///
/// Stops at the first blank line (end of headers). Folded values — RFC 5322
/// continuation lines starting with space/tab — are unfolded into the
/// preceding header's value. Order is preserved exactly as it appears in the
/// message. Values are returned raw (not RFC 2047-decoded); the table shows
/// the message as it is on the wire.
fn parse_headers(raw: &str) -> Vec<(String, String)> {
    let end = raw
        .find("\r\n\r\n")
        .or_else(|| raw.find("\n\n"))
        .unwrap_or(raw.len());
    let mut out: Vec<(String, String)> = Vec::new();
    for line in raw[..end].split('\n') {
        let line = line.strip_suffix('\r').unwrap_or(line);
        if line.is_empty() {
            continue;
        }
        if line.starts_with(' ') || line.starts_with('\t') {
            // Folded continuation — append to the current header's value.
            if let Some(last) = out.last_mut() {
                last.1.push(' ');
                last.1.push_str(line.trim_start());
            }
            continue;
        }
        match line.split_once(':') {
            Some((name, value)) => out.push((name.trim().to_string(), value.trim().to_string())),
            None => out.push((String::new(), line.trim().to_string())),
        }
    }
    out
}

/// Open a URL in the system default browser. Per-OS launcher — the previous
/// `cmd /C start` was Windows-only, so links silently did nothing on Linux.
fn open_external(url: &str) {
    #[cfg(target_os = "windows")]
    let mut cmd = {
        let mut c = std::process::Command::new("cmd");
        c.args(["/C", "start", "", url]);
        c
    };
    #[cfg(target_os = "macos")]
    let mut cmd = {
        let mut c = std::process::Command::new("open");
        c.arg(url);
        c
    };
    #[cfg(target_os = "linux")]
    let mut cmd = {
        let mut c = std::process::Command::new("xdg-open");
        c.arg(url);
        c
    };
    if let Err(e) = cmd.spawn() {
        eprintln!("open_external: failed to launch browser for {url}: {e}");
    }
}
