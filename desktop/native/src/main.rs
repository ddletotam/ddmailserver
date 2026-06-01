//! ddmail-native — Slint shell + Blitz texture islands.
//! Sidebar = real conversations from the desktop cache; selecting one renders
//! its real cached message bodies as Blitz bubbles (single worker for all Blitz
//! work; click-time hit-testing for links; native context menu).

slint::include_modules!();

mod render;

use std::cell::RefCell;
use std::collections::{HashMap, HashSet, VecDeque};
use std::rc::Rc;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{mpsc, Arc, Mutex};
use std::time::Instant;

use slint::{Image, Model, ModelNotify, ModelRc, Rgba8Pixel, SharedPixelBuffer};

use ddmail_core::cache::Cache;
use ddmail_core::types::{Conversation, MessageBody};

const NAMES: [&str; 25] = [
    "Анна Соколова", "Команда AppSec", "Дмитрий П.", "Поддержка letotam",
    "Ольга Кузнецова", "DevSecOps канал", "Игорь Лебедев", "Мария В.",
    "Никита Орлов", "Рассылки", "Светлана Г.", "Павел Морозов",
    "QA дайджест", "Елена Фомина", "Артём Зайцев", "Релизы 4.x",
    "Юлия Беляева", "Сергей Котов", "HR отдел", "Григорий Н.",
    "Вера Полякова", "Алексей Тимофеев", "Финансы", "Дарья Жукова", "Roadmap",
];
const PALETTE: [&str; 6] = ["#2f80ed", "#27ae60", "#eb5757", "#9b51e0", "#f2994a", "#11998e"];

const ROW_W: u32 = 740;
const CACHE_CAP: usize = 200;
const WORKERS: usize = 1; // Blitz/stylo is not safe to run concurrently.

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
}

fn cache_db_path() -> Option<std::path::PathBuf> {
    let appdata = std::env::var("APPDATA").ok()?;
    Some(std::path::PathBuf::from(appdata).join("ru.letotam.ddmail").join("cache.db"))
}

/// Open the desktop cache and load the first account's conversations.
fn open_account() -> Option<(Cache, String, Vec<Conversation>)> {
    let path = cache_db_path()?;
    if !path.exists() {
        println!("cache.db not found at {}", path.display());
        return None;
    }
    let cache = Cache::open(&path).ok()?;
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
        })
        .collect()
}

fn html_escape(s: &str) -> String {
    s.replace('&', "&amp;").replace('<', "&lt;").replace('>', "&gt;")
}

/// Wrap a message body (its own HTML, or escaped plain text) in a chat bubble.
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
        .row {{ display: flex; padding: 4px 60px; }}
        .row.out {{ justify-content: flex-end; }}
        .row.in  {{ justify-content: flex-start; }}
        .bubble {{
            max-width: 72%; background: {bg}; border-radius: 16px; padding: 10px 14px;
            font-size: 15px; line-height: 1.4; color: #0f1419;
            box-shadow: 0 1px 2px rgba(0,0,0,0.12); overflow-wrap: anywhere;
        }}
        .row.out .bubble {{ border-bottom-right-radius: 4px; }}
        .row.in  .bubble {{ border-bottom-left-radius: 4px; }}
        .bubble img {{ max-width: 100%; height: auto; }}
        a {{ color: #2f80ed; }}
        </style></head>
        <body><div class="row {side}"><div class="bubble">{inner}</div></div></body></html>"#
    )
}

/// Work for the single Blitz worker. Render, hit-test and conversation set-up
/// all share one thread (Blitz/stylo cannot run concurrently).
enum Job {
    SetConversation { bodies: Vec<MessageBody>, width: u32 },
    Render { row: usize, html: String, h: u32 },
    HitTest { row: usize, html: String, x: f32, y: f32 },
}

thread_local! {
    static MODEL: RefCell<Option<Rc<ConvModel>>> = const { RefCell::new(None) };
}

static RENDER_COUNT: AtomicUsize = AtomicUsize::new(0);

/// Lazy, per-conversation model: htmls/heights are replaced on every selection;
/// textures render in the background and land in an LRU cache.
struct ConvModel {
    htmls: RefCell<Vec<String>>,
    heights: RefCell<Vec<u32>>,
    cache: RefCell<HashMap<usize, RowItem>>,
    order: RefCell<VecDeque<usize>>,
    requested: RefCell<HashSet<usize>>,
    placeholder: Image,
    tx: mpsc::Sender<Job>,
    notify: ModelNotify,
}

impl ConvModel {
    fn placeholder_row(&self, h: u32) -> RowItem {
        RowItem {
            img: self.placeholder.clone(),
            h: h as f32,
        }
    }

    /// Swap in a new conversation's rows (called on the UI thread).
    fn set_conversation(&self, htmls: Vec<String>, heights: Vec<u32>) {
        *self.htmls.borrow_mut() = htmls;
        *self.heights.borrow_mut() = heights;
        self.cache.borrow_mut().clear();
        self.order.borrow_mut().clear();
        self.requested.borrow_mut().clear();
        self.notify.reset();
    }

    fn fulfill(&self, row: usize, r: render::Rendered) {
        let Some(&h) = self.heights.borrow().get(row) else { return };
        let buf = SharedPixelBuffer::<Rgba8Pixel>::clone_from_slice(&r.rgba, r.width, r.height);
        let item = RowItem {
            img: Image::from_rgba8(buf),
            h: h as f32,
        };
        self.cache.borrow_mut().insert(row, item);
        self.order.borrow_mut().push_back(row);
        self.requested.borrow_mut().remove(&row);
        self.notify.row_changed(row);

        while self.cache.borrow().len() > CACHE_CAP {
            let victim = self.order.borrow_mut().pop_front();
            if let Some(v) = victim {
                if v != row {
                    self.cache.borrow_mut().remove(&v);
                    self.requested.borrow_mut().remove(&v);
                    self.notify.row_changed(v);
                }
            } else {
                break;
            }
        }
    }
}

impl Model for ConvModel {
    type Data = RowItem;

    fn row_count(&self) -> usize {
        self.heights.borrow().len()
    }

    fn row_data(&self, row: usize) -> Option<RowItem> {
        let h = *self.heights.borrow().get(row)?;
        if let Some(item) = self.cache.borrow().get(&row) {
            return Some(item.clone());
        }
        if self.requested.borrow_mut().insert(row) {
            let html = self.htmls.borrow()[row].clone();
            let _ = self.tx.send(Job::Render { row, html, h });
        }
        Some(self.placeholder_row(h))
    }

    fn model_tracker(&self) -> &dyn slint::ModelTracker {
        &self.notify
    }
}

fn main() {
    let ui = MainWindow::new().unwrap();

    let native = ui.window().scale_factor() as f64;
    let native = if native < 0.5 { 1.0 } else { native };
    const SSAA: f64 = 2.0;
    let scale = native * SSAA;
    println!("Native scale_factor = {native}, render scale = {scale}");

    // ----- Real account from cache (or synthetic fallback) -----
    let account = open_account();
    let displays = match &account {
        Some((_, _, convs)) => displays_from(convs),
        None => synthetic_displays(),
    };

    let convs: Vec<ConvItem> = displays
        .iter()
        .map(|d| ConvItem {
            name: d.name.clone().into(),
            preview: d.preview.clone().into(),
            initials: d.initials.clone().into(),
            color: slint::Brush::SolidColor(hex(&d.color)),
            time: "".into(),
        })
        .collect();
    ui.set_conversations(ModelRc::new(slint::VecModel::from(convs)));
    if let Some(d0) = displays.first() {
        ui.set_active_name(d0.name.clone().into());
        ui.set_active_initials(d0.initials.clone().into());
        ui.set_active_color(slint::Brush::SolidColor(hex(&d0.color)));
    }

    // ----- Single Blitz worker -----
    let (tx, rx) = mpsc::channel::<Job>();
    let rx = Arc::new(Mutex::new(rx));
    for _ in 0..WORKERS {
        let rx = Arc::clone(&rx);
        std::thread::spawn(move || {
            let rt = tokio::runtime::Runtime::new().unwrap();
            let _guard = rt.enter();
            loop {
                let job = {
                    let lock = rx.lock().unwrap();
                    lock.recv()
                };
                let Ok(job) = job else { break };
                match job {
                    Job::SetConversation { bodies, width } => {
                        let mut htmls = Vec::with_capacity(bodies.len());
                        let mut heights = Vec::with_capacity(bodies.len());
                        for b in &bodies {
                            let html = build_body_html(b);
                            let h = render::measure_height(&html, width).clamp(28, 8000);
                            htmls.push(html);
                            heights.push(h);
                        }
                        println!("conversation set: {} messages", bodies.len());
                        let _ = slint::invoke_from_event_loop(move || {
                            MODEL.with(|m| {
                                if let Some(m) = m.borrow().as_ref() {
                                    m.set_conversation(htmls, heights);
                                }
                            });
                        });
                    }
                    Job::Render { row, html, h } => {
                        let t = Instant::now();
                        let r = render::render_html_fixed(&html, ROW_W, h, scale);
                        let n = RENDER_COUNT.fetch_add(1, Ordering::Relaxed) + 1;
                        println!("render row {row} in {} ms (total {n})", t.elapsed().as_millis());
                        let _ = slint::invoke_from_event_loop(move || {
                            MODEL.with(|m| {
                                if let Some(m) = m.borrow().as_ref() {
                                    m.fulfill(row, r);
                                }
                            });
                        });
                    }
                    Job::HitTest { row, html, x, y } => {
                        match render::hit_test_link(&html, ROW_W, 12000, x, y) {
                            Some(url) => {
                                println!("link click row {row} -> {url}");
                                let _ = std::process::Command::new("cmd")
                                    .args(["/C", "start", "", &url])
                                    .spawn();
                            }
                            None => println!("click row {row} @({x:.0},{y:.0}) — no link"),
                        }
                    }
                }
            }
        });
    }

    // ----- Message model -----
    let placeholder = Image::from_rgba8(SharedPixelBuffer::<Rgba8Pixel>::new(1, 1));
    let model = Rc::new(ConvModel {
        htmls: RefCell::new(Vec::new()),
        heights: RefCell::new(Vec::new()),
        cache: RefCell::new(HashMap::new()),
        order: RefCell::new(VecDeque::new()),
        requested: RefCell::new(HashSet::new()),
        placeholder,
        tx: tx.clone(),
        notify: ModelNotify::default(),
    });
    MODEL.with(|m| *m.borrow_mut() = Some(model.clone()));
    ui.set_messages(ModelRc::from(model.clone()));

    // Open the first conversation that actually has cached message bodies
    // (the body cache is only populated for threads opened in the old client).
    if let Some((cache, key, convs)) = &account {
        let mut found = false;
        for (i, c) in convs.iter().enumerate() {
            let bodies = cache.load_message_bodies(key, &c.messages).unwrap_or_default();
            if bodies.is_empty() {
                continue;
            }
            println!("opening conversation {i} with {} cached bodies", bodies.len());
            ui.set_selected(i as i32);
            if let Some(d) = displays.get(i) {
                ui.set_active_name(d.name.clone().into());
                ui.set_active_initials(d.initials.clone().into());
                ui.set_active_color(slint::Brush::SolidColor(hex(&d.color)));
            }
            let _ = tx.send(Job::SetConversation { bodies, width: ROW_W });
            found = true;
            break;
        }
        if !found {
            println!("no conversation has cached bodies yet — open one in the old client first");
        }
    }

    // ----- Callbacks -----
    // Left click on a bubble → hit-test for a link.
    let model_hit = model.clone();
    ui.on_hit_test(move |row, x, y| {
        let i = row as usize;
        if let Some(html) = model_hit.htmls.borrow().get(i) {
            let _ = model_hit.tx.send(Job::HitTest {
                row: i,
                html: html.clone(),
                x,
                y,
            });
        }
    });

    // Conversation selection: update header + load that conversation's bodies.
    let ui_weak = ui.as_weak();
    let displays_sel = displays.clone();
    let account_sel = account;
    let tx_sel = tx.clone();
    ui.on_select(move |idx| {
        let Some(ui) = ui_weak.upgrade() else { return };
        let i = idx as usize;
        if let Some(d) = displays_sel.get(i) {
            ui.set_selected(idx);
            ui.set_active_name(d.name.clone().into());
            ui.set_active_initials(d.initials.clone().into());
            ui.set_active_color(slint::Brush::SolidColor(hex(&d.color)));
        }
        if let Some((cache, key, convs)) = &account_sel {
            if let Some(c) = convs.get(i) {
                if let Ok(bodies) = cache.load_message_bodies(key, &c.messages) {
                    let _ = tx_sel.send(Job::SetConversation { bodies, width: ROW_W });
                }
            }
        }
    });

    ui.run().unwrap();
}
