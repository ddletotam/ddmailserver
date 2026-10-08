//! ddmail-native — Slint shell + `emlrender` body rendering.
//! Sidebar = real conversations from the desktop cache; selecting one renders
//! its real message bodies as bitmaps composited as Slint images.

// Release builds run under the GUI subsystem so Windows doesn't spawn a console
// window at launch. Debug builds keep the console — that's where our `println!`
// diagnostics (tray, search, engine) go during development.
#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

slint::include_modules!();

mod account_store;
mod calendar_settings;
mod engine;
mod instance;
#[cfg(all(unix, not(target_os = "macos")))]
mod keylayout;
mod mailto;
mod merges;
mod notify;
mod policy;
mod recurrence;
mod reminders;
// The browser-free renderer — the only one. Same module on both platforms,
// which is the whole point: nothing here depends on a browser engine.
mod render;
mod render_common;
mod render_worker;
mod richtext;
mod richtext_render;
mod sanitize;
mod texture_cache;
mod toast;
mod toast_window;
#[cfg(any(windows, target_os = "linux"))]
mod tray;
mod window_state;

// The UI, by feature. Each module sees everything in this file through
// `use super::*`, and this file sees theirs through the globs below — so a
// function keeps its name wherever it lives.
mod accounts;
mod address_book;
mod bubble_html;
mod calendar;
mod composer;
mod conversation;
mod engine_events;
mod event_form;
mod links;
mod platform;
mod reminder_ui;
mod search;
mod selection;
mod shared;
mod shortcuts;
mod sidebar;
mod source_view;
mod tasks;

use std::cell::{Cell, RefCell};
use std::collections::{HashMap, HashSet};
use std::rc::Rc;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, mpsc};
use std::time::{Duration, Instant};

use slint::{Image, ModelRc, Rgba8Pixel, SharedPixelBuffer, VecModel};

use accounts::*;
use address_book::*;
use bubble_html::*;
use calendar::*;
use composer::*;
use conversation::*;
use engine_events::*;
use event_form::*;
use links::*;
use platform::*;
use reminder_ui::*;
use render_worker::Job;
use search::*;
use selection::*;
use shared::*;
use shortcuts::*;
use sidebar::*;
use source_view::*;
use tasks::*;

use ddmail_core::cache::Cache;
use ddmail_core::types::{
    Attachment, Contact, Conversation, MessageBody, MessageEnvelope, MessageRef,
};

/// The app icon (emerald speech bubble), bundled for the Slint window icon
/// (WM_SETICON) and the tray glyph — neither reads the .ico embedded in the exe.
pub(crate) const ICON_PNG: &[u8] = include_bytes!("../assets/ddmail_icon.png");

/// How many messages the server may scan when building the conversation list.
/// Mirrors the server-side default (handlers_desktop.go). A small cap combined
/// with the server's `ORDER BY uid ASC` fetch means it returns the OLDEST N
/// messages — so a multi-account aggregated INBOX with thousands of messages
/// drops recent conversations off the list entirely (ancient threads linger,
/// mail from a few days ago never appears). 5000 covers realistic inboxes;
/// deltas keep every subsequent sync cheap regardless.
const CONV_FETCH_LIMIT: u32 = 5000;

const DEFAULT_WIDTH: u32 = 740;

/// Default workday bounds (local hours). The calendar's work-hours view
/// shows one hour either side of these.

fn hex(s: &str) -> slint::Color {
    let s = s.trim_start_matches('#');
    let r = u8::from_str_radix(&s[0..2], 16).unwrap_or(0);
    let g = u8::from_str_radix(&s[2..4], 16).unwrap_or(0);
    let b = u8::from_str_radix(&s[4..6], 16).unwrap_or(0);
    slint::Color::from_rgb_u8(r, g, b)
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

fn main() {
    let _ = log::set_logger(&STDOUT_LOGGER).map(|()| log::set_max_level(log::LevelFilter::Info));
    // Name the rustls crypto backend before anything opens a connection:
    // reqwest, lettre and tungstenite each build their own client config off
    // the process default, and rustls panics rather than guess. See
    // `ddmail_core::tls`.
    ddmail_core::tls::init();
    // Деинсталлятор зовёт exe с этим флагом до удаления файлов: секреты
    // учёток живут в keyring ОС, и снос папки конфига их не трогает.
    if std::env::args().any(|a| a == "--forget-secrets") {
        engine::AccountConfig::forget_all_secrets();
        return;
    }
    // Запуск с `mailto:`-ссылкой — так система зовёт почтовую программу по
    // умолчанию (реестр: `"%1"`, `.desktop`: `%u`).
    let launch_request = instance::request_from_args(std::env::args().skip(1));
    // Single-instance guard: a second launch hands its request (raise the
    // window / open the mailto link) to the running one and exits.
    let _instance = single_instance::SingleInstance::new("ddmail-native-single").ok();
    if let Some(inst) = &_instance {
        if !inst.is_single() {
            if !instance::send(&launch_request) {
                eprintln!("ddmail is already running, но передать ему запрос не удалось");
            }
            return;
        }
    }

    // No forced login gate: the app opens straight to the UI. With no
    // configured connections it shows an empty state inviting the user to add
    // one from settings (connections are managed there, add/remove/edit at any
    // time). See rebuild_engine + the connections settings panel.

    let ui = MainWindow::new().unwrap();
    // Answer "which editing action is this key?" against the live layout.
    ui.global::<Shortcuts>().on_action(|text, held| shortcut_action(text.as_str(), held));

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
                ui.window().set_size(slint::LogicalSize::new(saved.width, saved.height));
                if saved.has_position() {
                    ui.window().set_position(slint::PhysicalPosition::new(saved.x, saved.y));
                }
                if saved.maximized {
                    ui.window().set_maximized(true);
                }
            }
            restore_done.store(true, Ordering::Relaxed);
        });
    }

    // Restore persisted calendar-view preferences (view toggles now; the
    // per-calendar maps are seeded into Shared below).
    let cal_set = calendar_settings::load();
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
    let last_written = std::cell::Cell::new(None::<(i32, i32, u32, u32, f32, bool)>);
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
    let mut loaded_merges = merges::load();
    // Разовый перевод сохранённых склеек на новую схему id. Трогает только
    // диалоги, где все адреса мои: их id раньше строился из пары
    // «айдентика|отправитель», а теперь из всего набора (`migrate_self_chat_ids`).
    // Остальные формы совпадают со старыми байт в байт и не переписываются.
    if let Some((cache, key, _)) = &account {
        let ids: Vec<String> = cache
            .load_identities(key)
            .map(|v| v.into_iter().map(|i| i.email).collect())
            .unwrap_or_default();
        if !ids.is_empty() && loaded_merges.migrate_self_chat_ids(&ids) {
            merges::save(&loaded_merges);
            println!("merges: id диалогов переведены на схему «набор адресов»");
        }
    }
    let startup_ident_colors = match &account {
        Some((c, k, _)) => identity_color_map(c, k),
        None => HashMap::new(),
    };
    // Стартовый сайдбар показывает уже склеенный вид (merges.json применён
    // к сырому списку из кэша); сырой список уходит в raw_convs.
    let startup_merged = match &account {
        Some((_, k, convs)) => apply_merges(convs, &loaded_merges, k),
        None => Vec::new(),
    };
    let displays = if startup_merged.is_empty() {
        synthetic_displays()
    } else {
        displays_from(&startup_merged, &startup_ident_colors)
    };

    ui.set_conversations(ModelRc::new(VecModel::from(sidebar_items(&displays, &HashMap::new()))));
    if let Some(d0) = displays.first() {
        ui.set_active_name(d0.name.clone().into());
        ui.set_active_initials(d0.initials.clone().into());
        ui.set_active_color(slint::Brush::SolidColor(hex(&d0.color)));
    }

    // ----- Bubble render worker (emlrender, see render_worker.rs) -----
    //
    // Shared with Job::SetConversation senders (send_render_job): holds the
    // seq of the newest enqueued job so the worker can skip/abort stale ones.
    let render_seq = Arc::new(AtomicU64::new(0));
    // Disk layer under the RAM texture cache — survives restarts, so warm
    // conversations skip layout entirely after a relaunch.
    let tex_disk = cache_db_path()
        .and_then(|p| p.parent().map(|d| d.to_path_buf()))
        .and_then(texture_cache::TextureDiskCache::open);
    let tx = render_worker::spawn(ui.as_weak(), Arc::clone(&render_seq), tex_disk);

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
        convs: RefCell::new(startup_merged),
        raw_convs: RefCell::new(init_convs),
        merges: RefCell::new(loaded_merges),
        ctx_attach: RefCell::new(None),
        displays: RefCell::new(displays.clone()),
        avatars: RefCell::new(HashMap::new()),
        current_msgs: RefCell::new(Vec::new()),
        current_bodies: RefCell::new(Vec::new()),
        open_gen: Cell::new(0),
        open_unread: RefCell::new(HashSet::new()),
        scroll_pending: Cell::new(false),
        chat_vp_y: Cell::new(-1.0),
        body_view_text: RefCell::new(HashSet::new()),
        pending_source_view: Cell::new(0),
        source_view_full: RefCell::new(String::new()),
        identity_colors: RefCell::new(startup_ident_colors),
        row_links: RefCell::new(Vec::new()),
        confirm_mode: Cell::new(0),
        row_text_runs: RefCell::new(Vec::new()),
        ctx_link: RefCell::new(None),
        link_apps: url_handler_apps(),
        pending_open_ref: RefCell::new(None),
        render_seq,
        current: Cell::new(0),
        width: Cell::new(DEFAULT_WIDTH),
        render_scale: Cell::new(1.0),
        tx,
        engine_tx: RefCell::new(None),
        policy_gen: Cell::new(loaded_policy.generation),
        policy: RefCell::new(loaded_policy),
        missed_mail: Cell::new(false),
        last_conv_id: RefCell::new(cal_set.last_conversation.clone()),
        compose: ComposeState {
            picked_identity: RefCell::new(None),
            pending_switch: RefCell::new(None),
            held_send: RefCell::new(None),
            pending_forward: RefCell::new(None),
            composer_identities: RefCell::new(Vec::new()),
            pending_compose: RefCell::new(None),
            pending_reply: RefCell::new(None),
            pending_sends: RefCell::new(Vec::new()),
            pending_send_seq: Cell::new(0),
            compose_sent_target: RefCell::new(None),
            compose_attachments: RefCell::new(Vec::new()),
            rich: RefCell::new(richtext::Editor::new()),
            rich_renderer: RefCell::new(None),
            rich_width: Cell::new(0.0),
            rich_dragging: Cell::new(false),
            rich_cid_seq: Cell::new(0),
        },
        selection: SelectionState {
            src_runs: RefCell::new(Vec::new()),
            src_sel_anchor: Cell::new(0),
            src_sel_head: Cell::new(0),
            src_sel_moved: Cell::new(false),
            src_sel_dragging: Cell::new(false),
            sel_row: Cell::new(-1),
            sel_anchor: Cell::new(0),
            sel_head: Cell::new(0),
            sel_dragging: Cell::new(false),
            sel_moved: Cell::new(false),
            sel_suppress_click: Cell::new(false),
            sel_click_streak: Cell::new(0),
            sel_click_at: Cell::new(None),
            sel_click_pos: Cell::new((-1, 0.0, 0.0)),
        },
        search: SearchState {
            search_query_inflight: RefCell::new(String::new()),
            search_contacts: RefCell::new(Vec::new()),
            search_messages: RefCell::new(Vec::new()),
            search_convs: RefCell::new(Vec::new()),
        },
        cal: CalendarState {
            calendars: RefCell::new(Vec::new()),
            calendar_visible: RefCell::new(HashMap::new()),
            calendar_colors: RefCell::new(HashMap::new()),
            calendar_events: RefCell::new(Vec::new()),
            calendar_week_start_days: Cell::new(week_start_days_today()),
            week_follows_today: Cell::new(true),
            grid_canvas_w: Cell::new(1000.0),
            grid_canvas_h: Cell::new(680.0),
            work_start: Cell::new(cal_set.work_start_hour.clamp(0, 23)),
            work_end: Cell::new(cal_set.work_end_hour.clamp(1, 24)),
            manual_hour_h: Cell::new(cal_set.manual_hour_height),
            manual_col_w: Cell::new(cal_set.manual_col_width),
            editing_event_id: Cell::new(0),
            pending_event_save: Cell::new(false),
            edit_cal_ids: RefCell::new(Vec::new()),
            edit_cal_accounts: RefCell::new(Vec::new()),
            event_accounts: RefCell::new(HashMap::new()),
            pending_open_event: Cell::new(0),
            pending_open_occ: Cell::new(0),
            pending_open_summary: RefCell::new(String::new()),
            last_cal_refetch: Cell::new(None),
            pending_cal_scroll: Cell::new(None),
            snooze_ctx: RefCell::new((0, 0, 0, 0, String::new())),
            cal_occ: RefCell::new(HashMap::new()),
        },
        contacts: AddressBookState {
            address_book: RefCell::new(Vec::new()),
            editing_contact_id: Cell::new(0),
            editing_contact_account: RefCell::new(String::new()),
            ce_account_keys: RefCell::new(Vec::new()),
        },
        accounts: AccountsState {
            cur_account_key: RefCell::new(String::new()),
            account_keys: RefCell::new(Vec::new()),
            account_states: RefCell::new(HashMap::new()),
            conn_dot_keys: RefCell::new(Vec::new()),
            reauth: RefCell::new(Vec::new()),
            add_conn_window: RefCell::new(None),
            settings_conn_keys: RefCell::new(Vec::new()),
        },
    });
    SHARED.with(|s| *s.borrow_mut() = Some(shared.clone()));
    // Подменю «Открыть с помощью…»: список фиксируется на старте (см.
    // url_handler_apps). Пусто — подменю не показывается вовсе.
    {
        let names: Vec<slint::SharedString> =
            shared.link_apps.iter().map(|(n, _)| n.as_str().into()).collect();
        println!("link handlers: {}", names.len());
        ui.set_ctx_link_apps(ModelRc::new(VecModel::from(names)));
    }
    sync_media_globals(&ui, &shared.policy.borrow());
    // Seed the composer from-picker from cached identities; refreshed again
    // whenever the engine resyncs identities.
    refresh_composer_identities(&ui, &shared);

    // Seed the persisted per-calendar maps (visibility deny-list + colour
    // overrides) now that Shared exists.
    {
        let mut vis = shared.cal.calendar_visible.borrow_mut();
        for id in &cal_set.hidden {
            vis.insert(*id, false);
        }
        *shared.cal.calendar_colors.borrow_mut() = cal_set.colors.clone();
    }

    // Seed the real display scale BEFORE the startup render so the first
    // conversation rasterizes at the right DPI immediately — otherwise the
    // width-watcher notices scale 1.0 → real and fires a scroll-less
    // re-render, which used to also cost the startup scroll anchor.
    {
        let sf = ui.window().scale_factor();
        if sf.is_finite() && sf > 0.0 {
            shared.render_scale.set(sf);
        }
    }

    // Открыть диалог, на котором закончили в прошлый раз; если его больше нет
    // (или у него нет закэшированных тел) — первый, у которого они есть.
    {
        let convs = shared.convs.borrow();
        // Ключ диалога — набор адресов (контракт §4), так что запомненный
        // находится и после пересинка, и при другом порядке списка.
        let last = shared.last_conv_id.borrow().clone();
        let preferred = convs.iter().position(|c| !last.is_empty() && c.id == last);
        // Запомненный — первым кандидатом, дальше все по порядку: у него
        // может не оказаться тел в кэше, и тогда пустая панель на старте
        // читалась бы как сломанный клиент.
        let order = preferred.into_iter().chain((0..convs.len()).filter(|i| Some(*i) != preferred));
        for i in order {
            let c = &convs[i];
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
                .map(|b| MessageRef {
                    folder: b.folder.clone(),
                    uid: b.uid,
                    message_id: b.message_id.clone(),
                    seen: true,
                })
                .collect();
            *shared.current_bodies.borrow_mut() = bodies.clone();
            refresh_composer_hints(&ui, &shared);
            // Startup scroll: same first-unread/end anchoring as a click.
            *shared.open_unread.borrow_mut() =
                c.messages.iter().filter(|m| !m.seen).map(|m| (m.folder.clone(), m.uid)).collect();
            shared.scroll_pending.set(true);
            // Consume: no fetch follows at startup — a leftover pending flag
            // would let a much later background refresh yank the viewport.
            let scroll = take_scroll_target(&shared, &bodies, true);
            send_render_job(&shared, bodies, scroll);
            // Довести сайдбар до выбранной строки. С задержкой по той же
            // причине, что и восстановление скролла переписки: мост живёт
            // внутри панели почты, а сейчас её ещё не существует — bump до
            // моста не дойдёт. Раньше это было не нужно: стартовый выбор
            // всегда попадал в первые строки, они и так видны.
            if i > 0 {
                nudge_sidebar_scroll(ui.as_weak(), i as f32 * 64.0, 200);
            }
            break;
        }
    }

    // ----- Live engine ----- (spawned from accounts.json; rebuilt on demand
    // when connections change, see rebuild_engine).
    rebuild_engine(&ui, &shared);

    // ----- Callbacks, by feature (each `wire_*` lives in its module) -----
    // Timers come back to this frame rather than being parked: a dropped
    // `Timer` stops, so they must live as long as the event loop.
    wire_sidebar(&ui, &shared);
    let _width_watcher = wire_viewport(&ui, &shared);
    wire_bubble_links(&ui, &shared);
    wire_identity_pick(&ui, &shared);
    wire_bubble_selection(&ui, &shared);
    wire_send(&ui, &shared);
    wire_composer_input(&ui, &shared);
    wire_search(&ui, &shared);
    wire_source_view(&ui, &shared);
    wire_message_actions(&ui, &shared);
    wire_view_switch(&ui, &shared);
    wire_tasks(&ui, &shared);
    wire_address_book(&ui, &shared);
    wire_calendar_nav(&ui, &shared);
    wire_settings(&ui, &shared);
    wire_snooze(&ui, &shared);
    wire_calendar_color(&ui, &shared);
    wire_event_card(&ui, &shared);
    wire_grid_editing(&ui, &shared);
    wire_edit_form(&ui, &shared);
    let _reminder_timers = start_reminder_timers(&ui);

    // Toast click callbacks (non-UI threads) reach the event loop through
    // this weak handle.
    let _ = UI_WEAK.set(ui.as_weak());

    // System tray: left-click / «Открыть» re-shows the window, «Выход» quits.
    setup_tray_and_icon(&ui, &shared);

    // Приёмник запросов вторых запусков. Поднимается только теперь, когда
    // `SHARED` и `UI_WEAK` на месте: раньше пришедшую ссылку некуда было бы
    // открыть. Вторая копия, стартовавшая в этом окне, подождёт (`instance::send`
    // повторяет попытки).
    {
        let weak = ui.as_weak();
        if let Err(e) = instance::serve(move |req| {
            let _ = weak.upgrade_in_event_loop(move |ui| handle_instance_request(&ui, req));
        }) {
            eprintln!("instance: приёмник не поднят ({e}) — вторые запуски ничего не передадут");
        }
    }
    // Холодный старт по ссылке: открываем письмо первым же тиком цикла, после
    // стартового выбора диалога, — иначе тот перебил бы compose-режим.
    if let instance::Request::Mailto(url) = launch_request {
        let weak = ui.as_weak();
        let _ = slint::invoke_from_event_loop(move || {
            if let Some(ui) = weak.upgrade() {
                println!("mailto from launch -> {url}");
                open_link_target(&ui, &url);
            }
        });
    }

    // Show the window, then run the loop in "stay alive past the last window"
    // mode: closing the window via its ✕ returns CloseRequestResponse::HideWindow
    // (above), which hides it to the tray. `ui.run()` would quit the loop when
    // that last window hides; `run_event_loop_until_quit` keeps the process
    // alive in the tray until the tray's "Выход" calls `quit_event_loop`.
    ui.show().unwrap();
    slint::run_event_loop_until_quit().unwrap();
}
