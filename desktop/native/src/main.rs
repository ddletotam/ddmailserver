//! ddmail-native — Slint shell + Ultralight (WebKit) body rendering.
//! Sidebar = real conversations from the desktop cache; selecting one renders
//! its real message bodies as Ultralight bitmaps composited as Slint images.

slint::include_modules!();

mod engine;
mod policy;
mod render_common;
#[cfg(target_os = "linux")]
#[path = "render_webkit.rs"]
mod render;
#[cfg(windows)]
#[path = "render_webview2.rs"]
mod render;
mod sanitize;
mod window_state;

use std::cell::{Cell, RefCell};
use std::collections::HashMap;
use std::rc::Rc;
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

const DEFAULT_WIDTH: u32 = 740;

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
}

fn cache_db_path() -> Option<std::path::PathBuf> {
    // Pick the per-OS location of the (Tauri-era) cache dir so the new
    // client reads the same cache.db the user already has.
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

fn displays_from(convs: &[Conversation]) -> Vec<Disp> {
    convs
        .iter()
        .enumerate()
        .map(|(i, c)| {
            let name = conv_name(c);
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
    ui.set_reply_ribbon_visible(false);
    ui.set_reply_ribbon_from("".into());
    ui.set_reply_ribbon_preview("".into());
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
            let sanitized = sanitize::sanitize_email_html(h);
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
    },
    HitTest { row: usize, x: f32, y: f32 },
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
    /// Latest events from the engine, kept so toggling visibility /
    /// changing hour-range can re-layout without a server round-trip.
    calendar_events: RefCell<Vec<ddmail_core::types::DesktopCalendarEvent>>,
    /// First day of the currently displayed week (Monday) in local
    /// time, as days since the unix epoch. Stored as i64 so the
    /// timezone-conversion math is straightforward.
    calendar_week_start_days: Cell<i64>,
}

thread_local! {
    /// Set once on the UI thread so engine-result closures (posted via
    /// invoke_from_event_loop, which must be Send + 'static and can't capture
    /// the Rc) can reach the shared state.
    static SHARED: RefCell<Option<Rc<Shared>>> = const { RefCell::new(None) };
}

/// Open a conversation by index: show cached bodies immediately, and (if a live
/// engine is running) fire a background fetch to refresh them.
fn open_conversation(sh: &Shared, idx: usize) {
    let t0 = Instant::now();
    sh.current.set(idx);
    let convs = sh.convs.borrow();
    let Some(c) = convs.get(idx) else { return };
    let conv_label = c.label.clone();
    let msg_count = c.messages.len();
    if let (Some(cache), key) = (&sh.cache, &sh.key) {
        let t_load_start = Instant::now();
        let bodies = cache.load_message_bodies(key, &c.messages).unwrap_or_default();
        let load_ms = t_load_start.elapsed().as_millis();
        if !bodies.is_empty() {
            *sh.current_msgs.borrow_mut() =
                bodies.iter().map(|b| MessageRef { folder: b.folder.clone(), uid: b.uid }).collect();
            println!(
                "[perf] open_conversation idx={idx} label={conv_label:?} \
                 messages={msg_count} bodies={} cache_load={load_ms}ms enqueue@{:?}",
                bodies.len(),
                t0.elapsed()
            );
            let policy = sh.policy.borrow().clone();
            let policy_gen = sh.policy_gen.get();
            let _ = sh.tx.send(Job::SetConversation {
                bodies,
                width: sh.width.get(),
                policy,
                policy_gen,
            });
        } else {
            println!(
                "[perf] open_conversation idx={idx} label={conv_label:?} \
                 messages={msg_count} cache_miss (no cached bodies) cache_load={load_ms}ms"
            );
        }
    }
    if let Some(etx) = sh.engine_tx.borrow().as_ref() {
        let _ = etx.send(engine::EngineCmd::FetchMessages { messages: c.messages.clone() });
    }
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

/// Compute the [from_ms, to_ms) range covering the displayed week
/// (5 or 7 days, full 24h regardless of hour-toggle).
fn week_range_ms(week_start_days: i64, day_count: i32) -> (i64, i64) {
    let day_ms: i64 = 24 * 60 * 60 * 1000;
    let from = week_start_days * day_ms;
    let to = from + day_count as i64 * day_ms;
    (from, to)
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
    let title = {
        const MONTHS: [&str; 12] = [
            "Январь", "Февраль", "Март", "Апрель", "Май", "Июнь",
            "Июль", "Август", "Сентябрь", "Октябрь", "Ноябрь", "Декабрь",
        ];
        format!("{} {}", MONTHS[(monday.month() - 1) as usize], monday.year())
    };
    ui.set_week_title(title.into());
    ui.set_hour_height(48.0);
    let non_work = ui.get_show_non_work_hours();
    ui.set_hour_start(if non_work { 0 } else { 8 });
    ui.set_hour_end(if non_work { 24 } else { 18 });

    // Sidebar — calendar list. Sorted by name for stability.
    let cal_items: Vec<CalendarItem> = {
        let cals = sh.calendars.borrow();
        let visibility = sh.calendar_visible.borrow();
        let mut v: Vec<&ddmail_core::types::DesktopCalendar> = cals.iter().collect();
        v.sort_by(|a, b| a.name.to_lowercase().cmp(&b.name.to_lowercase()));
        v.into_iter()
            .map(|c| CalendarItem {
                id: c.id as i32,
                name: c.name.clone().into(),
                color: hex(if c.color.is_empty() { "#3788d8" } else { &c.color }).into(),
                visible: *visibility.get(&c.id).unwrap_or(&true),
            })
            .collect()
    };
    ui.set_calendars(slint::ModelRc::new(slint::VecModel::from(cal_items)));

    // Place event blocks.
    let day_ms: i64 = 24 * 60 * 60 * 1000;
    let week_start_ms = week_days * day_ms;
    let hour_height: f32 = ui.get_hour_height();
    let hour_start = ui.get_hour_start();
    let hour_end = ui.get_hour_end();
    let visible_top_ms = hour_start as i64 * 60 * 60 * 1000;
    let visible_bottom_ms = hour_end as i64 * 60 * 60 * 1000;
    let blocks: Vec<EventBlock> = {
        let events = sh.calendar_events.borrow();
        let visibility = sh.calendar_visible.borrow();
        let cals = sh.calendars.borrow();
        let color_for = |cal_id: i64| -> String {
            cals.iter()
                .find(|c| c.id == cal_id)
                .map(|c| if c.color.is_empty() { "#3788d8".to_string() } else { c.color.clone() })
                .unwrap_or_else(|| "#3788d8".to_string())
        };
        events
            .iter()
            .filter(|e| *visibility.get(&e.calendar_id).unwrap_or(&true))
            .filter_map(|e| {
                let end_ms = e.dtend.unwrap_or(e.dtstart + 30 * 60 * 1000);
                let day = ((e.dtstart - week_start_ms) / day_ms) as i32;
                if day < 0 || day >= day_count {
                    return None;
                }
                let day_start_ms = week_start_ms + day as i64 * day_ms;
                let start_off_ms = (e.dtstart - day_start_ms).max(0);
                let end_off_ms = (end_ms - day_start_ms).min(day_ms);
                // Clip to visible hour range.
                let top_ms = start_off_ms.max(visible_top_ms);
                let bot_ms = end_off_ms.min(visible_bottom_ms);
                if bot_ms <= top_ms {
                    return None;
                }
                let to_px = |ms: i64| -> f32 {
                    let hours = (ms - visible_top_ms) as f32 / (60.0 * 60.0 * 1000.0);
                    hours * hour_height
                };
                let top = to_px(top_ms);
                let h = (to_px(bot_ms) - top).max(18.0);
                let fmt_hm = |abs_ms: i64| -> String {
                    use chrono::{Local, TimeZone, Timelike};
                    let dt = Local.timestamp_millis_opt(abs_ms).single();
                    match dt {
                        Some(d) => format!("{:02}:{:02}", d.hour(), d.minute()),
                        None => "??:??".into(),
                    }
                };
                let time = if e.all_day {
                    "весь день".to_string()
                } else {
                    format!("{} – {}", fmt_hm(e.dtstart), fmt_hm(end_ms))
                };
                Some(EventBlock {
                    id: e.id as i32,
                    day,
                    top,
                    h,
                    color: hex(&color_for(e.calendar_id)).into(),
                    title: e.summary.clone().into(),
                    time: time.into(),
                    all_day: e.all_day,
                })
            })
            .collect()
    };
    ui.set_events(slint::ModelRc::new(slint::VecModel::from(blocks)));
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
    let ui = MainWindow::new().unwrap();

    // Restore the persisted window size + sidebar width before the
    // first paint, so the UI opens where the user left it instead of
    // at the hard-coded defaults.
    let saved = window_state::load();
    ui.window().set_size(slint::LogicalSize::new(saved.width, saved.height));
    ui.set_sidebar_width(saved.sidebar_width);

    // Seed calendar view with sane defaults so the grid lays itself out
    // even before the engine produces any real data. Real `events` and
    // `calendars` arrive via FetchCalendars / FetchEvents.
    apply_calendar_defaults(&ui);

    // Save on close — the only reliable trigger Slint gives us for
    // "window is going away" on a clean exit. See the
    // [[window-state-save-on-close]] memory note for the matching Tauri
    // behaviour we're replicating.
    let ui_weak_close = ui.as_weak();
    ui.window().on_close_requested(move || {
        if let Some(ui) = ui_weak_close.upgrade() {
            let win = ui.window();
            let scale = win.scale_factor().max(0.1);
            let physical = win.size();
            let state = window_state::WindowState {
                width: physical.width as f32 / scale,
                height: physical.height as f32 / scale,
                sidebar_width: ui.get_sidebar_width(),
            };
            window_state::save(&state);
        }
        slint::CloseRequestResponse::HideWindow
    });

    let account = open_account();
    let displays = match &account {
        Some((_, _, convs)) => displays_from(convs),
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
    let ui_weak = ui.as_weak();
    {
        let rx = Arc::clone(&rx);
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
            let mut body_cache: HashMap<(String, u32, u32, u64),
                (SharedPixelBuffer<Rgba8Pixel>, f32, Vec<render_common::LinkRect>)> = HashMap::new();
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
                    Job::SetConversation { bodies, width, policy, policy_gen } => {
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

                        let mut packs: Vec<(SharedPixelBuffer<Rgba8Pixel>, f32)> =
                            Vec::with_capacity(n);
                        let mut cache_hits = 0usize;
                        let mut fallback_used = 0usize;
                        let mut render_ms_total = 0u128;
                        let mut pack_ms_total = 0u128;
                        for (i, body) in bodies.iter().enumerate() {
                            let key = (body.folder.clone(), body.uid, width, policy_gen);
                            let (buf, h, links) = if let Some(cached) = body_cache.get(&key) {
                                cache_hits += 1;
                                cached.clone()
                            } else {
                                // First render: try the full HTML. If WebKit
                                // doesn't manage to paint anything we retry with
                                // the text-only bubble — keeps "missing bubble"
                                // failures from being silent.
                                let html = build_body_html(body, &policy);
                                let t_r = Instant::now();
                                let mut result = engine.render_one(&html, width);
                                let text_available = body
                                    .text
                                    .as_deref()
                                    .map(|s| !s.trim().is_empty())
                                    .unwrap_or(false);
                                if !result.successful() && text_available {
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
                                body_cache.insert(key, entry.clone());
                                entry
                            };
                            packs.push((buf, h));
                            row_links.push(links);
                            // Push progress to the UI — one event per body.
                            let done = (i + 1) as i32;
                            let _ = ui_weak.upgrade_in_event_loop(move |ui| {
                                ui.set_render_progress(done);
                            });
                        }
                        println!(
                            "[perf] render N={n} width={width}px cache_hits={cache_hits} \
                             fallback={fallback_used} ultralight={render_ms_total}ms \
                             pack={pack_ms_total}ms total_job={}ms",
                            t_wall.elapsed().as_millis()
                        );
                        let _ = ui_weak.upgrade_in_event_loop(move |ui| {
                            // Wrap each SharedPixelBuffer in an Image —
                            // this is cheap (refcount bump, no memcpy)
                            // and is the only step that has to run on
                            // the UI thread.
                            let rows: Vec<RowItem> = packs
                                .into_iter()
                                .map(|(buf, h)| RowItem {
                                    img: Image::from_rgba8(buf),
                                    h,
                                })
                                .collect();
                            ui.set_messages(ModelRc::new(VecModel::from(rows)));
                            // Hide the progress bar.
                            ui.set_render_total(0);
                            ui.set_render_progress(0);
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
    let shared = Rc::new(Shared {
        cache,
        key,
        convs: RefCell::new(init_convs),
        displays: RefCell::new(displays.clone()),
        avatars: RefCell::new(HashMap::new()),
        current_msgs: RefCell::new(Vec::new()),
        current: Cell::new(0),
        width: Cell::new(DEFAULT_WIDTH),
        tx,
        engine_tx: RefCell::new(None),
        search_query_inflight: RefCell::new(String::new()),
        search_contacts: RefCell::new(Vec::new()),
        search_messages: RefCell::new(Vec::new()),
        pending_compose: RefCell::new(None),
        pending_reply: RefCell::new(None),
        policy: RefCell::new(policy::load()),
        policy_gen: Cell::new(0),
        calendars: RefCell::new(Vec::new()),
        calendar_visible: RefCell::new(HashMap::new()),
        calendar_events: RefCell::new(Vec::new()),
        calendar_week_start_days: Cell::new(week_start_days_today()),
    });
    SHARED.with(|s| *s.borrow_mut() = Some(shared.clone()));

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
            if let Some(d) = displays.get(i) {
                ui.set_active_name(d.name.clone().into());
                ui.set_active_initials(d.initials.clone().into());
                ui.set_active_color(slint::Brush::SolidColor(hex(&d.color)));
            }
            let policy = shared.policy.borrow().clone();
            let policy_gen = shared.policy_gen.get();
            let _ = shared.tx.send(Job::SetConversation {
                bodies,
                width: shared.width.get(),
                policy,
                policy_gen,
            });
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
        if let Some(d) = sh_sel.displays.borrow().get(real_idx) {
            ui.set_active_name(d.name.clone().into());
            ui.set_active_initials(d.initials.clone().into());
            ui.set_active_color(slint::Brush::SolidColor(hex(&d.color)));
        }
        if was_pending {
            refresh_sidebar(&sh_sel, &ui);
        }
        // Highlight the real row at its post-refresh model index.
        ui.set_selected(real_idx as i32);
        open_conversation(&sh_sel, real_idx);
    });

    let sh_rs = shared.clone();
    ui.on_viewport_resized(move |w| {
        let neww = (w as u32).max(240);
        if (neww as i32 - sh_rs.width.get() as i32).abs() < 24 {
            return;
        }
        sh_rs.width.set(neww);
        open_conversation(&sh_rs, sh_rs.current.get());
    });

    let tx_hit = shared.tx.clone();
    ui.on_hit_test(move |row, x, y| {
        let _ = tx_hit.send(Job::HitTest { row: row as usize, x, y });
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
        // After a successful staging the override fields reset so the
        // next message starts blank again. Keeps the chevron panel from
        // silently inheriting last message's headers.
        let clear_overrides = || {
            if let Some(u) = ui_weak_send.upgrade() {
                u.set_composer_subject("".into());
                u.set_composer_cc("".into());
            }
        };

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
        // after it. Fall back to conversation last_subject if cache is
        // unavailable.
        let cached = sh_send
            .cache
            .as_ref()
            .and_then(|cache| cache.load_message_bodies(&sh_send.key, &c.messages).ok())
            .unwrap_or_default();
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
            });
            clear_overrides();
        } else {
            eprintln!("send: no live engine (set DDMAIL_* env)");
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
                if let Some(d) = sh_sel_c.displays.borrow().get(conv_idx) {
                    ui.set_selected(conv_idx as i32);
                    ui.set_active_name(d.name.clone().into());
                    ui.set_active_initials(d.initials.clone().into());
                    ui.set_active_color(slint::Brush::SolidColor(hex(&d.color)));
                }
            }
            open_conversation(&sh_sel_c, conv_idx);
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
                if let Some(d) = sh_sel_m.displays.borrow().get(conv_idx) {
                    ui.set_selected(conv_idx as i32);
                    ui.set_active_name(d.name.clone().into());
                    ui.set_active_initials(d.initials.clone().into());
                    ui.set_active_color(slint::Brush::SolidColor(hex(&d.color)));
                }
            }
            open_conversation(&sh_sel_m, conv_idx);
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
        if action == "toggle-media" || action == "toggle-scripts" {
            let body_opt = sh_act
                .cache
                .as_ref()
                .and_then(|cache| cache.load_message_bodies(&sh_act.key, &[msg.clone()]).ok())
                .and_then(|mut v| v.pop());
            let Some(b) = body_opt else { return };
            let sender = b.from_addr.clone();
            let mut p = sh_act.policy.borrow_mut();
            if action == "toggle-media" {
                let now_on = p.toggle_media(&sender);
                println!("[policy] media for {sender}: {}", if now_on { "ON" } else { "OFF" });
            } else {
                let now_on = p.toggle_scripts(&sender);
                println!("[policy] scripts for {sender}: {}", if now_on { "ON" } else { "OFF" });
            }
            policy::save(&p);
            drop(p);
            sh_act.policy_gen.set(sh_act.policy_gen.get() + 1);
            open_conversation(&sh_act, sh_act.current.get());
            return;
        }

        // Reply doesn't need the live engine — we just stage the bubble's
        // body into the quote ribbon and let the next Send pick up the
        // subject + threading headers.
        if action == "reply" {
            let body_opt = sh_act
                .cache
                .as_ref()
                .and_then(|cache| cache.load_message_bodies(&sh_act.key, &[msg.clone()]).ok())
                .and_then(|mut v| v.pop());
            let Some(body) = body_opt else {
                eprintln!("reply: body not cached for {msg:?}");
                return;
            };
            if let Some(ui) = ui_weak_act.upgrade() {
                enter_reply_mode(&sh_act, &ui, body);
            }
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
        }
    });
    let ui_weak_nw = ui.as_weak();
    let sh_nw = shared.clone();
    ui.on_calendar_toggle_non_work_hours(move || {
        if let Some(ui) = ui_weak_nw.upgrade() {
            ui.set_show_non_work_hours(!ui.get_show_non_work_hours());
            apply_calendar_view(&ui, &sh_nw);
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
        }
    });
    ui.on_event_clicked(|id| println!("event clicked {id} (TODO: open event-detail popup)"));

    ui.run().unwrap();
}

/// Apply an engine result on the UI thread (reaches Shared via the thread-local).
fn handle_engine_result(ui: &MainWindow, res: engine::EngineResult) {
    match res {
        engine::EngineResult::Conversations(convs) => {
            println!("engine: {} live conversations", convs.len());
            let displays = displays_from(&convs);
            SHARED.with(|s| {
                if let Some(sh) = s.borrow().as_ref() {
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
                    *sh.convs.borrow_mut() = convs;
                    *sh.displays.borrow_mut() = displays;
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
        engine::EngineResult::Messages(bodies) => {
            SHARED.with(|s| {
                if let Some(sh) = s.borrow().as_ref() {
                    *sh.current_msgs.borrow_mut() = bodies
                        .iter()
                        .map(|b| MessageRef { folder: b.folder.clone(), uid: b.uid })
                        .collect();
                    let _ = sh.tx.send(Job::SetConversation {
                        bodies,
                        width: sh.width.get(),
                        policy: sh.policy.borrow().clone(),
                        policy_gen: sh.policy_gen.get(),
                    });
                }
            });
        }
        engine::EngineResult::Done(what) => {
            println!("engine: {what} done — refreshing");
            SHARED.with(|s| {
                if let Some(sh) = s.borrow().as_ref() {
                    if let Some(etx) = sh.engine_tx.borrow().as_ref() {
                        let _ = etx.send(engine::EngineCmd::FetchConversations { limit: 200 });
                        // Reopen current conversation to refresh its bodies.
                        let cur = sh.current.get();
                        if let Some(c) = sh.convs.borrow().get(cur) {
                            let _ = etx.send(engine::EngineCmd::FetchMessages {
                                messages: c.messages.clone(),
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
