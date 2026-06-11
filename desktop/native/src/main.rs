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
#[cfg(windows)]
mod tray;
#[cfg(target_os = "linux")]
#[path = "render_webkit.rs"]
mod render;
#[cfg(windows)]
#[path = "render_webview2.rs"]
mod render;
mod sanitize;
mod texture_cache;
mod window_state;

use std::cell::{Cell, RefCell};
use std::collections::{HashMap, HashSet};
use std::rc::Rc;
use std::sync::atomic::{AtomicU64, Ordering};
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
const WORKDAY_START: i32 = 9;
const WORKDAY_END: i32 = 18;

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
    bubble_template(b.is_outgoing, &format!("{inner}{}", attachment_chips(b)))
}

/// Text-only bubble — the fallback we render when WebKit chokes on an
/// HTML body (timeout or empty paint). Preserves linebreaks via
/// `white-space: pre-wrap`, and still appends attachment chips.
fn build_text_only_html(b: &MessageBody) -> String {
    let escaped = html_escape(b.text.as_deref().unwrap_or(""));
    let inner = format!("<div style=\"white-space:pre-wrap\">{escaped}</div>");
    bubble_template(b.is_outgoing, &format!("{inner}{}", attachment_chips(b)))
}

/// Attachment-chip HTML appended below every bubble's main body.
/// Clickable via the link hit-test (currently disabled after the
/// WebKit migration — see render.rs) using an internal
/// `ddmail-attach:folder|uid|index|filename` scheme decoded on the
/// UI thread.
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

fn bubble_template(is_outgoing: bool, inner: &str) -> String {
    let side = if is_outgoing { "out" } else { "in" };
    let bg = if is_outgoing { "#cfe6ff" } else { "#ffffff" };
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
        </style></head>
        <body><div class="row {side}"><div class="bubble">{inner}</div></div></body></html>"#
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
}

/// Everything the render worker knows about a row besides its bitmap —
/// shipped to the UI thread to fill RowItem (context-menu data included).
struct RowMeta {
    h: f32,
    has_html: bool,
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
    /// email(lowercase) → пастельный цвет айдентики (подкраска строк
    /// сайдбара по received_by). Обновляется при каждом списке диалогов.
    identity_colors: RefCell<HashMap<String, String>>,
    /// UI-thread copy of the per-row link rects (CSS px, bubble-relative) —
    /// drives the pointer cursor over links. The render worker keeps its
    /// own copy for click hit-testing; this one answers synchronous
    /// hover-binding queries without a worker round-trip.
    row_links: RefCell<Vec<Vec<render_common::LinkRect>>>,
    /// What the shared confirmation modal confirms: 1 = удалить диалог,
    /// 2 = спам (blacklist + purge отправителя).
    confirm_mode: Cell<u8>,
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
    /// Event being edited (0 in create mode).
    editing_event_id: Cell<i64>,
    /// Writable calendar ids, parallel to the edit-form's ComboBox model.
    edit_cal_ids: RefCell<Vec<i64>>,
    /// Files staged for the next outgoing message, picked via the composer's
    /// attach button. Parallel to the `composer-attachments` Slint model
    /// (which holds just the basenames). Cleared once a message is staged.
    compose_attachments: RefCell<Vec<std::path::PathBuf>>,
}

thread_local! {
    /// Set once on the UI thread so engine-result closures (posted via
    /// invoke_from_event_loop, which must be Send + 'static and can't capture
    /// the Rc) can reach the shared state.
    static SHARED: RefCell<Option<Rc<Shared>>> = const { RefCell::new(None) };
}

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
    *sh.open_unread.borrow_mut() =
        unread.iter().map(|m| (m.folder.clone(), m.uid)).collect();
    sh.scroll_pending.set(true);

    if let (Some(cache), key) = (&sh.cache, &sh.key) {
        let t_load_start = Instant::now();
        let bodies = cache.load_message_bodies(key, &c.messages).unwrap_or_default();
        let load_ms = t_load_start.elapsed().as_millis();
        if !bodies.is_empty() {
            *sh.current_msgs.borrow_mut() =
                bodies.iter().map(|b| MessageRef { folder: b.folder.clone(), uid: b.uid, seen: true }).collect();
            *sh.current_bodies.borrow_mut() = bodies.clone();
            println!(
                "[perf] open_conversation idx={idx} label={conv_label:?} \
                 messages={msg_count} bodies={} cache_load={load_ms}ms enqueue@{:?}",
                bodies.len(),
                t0.elapsed()
            );
            let scroll = take_scroll_target(sh, &bodies);
            send_render_job(sh, bodies, scroll);
        } else {
            println!(
                "[perf] open_conversation idx={idx} label={conv_label:?} \
                 messages={msg_count} cache_miss (no cached bodies) cache_load={load_ms}ms"
            );
        }
    }
    if let Some(etx) = sh.engine_tx.borrow().as_ref() {
        let _ = etx.send(engine::EngineCmd::FetchMessages { messages: c.messages.clone(), generation });
        // Opening a conversation reads it: push \Seen for everything that
        // was unread. The scroll anchor above is already snapshotted.
        if !unread.is_empty() {
            let _ = etx.send(engine::EngineCmd::SetFlags {
                messages: unread,
                flags: "\\Seen".into(),
                add: true,
            });
        }
    }
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

/// Consume the pending post-open scroll: row index of the first unread
/// body (top-aligned), or -1 for "scroll to the end" when everything was
/// already read. None when this render isn't the first one after open.
fn take_scroll_target(sh: &Shared, bodies: &[MessageBody]) -> Option<i32> {
    if !sh.scroll_pending.get() {
        return None;
    }
    sh.scroll_pending.set(false);
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
    }
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
        panel_collapsed: ui.get_calendar_panel_collapsed(),
        workdays_only: ui.get_workdays_only(),
        show_non_work_hours: ui.get_show_non_work_hours(),
    });
}

fn apply_calendar_view(ui: &MainWindow, sh: &Shared) {
    use chrono::{Datelike, Duration, NaiveDate};
    let workdays = ui.get_workdays_only();
    let day_count = if workdays { 5 } else { 7 } as i32;
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
    ui.set_hour_height(48.0);
    // Work-hours view spans one hour either side of the workday (9–18 by
    // default → 8–19); the toggle expands to the full 0–24.
    let non_work = ui.get_show_non_work_hours();
    ui.set_hour_start(if non_work { 0 } else { WORKDAY_START - 1 });
    ui.set_hour_end(if non_work { 24 } else { WORKDAY_END + 1 });

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

    // Place event blocks. Recurring masters are expanded into the visible
    // week here (the server sends the master + its EXDATEs and leaves the
    // expansion to us); an occurrence that crosses midnight is split into
    // one block per day it touches. All-day events are routed out of the
    // hour grid into the band above it.
    let day_ms: i64 = 24 * 60 * 60 * 1000;
    // Week window in LOCAL time: the grid's day columns are local days, so
    // the window must start at local Monday midnight, not UTC midnight —
    // otherwise every event shifts by the UTC offset (+3 here) and
    // midnight-adjacent ones land in the wrong column.
    let week_start_ms = local_midnight_ms(monday).unwrap_or(week_days * day_ms);
    let week_end_ms = week_start_ms + day_count as i64 * day_ms;
    let hour_height: f32 = ui.get_hour_height();
    let hour_start = ui.get_hour_start();
    let hour_end = ui.get_hour_end();
    let visible_top_ms = hour_start as i64 * 60 * 60 * 1000;
    let visible_bottom_ms = hour_end as i64 * 60 * 60 * 1000;
    let (mut blocks, all_day_blocks, all_day_rows): (Vec<EventBlock>, Vec<AllDayBlock>, i32) = {
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
        let to_px = |ms: i64| -> f32 {
            let hours = (ms - visible_top_ms) as f32 / (60.0 * 60.0 * 1000.0);
            hours * hour_height
        };
        let fmt_hm = |abs_ms: i64| -> String {
            use chrono::{Local, TimeZone, Timelike};
            match Local.timestamp_millis_opt(abs_ms).single() {
                Some(d) => format!("{:02}:{:02}", d.hour(), d.minute()),
                None => "??:??".into(),
            }
        };

        let mut blocks: Vec<EventBlock> = Vec::new();
        let mut all_day_blocks: Vec<AllDayBlock> = Vec::new();
        // How many all-day chips already sit in each day column, so
        // overlapping all-day events stack instead of overprinting.
        let mut all_day_fill = vec![0i32; day_count.max(0) as usize];

        // "My" addresses for the unanswered-meeting check (Tauri's
        // isUnansweredMeeting): account key + every known identity.
        let idents = sh.identity_colors.borrow();
        let me_key = sh.key.to_lowercase();

        for e in events.iter() {
            if !*visibility.get(&e.calendar_id).unwrap_or(&true) {
                continue;
            }
            let color = color_for(e.calendar_id);
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
            // Diagnostics: a recurring master that expands to nothing this
            // week is either legitimately out of window or an expand() bug
            // — log it so missing-event reports are checkable from the log.
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
                // Day range the occurrence touches. `end_ms - 1` so an event
                // ending exactly on midnight doesn't bleed into the next day.
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
                for day in first.max(0)..=last.min(day_count - 1) {
                    let day_start_ms = week_start_ms + day as i64 * day_ms;
                    let seg_start = (o.start_ms - day_start_ms).max(0);
                    let seg_end = (o.end_ms - day_start_ms).min(day_ms);
                    let top_ms = seg_start.max(visible_top_ms);
                    let bot_ms = seg_end.min(visible_bottom_ms);
                    if bot_ms <= top_ms {
                        continue;
                    }
                    let top = to_px(top_ms);
                    let h = (to_px(bot_ms) - top).max(18.0);
                    blocks.push(EventBlock {
                        id: e.id as i32,
                        day,
                        top,
                        h,
                        color,
                        title: if e.summary.is_empty() {
                            "(без названия)".into()
                        } else {
                            e.summary.clone().into()
                        },
                        time: format!("{} – {}", fmt_hm(o.start_ms), fmt_hm(o.end_ms)).into(),
                        all_day: false,
                        xf: 0.0,
                        wf: 1.0,
                        count: att_count,
                        tentative,
                    });
                }
            }
        }
        let rows = *all_day_fill.iter().max().unwrap_or(&0);
        println!(
            "[cal] layout: events_total={} blocks={} all_day={} week_start_ms={} \
             window=[{}..{}) sample={:?}",
            events.len(),
            blocks.len(),
            all_day_blocks.len(),
            week_start_ms,
            week_start_ms,
            week_end_ms,
            events
                .iter()
                .take(3)
                .map(|e| (e.id, e.calendar_id, e.dtstart, e.all_day, e.rrule.clone()))
                .collect::<Vec<_>>()
        );
        (blocks, all_day_blocks, rows)
    };
    assign_overlap_lanes(&mut blocks, day_count);
    ui.set_events(slint::ModelRc::new(slint::VecModel::from(blocks)));
    ui.set_all_day_events(slint::ModelRc::new(slint::VecModel::from(all_day_blocks)));
    ui.set_all_day_rows(all_day_rows);
}

/// Re-fire FetchCalendarEvents for the currently displayed week. Also
/// flips `calendar-loading` on so the topbar shows a "Загрузка…" pill
/// until the result lands.
fn refetch_calendar_events(ui: &MainWindow, sh: &Shared) {
    let workdays = ui.get_workdays_only();
    let day_count = if workdays { 5 } else { 7 };
    let (from_ms, to_ms) = week_range_ms(sh.calendar_week_start_days.get(), day_count);
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
fn fill_writable_calendars(ui: &MainWindow, sh: &Shared) -> Vec<i64> {
    let cals = sh.calendars.borrow();
    let mut ids = Vec::new();
    let mut names: Vec<slint::SharedString> = Vec::new();
    for c in cals.iter().filter(|c| c.can_write) {
        ids.push(c.id);
        names.push(c.name.clone().into());
    }
    ui.set_edit_calendars(slint::ModelRc::new(slint::VecModel::from(names)));
    ids
}

/// Open the create form (blank, default now → +1h, first writable calendar).
fn open_create_form(ui: &MainWindow, sh: &Shared) {
    let ids = fill_writable_calendars(ui, sh);
    *sh.edit_cal_ids.borrow_mut() = ids;
    sh.editing_event_id.set(0);
    let now = chrono::Local::now().timestamp_millis();
    ui.set_edit_is_create(true);
    ui.set_edit_title("".into());
    ui.set_edit_all_day(false);
    ui.set_edit_start(fmt_form(now, false).into());
    ui.set_edit_end(fmt_form(now + 3_600_000, false).into());
    ui.set_edit_location("".into());
    ui.set_edit_description("".into());
    ui.set_edit_calendar_idx(0);
    ui.set_edit_visible(true);
}

/// Open the edit form populated from an existing event.
fn open_edit_form(ui: &MainWindow, sh: &Shared, ev: &ddmail_core::types::DesktopCalendarEvent) {
    let ids = fill_writable_calendars(ui, sh);
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
        let _ = etx.send(engine::EngineCmd::PatchEvent { event_id: editing, body });
    }
    ui.set_edit_visible(false);
}

/// Seed the calendar view's read-only state from the current toggles
/// (workdays-only, non-work-hours). Day labels are filled in based on
/// the current week; events / calendars stay empty until the engine
/// produces them.
fn apply_calendar_defaults(ui: &MainWindow) {
    use chrono::{Datelike, Duration, Local};
    let workdays = ui.get_workdays_only();
    let day_count = if workdays { 5 } else { 7 } as i32;
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
    let title = {
        use chrono::Datelike as _;
        const MONTHS: [&str; 12] = [
            "Январь", "Февраль", "Март", "Апрель", "Май", "Июнь",
            "Июль", "Август", "Сентябрь", "Октябрь", "Ноябрь", "Декабрь",
        ];
        format!("{} {}", MONTHS[(monday.month() - 1) as usize], monday.year())
    };
    ui.set_week_title(title.into());
    ui.set_hour_height(48.0);
    if ui.get_hour_end() == 0 {
        ui.set_hour_start(8);
        ui.set_hour_end(18);
    }
    // Empty models so the for-loops don't trip on undefined.
    ui.set_calendars(slint::ModelRc::new(slint::VecModel::from(Vec::<CalendarItem>::new())));
    ui.set_events(slint::ModelRc::new(slint::VecModel::from(Vec::<EventBlock>::new())));
}

fn main() {
    // Single-instance guard: a second launch exits instead of opening a
    // duplicate window. (Focusing the existing window needs IPC — TODO.)
    let _instance = single_instance::SingleInstance::new("ddmail-native-single").ok();
    if let Some(inst) = &_instance {
        if !inst.is_single() {
            eprintln!("ddmail is already running");
            return;
        }
    }

    let ui = MainWindow::new().unwrap();

    // Restore the persisted window geometry + sidebar width before the
    // first paint, so the UI opens exactly where the user left it instead
    // of at the hard-coded defaults.
    let saved = window_state::load();
    ui.window().set_size(slint::LogicalSize::new(saved.width, saved.height));
    if saved.has_position() {
        ui.window().set_position(slint::PhysicalPosition::new(saved.x, saved.y));
    }
    ui.set_sidebar_width(saved.sidebar_width);

    // Restore persisted calendar-view preferences (panel state + view
    // toggles now; the per-calendar maps are seeded into Shared below).
    let cal_set = calendar_settings::load();
    ui.set_calendar_panel_collapsed(cal_set.panel_collapsed);
    ui.set_workdays_only(cal_set.workdays_only);
    ui.set_show_non_work_hours(cal_set.show_non_work_hours);
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
    let last_geom = std::cell::Cell::new((0i32, 0i32, 0u32, 0u32, 0.0f32));
    let geometry_saver = slint::Timer::default();
    geometry_saver.start(
        slint::TimerMode::Repeated,
        std::time::Duration::from_millis(500),
        move || {
            let Some(ui) = ui_weak_geom.upgrade() else { return };
            let win = ui.window();
            if win.is_maximized() || win.is_minimized() {
                return;
            }
            let pos = win.position();
            let size = win.size();
            let sidebar = ui.get_sidebar_width();
            let snapshot = (pos.x, pos.y, size.width, size.height, sidebar);
            if last_geom.get() == snapshot {
                return;
            }
            last_geom.set(snapshot);
            let scale = win.scale_factor().max(0.1);
            window_state::save(&window_state::WindowState {
                width: size.width as f32 / scale,
                height: size.height as f32 / scale,
                sidebar_width: sidebar,
                x: pos.x,
                y: pos.y,
            });
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
                (SharedPixelBuffer<Rgba8Pixel>, f32, Vec<render_common::LinkRect>)> = HashMap::new();
            // FIFO insertion order for the RAM cache: bitmaps are megabytes
            // each, so cap the entry count and drop the oldest (the disk
            // layer below still has them — eviction only costs a PNG decode).
            const RAM_CAP: usize = 400;
            let mut ram_order: Vec<(String, u32, u32, u64, u8, u64)> = Vec::new();
            // Per-row clickable link rects (CSS px), parallel to the rendered
            // rows. Renderer-agnostic; the click is a pure point-in-rect test.
            let mut row_links: Vec<Vec<render_common::LinkRect>> = Vec::new();
            loop {
                let job = {
                    let lock = rx.lock().unwrap();
                    lock.recv()
                };
                let Ok(job) = job else { break };
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
                            let force_text = mode == 1 && has_html;
                            // Content fingerprint: bodies are mostly immutable,
                            // but cid:→data: healing rewrites the HTML — the
                            // texture must miss when the content changed.
                            let fp = texture_cache::fnv1a(body.html.as_deref().unwrap_or(""));
                            let key = (body.folder.clone(), body.uid, width, policy_gen, mode, fp);
                            let mut remember =
                                |key: &(String, u32, u32, u64, u8, u64),
                                 entry: &(SharedPixelBuffer<Rgba8Pixel>, f32, Vec<render_common::LinkRect>),
                                 body_cache: &mut HashMap<_, _>,
                                 ram_order: &mut Vec<(String, u32, u32, u64, u8, u64)>| {
                                    body_cache.insert(key.clone(), entry.clone());
                                    ram_order.push(key.clone());
                                    if ram_order.len() > RAM_CAP {
                                        let oldest = ram_order.remove(0);
                                        body_cache.remove(&oldest);
                                    }
                                };
                            let (buf, h, links) = if let Some(cached) = body_cache.get(&key) {
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
                                let entry = (buf, de.h, de.links);
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
                                let mut result = engine.render_one(&html, width);
                                let text_available = body
                                    .text
                                    .as_deref()
                                    .map(|s| !s.trim().is_empty())
                                    .unwrap_or(false);
                                if !result.successful() && text_available && !force_text {
                                    fallback_used += 1;
                                    let text_html = build_text_only_html(body);
                                    result = engine.render_one(&text_html, width);
                                }
                                render_ms_total += t_r.elapsed().as_millis();
                                let bitmap = result.bitmap;
                                let t_p = Instant::now();
                                let buf = SharedPixelBuffer::<Rgba8Pixel>::clone_from_slice(
                                    &bitmap.rgba, bitmap.width, bitmap.height,
                                );
                                pack_ms_total += t_p.elapsed().as_millis();
                                let entry = (buf, bitmap.height as f32, result.links);
                                println!(
                                    "[perf]   body uid={} h={}px painted={} ready={} links={}",
                                    body.uid, bitmap.height, result.painted_height,
                                    result.view_ready, entry.2.len()
                                );
                                if let Some(t) = tex_disk.as_ref() {
                                    t.store(
                                        &body.folder, body.uid, width, policy_gen, mode, fp,
                                        &bitmap.rgba, bitmap.width, bitmap.height,
                                        entry.1, &entry.2,
                                    );
                                }
                                remember(&key, &entry, &mut body_cache, &mut ram_order);
                                entry
                            };
                            // Context-menu data: per-sender / per-host
                            // checkbox states reflect the policy this job
                            // rendered under (a toggle re-renders anyway).
                            let sender_lc = body.from_addr.to_lowercase();
                            let (media_host, script_host) = sanitize::first_external_hosts(
                                body.html.as_deref().unwrap_or(""),
                            );
                            packs.push((buf, RowMeta {
                                h,
                                has_html,
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
                        // UI-thread link rects for the pointer cursor (the
                        // worker keeps its own copy for click hit-testing).
                        let links_for_ui = row_links.clone();
                        let _ = ui_weak.upgrade_in_event_loop(move |ui| {
                            // Wrap each SharedPixelBuffer in an Image —
                            // this is cheap (refcount bump, no memcpy)
                            // and is the only step that has to run on
                            // the UI thread.
                            SHARED.with(|s| {
                                if let Some(sh) = s.borrow().as_ref() {
                                    *sh.row_links.borrow_mut() = links_for_ui;
                                }
                            });
                            let rows: Vec<RowItem> = packs
                                .into_iter()
                                .map(|(buf, m)| RowItem {
                                    img: Image::from_rgba8(buf),
                                    h: m.h,
                                    has_html: m.has_html,
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
                }
            }
        });
    }

    // ----- Shared state -----
    let (cache, key, init_convs) = match account {
        Some((c, k, convs)) => (Some(c), k, convs),
        None => (None, String::new(), Vec::new()),
    };
    let loaded_policy = policy::load();
    let shared = Rc::new(Shared {
        cache,
        key,
        convs: RefCell::new(init_convs),
        displays: RefCell::new(displays.clone()),
        avatars: RefCell::new(HashMap::new()),
        current_msgs: RefCell::new(Vec::new()),
        current_bodies: RefCell::new(Vec::new()),
        open_gen: Cell::new(0),
        open_unread: RefCell::new(HashSet::new()),
        scroll_pending: Cell::new(false),
        body_view_text: RefCell::new(HashSet::new()),
        pending_forward: RefCell::new(None),
        pending_source_view: Cell::new(0),
        identity_colors: RefCell::new(startup_ident_colors),
        row_links: RefCell::new(Vec::new()),
        confirm_mode: Cell::new(0),
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
        editing_event_id: Cell::new(0),
        edit_cal_ids: RefCell::new(Vec::new()),
        compose_attachments: RefCell::new(Vec::new()),
    });
    SHARED.with(|s| *s.borrow_mut() = Some(shared.clone()));
    sync_media_globals(&ui, &shared.policy.borrow());

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
                .map(|b| MessageRef { folder: b.folder.clone(), uid: b.uid, seen: true })
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
            let scroll = take_scroll_target(&shared, &bodies);
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
        let live_cfg = engine::AccountConfig::load();
        let cfg = live_cfg.clone().unwrap_or_else(|| {
            // No live config: reconstruct just enough so that the engine's
            // `key = cfg.account_key()` matches what's already in the cache
            // (`{username}@{host}` format). Anything provider-touching
            // stays empty / unreachable; only cache-backed reads make sense.
            let key = shared.key.clone();
            let (username, host) = key
                .rsplit_once('@')
                .map(|(u, h)| (u.to_string(), h.to_string()))
                .unwrap_or_else(|| (key.clone(), String::new()));
            engine::AccountConfig {
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
            }
        });
        let ui_weak_eng = ui.as_weak();
        let etx = engine::spawn(cfg, cache, move |res| {
            let _ = ui_weak_eng.upgrade_in_event_loop(move |ui| handle_engine_result(&ui, res));
        });
        if live_cfg.is_some() {
            let _ = etx.send(engine::EngineCmd::FetchConversations { limit: 200 });
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
                });
            } else {
                println!("delete conversation {conv_id} ({} messages)", refs.len());
                let _ = etx.send(engine::EngineCmd::Delete { messages: refs });
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
    ui.on_hit_test(move |row, x, y| {
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
                    attachments: attachments.clone(),
                    forward_attachments: Some(MessageRef {
                        folder: orig.folder.clone(),
                        uid: orig.uid,
                        seen: true,
                    }),
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
                    attachments: attachments.clone(),
                    forward_attachments: None,
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
                    attachments: attachments.clone(),
                    forward_attachments: None,
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
                attachments,
                forward_attachments: None,
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
                });
            }
            return;
        }
        // «Исходник тела» — the HTML part is already in memory.
        if action == "show-body-source" {
            let body_opt = sh_act.current_bodies.borrow().get(row).cloned();
            let Some(body) = body_opt else { return };
            if let Some(ui) = ui_weak_act.upgrade() {
                ui.set_source_view_title(format!("Исходник тела — {}", body.subject).into());
                ui.set_source_view_text(body.html.unwrap_or_default().into());
                ui.set_source_view_visible(true);
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
                let _ = etx.send(engine::EngineCmd::Delete { messages: vec![msg] });
            }
            "read" => {
                let _ = etx.send(engine::EngineCmd::SetFlags {
                    messages: vec![msg],
                    flags: "\\Seen".into(),
                    add: true,
                });
            }
            "unread" => {
                let _ = etx.send(engine::EngineCmd::SetFlags {
                    messages: vec![msg],
                    flags: "\\Seen".into(),
                    add: false,
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
            }
        }
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

    let ui_weak_wd = ui.as_weak();
    let sh_wd = shared.clone();
    ui.on_calendar_toggle_workdays(move || {
        if let Some(ui) = ui_weak_wd.upgrade() {
            ui.set_workdays_only(!ui.get_workdays_only());
            apply_calendar_view(&ui, &sh_wd);
            refetch_calendar_events(&ui, &sh_wd);
            save_calendar_settings(&ui, &sh_wd);
        }
    });
    let ui_weak_nw = ui.as_weak();
    let sh_nw = shared.clone();
    ui.on_calendar_toggle_non_work_hours(move || {
        if let Some(ui) = ui_weak_nw.upgrade() {
            ui.set_show_non_work_hours(!ui.get_show_non_work_hours());
            apply_calendar_view(&ui, &sh_nw);
            save_calendar_settings(&ui, &sh_nw);
        }
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

        let date_of = |ms: i64| {
            Local
                .timestamp_millis_opt(ms)
                .single()
                .map(|d| format!("{:02}.{:02}.{}", d.day(), d.month(), d.year()))
                .unwrap_or_default()
        };
        let dt = |ms: i64| {
            Local
                .timestamp_millis_opt(ms)
                .single()
                .map(|d| format!("{:02}.{:02} {:02}:{:02}", d.day(), d.month(), d.hour(), d.minute()))
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
            format!("{} – {}", dt(ev.dtstart), tm(end))
        } else {
            dt(ev.dtstart)
        };

        let organizer = match (ev.organizer_name.is_empty(), ev.organizer_email.is_empty()) {
            (true, true) => String::new(),
            (true, false) => ev.organizer_email.clone(),
            (false, true) => ev.organizer_name.clone(),
            (false, false) => format!("{} <{}>", ev.organizer_name, ev.organizer_email),
        };
        let attendees = ev
            .attendees
            .iter()
            .map(|a| {
                let n = if a.name.is_empty() { a.email.clone() } else { a.name.clone() };
                if a.partstat.is_empty() { n } else { format!("{n} ({})", a.partstat) }
            })
            .collect::<Vec<_>>()
            .join(", ");

        ui.set_detail_title(
            if ev.summary.is_empty() { "(без названия)".into() } else { ev.summary.clone() }.into(),
        );
        ui.set_detail_when(when.into());
        ui.set_detail_location(ev.location.clone().into());
        ui.set_detail_organizer(organizer.into());
        ui.set_detail_attendees(attendees.into());
        ui.set_detail_description(ev.description.clone().into());
        ui.set_detail_event_id(ev.id as i32);
        ui.set_detail_visible(true);
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
            for row in due {
                notify::notify(
                    &format!("\u{23f0} {}", row.summary),
                    &reminders::body_for(&row, now_ms),
                );
            }
        },
    );

    // System tray (Windows): left-click / "Открыть" re-shows the window,
    // "Выход" quits. Kept alive until the event loop ends.
    #[cfg(windows)]
    let _tray = {
        let ui_open = ui.as_weak();
        tray::setup(
            move || {
                if let Some(ui) = ui_open.upgrade() {
                    let _ = ui.show();
                }
            },
            || slint::quit_event_loop().unwrap(),
        )
    };

    ui.run().unwrap();
}

/// Apply an engine result on the UI thread (reaches Shared via the thread-local).
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
                    // conversations — refresh the row-tint map from cache.
                    if let Some(cache) = &sh.cache {
                        *sh.identity_colors.borrow_mut() = identity_color_map(cache, &sh.key);
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
                        .map(|b| MessageRef { folder: b.folder.clone(), uid: b.uid, seen: true })
                        .collect();
                    *sh.current_bodies.borrow_mut() = bodies.clone();
                    // First render after open may be THIS one (conversation
                    // wasn't cached) — apply the pending scroll if so.
                    let scroll = take_scroll_target(sh, &bodies);
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
            SHARED.with(|s| {
                if let Some(sh) = s.borrow().as_ref() {
                    if let Some(etx) = sh.engine_tx.borrow().as_ref() {
                        let _ = etx.send(engine::EngineCmd::FetchConversations { limit: 200 });
                        // Reopen current conversation to refresh its bodies.
                        let cur = sh.current.get();
                        if let Some(c) = sh.convs.borrow().get(cur) {
                            let _ = etx.send(engine::EngineCmd::FetchMessages {
                                messages: c.messages.clone(),
                                generation: sh.open_gen.get(),
                            });
                        }
                    }
                }
            });
        }
        engine::EngineResult::Event(ev) => {
            use ddmail_core::event::EngineEvent;
            match ev {
                EngineEvent::NewMail { folder, count } => {
                    println!("engine event: new mail in {folder} (+{count}) — refetching");
                    let body = if count > 1 {
                        format!("{count} новых писем в {folder}")
                    } else {
                        format!("Новое письмо в {folder}")
                    };
                    notify::notify("ddmail", &body);
                    SHARED.with(|s| {
                        if let Some(sh) = s.borrow().as_ref() {
                            if let Some(etx) = sh.engine_tx.borrow().as_ref() {
                                let _ = etx.send(engine::EngineCmd::FetchConversations { limit: 200 });
                            }
                        }
                    });
                }
                EngineEvent::ConnectionState { state, message } => {
                    println!("engine connection: {state} {}", message.unwrap_or_default());
                }
                EngineEvent::CalendarUpdated { calendar_id } => {
                    println!("engine event: calendar {calendar_id} updated");
                }
                EngineEvent::TokenRefreshed { account_id, .. } => {
                    println!("engine event: token refreshed for {account_id}");
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
                        let _ = etx.send(engine::EngineCmd::FetchConversations { limit: 200 });
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
            let _ = std::process::Command::new("cmd")
                .args(["/C", "start", "", &path])
                .spawn();
        }
        engine::EngineResult::Source { uid, raw } => {
            SHARED.with(|s| {
                if let Some(sh) = s.borrow().as_ref() {
                    let what = sh.pending_source_view.get();
                    sh.pending_source_view.set(0);
                    let (title, text) = if what == 1 {
                        // Header block = up to the first blank line.
                        let end = raw
                            .find("\r\n\r\n")
                            .or_else(|| raw.find("\n\n"))
                            .unwrap_or(raw.len());
                        (format!("Заголовки (id {uid})"), raw[..end].to_string())
                    } else {
                        (format!("Исходник сообщения (id {uid})"), raw)
                    };
                    ui.set_source_view_title(title.into());
                    ui.set_source_view_text(text.into());
                    ui.set_source_view_visible(true);
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
        engine::EngineResult::CalendarEvents(events) => {
            println!("engine: {} calendar events", events.len());
            SHARED.with(|s| {
                if let Some(sh) = s.borrow().as_ref() {
                    let now_ms = chrono::Utc::now().timestamp_millis();
                    if let Some(c) = sh.cache.as_ref() {
                        reminders::seed(c, &events, now_ms);
                    }
                    *sh.calendar_events.borrow_mut() = events;
                    apply_calendar_view(ui, sh);
                }
            });
            ui.set_calendar_loading(false);
        }
        engine::EngineResult::Error(e) => eprintln!("engine error: {e}"),
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
    let _ = std::process::Command::new("cmd")
        .args(["/C", "start", "", &url])
        .spawn();
}
