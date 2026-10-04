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
#[cfg(all(unix, not(target_os = "macos")))]
mod keylayout;
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
mod engine_events;
mod event_form;
mod links;
mod platform;
mod reminder_ui;
mod search;
mod selection;
mod shortcuts;
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
use engine_events::*;
use event_form::*;
use links::*;
use platform::*;
use reminder_ui::*;
use render_worker::Job;
use search::*;
use selection::*;
use shortcuts::*;
use source_view::*;
use tasks::*;

use ddmail_core::cache::Cache;
use ddmail_core::types::{
    Attachment, Contact, Conversation, MessageBody, MessageEnvelope, MessageRef,
};

const NAMES: [&str; 25] = [
    "Анна Соколова",
    "Команда AppSec",
    "Дмитрий П.",
    "Поддержка letotam",
    "Ольга Кузнецова",
    "DevSecOps канал",
    "Игорь Лебедев",
    "Мария В.",
    "Никита Орлов",
    "Рассылки",
    "Светлана Г.",
    "Павел Морозов",
    "QA дайджест",
    "Елена Фомина",
    "Артём Зайцев",
    "Релизы 4.x",
    "Юлия Беляева",
    "Сергей Котов",
    "HR отдел",
    "Григорий Н.",
    "Вера Полякова",
    "Алексей Тимофеев",
    "Финансы",
    "Дарья Жукова",
    "Roadmap",
];
const PALETTE: [&str; 6] = ["#2f80ed", "#27ae60", "#eb5757", "#9b51e0", "#f2994a", "#11998e"];

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
    /// Строка — пользовательская склейка нескольких диалогов (merges.json).
    merged: bool,
}

/// Pastel palette for identities lacking a server-side colour. Used as the
/// SIDEBAR ROW TINT — a soft wash behind the conversation row, so it must
/// stay light. The first 15 mirror the old Tauri identityStore +
/// imap.rs::fetch_identities_impl (so existing rows don't change colour);
/// the rest extend the wheel for users with many aliases.
/// `IDENT_VIVID` is the hue-aligned saturated counterpart — same index, same
/// hue, used only for the from-picker dot (see `refresh_composer_identities`).
const IDENT_PASTEL: [&str; 24] = [
    "#FFE4E1", "#E8F5E9", "#E3F2FD", "#FFF9C4", "#F3E5F5", "#E0F7FA", "#FBE9E7", "#F1F8E9",
    "#EDE7F6", "#E8EAF6", "#FCE4EC", "#E0F2F1", "#FFF3E0", "#F9FBE7", "#EFEBE9", "#ECEFF1",
    "#FFF8E1", "#E1F5FE", "#DCEDC8", "#FFCDD2", "#F8BBD0", "#D1C4E9", "#B2DFDB", "#B3E5FC",
];
/// Saturated counterpart to `IDENT_PASTEL`, index-aligned by hue. Used ONLY
/// for the from-picker dot: at 12px a pastel dot is invisible, so the sender
/// selector shows the intense version while the sidebar keeps the wash.
const IDENT_VIVID: [&str; 24] = [
    "#E53935", "#43A047", "#1E88E5", "#FDD835", "#8E24AA", "#00ACC1", "#F4511E", "#7CB342",
    "#5E35B1", "#3949AB", "#D81B60", "#00897B", "#FB8C00", "#C0CA33", "#6D4C41", "#546E7A",
    "#FFB300", "#039BE5", "#558B2F", "#C62828", "#AD1457", "#6A1B9A", "#00695C", "#0277BD",
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
                merged: c.merged,
            }
        })
        .collect()
}

/// Effective account key диалога: загрузка из кэша на старте оставляет
/// account_key пустым — это первичный аккаунт.
fn eff_account(fallback: &str, c: &Conversation) -> String {
    if c.account_key.is_empty() { fallback.to_string() } else { c.account_key.clone() }
}

fn conv_merge_key(fallback: &str, c: &Conversation) -> merges::MergeKey {
    merges::MergeKey { account: eff_account(fallback, c), id: c.id.clone() }
}

/// Свернуть сырой список бесед по merges.json: члены группы склеиваются в
/// один синтетический диалог на позиции самого свежего из них. Имя, аватар
/// и identity — от первичного (головы группы); дата/тема-превью — от самого
/// свежего; счётчики суммируются, refs сообщений конкатенируются (тела
/// потом сортируются по date_ts в load_message_bodies). Группа, от которой
/// в списке остался один диалог, рендерится как есть.
///
/// Поверх склеек ложатся пользовательские имена (`Merges::names`): они
/// меняют только `label` в этом виде — сырой список, кэш и письма их не
/// видят (контракт §4, «Переименование диалога»).
fn apply_merges(raw: &[Conversation], m: &merges::Merges, fallback: &str) -> Vec<Conversation> {
    let mut out = merge_groups(raw, m, fallback);
    if !m.names.is_empty() {
        for c in &mut out {
            if let Some(name) = m.name_of(&conv_merge_key(fallback, c)) {
                c.label = name.to_string();
            }
        }
    }
    out
}

fn merge_groups(raw: &[Conversation], m: &merges::Merges, fallback: &str) -> Vec<Conversation> {
    if m.groups.is_empty() {
        return raw.to_vec();
    }
    let mut group_of: HashMap<(String, String), usize> = HashMap::new();
    for (gi, g) in m.groups.iter().enumerate() {
        for k in g {
            group_of.insert((k.account.clone(), k.id.clone()), gi);
        }
    }
    enum Slot {
        Single(usize), // index into raw
        Group(usize),  // index into m.groups, placed at first member seen
    }
    let mut slots: Vec<Slot> = Vec::new();
    let mut members: HashMap<usize, Vec<usize>> = HashMap::new();
    for (i, c) in raw.iter().enumerate() {
        match group_of.get(&(eff_account(fallback, c), c.id.clone())) {
            Some(&gi) => {
                let v = members.entry(gi).or_default();
                if v.is_empty() {
                    slots.push(Slot::Group(gi));
                }
                v.push(i);
            }
            None => slots.push(Slot::Single(i)),
        }
    }
    let mut out = Vec::with_capacity(slots.len());
    for s in slots {
        match s {
            Slot::Single(i) => out.push(raw[i].clone()),
            Slot::Group(gi) => {
                let idxs = &members[&gi];
                if idxs.len() == 1 {
                    out.push(raw[idxs[0]].clone());
                    continue;
                }
                let head = m.groups[gi].first();
                let primary = idxs
                    .iter()
                    .find(|&&i| {
                        head.is_some_and(|h| {
                            h.id == raw[i].id && h.account == eff_account(fallback, &raw[i])
                        })
                    })
                    .copied()
                    .unwrap_or(idxs[0]);
                let newest =
                    idxs.iter().max_by_key(|&&i| raw[i].last_date_ts).copied().unwrap_or(idxs[0]);
                let mut combined = raw[primary].clone();
                combined.merged = true;
                combined.last_date = raw[newest].last_date.clone();
                combined.last_date_ts = raw[newest].last_date_ts;
                combined.last_subject = raw[newest].last_subject.clone();
                combined.unread_count = idxs.iter().map(|&i| raw[i].unread_count).sum();
                combined.total_count = idxs.iter().map(|&i| raw[i].total_count).sum();
                combined.is_group = idxs.iter().any(|&i| raw[i].is_group);
                combined.messages = Vec::new();
                combined.counterparts = Vec::new();
                combined.draft = None;
                let mut seen_addr: HashSet<String> = HashSet::new();
                for &i in idxs {
                    combined.messages.extend(raw[i].messages.iter().cloned());
                    for cp in &raw[i].counterparts {
                        if seen_addr.insert(cp.addr.to_lowercase()) {
                            combined.counterparts.push(cp.clone());
                        }
                    }
                    if combined.draft.is_none() {
                        combined.draft = raw[i].draft.clone();
                    }
                }
                out.push(combined);
            }
        }
    }
    out
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
            merged: false,
        })
        .collect()
}

/// UI-thread state shared by the select/resize/engine-result paths. All mail
/// state is interior-mutable so the live engine refresh can replace it.
struct Shared {
    cache: Option<Arc<Cache>>,
    key: String,
    /// Склеенный вид (merges.json применён) — то, что показывает сайдбар;
    /// все индексы UI указывают сюда.
    convs: RefCell<Vec<Conversation>>,
    /// Сырой список от движка/кэша, ДО применения склеек. Дельта-мерж
    /// Conversations работает по нему (id склейки — синтетический и в
    /// дельтах не встречается), merge/unmerge пересобирают convs из него.
    raw_convs: RefCell<Vec<Conversation>>,
    /// Пользовательские объединения диалогов; персистятся в merges.json.
    merges: RefCell<merges::Merges>,
    /// Вложение под правым кликом (folder, uid, index, filename) — цель
    /// пунктов «Открыть/Сохранить вложение» контекстного меню пузыря.
    ctx_attach: RefCell<Option<(String, u32, usize, String)>>,
    /// Явно выбранный в дропдауне отправитель (lowercase email). Закрепляет
    /// пользовательский выбор: побеждает авто-наведение на identity беседы и
    /// переустановку индекса при дельта-refetch; on_send читает его
    /// приоритетно. None = явного выбора нет, действует авто-логика.
    /// Сбрасывается при смене контекста (открытие беседы / новое письмо).
    picked_identity: RefCell<Option<String>>,
    /// Диалог, в который надо перейти, когда он появится в списке: отправка с
    /// другого адреса образует свой набор адресов, то есть свою беседу, и она
    /// возникает не мгновенно — сначала письмо должно долететь до «Отправленных»
    /// и вернуться синком. Ставится галочкой в диалоге «не тот адрес».
    pending_switch: RefCell<Option<String>>,

    /// Отправка, задержанная диалогом «отвечаешь не с того адреса»: текст
    /// письма ждёт решения. Set → показан диалог; on_send при повторном входе
    /// забирает текст отсюда и проверку уже не делает.
    ///
    /// Задержка нужна потому, что композер очищает поле сразу при отправке:
    /// без этого отменённое письмо просто пропало бы.
    held_send: RefCell<Option<String>>,
    displays: RefCell<Vec<Disp>>,
    avatars: RefCell<HashMap<String, Image>>,
    /// account_key of the currently open conversation. Addressed engine
    /// commands (body/flags/delete/source/attachment/send) carry it so they
    /// route to the right server. Empty falls back to the primary account.
    cur_account_key: RefCell<String>,
    /// All account keys (the indicator's denominator) and their last-known
    /// connection state ("connecting" | "connected" | "error" | "auth").
    /// Drives the aggregate green/yellow/red status light.
    account_keys: RefCell<Vec<String>>,
    account_states: RefCell<HashMap<String, String>>,
    /// Ключи строк списка учёток под индикатором связи (порядок
    /// accounts.json) — по индексу строки `relogin` находит, какую учётку
    /// открывать в форме входа.
    conn_dot_keys: RefCell<Vec<String>>,
    /// Учётки, чья сессия мертва и ждёт пароля — в порядке accounts.json.
    /// Отдельно от `account_states`, потому что состояние липкое: watcher
    /// после отказа ещё успевает крикнуть "connecting"/"error", и в общей
    /// карте «нужен вход» тут же затиралось бы на «нет связи». Снимается
    /// только удачным коннектом, ротацией токена или пересборкой движка.
    reauth: RefCell<Vec<String>>,
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
    /// Где стоял скролл чата, когда мы уходили из почты в календарь/книгу.
    /// Положительный отступ от верха; -1 = ещё не уходили.
    ///
    /// Пока пользователь был не в почте, в ОТКРЫТЫЙ диалог пришло письмо.
    /// Перерисовать панель было некому — её не существовало, — поэтому
    /// возврат в почту должен не восстанавливать позицию, а открыть диалог
    /// заново: иначе новое письмо не появится в переписке вовсе.
    missed_mail: Cell<bool>,
    /// Диалог, открытый последним: пишется в calendar.json при каждом
    /// открытии, чтобы следующий запуск вернулся к нему, а не к первому в
    /// списке. Ключ — `Conversation::id` (набор адресов), он переживает и
    /// пересинк, и смену порядка списка.
    last_conv_id: RefCell<String>,
    /// Панель почты условная (`if root.view-mode == 0` в app.slint), а `if` в
    /// Slint уничтожает поддерево — возврат даёт новый ListView с viewport-y = 0.
    /// Без этого снимка диалог открывался в начале, а не там, где его
    /// оставили (для непрочитанного — не на свежем письме).
    chat_vp_y: Cell<f32>,
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
    /// the only copy: the hover cursor, the link click (`on_hit_test`) and
    /// the context-menu probe all hit-test against it synchronously, without
    /// a round-trip to the render worker.
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
    /// Серия кликов для выделения слова (второй) и строки (третий). Slint даёт
    /// только `clicked`, ни двойного, ни тройного события у него нет, поэтому
    /// серию считаем сами по нажатиям: время, строка и точка предыдущего.
    sel_click_streak: Cell<u32>,
    sel_click_at: Cell<Option<Instant>>,
    sel_click_pos: Cell<(i32, f32, f32)>,
    /// Ссылка под курсором на момент показа контекстного меню и приложения,
    /// умеющие её открыть (имя + .desktop). Список читается один раз при старте.
    ctx_link: RefCell<Option<String>>,
    link_apps: Vec<(String, std::path::PathBuf)>,
    /// Toast-click navigation: scroll to this (folder, uid) once its body
    /// is rendered. Takes priority over the unread-anchor logic.
    pending_open_ref: RefCell<Option<(String, u32)>>,
    /// Render-job sequence shared with the render worker (see Job::seq).
    render_seq: Arc<AtomicU64>,
    current: Cell<usize>,
    width: Cell<u32>,
    /// UI window scale factor last seen by the width watcher; render jobs
    /// carry it so the renderer rasterizes at the display's real DPI.
    render_scale: Cell<f32>,
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
    /// Секция «Диалоги» — локальные совпадения (`local_search_convs`).
    search_convs: RefCell<Vec<ConvHit>>,
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
    /// Optimistic-send stubs: an outgoing bubble goes into the open pane
    /// the moment «Отправить» is clicked, mirrored here so it can be
    /// reconciled (dropped when the real message comes back in a
    /// FetchMessages answer) or rolled back (send failed → bubble removed,
    /// text restored to the composer). Cleared on conversation switch —
    /// the stub lives and dies with the pane it was drawn in.
    pending_sends: RefCell<Vec<PendingSend>>,
    /// Synthetic uid source for stub bodies (folder = PENDING_FOLDER);
    /// unique within the session so render-cache keys never collide.
    pending_send_seq: Cell<u32>,
    /// Recipient of a just-sent transient compose. Set by
    /// EngineResult::Sent, consumed by the Conversations handler: as soon
    /// as the delta brings the (possibly brand-new) conversation row, the
    /// UI redirects to it instead of leaving the user on the stub pane.
    compose_sent_target: RefCell<Option<String>>,
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
    /// Сетка стоит на «этой» неделе не потому, что её туда увели, а потому что
    /// это сегодняшняя неделя — значит при перекате суток её надо подтянуть.
    /// Ставится на каждом явном смещении недели (`неделя == сегодняшняя`), в
    /// момент проверки сравнить уже нельзя: после переката отображаемая неделя
    /// в любом случае не равна сегодняшней, и «оставили сами» от «убежало
    /// время» не отличить.
    week_follows_today: Cell<bool>,
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
    /// Форма события отправлена и ждёт ответа движка. Карточка закрывается по
    /// подтверждению, а не по факту нажатия: закрываясь сразу, она уносила с
    /// собой и отказ сервера — успех и провал выглядели одинаково (ничего).
    pending_event_save: Cell<bool>,
    /// Writable calendar ids, parallel to the edit-form's ComboBox model.
    edit_cal_ids: RefCell<Vec<i64>>,
    /// account_key of each writable calendar (parallel to edit_cal_ids), so a
    /// newly-created event routes to the calendar's owning account.
    edit_cal_accounts: RefCell<Vec<String>>,
    /// Last-fetched address book (parallel to the `address-book` Slint model),
    /// so the contact editor can read a row's full data by index.
    address_book: RefCell<Vec<ddmail_core::types::DesktopContact>>,
    /// Contact being edited (0 in create mode).
    editing_contact_id: Cell<i64>,
    /// account_key of the contact under edit, for multi-account write routing
    /// (empty ⇒ the engine falls back to the first account).
    editing_contact_account: RefCell<String>,
    /// account_keys parallel to the contact editor's account ComboBox (create).
    ce_account_keys: RefCell<Vec<String>>,
    /// event id → owning account_key, from the last events fetch, so calendar
    /// writes (rsvp/patch/delete) route to the right connection.
    event_accounts: RefCell<HashMap<i64, String>>,
    /// Keeps the add/edit-connection modal alive while it's open.
    add_conn_window: RefCell<Option<LoginWindow>>,
    /// account_keys parallel to the settings connections list (for edit/delete
    /// by row index).
    settings_conn_keys: RefCell<Vec<String>>,
    /// Files staged for the next outgoing message, picked via the composer's
    /// attach button. Parallel to the `composer-attachments` Slint model
    /// (which holds just the basenames). Cleared once a message is staged.
    compose_attachments: RefCell<Vec<std::path::PathBuf>>,
    /// Rich-text документ композера — источник истины для тела письма
    /// (Slint-свойство `composer-text` лишь его plain-зеркало).
    rich: RefCell<richtext::Editor>,
    /// Вёрстка/растеризация композера. Ленивая: сборка `FontSystem` читает
    /// системные шрифты (сотни мс), а композер нужен не в первую секунду.
    rich_renderer: RefCell<Option<richtext_render::Renderer>>,
    /// Ширина колонки текста, логические px — приходит из Slint (`rt-resize`).
    rich_width: Cell<f32>,
    /// Идёт протяжка выделения мышью.
    rich_dragging: Cell<bool>,
    /// Источник уникальных Content-ID для вставленных картинок.
    rich_cid_seq: Cell<u64>,
    /// Event a reminder toast asked to open (0 = none); consumed once the
    /// calendar events for its week arrive from the engine. The occurrence
    /// start + summary ride along for the stale-id fallback: the server
    /// re-creates events under new ids on calendar re-sync, so the id a
    /// reminder was seeded with may be dead by the time the toast is
    /// clicked — the meeting is then recovered by occurrence instead.
    pending_open_event: Cell<i64>,
    pending_open_occ: Cell<i64>,
    pending_open_summary: RefCell<String>,
    /// Last CalendarUpdated-driven refetch — debounces the server's push
    /// bursts (one per calendar per sync cycle) to one refetch per window.
    last_cal_refetch: Cell<Option<std::time::Instant>>,
    /// Hour the calendar grid should scroll to on the next layout (None =
    /// no request). Set when the calendar view opens; consumed by
    /// `apply_calendar_view` once real calendar data has arrived, so the
    /// scroll target is computed against the final hour-height, not the
    /// pre-data defaults. Re-issued on every apply until then.
    pending_cal_scroll: Cell<Option<f32>>,
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

/// Optimistic local removal of the conversation at `cur` (delete + spam-purge
/// share it): drop the cache row, rebuild the sidebar, and select a neighbour.
/// The engine resets the full-sync stamp on success, so the follow-up refetch
/// reconciles with the server.
fn optimistic_remove_conversation(ui: &MainWindow, sh: &Shared, cur: usize, conv_id: &str) {
    if let Some(cache) = &sh.cache {
        cache.delete_conversation(&sh.key, conv_id).ok();
    }
    {
        let mut convs = sh.convs.borrow_mut();
        if cur < convs.len() {
            convs.remove(cur);
        }
        let displays = displays_from(&convs, &sh.identity_colors.borrow());
        let items = sidebar_items(&displays, &sh.avatars.borrow());
        ui.set_conversations(ModelRc::new(VecModel::from(items)));
        *sh.displays.borrow_mut() = displays;
    }
    let len = sh.convs.borrow().len();
    if len == 0 {
        ui.set_messages(ModelRc::new(VecModel::from(Vec::<RowItem>::new())));
        ui.set_render_total(0);
        sh.current_msgs.borrow_mut().clear();
        sh.current_bodies.borrow_mut().clear();
        return;
    }
    let next = cur.min(len - 1);
    ui.set_selected(next as i32);
    apply_active_header(ui, sh, next);
    open_conversation(ui, sh, next);
    ui.set_sidebar_row_y(next as f32 * 64.0);
    ui.set_sidebar_scroll_seq(ui.get_sidebar_scroll_seq() + 1);
}

/// UI weak handle reachable from non-UI threads (toast click callbacks hop
/// to the event loop through it).
static UI_WEAK: std::sync::OnceLock<slint::Weak<MainWindow>> = std::sync::OnceLock::new();

/// Зеркалит optimistic-mark-seen в сырой список (raw_convs): без этого
/// пересборка склеенного вида (merge/unmerge) воскресила бы уже погашенный
/// unread-бейдж до прихода серверной дельты.
fn mark_raw_seen(sh: &Shared, conv_key: &merges::MergeKey, conv_merged: bool) {
    let keys: Vec<merges::MergeKey> =
        if conv_merged { sh.merges.borrow().members_of(conv_key) } else { vec![conv_key.clone()] };
    let mut raw = sh.raw_convs.borrow_mut();
    for c in raw.iter_mut() {
        if keys.contains(&conv_merge_key(&sh.key, c)) {
            c.unread_count = 0;
            for m in c.messages.iter_mut() {
                m.seen = true;
            }
        }
    }
}

/// Пересобрать склеенный вид из raw_convs (после merge/unmerge), обновить
/// сайдбар и заново найти диалог `select_id`. `reopen` — перечитать его
/// содержимое (набор сообщений изменился); false — только поправить индекс
/// выделения, не трогая открытую панель.
fn rebuild_merged_view(ui: &MainWindow, sh: &Shared, select_id: Option<String>, reopen: bool) {
    let merged = apply_merges(&sh.raw_convs.borrow(), &sh.merges.borrow(), &sh.key);
    let displays = displays_from(&merged, &sh.identity_colors.borrow());
    *sh.convs.borrow_mut() = merged;
    *sh.displays.borrow_mut() = displays;
    refresh_sidebar(sh, ui);
    if let Some(id) = select_id {
        let idx = sh.convs.borrow().iter().position(|c| c.id == id);
        if let Some(idx) = idx {
            sh.current.set(idx);
            ui.set_selected(idx as i32);
            apply_active_header(ui, sh, idx);
            if reopen {
                open_conversation(ui, sh, idx);
            }
            ui.set_sidebar_row_y(idx as f32 * 64.0);
            ui.set_sidebar_scroll_seq(ui.get_sidebar_scroll_seq() + 1);
        }
    }
}

/// Open a conversation by index: show cached bodies immediately, and (if a live
/// engine is running) fire a background fetch to refresh them.
/// `#[track_caller]` — чтобы `[perf]`-строка называла, ОТКУДА диалог открыли.
/// Путей больше десятка (клик в списке, стрелки, поиск, тост, отправка,
/// склейка, возврат в почту), а по логу они выглядели одинаково: на поиск
/// виновника «кто это открыл диалог» уходил час на каждый заход.
#[track_caller]
fn open_conversation(ui: &MainWindow, sh: &Shared, idx: usize) {
    let caller = std::panic::Location::caller();
    let t0 = Instant::now();
    // Незаконченное переименование относится к прежнему диалогу — снять,
    // иначе Enter переименовал бы уже этот. Перечит того же диалога (дельта,
    // новое письмо) поле не трогает: пользователь может быть посреди ввода.
    if sh.current.get() != idx {
        ui.set_rename_open(false);
    }
    sh.current.set(idx);
    // New conversation generation: any in-flight FetchMessages answer for
    // the previously open conversation will be dropped on arrival.
    let generation = sh.open_gen.get() + 1;
    sh.open_gen.set(generation);
    let convs = sh.convs.borrow();
    let Some(c) = convs.get(idx) else { return };
    let conv_label = c.label.clone();
    let msg_count = c.messages.len();
    // Открытие диалога само по себе показывает всё, что в нём есть, — повод
    // перерисовывать его при возврате в почту снят.
    sh.missed_mail.set(false);
    // Запомнить открытый диалог — следующий запуск вернётся к нему, а не к
    // первому в списке. Пишем сразу, как и остальные настройки: «сохраним на
    // выходе» не выживает ни выхода через трей, ни kill. Сравнение — чтобы
    // ходьба стрелками по одному и тому же диалогу не переписывала файл.
    if *sh.last_conv_id.borrow() != c.id {
        *sh.last_conv_id.borrow_mut() = c.id.clone();
        save_calendar_settings(ui, sh);
    }
    // Which account this conversation belongs to (empty → primary). Drives the
    // cache namespace and every addressed command issued while it's open.
    let akey = if c.account_key.is_empty() { sh.key.clone() } else { c.account_key.clone() };
    sh.cur_account_key.replace(akey.clone());
    // Смена контекста — сбрасываем закреплённый ручной выбор отправителя:
    // новая беседа по умолчанию отвечает со своей received_by identity, и
    // aim ниже её проставляет. Пользователь снова может переопределить.
    sh.picked_identity.borrow_mut().take();
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
    // Optimistic-send stubs live and die with the pane they were drawn in.
    sh.pending_sends.borrow_mut().clear();

    // Unread snapshot BEFORE we mark anything read — it anchors the
    // scroll (first unread at top; none unread → scroll to the end).
    let unread: Vec<MessageRef> = c.messages.iter().filter(|m| !m.seen).cloned().collect();
    let had_unread = !unread.is_empty();
    *sh.open_unread.borrow_mut() = unread.iter().map(|m| (m.folder.clone(), m.uid)).collect();
    sh.scroll_pending.set(true);

    if let Some(cache) = &sh.cache {
        let key = &akey;
        let t_load_start = Instant::now();
        let bodies = cache.load_message_bodies(key, &c.messages).unwrap_or_default();
        let load_ms = t_load_start.elapsed().as_millis();
        if !bodies.is_empty() {
            *sh.current_msgs.borrow_mut() = bodies
                .iter()
                .map(|b| MessageRef {
                    folder: b.folder.clone(),
                    uid: b.uid,
                    message_id: b.message_id.clone(),
                    seen: true,
                })
                .collect();
            *sh.current_bodies.borrow_mut() = bodies.clone();
            println!(
                "[perf] open_conversation idx={idx} label={conv_label:?} \
                 messages={msg_count} bodies={} cache_load={load_ms}ms \
                 enqueue@{:?} caller={caller}",
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
                 messages={msg_count} cache_miss (no cached bodies) \
                 cache_load={load_ms}ms caller={caller}"
            );
        }
    }
    // Ghost hints in the chevron panel follow the newly opened dialog.
    refresh_composer_hints(ui, sh);
    if let Some(etx) = sh.engine_tx.borrow().as_ref() {
        // A toast-click target may be newer than the cached conversation
        // refs — make sure the fetch includes it.
        let mut fetch_refs = c.messages.clone();
        if let Some((f, u)) = sh.pending_open_ref.borrow().clone() {
            if !fetch_refs.iter().any(|m| m.uid == u) {
                // Toast target identified only by (folder, uid); no RFC
                // Message-ID here — server falls back to uid for this one.
                fetch_refs.push(MessageRef {
                    folder: f,
                    uid: u,
                    message_id: String::new(),
                    seen: false,
                });
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
        let key_flag = sh.convs.borrow_mut().get_mut(idx).map(|c| {
            c.unread_count = 0;
            for m in c.messages.iter_mut() {
                m.seen = true;
            }
            (conv_merge_key(&sh.key, c), c.merged)
        });
        if let Some((k, merged)) = key_flag {
            mark_raw_seen(sh, &k, merged);
        }
        let displays = displays_from(&sh.convs.borrow(), &sh.identity_colors.borrow());
        *sh.displays.borrow_mut() = displays;
        refresh_sidebar(sh, ui);
        tray_sync_dot(sh);
    }
}

/// Tauri-era header meta line: counterpart address (1:1) or participants
/// (group), plus " → receiving identity". Кусками: у каждого адреса свой
/// `addr`, клик по нему копирует адрес (контракт §4, «Шапка диалога»).
fn conv_meta_parts(c: &Conversation) -> Vec<MetaPart> {
    let part = |text: &str, addr: &str| MetaPart { text: text.into(), addr: addr.into() };
    let mut out = Vec::new();
    if c.is_group {
        for (i, cp) in c.counterparts.iter().enumerate() {
            if i > 0 {
                out.push(part(", ", ""));
            }
            let text = if cp.name.is_empty() { &cp.addr } else { &cp.name };
            out.push(part(text, &cp.addr));
        }
    } else if let Some(cp) = c.counterparts.first() {
        out.push(part(&cp.addr, &cp.addr));
    }
    if !c.received_by.is_empty() {
        out.push(part(" → ", ""));
        out.push(part(&c.received_by, &c.received_by));
    }
    out
}

/// Ненавязчивое подтверждение мутации — плашка «✓ …» над композером ~2 с
/// (контракт §5: никогда не тостом).
fn flash_confirm(ui: &MainWindow, text: &str) {
    ui.set_send_confirm_text(text.into());
    ui.set_send_confirm_visible(true);
    let uiw = ui.as_weak();
    slint::Timer::single_shot(std::time::Duration::from_millis(2000), move || {
        if let Some(u) = uiw.upgrade() {
            u.set_send_confirm_visible(false);
        }
    });
}

/// Set every header property for conversation `idx` in one place: name,
/// initials, avatar colour, identity tint and the meta line. Replaces the
/// four hand-synced copies the review flagged.
fn apply_active_header(ui: &MainWindow, sh: &Shared, idx: usize) {
    if let Some(d) = sh.displays.borrow().get(idx) {
        ui.set_active_name(d.name.clone().into());
        ui.set_active_initials(d.initials.clone().into());
        ui.set_active_color(slint::Brush::SolidColor(hex(&d.color)));
        // Та же аватарка, что в строке списка — из того же кэша по тому же
        // ключу; инициалы остаются запасным вариантом.
        let avatar = sh.avatars.borrow().get(&d.email).cloned();
        ui.set_active_has_avatar(avatar.is_some());
        ui.set_active_avatar(avatar.unwrap_or_default());
        ui.set_active_ident_color(if d.ident_color.is_empty() {
            slint::Brush::SolidColor(hex("#ffffff"))
        } else {
            slint::Brush::SolidColor(hex(&d.ident_color))
        });
    }
    let meta = sh.convs.borrow().get(idx).map(conv_meta_parts).unwrap_or_default();
    ui.set_active_meta_parts(ModelRc::new(VecModel::from(meta)));
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
    let modes: Vec<u8> =
        bodies.iter().map(|b| u8::from(overrides.contains(&(b.folder.clone(), b.uid)))).collect();
    drop(overrides);
    // Склейка — свойство открытого диалога, а не писем: новое письмо
    // compose-режима ни к какой склейке не относится.
    let merged = sh.pending_compose.borrow().is_none()
        && sh.convs.borrow().get(sh.current.get()).is_some_and(|c| c.merged);
    let _ = sh.tx.send(Job::SetConversation {
        bodies,
        width: sh.width.get(),
        policy: sh.policy.borrow().clone(),
        policy_gen: sh.policy_gen.get(),
        seq,
        scroll_to,
        modes,
        scale: sh.render_scale.get(),
        merged,
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
                is_merged: d.merged,
            }
        })
        .collect()
}

/// One row representing the transient-compose target — rendered at the
/// very top of the sidebar so the user sees "a chat" with the new
/// recipient before any message has been sent. Telegram does the same.
fn pending_compose_item(target: &str) -> ConvItem {
    let initials = target.chars().next().map(|c| c.to_uppercase().to_string()).unwrap_or_default();
    ConvItem {
        name: target.to_string().into(),
        preview: "Новое сообщение".into(),
        initials: initials.into(),
        color: slint::Brush::SolidColor(hex("#10b981")),
        time: "".into(),
        has_avatar: false,
        avatar: Image::default(),
        ident_color: slint::Brush::SolidColor(slint::Color::from_argb_u8(0, 0, 0, 0)),
        unread: 0,
        highlight: false,
        is_merged: false,
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

/// Через `delay_ms` повторить bump `chat-scroll-seq`, если переписка ещё не
/// доехала до `target`. Нужно всюду, где панель почты могла создаваться в
/// момент bump'а: мост `changed x` живёт ВНУТРИ панели, и bump, сделанный до
/// её появления, до моста не доходит — ни `changed`, ни первая раскладка его
/// не подберут (см. контракт §4).
///
/// Проверка по зеркалу `chat-vp-y` (свежая панель обнуляет его в `init`)
/// делает повтор безвредным: доехали или пользователь сам увёл вид — не
/// дёргаем. Цель клампится по содержимому здесь же, иначе «в конец» (`1e9`,
/// как его ставит рендер) никогда не совпало бы с реальной позицией и
/// каждый повтор считал бы, что не доехали.
fn nudge_chat_scroll(ui_weak: slint::Weak<MainWindow>, target: f32, delay_ms: u64) {
    slint::Timer::single_shot(std::time::Duration::from_millis(delay_ms), move || {
        let Some(ui) = ui_weak.upgrade() else { return };
        let content_h = ui.get_chat_content_h();
        let view_h = ui.get_chat_view_h();
        let want = if content_h > 0.0 && view_h > 0.0 {
            target.min((content_h - view_h).max(0.0))
        } else {
            target
        };
        if (-ui.get_chat_vp_y() - want).abs() <= 1.0 {
            return;
        }
        ui.set_chat_scroll_seq(ui.get_chat_scroll_seq() + 1);
    });
}

/// То же для сайдбара: строка выбранного диалога должна быть видна, а её
/// bump теряется по той же причине. Своего зеркала позиции у списка нет,
/// поэтому просто повторяем — мост доводит строку до видимости и ничего не
/// делает, если она уже видна.
fn nudge_sidebar_scroll(ui_weak: slint::Weak<MainWindow>, row_y: f32, delay_ms: u64) {
    slint::Timer::single_shot(std::time::Duration::from_millis(delay_ms), move || {
        let Some(ui) = ui_weak.upgrade() else { return };
        ui.set_sidebar_row_y(row_y);
        ui.set_sidebar_scroll_seq(ui.get_sidebar_scroll_seq() + 1);
    });
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
    // Single-instance guard: a second launch exits instead of opening a
    // duplicate window. (Focusing the existing window needs IPC — TODO.)
    let _instance = single_instance::SingleInstance::new("ddmail-native-single").ok();
    if let Some(inst) = &_instance {
        if !inst.is_single() {
            eprintln!("ddmail is already running");
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
        picked_identity: RefCell::new(None),
        pending_switch: RefCell::new(None),
        held_send: RefCell::new(None),
        displays: RefCell::new(displays.clone()),
        avatars: RefCell::new(HashMap::new()),
        cur_account_key: RefCell::new(String::new()),
        account_keys: RefCell::new(Vec::new()),
        account_states: RefCell::new(HashMap::new()),
        conn_dot_keys: RefCell::new(Vec::new()),
        reauth: RefCell::new(Vec::new()),
        current_msgs: RefCell::new(Vec::new()),
        current_bodies: RefCell::new(Vec::new()),
        open_gen: Cell::new(0),
        open_unread: RefCell::new(HashSet::new()),
        scroll_pending: Cell::new(false),
        chat_vp_y: Cell::new(-1.0),
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
        sel_click_streak: Cell::new(0),
        sel_click_at: Cell::new(None),
        sel_click_pos: Cell::new((-1, 0.0, 0.0)),
        ctx_link: RefCell::new(None),
        link_apps: url_handler_apps(),
        pending_open_ref: RefCell::new(None),
        render_seq,
        current: Cell::new(0),
        width: Cell::new(DEFAULT_WIDTH),
        render_scale: Cell::new(1.0),
        tx,
        engine_tx: RefCell::new(None),
        search_query_inflight: RefCell::new(String::new()),
        search_contacts: RefCell::new(Vec::new()),
        search_messages: RefCell::new(Vec::new()),
        search_convs: RefCell::new(Vec::new()),
        pending_compose: RefCell::new(None),
        pending_reply: RefCell::new(None),
        pending_sends: RefCell::new(Vec::new()),
        pending_send_seq: Cell::new(0),
        compose_sent_target: RefCell::new(None),
        policy_gen: Cell::new(loaded_policy.generation),
        policy: RefCell::new(loaded_policy),
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
        missed_mail: Cell::new(false),
        last_conv_id: RefCell::new(cal_set.last_conversation.clone()),
        editing_event_id: Cell::new(0),
        pending_event_save: Cell::new(false),
        edit_cal_ids: RefCell::new(Vec::new()),
        edit_cal_accounts: RefCell::new(Vec::new()),
        address_book: RefCell::new(Vec::new()),
        editing_contact_id: Cell::new(0),
        editing_contact_account: RefCell::new(String::new()),
        ce_account_keys: RefCell::new(Vec::new()),
        event_accounts: RefCell::new(HashMap::new()),
        add_conn_window: RefCell::new(None),
        settings_conn_keys: RefCell::new(Vec::new()),
        compose_attachments: RefCell::new(Vec::new()),
        rich: RefCell::new(richtext::Editor::new()),
        rich_renderer: RefCell::new(None),
        rich_width: Cell::new(0.0),
        rich_dragging: Cell::new(false),
        rich_cid_seq: Cell::new(0),
        pending_open_event: Cell::new(0),
        pending_open_occ: Cell::new(0),
        pending_open_summary: RefCell::new(String::new()),
        last_cal_refetch: Cell::new(None),
        pending_cal_scroll: Cell::new(None),
        snooze_ctx: RefCell::new((0, 0, 0, 0, String::new())),
        cal_occ: RefCell::new(HashMap::new()),
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
        let mut vis = shared.calendar_visible.borrow_mut();
        for id in &cal_set.hidden {
            vis.insert(*id, false);
        }
        *shared.calendar_colors.borrow_mut() = cal_set.colors.clone();
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

    // ── Объединение диалогов (правый клик по строке сайдбара) ──
    // «Объединить с открытым диалогом»: кликнутая строка вливается в группу
    // открытого; открытый остаётся первичным (его имя/аватар/identity).
    let ui_weak_cm = ui.as_weak();
    let sh_cm = shared.clone();
    ui.on_conv_merge(move |model_idx| {
        let Some(ui) = ui_weak_cm.upgrade() else { return };
        if sh_cm.pending_compose.borrow().is_some() {
            return; // transient compose: индексы сдвинуты, открытого диалога нет
        }
        let src_idx = model_idx as usize;
        let dst_idx = sh_cm.current.get();
        if src_idx == dst_idx {
            return;
        }
        let resolved = {
            let convs = sh_cm.convs.borrow();
            match (convs.get(dst_idx), convs.get(src_idx)) {
                (Some(dst), Some(src)) => Some((
                    conv_merge_key(&sh_cm.key, dst),
                    conv_merge_key(&sh_cm.key, src),
                    dst.id.clone(),
                )),
                _ => None,
            }
        };
        let Some((dst_key, src_key, dst_id)) = resolved else { return };
        // Refs сообщений склейки ходят в движок под ОДНИМ account_key —
        // диалоги разных подключений склеить нельзя.
        if dst_key.account != src_key.account {
            toast_window::show(
                2, // amber
                0,
                "Объединение недоступно",
                "Диалоги из разных подключений объединить нельзя.",
                false,
                600,
                || {},
                || {},
                || {},
            );
            return;
        }
        {
            let mut m = sh_cm.merges.borrow_mut();
            let target = m.members_of(&dst_key);
            let source = m.members_of(&src_key);
            m.merge(target, source);
            merges::save(&m);
        }
        println!("merge: {} + {} (primary {})", dst_key.id, src_key.id, dst_id);
        // Открытый диалог получил новые сообщения — перечитываем его.
        rebuild_merged_view(&ui, &sh_cm, Some(dst_id), true);
    });

    // «Разъединить диалоги»: группа удаляется, члены возвращаются в сайдбар
    // отдельными строками. Выделение остаётся на первичном (тот же id).
    let ui_weak_um = ui.as_weak();
    let sh_um = shared.clone();
    ui.on_conv_unmerge(move |model_idx| {
        let Some(ui) = ui_weak_um.upgrade() else { return };
        if sh_um.pending_compose.borrow().is_some() {
            return;
        }
        let idx = model_idx as usize;
        let resolved = {
            let convs = sh_um.convs.borrow();
            match convs.get(idx) {
                Some(c) if c.merged => Some((conv_merge_key(&sh_um.key, c), c.id.clone())),
                _ => None,
            }
        };
        let Some((key, id)) = resolved else { return };
        {
            let mut m = sh_um.merges.borrow_mut();
            m.unmerge(&key);
            merges::save(&m);
        }
        println!("unmerge: {}", key.id);
        // Если распустили ОТКРЫТУЮ склейку — перечитать панель (в ней
        // останется только первичный); чужую — не дёргать открытый диалог.
        let was_open = idx == sh_um.current.get();
        let keep_id = if was_open {
            Some(id)
        } else {
            sh_um.convs.borrow().get(sh_um.current.get()).map(|c| c.id.clone())
        };
        rebuild_merged_view(&ui, &sh_um, keep_id, was_open);
    });

    // Переименование открытого диалога (двойной клик по имени в шапке).
    // Пишется в merges.json рядом со склейками и меняет только имя в
    // клиенте: письма, кэш и сервер его не видят. Пустое — снять имя.
    let ui_weak_rn = ui.as_weak();
    let sh_rn = shared.clone();
    ui.on_rename_conversation(move |name| {
        let Some(ui) = ui_weak_rn.upgrade() else { return };
        if sh_rn.pending_compose.borrow().is_some() {
            return; // нового письма ещё нет в списке — переименовывать нечего
        }
        let resolved = {
            let convs = sh_rn.convs.borrow();
            convs.get(sh_rn.current.get()).map(|c| (conv_merge_key(&sh_rn.key, c), c.id.clone()))
        };
        let Some((key, id)) = resolved else { return };
        {
            let mut m = sh_rn.merges.borrow_mut();
            if m.name_of(&key).unwrap_or("") == name.trim() {
                return; // ничего не поменялось — ни записи, ни плашки
            }
            m.rename(key.clone(), &name);
            merges::save(&m);
        }
        println!("rename: {} -> {:?}", key.id, name.trim());
        // Тела не меняются — панель не перечитываем, только сайдбар и шапку.
        rebuild_merged_view(&ui, &sh_rn, Some(id), false);
        flash_confirm(&ui, "✓ Переименовано");
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
        ui.set_confirm_is_spam(false);
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
        // A conversation must have at least one counterpart to be spam-worthy,
        // but we DON'T trust which one is the sender — for BCC-blasts the
        // participant set mixes From and To. The server resolves the real
        // sender from the message ids; here we only gate on non-emptiness.
        if c.counterparts.iter().all(|cp| cp.addr.is_empty()) {
            return;
        }
        sh_spam.confirm_mode.set(2);
        ui.set_confirm_is_spam(true);
        ui.set_confirm_delete_title("В спам?".into());
        ui.set_confirm_delete_text(
            "Удалить письма этого диалога и заблокировать отправителя. \
             «Домен» останавливает спам с меняющихся адресов одного домена."
                .into(),
        );
        ui.set_confirm_delete_visible(true);
    });
    let ui_weak_delk = ui.as_weak();
    let sh_delk = shared.clone();
    ui.on_delete_conversation_confirmed(move || {
        let Some(ui) = ui_weak_delk.upgrade() else { return };
        let cur = sh_delk.current.get();
        sh_delk.confirm_mode.set(0);
        let (conv_id, refs) = {
            let convs = sh_delk.convs.borrow();
            let Some(c) = convs.get(cur) else { return };
            (c.id.clone(), c.messages.clone())
        };
        if let Some(etx) = sh_delk.engine_tx.borrow().as_ref() {
            println!("delete conversation {conv_id} ({} messages)", refs.len());
            let _ = etx.send(engine::EngineCmd::Delete {
                messages: refs,
                account_key: sh_delk.cur_account_key.borrow().clone(),
            });
        }
        optimistic_remove_conversation(&ui, &sh_delk, cur, &conv_id);
    });

    // Spam: blacklist + purge. `scope` ("address"|"domain") comes from which
    // button the user pressed. We send the conversation's message ids so the
    // SERVER resolves the real sender (the client can't tell From from To in a
    // grouped/BCC conversation); `fallback_addr` is only a hint for sources
    // that resolve no ids. On success a «✓ …» plashka names what was blocked.
    let ui_weak_spamc = ui.as_weak();
    let sh_spamc = shared.clone();
    ui.on_spam_confirmed(move |scope| {
        let Some(ui) = ui_weak_spamc.upgrade() else { return };
        let cur = sh_spamc.current.get();
        sh_spamc.confirm_mode.set(0);
        let (conv_id, refs, fallback_addr) = {
            let convs = sh_spamc.convs.borrow();
            let Some(c) = convs.get(cur) else { return };
            (
                c.id.clone(),
                c.messages.clone(),
                c.counterparts.first().map(|cp| cp.addr.to_lowercase()).unwrap_or_default(),
            )
        };
        let ids: Vec<i64> = refs.iter().map(|m| m.uid as i64).collect();
        if let Some(etx) = sh_spamc.engine_tx.borrow().as_ref() {
            println!("spam purge scope={scope} ({} rows)", ids.len());
            let _ = etx.send(engine::EngineCmd::BlacklistAndPurge {
                scope: scope.to_string(),
                fallback_addr,
                message_ids: ids,
                account_key: sh_spamc.cur_account_key.borrow().clone(),
            });
        }
        optimistic_remove_conversation(&ui, &sh_spamc, cur, &conv_id);
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
    // the first frame and any future missed events. It also tracks the
    // window scale factor: a DPI change re-renders so the bitmap comes out
    // at the display's real scale (crisp bubbles after a monitor move).
    let ui_weak_ww = ui.as_weak();
    let sh_ww = shared.clone();
    let width_watcher = slint::Timer::default();
    width_watcher.start(
        slint::TimerMode::Repeated,
        std::time::Duration::from_millis(500),
        move || {
            if let Some(ui) = ui_weak_ww.upgrade() {
                let sf = ui.window().scale_factor();
                if sf.is_finite() && sf > 0.0 && (sf - sh_ww.render_scale.get()).abs() > 0.01 {
                    sh_ww.render_scale.set(sf);
                    let bodies = sh_ww.current_bodies.borrow().clone();
                    if !bodies.is_empty() {
                        send_render_job(&sh_ww, bodies, None);
                    }
                }
                let w = ui.get_chat_width();
                if w > 0.0 {
                    ui.invoke_viewport_resized(w);
                }
            }
        },
    );

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

    // Явный выбор отправителя из дропдауна — закрепляем email, чтобы он
    // пережил дельта-refresh и авто-наведение (см. picked_identity).
    let sh_ip = shared.clone();
    ui.on_identity_picked(move |ii| {
        let email = sh_ip.composer_identities.borrow().get(ii.max(0) as usize).cloned();
        if let Some(email) = email {
            println!("identity picked: {email}");
            *sh_ip.picked_identity.borrow_mut() = Some(email);
        }
    });

    // ── Mouse text selection over bubbles ──
    let ui_weak_ss = ui.as_weak();
    let sh_ss = shared.clone();
    ui.on_sel_start(move |row, x, y| {
        let Some(ui) = ui_weak_ss.upgrade() else { return };
        // Серия кликов: второй выделяет слово, третий — строку. Считаем сами,
        // у Slint нет ни двойного, ни тройного события. Порог 4px гасит дрожь
        // руки, но не даёт склеить клики по разным словам.
        let now = Instant::now();
        let (prow, px, py) = sh_ss.sel_click_pos.get();
        let same_spot = prow == row && (px - x).abs() < 4.0 && (py - y).abs() < 4.0;
        let quick = sh_ss
            .sel_click_at
            .get()
            .is_some_and(|t| now.duration_since(t) < Duration::from_millis(450));
        let streak = if same_spot && quick { sh_ss.sel_click_streak.get() + 1 } else { 1 };
        sh_ss.sel_click_streak.set(streak);
        sh_ss.sel_click_at.set(Some(now));
        sh_ss.sel_click_pos.set((row, x, y));

        sh_ss.sel_dragging.set(false);
        sh_ss.sel_moved.set(false);
        sh_ss.sel_row.set(-1);
        if let Some(runs) = sh_ss.row_text_runs.borrow().get(row as usize) {
            if let Some(i) = nearest_run(runs, x, y) {
                sh_ss.sel_row.set(row);
                let (anchor, head) = match streak {
                    1 => (i, i),
                    // Прогон и есть слово, так что двойной клик — это ровно он.
                    2 => (i, i),
                    _ => line_bounds(runs, i),
                };
                sh_ss.sel_anchor.set(anchor);
                sh_ss.sel_head.set(head);
                sh_ss.sel_dragging.set(true);
                if streak >= 2 {
                    // Выделение уже состоялось: пусть держится после отпускания
                    // (иначе `sel_end` сочтёт это кликом) и не открывает ссылку,
                    // если кликнули по ней.
                    sh_ss.sel_moved.set(true);
                    sh_ss.sel_suppress_click.set(true);
                }
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
        let head =
            sh_sm.row_text_runs.borrow().get(row as usize).and_then(|runs| nearest_run(runs, x, y));
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
    // ---- «Отвечаешь не с того адреса»: решения по задержанной отправке ----
    {
        let weak = ui.as_weak();
        let sh = shared.clone();
        ui.on_from_mismatch_pick(move |index| {
            let Some(ui) = weak.upgrade() else { return };
            let email = sh.composer_identities.borrow().get(index.max(0) as usize).cloned();
            if let Some(email) = email {
                // Закрепляем как ручной выбор — иначе дельта-refetch собьёт
                // индекс пикера обратно, и уйдёт снова не то.
                *sh.picked_identity.borrow_mut() = Some(email.clone());
                aim_composer_identity(&ui, &sh, &email);
                // Письмо уедет в диалог своего набора адресов. Переходим туда
                // только если попросили галочкой и адрес действительно другой.
                let switching = ui.get_from_mismatch_switch()
                    && ui.get_from_mismatch_index() != ui.get_from_mismatch_expected_index();
                *sh.pending_switch.borrow_mut() =
                    if switching { target_conversation_id(&ui, &sh, &email) } else { None };
            }
            let text = sh.held_send.borrow().clone().unwrap_or_default();
            ui.invoke_send(text.into());
        });
    }
    {
        let sh = shared.clone();
        ui.on_from_mismatch_cancel(move || {
            // Возвращать текст не нужно: композер чистится только на
            // успешной ветке отправки (clear_overrides), документ на месте.
            // Снимаем и задержанную отправку, и ожидание перехода.
            sh.held_send.borrow_mut().take();
            sh.pending_switch.borrow_mut().take();
        });
    }

    let ui_weak_send = ui.as_weak();
    let sh_send = shared.clone();
    ui.on_send(move |text| {
        let text = text.to_string();
        // Тело письма — rich-документ композера; `text` (plain-зеркало) идёт
        // в text/plain-часть и в заглушку optimistic send. Пустой текст ещё
        // не значит «нечего отправлять»: письмо из одной картинки — валидное.
        let (rich_html, rich_images) = {
            let ed = sh_send.rich.borrow();
            // Вложения спрашиваем отдельно: `ed.is_empty()` знает только
            // документ редактора — абзацы и inline-картинки, — а прикреплённые
            // файлы лежат в `compose_attachments`. Без этой проверки письмо из
            // одного вложения без единого слова не отправлялось, и кнопка при
            // этом молчала: обработчик выходил здесь же, до всякой обратной
            // связи.
            let has_attachments = !sh_send.compose_attachments.borrow().is_empty();
            if ed.is_empty() && text.trim().is_empty() && !has_attachments {
                eprintln!("send: нечего отправлять — ни текста, ни картинок, ни вложений");
                return;
            }
            (ed.html(), ed.images())
        };
        let inline_atts: Vec<ddmail_core::types::OutgoingAttachment> = rich_images
            .iter()
            .map(|img| ddmail_core::types::OutgoingAttachment {
                filename: format!("{}.png", img.cid.split('@').next().unwrap_or("image")),
                mime_type: img.mime.clone(),
                content: img.bytes.as_ref().clone(),
                content_id: Some(img.cid.clone()),
            })
            .collect();
        // Отправка, возвращённая диалогом «не тот адрес», проверку уже прошла.
        let resumed = sh_send.held_send.borrow_mut().take().is_some();
        if !resumed {
            if let Some(ui) = ui_weak_send.upgrade() {
                if let Some((expected, current)) = from_mismatch(&ui, &sh_send) {
                    *sh_send.held_send.borrow_mut() = Some(text.clone());
                    // Список для дропдауна + предвыбор на адресе диалога:
                    // правильный вариант уже выбран, подтвердить — один клик.
                    let addresses = sh_send.composer_identities.borrow().clone();
                    let picked = addresses.iter().position(|e| *e == expected).unwrap_or(0);
                    ui.set_from_mismatch_options(ModelRc::new(VecModel::from(
                        addresses.iter().map(|e| slint::SharedString::from(e.as_str())).collect::<Vec<_>>(),
                    )));
                    ui.set_from_mismatch_index(picked as i32);
                    ui.set_from_mismatch_expected_index(picked as i32);
                    // Галочка каждый раз с нуля: переход — осознанный выбор,
                    // а не залипшая настройка.
                    ui.set_from_mismatch_switch(false);
                    ui.set_from_mismatch_expected(expected.into());
                    ui.set_from_mismatch_current(current.into());
                    ui.set_from_mismatch_visible(true);
                    return;
                }
            }
        }
        // Graceful guard: if this thread belongs to a connection that was
        // since removed, the send has nowhere to go — say so plainly instead
        // of misrouting to another account.
        {
            let ak = sh_send.cur_account_key.borrow().clone();
            let alive = ak.is_empty() || sh_send.account_keys.borrow().iter().any(|k| *k == ak);
            if !alive {
                toast_window::show(
                    2,
                    0,
                    "Отправка недоступна",
                    "Это письмо из подключения, которое было удалено. Добавьте подключение заново, чтобы отправлять с этого адреса.",
                    false,
                    600,
                    || {},
                    || {},
                    || {},
                );
                return;
            }
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
        // Explicit «Кому» override — same contract as the subject override:
        // a filled field is the user's explicit order and beats EVERY
        // auto-derivation, reply-all included. Ignoring it while showing an
        // editable field once sent a reply to 18 people instead of one.
        let to_override: Vec<String> = ui_now
            .as_ref()
            .map(|u| u.get_composer_to().to_string())
            .unwrap_or_default()
            .split(|c: char| c == ',' || c == ';')
            .map(|s| s.trim().to_string())
            .filter(|s| !s.is_empty())
            .collect();
        // Sending identity: закреплённый ручной выбор (picked_identity) имеет
        // приоритет над индексом пикера — индекс могла сбить дельта-refetch
        // между выбором и отправкой (баг «выбрал dd, ушло info»). Без явного
        // выбора — резолвим по индексу через Shared-список (Slint-модель
        // только для отрисовки). None до синка identities → движок подставит
        // адрес аккаунта.
        let from_identity: Option<String> = sh_send
            .picked_identity
            .borrow()
            .clone()
            .or_else(|| {
                ui_now.as_ref().and_then(|u| {
                    let idx = u.get_composer_identity_index();
                    sh_send
                        .composer_identities
                        .borrow()
                        .get(idx.max(0) as usize)
                        .cloned()
                })
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
                u.set_composer_to("".into());
                // Тело чистим здесь, а не в Slint при клике: до этой точки
                // ветка могла отказаться отправлять (нет адресата, мёртвое
                // подключение) — набранное тогда обязано остаться на месте.
                rich_clear(&u, &sh_send);
                refresh_attachment_chips(&u, &sh_send);
            }
        };

        // Branch 0: forward — explicit recipients from the «Кому» field;
        // the typed text is the covering note, the original's text goes
        // below it after a separator, attachments re-attach engine-side.
        // Клон берётся ОТДЕЛЬНЫМ стейтментом, а не в скрутинии `if let`:
        // временное значение из скрутинии живёт до конца блока, поэтому `Ref`
        // пережил бы `exit_reply_mode` в конце ветки, а тот пишет в эту же
        // ячейку. Именно так клиент и умирал сразу после отправки —
        // «RefCell already borrowed», main.rs:1063. То же и в двух ветках ниже.
        let forwarded = sh_send.pending_forward.borrow().clone();
        if let Some(orig) = forwarded {
            let to = to_override.clone();
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
            // HTML-версия: набранная сопроводиловка (со стилями) + тот же
            // блок пересылки, экранированный как обычный текст.
            let body_html = format!(
                "{rich_html}<br><div>---------- Пересланное сообщение ----------</div>\
                 <div>От: {}</div><div>Дата: {}</div><div>Тема: {}</div><br><div>{}</div>",
                html_escape_plain(&from_line),
                html_escape_plain(&orig.date),
                html_escape_plain(&orig.subject),
                html_escape_plain(&orig_text).replace('\n', "<br>")
            );
            if let Some(etx) = sh_send.engine_tx.borrow().as_ref() {
                println!("forwarding {}/{} to {to:?}", orig.folder, orig.uid);
                let _ = etx.send(engine::EngineCmd::Send {
                    to,
                    cc: cc.clone(),
                    subject,
                    body: body_text,
                    html: body_html,
                    inline: inline_atts.clone(),
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
        let compose_target = sh_send.pending_compose.borrow().clone();
        if let Some(target) = compose_target {
            let subject = if !subject_override.is_empty() {
                subject_override.clone()
            } else {
                "Новое сообщение".to_string()
            };
            if let Some(etx) = sh_send.engine_tx.borrow().as_ref() {
                println!("sending new message to {target}");
                let to = if to_override.is_empty() { vec![target] } else { to_override.clone() };
                let hdr = StubHeaders { subject: subject.clone(), to: to.clone(), cc: cc.clone() };
                let _ = etx.send(engine::EngineCmd::Send {
                    to,
                    cc: cc.clone(),
                    subject,
                    body: text.clone(),
                    html: rich_html.clone(),
                    inline: inline_atts.clone(),
                    in_reply_to: None,
                    references: None,
                    from: from_identity.clone(),
                    attachments: attachments.clone(),
                    forward_attachments: None,
                    account_key: sh_send.cur_account_key.borrow().clone(),
                });
                clear_overrides();
                // Optimistic bubble in the (empty) compose pane; no
                // conversation exists yet, so the stub carries no conv id.
                append_send_stub(
                    &sh_send,
                    &text,
                    Some(stub_html(&rich_html, &rich_images)),
                    stub_attachments(&attachments),
                    &from_identity.clone().unwrap_or_else(|| sh_send.key.clone()),
                    hdr,
                    "",
                );
            } else {
                eprintln!("send: no live engine (set DDMAIL_* env)");
            }
            return;
        }
        // Branch 2: explicit reply via quote ribbon.
        let quoted_reply = sh_send.pending_reply.borrow().clone();
        if let Some(reply_body) = quoted_reply {
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
            // Explicit «Кому» beats the reply-all derivation entirely.
            let to = if to_override.is_empty() { to } else { to_override.clone() };
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
                let hdr = StubHeaders { subject: subject.clone(), to: to.clone(), cc: cc.clone() };
                let _ = etx.send(engine::EngineCmd::Send {
                    to, cc: cc.clone(), subject, body: text.clone(),
                    html: rich_html.clone(), inline: inline_atts.clone(),
                    in_reply_to, references,
                    from: from_identity.clone(),
                    attachments: attachments.clone(),
                    forward_attachments: None,
                    account_key: sh_send.cur_account_key.borrow().clone(),
                });
                clear_overrides();
                let conv_id = sh_send
                    .convs
                    .borrow()
                    .get(sh_send.current.get())
                    .map(|c| c.id.clone())
                    .unwrap_or_default();
                append_send_stub(
                    &sh_send,
                    &text,
                    Some(stub_html(&rich_html, &rich_images)),
                    stub_attachments(&attachments),
                    &from_identity.clone().unwrap_or_else(|| sh_send.key.clone()),
                    hdr,
                    &conv_id,
                );
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
        let to: Vec<String> = if !to_override.is_empty() {
            // Explicit «Кому» beats the conversation's counterparts.
            to_override.clone()
        } else {
            c.counterparts
                .iter()
                .map(|cp| cp.addr.clone())
                .filter(|a| !a.is_empty())
                .collect()
        };
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
        // Release the convs/current_bodies borrows before the stub append —
        // it re-borrows current_bodies mutably.
        let conv_id = c.id.clone();
        drop(cached);
        drop(convs);
        if let Some(etx) = sh_send.engine_tx.borrow().as_ref() {
            println!("sending reply to {to:?}");
            // Described before the list is handed to the engine — the stub
            // shows the same chips as the message going out.
            let stub_atts = stub_attachments(&attachments);
            let hdr = StubHeaders { subject: subject.clone(), to: to.clone(), cc: cc.clone() };
            let _ = etx.send(engine::EngineCmd::Send {
                to, cc, subject, body: text.clone(),
                html: rich_html.clone(), inline: inline_atts.clone(),
                in_reply_to, references,
                from: from_identity.clone(),
                attachments,
                forward_attachments: None,
                account_key: sh_send.cur_account_key.borrow().clone(),
            });
            clear_overrides();
            append_send_stub(
                &sh_send,
                &text,
                Some(stub_html(&rich_html, &rich_images)),
                stub_atts,
                &from_identity.clone().unwrap_or_else(|| sh_send.key.clone()),
                hdr,
                &conv_id,
            );
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
        let paths = pick_attachment_files(&u);
        if paths.is_empty() {
            return;
        }
        sh_att.compose_attachments.borrow_mut().extend(paths);
        refresh_attachment_chips(&u, &sh_att);
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

    // ── Rich-text композер ──
    //
    // Slint отдаёт сюда ширину колонки, клавиши и мышь; обратно уезжают
    // битмап и геометрия каретки (rich_refresh). Модель — sh.rich.
    let ui_weak_rtw = ui.as_weak();
    let sh_rtw = shared.clone();
    ui.on_rt_resize(move |w| {
        let Some(u) = ui_weak_rtw.upgrade() else { return };
        // Ширина скачет на каждом кадре ресайза — перевёрстываем только на
        // реальном изменении (сравнение в логических px с допуском ½ px).
        if (sh_rtw.rich_width.get() - w).abs() < 0.5 {
            return;
        }
        sh_rtw.rich_width.set(w);
        rich_refresh(&u, &sh_rtw);
    });
    let ui_weak_rtk = ui.as_weak();
    let sh_rtk = shared.clone();
    ui.on_rt_key(move |text, ctrl, shift, alt| {
        let Some(u) = ui_weak_rtk.upgrade() else { return false };
        rich_key(&u, &sh_rtk, text.as_str(), ctrl, shift, alt)
    });
    let ui_weak_rtp = ui.as_weak();
    let sh_rtp = shared.clone();
    ui.on_rt_pointer(move |x, y, kind| {
        let Some(u) = ui_weak_rtp.upgrade() else { return };
        let pos = {
            let slot = sh_rtp.rich_renderer.borrow();
            let Some(r) = slot.as_ref() else { return };
            r.pos_at(x, y)
        };
        match kind {
            // Нажатие ставит каретку и начинает протяжку; Shift+клик тянет
            // выделение от прежнего якоря (как в любом текстовом поле).
            0 => {
                sh_rtp.rich_dragging.set(true);
                sh_rtp.rich.borrow_mut().set_caret(pos, false);
            }
            1 => {
                if !sh_rtp.rich_dragging.get() {
                    return;
                }
                sh_rtp.rich.borrow_mut().set_caret(pos, true);
            }
            2 => sh_rtp.rich_dragging.set(false),
            _ => sh_rtp.rich.borrow_mut().select_word_at(pos),
        }
        rich_refresh(&u, &sh_rtp);
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
            // Answer instantly from client-side state (address book + sidebar
            // counterparts) so contacts appear on the first keystroke — the
            // engine result below only augments this with cache + messages.
            if !trimmed.is_empty() {
                let q_lc = trimmed.to_lowercase();
                // Диалоги — целиком локально и только здесь: ответ движка
                // (ниже) их не касается, он приносит контакты кэша и письма.
                let subjects = sh_typed
                    .cache
                    .as_ref()
                    .and_then(|c| {
                        c.body_subjects().map_err(|e| eprintln!("search subjects: {e}")).ok()
                    })
                    .unwrap_or_default();
                let hits =
                    local_search_convs(&sh_typed.convs.borrow(), &subjects, &sh_typed.key, &q_lc);
                let local = local_search_contacts(
                    &sh_typed.address_book.borrow(),
                    &conv_hit_addrs(&hits),
                    &q_lc,
                );
                let c_items = contact_items(&local);
                ui.set_search_convs(ModelRc::new(VecModel::from(conv_hit_items(&hits))));
                *sh_typed.search_convs.borrow_mut() = hits;
                *sh_typed.search_contacts.borrow_mut() = local;
                ui.set_search_contacts(ModelRc::new(VecModel::from(c_items)));
            }
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
        sh_clr.search_convs.borrow_mut().clear();
        if let Some(ui) = ui_weak_sc.upgrade() {
            ui.set_search_convs(ModelRc::new(VecModel::from(Vec::<ConvHitItem>::new())));
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
        let best = convs
            .iter()
            .enumerate()
            .filter(|(_, c)| {
                c.counterparts
                    .first()
                    .map(|cp| cp.addr.to_lowercase() == target_lc)
                    .unwrap_or(false)
            })
            .max_by_key(|(_, c)| c.last_date_ts);
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

    // Строка секции «Диалоги»: открыть диалог. Ищем по ключу — список мог
    // перестроиться дельтой, пока выпадашка была открыта.
    let ui_weak_sel_d = ui.as_weak();
    let sh_sel_d = shared.clone();
    ui.on_search_select_conv(move |idx| {
        let Some(hit) = sh_sel_d.search_convs.borrow().get(idx as usize).cloned() else { return };
        let conv_idx = sh_sel_d
            .convs
            .borrow()
            .iter()
            .position(|c| c.id == hit.id && eff_account(&sh_sel_d.key, c) == hit.account);
        let Some(ui) = ui_weak_sel_d.upgrade() else { return };
        ui.set_search_open(false);
        let Some(conv_idx) = conv_idx else {
            println!("search-select-conv: {} is gone from the list", hit.id);
            return;
        };
        sh_sel_d.search_query_inflight.borrow_mut().clear();
        ui.set_search_query("".into());
        ui.set_selected(conv_idx as i32);
        apply_active_header(&ui, &sh_sel_d, conv_idx);
        open_conversation(&ui, &sh_sel_d, conv_idx);
        ui.set_sidebar_row_y(conv_idx as f32 * 64.0);
        ui.set_sidebar_scroll_seq(ui.get_sidebar_scroll_seq() + 1);
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
        let conv_idx = convs
            .iter()
            .position(|c| c.messages.iter().any(|m| m.folder == env.folder && m.uid == env.uid));
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
                let rects =
                    selection_rects_for(&runs, sh2.src_sel_anchor.get(), sh2.src_sel_head.get());
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
            // Headers come out of the cache when they are there: the sync
            // already had the whole message in hand and kept the wire header
            // block, so asking the server again would be a login and a
            // multi-megabyte download for thirty lines.
            if action == "show-headers" {
                let cached = sh_act
                    .current_bodies
                    .borrow()
                    .get(row)
                    .map(|b| b.raw_headers.clone())
                    .filter(|h| !h.is_empty());
                if let Some(raw) = cached {
                    if let Some(ui) = ui_weak_act.upgrade() {
                        show_headers(&ui, msg.uid, &raw);
                    }
                    return;
                }
            }
            sh_act.pending_source_view.set(if action == "show-headers" { 1 } else { 2 });
            if let Some(etx) = sh_act.engine_tx.borrow().as_ref() {
                let _ = etx.send(engine::EngineCmd::FetchSource {
                    folder: msg.folder.clone(),
                    uid: msg.uid,
                    account_key: sh_act.cur_account_key.borrow().clone(),
                    headers_only: action == "show-headers",
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
            if mode == 0 {
                // Возврат в почту. Панель почты условная, поэтому ListView
                // здесь СВЕЖИЙ и стоит в начале — вернуть скролл туда, где
                // его оставили, обязаны мы.
                //
                // Снимок ОДНОРАЗОВЫЙ: «Почта» при уже открытой почте зовёт
                // этот же обработчик, но панель не пересоздаётся, и
                // восстановление дёрнуло бы вид на старую позицию без
                // причины. Нечего восстанавливать → не трогаем вообще: на
                // первом входе анкор ставит сам open_conversation.
                //
                // Флаг ставим напрямую, а не бампом chat-scroll-seq: мост
                // (`changed x`) живёт внутри той же условной панели, и при её
                // пересоздании новое значение seq — начальное, а не
                // изменение, так что `changed` не сработает. Применит скролл
                // сам ListView на первом реальном layout (`viewport-height` /
                // `height`), как это уже сделано для сетки календаря.
                // Пока нас не было, в открытый диалог пришло письмо —
                // возвращать позицию, на которой ушли, значит спрятать его
                // под сгибом (а точнее — не показать вовсе: панели не
                // существовало, и в переписку письмо не дорисовывалось).
                // Открываем диалог заново: анкор встанет на первое
                // непрочитанное, то есть на него.
                let saved = sh_view.chat_vp_y.replace(-1.0);
                if sh_view.missed_mail.replace(false) {
                    let idx = sh_view.current.get();
                    apply_active_header(&ui, &sh_view, idx);
                    open_conversation(&ui, &sh_view, idx);
                } else if saved >= 0.0 {
                    ui.set_chat_scroll_y(saved);
                    ui.set_chat_scroll_pending(true);
                    // Одного флага мало: при создании панели с уже готовой
                    // геометрией пропадают ОБА триггера — bump seq был до
                    // появления моста, а первая раскладка не «изменение»
                    // свойства, так что ни один `changed` не срабатывает и
                    // переписка остаётся в начале. Тот же приём, что у сетки
                    // календаря (`scroll_calendar_to_hour`): доложить
                    // bump'ом после того, как панель уже существует.
                    nudge_chat_scroll(ui.as_weak(), saved, 120);
                    // Вторая попытка — на случай, когда к 120 мс раскладка
                    // ещё не настоящая (длинная переписка, рендер догоняет).
                    // Обе проверяют, доехал ли вид, и молчат, если да.
                    nudge_chat_scroll(ui.as_weak(), saved, 400);
                }
            } else {
                // Уходим из почты — снять позицию ДО того, как панель
                // уничтожится. chat-vp-y отрицательный (offset вверх).
                sh_view.chat_vp_y.set((-ui.get_chat_vp_y()).max(0.0));
            }
            if mode == 1 {
                // Land the viewport on the working day, not on 00:00 —
                // consumed by apply_calendar_view once the layout is real.
                sh_view.pending_cal_scroll.set(Some(sh_view.work_start.get() as f32));
                apply_calendar_view(&ui, &sh_view);
                if let Some(etx) = sh_view.engine_tx.borrow().as_ref() {
                    let _ = etx.send(engine::EngineCmd::FetchCalendars);
                }
                refetch_calendar_events(&ui, &sh_view);
            } else if mode == 2 {
                // Enter the address book: load the full book (empty query).
                ui.set_contacts_query("".into());
                fetch_contacts(&sh_view, "");
            } else if mode == 3 {
                fetch_tasks(&ui, &sh_view);
            }
        }
    });

    // Tasks: manual refresh, and the "show completed" toggle (which changes
    // what the server is asked for, so it has to re-fetch rather than filter
    // locally).
    let ui_weak_tr = ui.as_weak();
    let sh_tr = shared.clone();
    ui.on_tasks_refresh(move || {
        if let Some(ui) = ui_weak_tr.upgrade() {
            fetch_tasks(&ui, &sh_tr);
        }
    });

    // Tick a task off. The row is updated straight away and the server is told
    // in the background: waiting for a CalDAV round-trip before the checkbox
    // moves makes the list feel broken.
    let ui_weak_tt = ui.as_weak();
    let sh_tt = shared.clone();
    ui.on_task_toggle(move |id| {
        let Some(ui) = ui_weak_tt.upgrade() else { return };
        toggle_task(&ui, &sh_tt, id as i64);
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
        sh_cadd.editing_contact_account.borrow_mut().clear();
        // Populate the account picker (labels + parallel keys).
        {
            let accounts = engine::AccountConfig::load_all();
            let labels: Vec<slint::SharedString> = accounts
                .iter()
                .map(|a| if a.email.is_empty() { a.account_key() } else { a.email.clone() }.into())
                .collect();
            *sh_cadd.ce_account_keys.borrow_mut() =
                accounts.iter().map(|a| a.account_key()).collect();
            ui.set_ce_accounts(ModelRc::new(VecModel::from(labels)));
            ui.set_ce_account_idx(0);
        }
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
        *sh_ced.editing_contact_account.borrow_mut() = c.account_key.clone();
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
        let ak = if id == 0 {
            // Create → the account chosen in the picker.
            let idx = ui.get_ce_account_idx().max(0) as usize;
            sh_csave.ce_account_keys.borrow().get(idx).cloned().unwrap_or_default()
        } else {
            sh_csave.editing_contact_account.borrow().clone()
        };
        if id == 0 {
            let _ = etx.send(engine::EngineCmd::CreateContact { body, account_key: ak });
        } else {
            let _ = etx.send(engine::EngineCmd::UpdateContact { id, body, account_key: ak });
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
            let ak = sh_cdel.editing_contact_account.borrow().clone();
            if let Some(etx) = sh_cdel.engine_tx.borrow().as_ref() {
                let _ = etx.send(engine::EngineCmd::DeleteContact { id, account_key: ak });
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
            sh.week_follows_today.set(new_start == week_start_days_today());
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
        // A manual zoom overrides any queued programmatic scroll.
        sh_zh.pending_cal_scroll.set(None);
        let canvas_h = sh_zh.grid_canvas_h.get().max(MIN_HOUR_H);
        let cur = if sh_zh.manual_hour_h.get() > 0.0 {
            sh_zh.manual_hour_h.get()
        } else {
            ui.get_hour_height()
        };
        let factor = if delta > 0.0 { 1.1 } else { 1.0 / 1.1 };
        // Zooming out при упоре в пол returns to autofit (manual = 0): the
        // grid collapses back to the work-hours band. Without this escape
        // hatch a single ctrl-wheel pinned the layout to the full 0–24
        // scroll forever — every launch then opened on the night hours.
        if delta < 0.0 && cur <= MIN_HOUR_H + 0.5 {
            sh_zh.manual_hour_h.set(0.0);
            apply_calendar_view(&ui, &sh_zh);
            save_calendar_settings(&ui, &sh_zh);
            return;
        }
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
        // Same escape hatch as the hour zoom: bottoming out returns to
        // autofit column widths.
        if delta < 0.0 && cur <= MIN_COL_W + 0.5 {
            sh_zd.manual_col_w.set(0.0);
            apply_calendar_view(&ui, &sh_zd);
            save_calendar_settings(&ui, &sh_zd);
            return;
        }
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
            apply_reminder_visibility(&sh_vis, id, !cur);
            apply_calendar_view(&ui, &sh_vis);
            save_calendar_settings(&ui, &sh_vis);
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
    // Empty-state CTA → open the add-connection modal.
    let ui_weak_afc = ui.as_weak();
    ui.on_add_first_connection(move || {
        open_add_connection(ui_weak_afc.clone(), None);
    });

    // Settings → Подключения: add / edit / delete.
    let ui_weak_addc = ui.as_weak();
    ui.on_add_connection(move || {
        open_add_connection(ui_weak_addc.clone(), None);
    });
    let ui_weak_editc = ui.as_weak();
    let sh_editc = shared.clone();
    ui.on_edit_connection(move |idx| {
        let key = sh_editc.settings_conn_keys.borrow().get(idx.max(0) as usize).cloned();
        let Some(key) = key else { return };
        let cfg = engine::AccountConfig::load_all().into_iter().find(|a| a.account_key() == key);
        open_add_connection(ui_weak_editc.clone(), cfg);
    });
    // Плашка «сессия истекла» и строка под индикатором связи: тот же вход,
    // что «Изменить», но по индексу списка учёток (он существует с первого
    // события связи, тогда как список настроек наполняется только при
    // открытии модалки).
    let ui_weak_relog = ui.as_weak();
    let sh_relog = shared.clone();
    ui.on_relogin(move |idx| {
        let key = sh_relog.conn_dot_keys.borrow().get(idx.max(0) as usize).cloned();
        let Some(key) = key else { return };
        let cfg = engine::AccountConfig::load_all().into_iter().find(|a| a.account_key() == key);
        open_add_connection(ui_weak_relog.clone(), cfg);
    });

    let ui_weak_delc = ui.as_weak();
    let sh_delc = shared.clone();
    ui.on_delete_connection(move |idx| {
        let Some(ui) = ui_weak_delc.upgrade() else { return };
        let key = sh_delc.settings_conn_keys.borrow().get(idx.max(0) as usize).cloned();
        let Some(key) = key else { return };
        engine::AccountConfig::remove_account(&key);
        rebuild_engine(&ui, &sh_delc);
        refresh_connections(&ui, &sh_delc);
    });

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
                    format!(
                        "{}:{} · {}",
                        c.host,
                        c.port,
                        if c.use_tls { "TLS" } else { "без TLS" }
                    )
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
        refresh_connections(&ui, &sh_set);
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
    let sh_snz = shared.clone();
    ui.on_snooze_choice(move |choice| {
        let (eid, occ, occ_end, toast_id, summary) = sh_snz.snooze_ctx.borrow().clone();
        if eid != 0 {
            let now_ms = chrono::Utc::now().timestamp_millis();
            let at_start = choice == "atstart";
            let fire_at =
                if at_start { occ } else { now_ms + choice.parse::<i64>().unwrap_or(5) * 60_000 };
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
            sh_cc.calendar_colors.borrow_mut().insert(cal_id as i64, (*hex_color).to_string());
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
            "января",
            "февраля",
            "марта",
            "апреля",
            "мая",
            "июня",
            "июля",
            "августа",
            "сентября",
            "октября",
            "ноября",
            "декабря",
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
                AttRow { name: n.into(), status: st.into(), color: hex(col) }
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
            if ev.summary.is_empty() {
                "(без названия)".into()
            } else {
                ev.summary.clone()
            }
            .into(),
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
                // Голый хост тоже ссылка (CONFERENCE/X-…-CONFERENCE часто без
                // схемы) — схему достраивает handle_link на клике; признак и
                // клик считает одна функция (см. карточку правки).
                let is_link = click_target(&value, LinkOrigin::Text).is_some();
                EventExtraItem { label: label.into(), value: value.into(), is_link }
            })
            .collect();
        // Сравнивать с найденными в тексте ссылками надо по нормализованному
        // виду: в extras лежит голый хост, а extract_urls отдаёт его со схемой.
        let extra_urls: std::collections::HashSet<String> =
            extras.iter().filter(|x| x.is_link).filter_map(|x| link_target(&x.value)).collect();
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
            // Свойства события — плоский текст: и схему достроить, и хвостовую
            // пунктуацию срезать.
            handle_link(&ui, url.to_string(), LinkOrigin::Text);
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
            let ak = sh_rsvp.event_accounts.borrow().get(&(id as i64)).cloned().unwrap_or_default();
            let _ = etx.send(engine::EngineCmd::Rsvp {
                event_id: id as i64,
                partstat: partstat.to_string(),
                account_key: ak,
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
        let ak = sh_del.event_accounts.borrow().get(&id).cloned().unwrap_or_default();
        if let Some(etx) = sh_del.engine_tx.borrow().as_ref() {
            println!("delete event {id}");
            let _ = etx.send(engine::EngineCmd::DeleteEvent { event_id: id, account_key: ak });
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
        let (week_start_ms, _) = week_range_ms(sh_gc.calendar_week_start_days.get(), day_count);
        let start_ms = week_start_ms + day * day_ms + snapped * 60_000;
        open_create_form_at(&ui, &sh_gc, start_ms);
    });

    // Drag-to-move a block to a new day/time (writable calendars only — the
    // block's TouchArea won't even start a drag otherwise). The ghost's final
    // top-left (grid px) → nearest day column + 15-min-snapped start; duration
    // and all other fields are preserved.
    let ui_weak_gm = ui.as_weak();
    let sh_gm = shared.clone();
    ui.on_grid_event_moved(move |id, orig_x, _orig_y, new_x, new_y| {
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
                    eprintln!(
                        "[cal] move: no occurrence for id={id} day={orig_day} — drop ignored"
                    );
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
                Some(e) => {
                    (e.summary.clone(), e.description.clone(), e.location.clone(), e.all_day)
                }
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
        let ak = sh_gm.event_accounts.borrow().get(&(id as i64)).cloned().unwrap_or_default();
        if let Some(etx) = sh_gm.engine_tx.borrow().as_ref() {
            let _ = etx.send(engine::EngineCmd::PatchEvent {
                event_id: id as i64,
                body,
                account_key: ak,
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
                Some(e) => {
                    (e.summary.clone(), e.description.clone(), e.location.clone(), e.all_day)
                }
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
        let ak = sh_gr.event_accounts.borrow().get(&(id as i64)).cloned().unwrap_or_default();
        if let Some(etx) = sh_gr.engine_tx.borrow().as_ref() {
            let _ = etx.send(engine::EngineCmd::PatchEvent {
                event_id: id as i64,
                body,
                account_key: ak,
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
            SHARED.with(|s| {
                if let Some(sh) = s.borrow().as_ref() {
                    sh.pending_event_save.set(false);
                }
            });
            ui.set_edit_busy(false);
            ui.set_edit_error("".into());
            ui.set_edit_visible(false);
        }
    });
    ui.on_edit_open_url(move |url| {
        // Поля правки события — тот же плоский текст, что и в карточке
        // просмотра, и тот же белый список схем.
        match click_target(url.as_str(), LinkOrigin::Text) {
            Some(target) => open_external(&target),
            None => eprintln!("edit open url: нечего открывать — {url}"),
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
                let borrow = s.borrow();
                let Some(sh) = borrow.as_ref() else { return Vec::new() };
                let Some(c) = sh.cache.as_ref() else { return Vec::new() };
                // Видимость календаря проверяется здесь, в момент выстрела, а
                // не только при посеве и на переключателе: строка могла
                // взвестись до выключения и не попасть под purge (событие вне
                // загруженного окна, переезд между календарями). Строка
                // остаётся взведённой — включат календарь, зазвонит сама.
                let vis = sh.calendar_visible.borrow();
                let events = sh.calendar_events.borrow();
                let hidden = |row: &ddmail_core::cache::ReminderRow| -> bool {
                    let cal = if row.calendar_id != 0 {
                        row.calendar_id
                    } else {
                        // Строка старой схемы: календарь берём из снимка.
                        events
                            .iter()
                            .find(|e| e.id == row.event_id)
                            .map(|e| e.calendar_id)
                            .unwrap_or(0)
                    };
                    cal != 0 && !*vis.get(&cal).unwrap_or(&true)
                };
                reminders::scan(c, now_ms, &hidden)
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
                            toast_window::KIND_STARTED,
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
                            toast_window::KIND_SOON,
                            eid,
                            &title,
                            &body,
                            true,
                            reminders::SOON_TIMEOUT_SECS,
                            move || reminder_dispatch("cancel-occ", eid, occ, seq, String::new()),
                            move || reminder_dispatch("open-stay", eid, occ, seq, s_body.clone()),
                            move || {
                                reminder_dispatch("snooze-window", eid, occ, seq, s_act.clone())
                            },
                        );
                        toast_window::set_on_timeout(id, move || {
                            reminder_dispatch("timeout", eid, occ, seq, String::new())
                        });
                    }
                }
            }
        },
    );

    // Окно посева напоминаний — отдельно от сетки и привязано к `now`.
    // Пересчитывается на каждом тике, поэтому перекат суток (и недели) лечится
    // сам: раньше посев жил только на фетчах отображаемой недели, и клиент,
    // проживший выходные, всю следующую неделю тянул прошлую и молчал.
    // Тем же тиком сетка подтягивается за сегодняшним днём, если пользователь
    // её сам не увёл на другую неделю.
    let ui_weak_rw = ui.as_weak();
    let _reminder_window_timer = slint::Timer::default();
    _reminder_window_timer.start(
        slint::TimerMode::Repeated,
        std::time::Duration::from_secs(REMINDER_WINDOW_REFRESH_SECS),
        move || {
            let Some(ui) = ui_weak_rw.upgrade() else { return };
            SHARED.with(|s| {
                let borrow = s.borrow();
                let Some(sh) = borrow.as_ref() else { return };
                fetch_reminder_window(sh);
                let today = week_start_days_today();
                if sh.week_follows_today.get() && sh.calendar_week_start_days.get() != today {
                    println!("[cal] перекат недели: сетка идёт за сегодня → {today}");
                    sh.calendar_week_start_days.set(today);
                    apply_calendar_view(&ui, sh);
                    refetch_calendar_events(&ui, sh);
                }
            });
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
                // Точку НЕ гасим: она отражает факт непрочитанного и гаснет
                // сама, когда всё прочитано (tray_sync_dot по дельте).
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
                    // Точку НЕ гасим: она отражает факт непрочитанного и
                    // гаснет сама, когда всё прочитано (tray_sync_dot).
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

    // Первичная синхронизация точки: диалоги из кэша уже загружены, и письма,
    // пришедшие пока клиент был выключен, должны зажечь точку сразу — не
    // дожидаясь первой дельты движка (та её всё равно пере-подтвердит).
    #[cfg(any(windows, target_os = "linux"))]
    tray_sync_dot(&shared);

    // Set the window icon once the native window is realized (the HWND / X11
    // window doesn't exist yet here). Slint/winit doesn't pick up the exe's
    // embedded .ico (Windows) and sets no _NET_WM_ICON (X11), so the title
    // bar + taskbar would otherwise show the toolkit's default glyph. The
    // native handle appears some time after the loop starts (later on X11
    // than on Windows), so retry on a short ticker until it sticks.
    #[cfg(any(windows, target_os = "linux"))]
    {
        let icon_weak = ui.as_weak();
        let icon_timer = slint::Timer::default();
        let mut tries = 0u32;
        icon_timer.start(
            slint::TimerMode::Repeated,
            std::time::Duration::from_millis(100),
            move || {
                tries += 1;
                let done = icon_weak.upgrade().map(|ui| set_window_icon(&ui)).unwrap_or(true);
                if done || tries >= 50 {
                    // Defer the stop+drop out of the timer's own dispatch —
                    // never drop a Timer from inside its own callback (same
                    // rule as the toast windows).
                    let _ = slint::invoke_from_event_loop(|| {
                        ICON_TIMER.with(|t| {
                            if let Some(t) = t.borrow_mut().take() {
                                t.stop();
                            }
                        });
                    });
                }
            },
        );
        ICON_TIMER.with(|t| *t.borrow_mut() = Some(icon_timer));
    }

    // Show the window, then run the loop in "stay alive past the last window"
    // mode: closing the window via its ✕ returns CloseRequestResponse::HideWindow
    // (above), which hides it to the tray. `ui.run()` would quit the loop when
    // that last window hides; `run_event_loop_until_quit` keeps the process
    // alive in the tray until the tray's "Выход" calls `quit_event_loop`.
    ui.show().unwrap();
    slint::run_event_loop_until_quit().unwrap();
}

/// displays-index → sidebar model index (the transient compose row shifts
/// everything by one).
fn model_index(sh: &Shared, idx: usize) -> usize {
    if sh.pending_compose.borrow().is_some() { idx + 1 } else { idx }
}
