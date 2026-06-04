//! ddmail-native — Slint shell + Ultralight (WebKit) body rendering.
//! Sidebar = real conversations from the desktop cache; selecting one renders
//! its real message bodies as Ultralight bitmaps composited as Slint images.

slint::include_modules!();

mod engine;
mod render;

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

/// Shared logic for "enter transient compose mode". Pins the chat header
/// to the new recipient, blanks the bubble list, deselects the sidebar
/// row (none of the existing conversations match), and stashes the
/// target email on `Shared.pending_compose` for `on_send` to pick up.
fn enter_compose_mode(sh: &Shared, ui: &MainWindow, email: &str) {
    let email = email.trim().to_lowercase();
    *sh.pending_compose.borrow_mut() = Some(email.clone());
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
fn build_body_html(b: &MessageBody) -> String {
    let side = if b.is_outgoing { "out" } else { "in" };
    let bg = if b.is_outgoing { "#cfe6ff" } else { "#ffffff" };
    let inner = match b.html.as_deref() {
        Some(h) if !h.trim().is_empty() => h.to_string(),
        _ => format!(
            "<div style=\"white-space:pre-wrap\">{}</div>",
            html_escape(b.text.as_deref().unwrap_or(""))
        ),
    };
    format!(
        r#"<!DOCTYPE html><html><head><style>
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
        </style></head>
        <body><div class="row {side}"><div class="bubble">{inner}</div></div></body></html>"#
    )
}

enum Job {
    SetConversation { bodies: Vec<MessageBody>, width: u32 },
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
    sh.current.set(idx);
    let convs = sh.convs.borrow();
    let Some(c) = convs.get(idx) else { return };
    if let (Some(cache), key) = (&sh.cache, &sh.key) {
        let bodies = cache.load_message_bodies(key, &c.messages).unwrap_or_default();
        if !bodies.is_empty() {
            *sh.current_msgs.borrow_mut() =
                bodies.iter().map(|b| MessageRef { folder: b.folder.clone(), uid: b.uid }).collect();
            let _ = sh.tx.send(Job::SetConversation { bodies, width: sh.width.get() });
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

fn main() {
    let ui = MainWindow::new().unwrap();

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
    let (tx, rx) = mpsc::channel::<Job>();
    let rx = Arc::new(Mutex::new(rx));
    let ui_weak = ui.as_weak();
    {
        let rx = Arc::clone(&rx);
        let ui_weak = ui_weak.clone();
        std::thread::spawn(move || {
            let mut engine = render::Engine::new();
            loop {
                let job = {
                    let lock = rx.lock().unwrap();
                    lock.recv()
                };
                let Ok(job) = job else { break };
                match job {
                    Job::SetConversation { bodies, width } => {
                        let htmls: Vec<String> = bodies.iter().map(build_body_html).collect();
                        let t = Instant::now();
                        let bitmaps = engine.render_bodies(&htmls, width);
                        println!(
                            "rendered {} bodies @ {width}px in {} ms",
                            bitmaps.len(),
                            t.elapsed().as_millis()
                        );
                        let _ = ui_weak.upgrade_in_event_loop(move |ui| {
                            let rows: Vec<RowItem> = bitmaps
                                .into_iter()
                                .map(|b| {
                                    let buf = SharedPixelBuffer::<Rgba8Pixel>::clone_from_slice(
                                        &b.rgba, b.width, b.height,
                                    );
                                    RowItem {
                                        img: Image::from_rgba8(buf),
                                        h: b.height as f32,
                                    }
                                })
                                .collect();
                            ui.set_messages(ModelRc::new(VecModel::from(rows)));
                        });
                    }
                    Job::HitTest { row, x, y } => match engine.hit(row, x, y) {
                        Some(url) => {
                            println!("link click -> {url}");
                            let _ = std::process::Command::new("cmd")
                                .args(["/C", "start", "", &url])
                                .spawn();
                        }
                        None => println!("click row {row} @({x:.0},{y:.0}) — no link"),
                    },
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
            let _ = shared.tx.send(Job::SetConversation { bodies, width: shared.width.get() });
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
        let live_cfg = engine::AccountConfig::from_env();
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
        // Picking any real conversation leaves transient-compose mode.
        let was_pending = sh_sel.pending_compose.borrow_mut().take().is_some();
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

    // Composer → reply to the current conversation's counterpart, OR
    // brand-new send when we're in "transient compose" mode (entered via
    // the search dropdown: compose-row click or contact-with-no-conv).
    let sh_send = shared.clone();
    ui.on_send(move |text| {
        let text = text.to_string();
        if text.trim().is_empty() {
            return;
        }
        // Branch 1: transient compose target set via the search dropdown.
        if let Some(target) = sh_send.pending_compose.borrow().clone() {
            if let Some(etx) = sh_send.engine_tx.borrow().as_ref() {
                println!("sending new message to {target}");
                let _ = etx.send(engine::EngineCmd::Send {
                    to: vec![target],
                    subject: "Новое сообщение".to_string(),
                    body: text,
                    in_reply_to: None,
                    references: None,
                });
            } else {
                eprintln!("send: no live engine (set DDMAIL_* env)");
            }
            return;
        }
        // Branch 2: implicit reply within the currently selected conversation.
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
        let subject = if c.last_subject.to_lowercase().starts_with("re:") {
            c.last_subject.clone()
        } else {
            format!("Re: {}", c.last_subject)
        };
        // Best-effort threading headers from the most recent cached body.
        let (in_reply_to, references) = sh_send
            .cache
            .as_ref()
            .and_then(|cache| cache.load_message_bodies(&sh_send.key, &c.messages).ok())
            .and_then(|bodies| {
                bodies.last().map(|b| {
                    let irt = (!b.message_id.is_empty()).then(|| b.message_id.clone());
                    let mut refs = b.references.clone();
                    if !b.message_id.is_empty() {
                        refs.push(b.message_id.clone());
                    }
                    let refs = (!refs.is_empty()).then(|| refs.join(" "));
                    (irt, refs)
                })
            })
            .unwrap_or((None, None));
        if let Some(etx) = sh_send.engine_tx.borrow().as_ref() {
            println!("sending reply to {to:?}");
            let _ = etx.send(engine::EngineCmd::Send { to, subject, body: text, in_reply_to, references });
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
    let sh_act = shared.clone();
    ui.on_msg_action(move |row, action| {
        let row = row as usize;
        let action = action.to_string();
        let msg = sh_act.current_msgs.borrow().get(row).cloned();
        let Some(msg) = msg else { return };
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
        engine::EngineResult::Error(e) => eprintln!("engine error: {e}"),
    }
}
