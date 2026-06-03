//! ddmail-native — Slint shell + Ultralight (WebKit) body rendering.
//! Sidebar = real conversations from the desktop cache; selecting one renders
//! its real message bodies as Ultralight bitmaps composited as Slint images.

slint::include_modules!();

mod render;

use std::cell::Cell;
use std::rc::Rc;
use std::sync::{mpsc, Arc, Mutex};
use std::time::Instant;

use slint::{Image, ModelRc, Rgba8Pixel, SharedPixelBuffer, VecModel};

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
}

fn cache_db_path() -> Option<std::path::PathBuf> {
    let appdata = std::env::var("APPDATA").ok()?;
    Some(std::path::PathBuf::from(appdata).join("ru.letotam.ddmail").join("cache.db"))
}

fn open_account() -> Option<(Cache, String, Vec<Conversation>)> {
    let path = cache_db_path()?;
    if !path.exists() {
        println!("cache.db not found at {}", path.display());
        return None;
    }
    // Cache::new takes the app data dir (it appends cache.db).
    let dir = path.parent()?.to_path_buf();
    let cache = Cache::new(dir).ok()?;
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

/// UI-thread state shared by the select + resize callbacks.
struct Shared {
    account: Option<(Cache, String, Vec<Conversation>)>,
    current: Cell<usize>,
    width: Cell<u32>,
    tx: mpsc::Sender<Job>,
}

fn open_conversation(sh: &Shared, idx: usize) {
    sh.current.set(idx);
    if let Some((cache, key, convs)) = &sh.account {
        if let Some(c) = convs.get(idx) {
            let bodies = cache.load_message_bodies(key, &c.messages).unwrap_or_default();
            let _ = sh.tx.send(Job::SetConversation { bodies, width: sh.width.get() });
        }
    }
}

fn main() {
    let ui = MainWindow::new().unwrap();

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
    ui.set_conversations(ModelRc::new(VecModel::from(convs)));
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

    // ----- Shared state + callbacks -----
    let shared = Rc::new(Shared {
        account,
        current: Cell::new(0),
        width: Cell::new(DEFAULT_WIDTH),
        tx,
    });

    // Open the first conversation that has cached bodies.
    if let Some((cache, key, convs)) = &shared.account {
        for (i, c) in convs.iter().enumerate() {
            let bodies = cache.load_message_bodies(key, &c.messages).unwrap_or_default();
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

    let ui_weak2 = ui.as_weak();
    let displays_sel = displays.clone();
    let sh_sel = shared.clone();
    ui.on_select(move |idx| {
        let Some(ui) = ui_weak2.upgrade() else { return };
        let i = idx as usize;
        if let Some(d) = displays_sel.get(i) {
            ui.set_selected(idx);
            ui.set_active_name(d.name.clone().into());
            ui.set_active_initials(d.initials.clone().into());
            ui.set_active_color(slint::Brush::SolidColor(hex(&d.color)));
        }
        open_conversation(&sh_sel, i);
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

    // Left click on a bubble → hit-test the row's view for a link.
    let tx_hit = shared.tx.clone();
    ui.on_hit_test(move |row, x, y| {
        let _ = tx_hit.send(Job::HitTest { row: row as usize, x, y });
    });

    ui.run().unwrap();
}
