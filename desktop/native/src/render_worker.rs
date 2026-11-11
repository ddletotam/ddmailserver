//! Bubble render worker: lays the open conversation's mail bodies out with
//! `emlrender` off the UI thread and ships packed bitmaps back to it.
//!
//! Threads, and why each exists:
//!   * the coordinator (`spawn`) takes jobs off the channel, serves the RAM
//!     cache and drops a job a newer one has superseded (`seq`, latest wins);
//!   * per job, a scoped pool lays the cache misses out in parallel — one
//!     newsletter costs hundreds of ms of layout, and a conversation is
//!     dozens of them. `emlrender` lends every concurrent render its own text
//!     engine, so the threads do not queue on one font cache;
//!   * image fetchers. The first pass paints with only the remote images that
//!     are already in memory (`render::cached_images`), so a conversation
//!     appears without waiting on anyone's CDN; a mail that still lacks some
//!     goes to a fetcher, which downloads them, lays that one bubble out again
//!     and swaps the row in place (`apply_row_update`);
//!   * a disk writer PNG-encodes finished textures into the disk cache after
//!     the rows are already on screen.
//!
//! Link clicks never come through here: they are resolved on the UI thread
//! against the link rects shipped with the rows (`on_hit_test` in links.rs),
//! so a click is never stuck behind a render.

use std::collections::{HashMap, VecDeque};
use std::sync::atomic::{AtomicU64, AtomicUsize, Ordering};
use std::sync::{Arc, Mutex, mpsc};
use std::time::{Duration, Instant};

use slint::{ComponentHandle, Image, Model, ModelRc, Rgba8Pixel, SharedPixelBuffer, VecModel};

use ddmail_core::types::MessageBody;

use crate::render_common::{LinkRect, TextRun};
use crate::texture_cache::{self, TextureDiskCache};
use crate::{
    MainWindow, PENDING_FOLDER, RENDER_TEMPLATE_EPOCH, RowItem, SHARED, SelRect, build_body_html,
    build_source_html, build_text_only_html, mail_viewport, nudge_chat_scroll, policy,
    recipients_tip, render, sanitize,
};

pub(crate) enum Job {
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
        /// Image fetches of a superseded job are dropped the same way.
        seq: u64,
        /// After the rows land: None = keep scroll position (resize,
        /// policy toggle), Some(-1) = scroll to the end, Some(r) =
        /// scroll so row r (first unread) is at the top.
        scroll_to: Option<i32>,
        /// Per-body render mode, parallel to `bodies`: 0 = auto
        /// (HTML when present), 1 = force the text-only bubble.
        modes: Vec<u8>,
        /// UI window scale factor — emlrender rasterizes at this scale so
        /// the bitmap is 1:1 with physical pixels (crisp on HiDPI).
        scale: f32,
        /// Склеенный диалог: пузыри подписаны темой, у своих — подсказка с
        /// адресатами (контракт §4, «Склеенный диалог»).
        merged: bool,
        /// Окно переписки на момент отправки: (верх, высота) в CSS px. Нужно
        /// рендеру без `scroll_to` (ресайз, новое письмо), чтобы битмапы
        /// получили строки, которые сейчас на экране.
        view: (f32, f32),
    },
    /// Битмапы строк, вошедших в окно прокрутки или в запас вокруг него.
    /// `seq` — задание, чьи строки сейчас стоят в модели: просьба про старые
    /// строки пропускается.
    LoadRows { seq: u64, rows: Vec<usize> },
    /// Render the source/headers viewer text to a bitmap + word rects, so the
    /// modal reuses the fast bubble selection layer instead of Slint's
    /// (slow-on-large-text) TextInput.
    RenderSource { text: String, width: u32, scale: f32 },
}

/// Everything the render worker knows about a row besides its bitmap —
/// shipped to the UI thread to fill RowItem (context-menu data included).
struct RowMeta {
    has_html: bool,
    has_text: bool,
    viewing_html: bool,
    sender: String,
    media_host: String,
    m_sender_on: bool,
    m_host_on: bool,
    /// Подсказка с адресатами своего письма в склеенном диалоге; пусто —
    /// подсказки нет.
    recipients: String,
}

/// Texture key: (folder, uid, width, policy_gen, mode, content fingerprint).
/// The disk layer uses the same six fields (`TextureDiskCache::load/store`).
type Key = (String, u32, u32, u64, u8, u64);

/// A finished bubble: packed pixels (`SharedPixelBuffer` is Send + Sync,
/// unlike Slint's `Image`, so it is built here and only wrapped on the UI
/// thread), logical height in CSS px, and the geometry layers.
#[derive(Clone)]
struct Packed {
    buf: SharedPixelBuffer<Rgba8Pixel>,
    h: f32,
    links: Vec<LinkRect>,
    runs: Vec<TextRun>,
}

impl Packed {
    fn from_render(r: render::RenderResult) -> Self {
        // Logical (CSS px) display height: the bitmap is captured at `scale`
        // physical px per CSS px, and the Image box must stay in CSS px so
        // link/text rects keep mapping 1:1.
        let rscale = r.scale.max(0.25);
        let b = r.bitmap;
        let buf = SharedPixelBuffer::<Rgba8Pixel>::clone_from_slice(&b.rgba, b.width, b.height);
        Packed { buf, h: b.height as f32 / rscale, links: r.links, runs: r.runs }
    }
}

/// Finished bubbles in RAM, the oldest insert goes first (the disk layer
/// still has it — eviction only costs a PNG decode).
///
/// Предел — в байтах, а не в штуках: пузырь весит от сотни килобайт до
/// десятков мегабайт, и прежние 400 записей держали гигабайты. Модель строк
/// теперь хранит битмапы только окна прокрутки, так что кэш нужен для
/// возврата к недавним диалогам, а не для того, чтобы вместить весь открытый.
struct RamCache {
    map: HashMap<Key, Packed>,
    order: VecDeque<Key>,
    bytes: usize,
}

const RAM_BUDGET_BYTES: usize = 512 * 1024 * 1024;

fn packed_bytes(p: &Packed) -> usize {
    p.buf.width() as usize * p.buf.height() as usize * 4
}

impl RamCache {
    fn get(&self, k: &Key) -> Option<Packed> {
        self.map.get(k).cloned()
    }

    fn insert(&mut self, k: Key, p: Packed) {
        self.bytes += packed_bytes(&p);
        match self.map.insert(k.clone(), p) {
            Some(old) => self.bytes -= packed_bytes(&old),
            None => self.order.push_back(k),
        }
        while self.bytes > RAM_BUDGET_BYTES {
            let Some(old) = self.order.pop_front() else { break };
            if let Some(p) = self.map.remove(&old) {
                self.bytes -= packed_bytes(&p);
            }
        }
    }
}

/// A per-host image permission that can travel to a fetcher thread.
type Gate = Box<dyn Fn(&str) -> bool + Send + Sync>;

/// Remote images still to come for one bubble of a job.
struct ImageJob {
    seq: u64,
    row: usize,
    key: Key,
    /// The bubble document exactly as the first pass laid it out.
    html: String,
    gate: Gate,
    width: u32,
    viewport: Option<u32>,
    scale: f32,
}

/// What the threads of the worker share.
struct Ctx {
    ui: slint::Weak<MainWindow>,
    latest_seq: Arc<AtomicU64>,
    ram: Mutex<RamCache>,
    disk: Option<TextureDiskCache>,
    to_disk: Mutex<mpsc::Sender<(Key, Packed)>>,
    to_fetch: Mutex<mpsc::Sender<ImageJob>>,
    /// Точные высоты уже виденных пузырей: индекс высот диалога строится из
    /// них, не трогая ни битмапы, ни диск. Записи по несколько десятков байт —
    /// чистится целиком, если разрастётся.
    heights: Mutex<HashMap<Key, f32>>,
}

impl Ctx {
    fn stale(&self, seq: u64) -> bool {
        seq < self.latest_seq.load(Ordering::SeqCst)
    }

    /// Remember a finished bubble: RAM now, disk in the background. Pending-
    /// send stubs are transient — never persist their textures (the synthetic
    /// uid restarts every session and would collide).
    fn remember(&self, key: Key, p: &Packed) {
        self.note_height(&key, p.h);
        if key.0 != PENDING_FOLDER {
            let _ = self
                .to_disk
                .lock()
                .unwrap_or_else(|e| e.into_inner())
                .send((key.clone(), p.clone()));
        }
        self.ram.lock().unwrap_or_else(|e| e.into_inner()).insert(key, p.clone());
    }
}

impl Ctx {
    fn note_height(&self, key: &Key, h: f32) {
        let mut m = self.heights.lock().unwrap_or_else(|e| e.into_inner());
        if m.len() > 200_000 {
            m.clear();
        }
        m.insert(key.clone(), h);
    }

    /// Точная высота пузыря без его битмапа: память, RAM-кэш, диск (`.h`).
    fn known_height(&self, key: &Key) -> Option<f32> {
        if let Some(h) = self.heights.lock().unwrap_or_else(|e| e.into_inner()).get(key) {
            return Some(*h);
        }
        let h = self.ram.lock().unwrap_or_else(|e| e.into_inner()).get(key).map(|p| p.h).or_else(
            || {
                self.disk
                    .as_ref()
                    .and_then(|d| d.load_height(&key.0, key.1, key.2, key.3, key.4, key.5))
            },
        )?;
        self.note_height(key, h);
        Some(h)
    }
}

/// Layout threads per conversation. Leave a core to the UI thread; past four
/// the gain is eaten by memory bandwidth and one text engine per thread.
fn layout_threads() -> usize {
    std::thread::available_parallelism().map(|n| n.get().saturating_sub(1)).unwrap_or(1).clamp(1, 4)
}

/// Fetchers working at once. Each already downloads its own mail's images in
/// parallel inside the loader; more than one so that a single slow CDN does
/// not hold every other bubble's pictures. A fetch already in flight cannot
/// be cancelled — a superseded one finishes into the image cache (which the
/// next open of that conversation then hits) but is not laid out.
const FETCH_THREADS: usize = 3;

/// Start the worker. Returns the job channel; `latest_seq` is the counter
/// `send_render_job` bumps (see `Job::SetConversation::seq`).
pub(crate) fn spawn(
    ui: slint::Weak<MainWindow>,
    latest_seq: Arc<AtomicU64>,
    disk: Option<TextureDiskCache>,
) -> mpsc::Sender<Job> {
    let (tx, rx) = mpsc::channel::<Job>();
    let (disk_tx, disk_rx) = mpsc::channel::<(Key, Packed)>();
    let (fetch_tx, fetch_rx) = mpsc::channel::<ImageJob>();
    let ctx = Arc::new(Ctx {
        ui,
        latest_seq,
        ram: Mutex::new(RamCache { map: HashMap::new(), order: VecDeque::new(), bytes: 0 }),
        disk,
        to_disk: Mutex::new(disk_tx),
        to_fetch: Mutex::new(fetch_tx),
        heights: Mutex::new(HashMap::new()),
    });

    {
        let ctx = Arc::clone(&ctx);
        std::thread::spawn(move || {
            for (k, p) in disk_rx {
                if let Some(d) = ctx.disk.as_ref() {
                    d.store(
                        &k.0,
                        k.1,
                        k.2,
                        k.3,
                        k.4,
                        k.5,
                        p.buf.as_bytes(),
                        p.buf.width(),
                        p.buf.height(),
                        p.h,
                        &p.links,
                        &p.runs,
                    );
                }
            }
        });
    }

    let fetch_rx = Arc::new(Mutex::new(fetch_rx));
    for _ in 0..FETCH_THREADS {
        let ctx = Arc::clone(&ctx);
        let rx = Arc::clone(&fetch_rx);
        std::thread::spawn(move || {
            loop {
                let job = rx.lock().unwrap_or_else(|e| e.into_inner()).recv();
                let Ok(job) = job else { break };
                fetch_and_rerender(&ctx, job);
            }
        });
    }

    std::thread::spawn(move || {
        // Диалог, чьи строки сейчас в модели, и строки, которым фон ещё должен
        // точную высоту. Пока хвост не пуст, цикл не засыпает на `recv`, а
        // берёт по заходу между заданиями — любое задание важнее хвоста.
        let mut cur: Option<Conv> = None;
        let mut back: VecDeque<usize> = VecDeque::new();
        loop {
            let job = if back.is_empty() {
                match rx.recv() {
                    Ok(j) => Some(j),
                    Err(_) => break,
                }
            } else {
                match rx.try_recv() {
                    Ok(j) => Some(j),
                    Err(mpsc::TryRecvError::Empty) => None,
                    Err(mpsc::TryRecvError::Disconnected) => break,
                }
            };
            match job {
                Some(Job::SetConversation {
                    bodies,
                    width,
                    policy,
                    policy_gen,
                    seq,
                    scroll_to,
                    modes,
                    scale,
                    merged,
                    view,
                }) => {
                    if ctx.stale(seq) {
                        println!("[perf] render job seq={seq} superseded — skipped");
                        continue;
                    }
                    let mut conv =
                        Conv::new(bodies, width, policy, policy_gen, seq, modes, scale, merged);
                    back = open_conv(&ctx, &mut conv, scroll_to, view).unwrap_or_default();
                    cur = Some(conv);
                }
                Some(Job::LoadRows { seq, rows }) => {
                    if let Some(conv) = cur.as_mut().filter(|c| c.seq == seq && !ctx.stale(seq)) {
                        serve_rows(&ctx, conv, &rows);
                    }
                }
                Some(Job::RenderSource { text, width, scale }) => {
                    render_source(&ctx, &text, width, scale)
                }
                None => match cur.as_mut() {
                    Some(conv) if !ctx.stale(conv.seq) => background_step(&ctx, conv, &mut back),
                    _ => back.clear(),
                },
            }
        }
    });
    tx
}

/// Оценка высоты строки, которую ещё никто не раскладывал, когда в диалоге
/// нет ни одной известной.
const DEFAULT_ROW_H: f32 = 400.0;

/// Открытый диалог глазами воркера: что рисуем, ключи пузырей и высоты строк.
/// Битмапов здесь нет — они живут в кэшах и в модели (только для окна).
struct Conv {
    bodies: Vec<MessageBody>,
    width: u32,
    policy: policy::Policy,
    seq: u64,
    modes: Vec<u8>,
    scale: f32,
    merged: bool,
    keys: Vec<Key>,
    /// Высота строки в CSS px: точная, если `exact[i]`, иначе оценка.
    h: Vec<f32>,
    exact: Vec<bool>,
}

impl Conv {
    #[allow(clippy::too_many_arguments)]
    fn new(
        bodies: Vec<MessageBody>,
        width: u32,
        policy: policy::Policy,
        policy_gen: u64,
        seq: u64,
        modes: Vec<u8>,
        scale: f32,
        merged: bool,
    ) -> Self {
        let n = bodies.len();
        let keys = bodies
            .iter()
            .enumerate()
            .map(|(i, body)| {
                let mode = modes.get(i).copied().unwrap_or(0);
                let fp = fingerprint(body, scale, merged);
                (body.folder.clone(), body.uid, width, policy_gen, mode, fp)
            })
            .collect();
        Conv {
            bodies,
            width,
            policy,
            seq,
            modes,
            scale,
            merged,
            keys,
            h: vec![DEFAULT_ROW_H; n],
            exact: vec![false; n],
        }
    }

    fn mode(&self, i: usize) -> u8 {
        // Mode 1 = «Текстовая версия» override for this body.
        self.modes.get(i).copied().unwrap_or(0)
    }

    fn key(&self, i: usize) -> Key {
        self.keys[i].clone()
    }

    /// Context-menu data: per-sender / per-host checkbox states reflect the
    /// policy this job rendered under (a toggle re-renders anyway).
    fn meta(&self, i: usize) -> RowMeta {
        let body = &self.bodies[i];
        let policy = &self.policy;
        let (has_html, has_text) = content_of(body);
        let force_text = self.mode(i) == 1 && has_html;
        let sender_lc = body.from_addr.to_lowercase();
        let media_host = sanitize::first_external_host(body.html.as_deref().unwrap_or(""));
        RowMeta {
            recipients: if self.merged && body.is_outgoing {
                recipients_tip(body)
            } else {
                String::new()
            },
            has_html,
            has_text,
            viewing_html: has_html && !force_text,
            m_sender_on: policy.allow_media.contains(&sender_lc),
            m_host_on: !media_host.is_empty()
                && (policy.media_hosts.contains(&media_host)
                    || policy.allow_domains.contains(&media_host)),
            sender: body.from_addr.clone(),
            media_host,
        }
    }
}

fn content_of(body: &MessageBody) -> (bool, bool) {
    let has_html = body.html.as_deref().map(|s| !s.trim().is_empty()).unwrap_or(false);
    let has_text = body.text.as_deref().map(|s| !s.trim().is_empty()).unwrap_or(false);
    (has_html, has_text)
}

/// Content fingerprint: bodies are mostly immutable, but cid:→data: healing
/// rewrites the HTML — the texture must miss when the content changed. The
/// rasterization scale is folded in too: the same width at a different DPI
/// is a different bitmap.
fn fingerprint(body: &MessageBody, scale: f32, merged: bool) -> u64 {
    // Attachments тоже входят: чипы — часть пузыря (build_body_html), а
    // серверный список вложений может поменяться при неизменном HTML (фикс
    // inline-фильтра) — иначе старая текстура без чипа наслуживается с диска
    // вечно.
    let mut att_fp: u64 = 0;
    for a in &body.attachments {
        att_fp = att_fp.rotate_left(7)
            ^ texture_cache::fnv1a(&format!("{}|{}|{}", a.index, a.filename, a.size));
    }
    // Текст — обязательная часть отпечатка, а не «на всякий случай»: пустую
    // строку кэша движок теперь перезапрашивает (engine::body_is_blank), и у
    // текстового письма без вложений после перезапроса HTML так и остаётся
    // пустым. Без текста в fp отпечаток совпал бы с прежним, и с диска
    // наслужилась бы та самая пустая текстура — база вылечилась, а пузырь
    // остался пустым.
    texture_cache::fnv1a(body.html.as_deref().unwrap_or(""))
        .wrapping_add(RENDER_TEMPLATE_EPOCH.wrapping_mul(0x9E37_79B9_7F4A_7C15))
        ^ att_fp
        ^ texture_cache::fnv1a(body.text.as_deref().unwrap_or("")).rotate_left(21)
        ^ ((scale.to_bits() as u64) << 32)
        // Подпись темой — тоже содержимое пузыря: тот же текст в склейке и вне
        // её — разные битмапы.
        ^ if merged {
            texture_cache::fnv1a(&format!("subj|{}", body.subject)).rotate_left(43)
        } else {
            0
        }
}

/// How one row of the first pass came about (for the perf line).
#[derive(Clone, Copy, PartialEq)]
enum Source {
    Ram,
    Disk,
    Layout,
    /// Laid out, but the HTML broke and the text version stands in.
    Fallback,
}

/// First pass for one row that missed the RAM cache: the disk layer, or a
/// layout with the images already in memory. `Some(job)` = remote images are
/// still missing and the caller should hand the job to a fetcher.
fn first_pass(ctx: &Ctx, job: &Conv, i: usize) -> (Packed, Source, Option<ImageJob>) {
    let body = &job.bodies[i];
    let key = job.key(i);
    if let Some(de) =
        ctx.disk.as_ref().and_then(|t| t.load(&key.0, key.1, key.2, key.3, key.4, key.5))
    {
        // Disk layer: rendered in a previous session — a PNG decode instead
        // of a layout pass.
        let buf = SharedPixelBuffer::<Rgba8Pixel>::clone_from_slice(&de.rgba, de.width, de.height);
        let p = Packed { buf, h: de.h, links: de.links, runs: de.runs };
        ctx.ram.lock().unwrap_or_else(|e| e.into_inner()).insert(key, p.clone());
        return (p, Source::Disk, None);
    }
    let (has_html, has_text) = content_of(body);
    let force_text = job.mode(i) == 1 && has_html;
    // Try the full HTML (unless the text view is forced). If the layout broke
    // on this mail (`successful()` explains the signal) we retry with the
    // text-only bubble — keeps "missing bubble" failures from being silent.
    let html = if force_text {
        build_text_only_html(body, job.merged)
    } else {
        build_body_html(body, &job.policy, job.merged)
    };
    // Текстовой версии картинки не положены вовсе.
    let gate: Gate = if force_text {
        Box::new(render::no_remote)
    } else {
        Box::new(job.policy.media_gate(&body.from_addr))
    };
    let (images, complete) = render::cached_images(&html, &*gate);
    let viewport = (!force_text).then(|| mail_viewport(job.width, body));
    let result = render::render_with(&html, job.width, viewport, job.scale, &images);
    let (result, source, complete) = if !result.successful() && has_text && !force_text {
        let text_html = build_text_only_html(body, job.merged);
        (
            render::render(&text_html, job.width, job.scale, &render::no_remote),
            Source::Fallback,
            true,
        )
    } else {
        (result, Source::Layout, complete)
    };
    // A failed layout (see `successful()`) must NOT be cached — otherwise the
    // degenerate 1px bitmap sticks on disk and every later open serves that
    // instead of retrying. We still return it for this pass (an empty bubble
    // beats a missing one), just don't persist it. Neither is a bubble still
    // waiting for its pictures cached: the fetcher stores the finished one.
    let succeeded = result.successful();
    let p = Packed::from_render(result);
    println!(
        "[perf]   body uid={} h={}px links={} runs={} cached={} images={}",
        body.uid,
        p.buf.height(),
        p.links.len(),
        p.runs.len(),
        succeeded && complete,
        if complete { "all" } else { "pending" },
    );
    let fetch = if !succeeded {
        None
    } else if complete {
        ctx.remember(key, &p);
        None
    } else {
        Some(ImageJob {
            seq: job.seq,
            row: i,
            key,
            html,
            gate,
            width: job.width,
            viewport,
            scale: job.scale,
        })
    };
    (p, source, fetch)
}

/// Строки одним заходом пула: RAM-попадания сразу, остальные — диск или
/// раскладка в `layout_threads()` потоков. Задание, устаревшее посреди
/// работы, останавливает пул в пределах пузыря на поток — тогда результат
/// неполный, и вызывающий проверяет `ctx.stale` сам.
fn load_rows(
    ctx: &Ctx,
    conv: &Conv,
    rows: &[usize],
) -> Vec<(usize, Packed, Source, Option<ImageJob>)> {
    let mut out = Vec::new();
    let mut misses = Vec::new();
    {
        let ram = ctx.ram.lock().unwrap_or_else(|e| e.into_inner());
        for &i in rows {
            match ram.get(&conv.keys[i]) {
                Some(p) => out.push((i, p, Source::Ram, None)),
                None => misses.push(i),
            }
        }
    }
    if misses.is_empty() {
        return out;
    }
    let next = AtomicUsize::new(0);
    let results = Mutex::new(Vec::new());
    let threads = layout_threads().min(misses.len());
    std::thread::scope(|s| {
        for _ in 0..threads {
            s.spawn(|| {
                loop {
                    if ctx.stale(conv.seq) {
                        return;
                    }
                    let k = next.fetch_add(1, Ordering::SeqCst);
                    let Some(&i) = misses.get(k) else { return };
                    let (p, source, fetch) = first_pass(ctx, conv, i);
                    results.lock().unwrap_or_else(|e| e.into_inner()).push((i, p, source, fetch));
                }
            });
        }
    });
    out.extend(results.into_inner().unwrap_or_else(|e| e.into_inner()));
    out
}

/// Строки, которые окажутся на экране: при `scroll_to` — там, куда встанет
/// прокрутка открытия (`-1` — низ, `r` — строка r наверху), без него — в
/// окне с верхом `view_top`. Плюс по строке с краёв — на дробную прокрутку.
fn window_rows(h: &[f32], scroll_to: Option<i32>, view_top: f32, view_h: f32) -> Vec<usize> {
    let n = h.len();
    if n == 0 {
        return Vec::new();
    }
    let total: f32 = h.iter().sum();
    let top = match scroll_to {
        Some(r) if r < 0 => (total - view_h).max(0.0),
        Some(r) => h.iter().take(r as usize).sum(),
        None => view_top.clamp(0.0, (total - view_h).max(0.0)),
    };
    let bottom = top + view_h;
    let mut out = Vec::new();
    let mut y = 0.0;
    for (i, &hi) in h.iter().enumerate() {
        if y + hi > top && y < bottom {
            out.push(i);
        }
        y += hi;
    }
    if let (Some(&first), Some(&last)) = (out.first(), out.last()) {
        if first > 0 {
            out.insert(0, first - 1);
        }
        if last + 1 < n {
            out.push(last + 1);
        }
    }
    out
}

/// Открытие (и любая перерисовка) диалога. Высоты всех строк — из индекса
/// (`Ctx::known_height`) или оценкой, битмапы — только окну, которое увидит
/// пользователь: стоимость открытия не растёт с длиной диалога (контракт
/// §4в-бис). Возвращает строки без точной высоты, ближние к окну первыми, —
/// их доделывает фон; `None` — задание устарело.
fn open_conv(
    ctx: &Ctx,
    conv: &mut Conv,
    scroll_to: Option<i32>,
    view: (f32, f32),
) -> Option<VecDeque<usize>> {
    let t_wall = Instant::now();
    let n = conv.bodies.len();

    let mut known = 0;
    for i in 0..n {
        if let Some(h) = ctx.known_height(&conv.keys[i]) {
            conv.h[i] = h;
            conv.exact[i] = true;
            known += 1;
        }
    }
    // Неизвестным — медиана известных этого же диалога: письма одного
    // отправителя обычно похожи, и прокрутка меньше прыгает, когда фон
    // доложит точные.
    let mut hs: Vec<f32> = (0..n).filter(|&i| conv.exact[i]).map(|i| conv.h[i]).collect();
    hs.sort_by(|a, b| a.partial_cmp(b).unwrap_or(std::cmp::Ordering::Equal));
    let estimate = hs.get(hs.len() / 2).copied().unwrap_or(DEFAULT_ROW_H);
    for i in 0..n {
        if !conv.exact[i] {
            conv.h[i] = estimate;
        }
    }

    // Окно может уточняться: строка, разложенная только что, получила
    // точную высоту, и экран покрыли другие строки. Несколько заходов
    // сходятся — каждый добирает только недостающие.
    let view_h = if view.1 > 1.0 { view.1 } else { 900.0 };
    let mut slots: Vec<Option<Packed>> = vec![None; n];
    let mut fetches: Vec<ImageJob> = Vec::new();
    let mut by_source = [0usize; 4];
    let t_layout = Instant::now();
    let mut window = 0;
    for _ in 0..8 {
        let missing: Vec<usize> = window_rows(&conv.h, scroll_to, view.0, view_h)
            .into_iter()
            .filter(|&i| slots[i].is_none())
            .collect();
        if missing.is_empty() {
            break;
        }
        let got = load_rows(ctx, conv, &missing);
        if ctx.stale(conv.seq) {
            println!("[perf] render job seq={} aborted mid-render — newer job queued", conv.seq);
            let _ = ctx.ui.upgrade_in_event_loop(move |ui| {
                ui.set_render_total(0);
                ui.set_render_progress(0);
            });
            return None;
        }
        for (i, p, source, fetch) in got {
            conv.h[i] = p.h;
            conv.exact[i] = true;
            by_source[source as usize] += 1;
            slots[i] = Some(p);
            fetches.extend(fetch);
            window += 1;
        }
    }
    let layout_ms = t_layout.elapsed().as_millis();
    println!(
        "[perf] render N={n} window={window} width={}px known_h={known} cache_hits={} \
         disk_hits={} laid_out={} fallback={} images_pending={} layout={layout_ms}ms total_job={}ms",
        conv.width,
        by_source[Source::Ram as usize],
        by_source[Source::Disk as usize],
        by_source[Source::Layout as usize],
        by_source[Source::Fallback as usize],
        fetches.len(),
        t_wall.elapsed().as_millis()
    );

    let rows: Vec<RowOut> = slots
        .into_iter()
        .enumerate()
        .map(|(i, packed)| RowOut { h: conv.h[i], packed, meta: conv.meta(i) })
        .collect();
    install_rows(ctx, conv.seq, rows, conv.width, scroll_to);

    // Only now: the rows are queued for the UI ahead of any update a fetcher
    // could send for them (one event loop, first in first out).
    let to_fetch = ctx.to_fetch.lock().unwrap_or_else(|e| e.into_inner());
    for f in fetches {
        let _ = to_fetch.send(f);
    }

    let win = window_rows(&conv.h, scroll_to, view.0, view_h);
    let center = win.get(win.len() / 2).copied().unwrap_or(n.saturating_sub(1));
    let mut back: Vec<usize> = (0..n).filter(|&i| !conv.exact[i]).collect();
    back.sort_by_key(|&i| i.abs_diff(center));
    Some(back.into())
}

/// Фон: точные высоты строкам, которых ещё никто не раскладывал, по заходу
/// в `layout_threads()` строк. Между заходами цикл воркера смотрит очередь,
/// так что просьба UI или другой диалог не ждут весь хвост.
fn background_step(ctx: &Ctx, conv: &mut Conv, back: &mut VecDeque<usize>) {
    let mut batch = Vec::new();
    while batch.len() < layout_threads() {
        let Some(i) = back.pop_front() else { break };
        if !conv.exact[i] {
            batch.push(i);
        }
    }
    if batch.is_empty() {
        return;
    }
    deliver(ctx, conv, &batch);
    if ctx.stale(conv.seq) {
        back.clear();
    }
}

/// Просьба UI: битмапы строк, вошедших в окно прокрутки.
fn serve_rows(ctx: &Ctx, conv: &mut Conv, rows: &[usize]) {
    let rows: Vec<usize> = rows.iter().copied().filter(|&i| i < conv.bodies.len()).collect();
    deliver(ctx, conv, &rows);
}

/// Загрузить строки и отдать их UI: высоту — всегда, битмап — если строка в
/// окне (решает `apply_row_update` на UI-потоке, там видно, где прокрутка).
fn deliver(ctx: &Ctx, conv: &mut Conv, rows: &[usize]) {
    let got = load_rows(ctx, conv, rows);
    if ctx.stale(conv.seq) {
        return;
    }
    let mut updates = Vec::with_capacity(got.len());
    let to_fetch = ctx.to_fetch.lock().unwrap_or_else(|e| e.into_inner());
    for (i, p, _, fetch) in got {
        conv.h[i] = p.h;
        conv.exact[i] = true;
        if let Some(f) = fetch {
            let _ = to_fetch.send(f);
        }
        updates.push((i, p));
    }
    drop(to_fetch);
    let seq = conv.seq;
    let latest = Arc::clone(&ctx.latest_seq);
    let _ = ctx.ui.upgrade_in_event_loop(move |ui| {
        if seq != latest.load(Ordering::SeqCst) {
            return;
        }
        for (row, p) in updates {
            apply_row_update(&ui, row, p);
        }
    });
}

/// Строка для модели: высота есть всегда, битмап — только у окна.
struct RowOut {
    h: f32,
    packed: Option<Packed>,
    meta: RowMeta,
}

/// Hand a finished first pass to the UI thread: one model swap for the whole
/// conversation, so bubbles appear together and in order, and the scroll
/// anchor is computed from final row heights.
fn install_rows(ctx: &Ctx, seq: u64, rows: Vec<RowOut>, width: u32, scroll_to: Option<i32>) {
    let rects_width = width as f32;
    let _ = ctx.ui.upgrade_in_event_loop(move |ui| {
        let t_ui = Instant::now();
        // The width these rows' link/text rects were extracted at — Slint
        // maps mouse coords into (and highlight rects out of) this space, so
        // hits stay exact even while the column width drifts from the render
        // width (resize-debounce window).
        ui.set_body_rects_width(rects_width);
        let (links, runs): (Vec<_>, Vec<_>) = rows
            .iter()
            .map(|r| {
                r.packed.as_ref().map(|p| (p.links.clone(), p.runs.clone())).unwrap_or_default()
            })
            .unzip();
        SHARED.with(|s| {
            if let Some(sh) = s.borrow().as_ref() {
                *sh.row_links.borrow_mut() = links;
                *sh.row_text_runs.borrow_mut() = runs;
                // Эти строки — задания `seq`: просьбы окна идут с ним же.
                sh.rows_seq.set(seq);
                sh.rows_requested.borrow_mut().clear();
                // Rows are being replaced — any active selection now points
                // at stale indices.
                sh.selection.sel_row.set(-1);
                ui.set_selection_row(-1);
            }
        });
        // Wrap each SharedPixelBuffer in an Image — cheap (refcount bump, no
        // memcpy) and the only step that has to run on the UI thread.
        let rows: Vec<RowItem> = rows
            .into_iter()
            .map(|RowOut { h, packed, meta: m }| RowItem {
                img: packed.map(|p| Image::from_rgba8(p.buf)).unwrap_or_default(),
                h,
                has_html: m.has_html,
                has_text: m.has_text,
                viewing_html: m.viewing_html,
                sender: m.sender.into(),
                media_host: m.media_host.into(),
                m_sender_on: m.m_sender_on,
                m_host_on: m.m_host_on,
                recipients: m.recipients.into(),
            })
            .collect();
        // Post-open scroll target: y offset of the first unread row, or
        // "very far down" for scroll-to-end (the Slint bridge clamps to the
        // content range).
        let scroll_y: Option<f32> = scroll_to.map(|sr| {
            if sr < 0 { 1.0e9 } else { rows.iter().take(sr as usize).map(|r| r.h).sum() }
        });
        // Точная высота содержимого для полосы прокрутки. У ListView
        // viewport-height — оценка по средней высоте элемента, а пузыри
        // различаются на два порядка: ползунок ездил не туда и менял размер на
        // ходу. Сумму мы и так уже считаем строкой выше.
        let content_h: f32 = rows.iter().map(|r| r.h).sum();
        ui.set_chat_content_h(content_h);
        let n = rows.len();
        ui.set_messages(ModelRc::new(VecModel::from(rows)));
        println!("[perf] install rows={n} ui={}ms", t_ui.elapsed().as_millis());
        // Окно вокруг экрана догружается по прокрутке (`sync_chat_window`), но
        // перерисовка без прокрутки (ресайз, новое письмо) вьюпорт не двигает —
        // спросим сами, когда раскладка доедет.
        let weak = ui.as_weak();
        slint::Timer::single_shot(Duration::from_millis(30), move || {
            if let Some(ui) = weak.upgrade() {
                sync_chat_window(&ui);
            }
        });
        // Hide the progress bar.
        ui.set_render_total(0);
        ui.set_render_progress(0);
        if let Some(y) = scroll_y {
            ui.set_chat_scroll_y(y);
            ui.set_chat_scroll_seq(ui.get_chat_scroll_seq() + 1);
            // Панель почты могла создаваться прямо сейчас: клик по тосту о
            // новом письме сам переключает view-mode в 0 и тут же открывает
            // диалог, так что этот bump уходит в ещё не существующий мост.
            // Живой панели повторы ничего не стоят — позиция уже доехала, и
            // они молчат.
            nudge_chat_scroll(ui.as_weak(), y, 120);
            nudge_chat_scroll(ui.as_weak(), y, 400);
        }
        // Scroll-less render (width change, scale change, policy toggle) НЕ
        // трогает chat-scroll-pending: раньше он его сбрасывал, но на старте
        // scale-render прилетает между анкор-bump'ом и первым layout — и
        // убивал ещё-не-применённый скролл (первый диалог открывался в
        // начале, а не на свежем письме). После применения layout сам гасит
        // флаг в viewport-height handler, так что повторного re-anchor нет.
    });
}

/// Fetcher body: download one bubble's missing images, lay it out again with
/// them and swap the row in. Dropped at every step where its job went stale.
fn fetch_and_rerender(ctx: &Ctx, job: ImageJob) {
    if ctx.stale(job.seq) {
        return;
    }
    let t = Instant::now();
    let images = render::fetch_images(&job.html, &*job.gate);
    let fetch_ms = t.elapsed().as_millis();
    if ctx.stale(job.seq) {
        // The download still warmed the image cache: the next first pass of
        // this mail paints it straight away.
        return;
    }
    let result = render::render_with(&job.html, job.width, job.viewport, job.scale, &images);
    if !result.successful() {
        // Keep the first-pass bubble, placeholders and all.
        return;
    }
    let p = Packed::from_render(result);
    println!(
        "[perf]   images row={} uid={} fetch={fetch_ms}ms total={}ms h={}px",
        job.row,
        job.key.1,
        t.elapsed().as_millis(),
        p.buf.height()
    );
    // The finished bubble is what the caches keep — the same key the first
    // pass would have used, so the next open finds it in RAM or on disk.
    ctx.remember(job.key, &p);
    let (seq, row) = (job.seq, job.row);
    let latest = Arc::clone(&ctx.latest_seq);
    let _ = ctx.ui.upgrade_in_event_loop(move |ui| {
        // Same check on arrival: a newer job may have replaced the rows while
        // this one was being laid out.
        if seq != latest.load(Ordering::SeqCst) {
            return;
        }
        apply_row_update(&ui, row, p);
    });
}

/// Swap one row's bitmap for the version with its images, keeping what the
/// user is looking at where it is (contract §4, «Куда встаёт скролл диалога»).
fn apply_row_update(ui: &MainWindow, row: usize, p: Packed) {
    let model = ui.get_messages();
    let Some(vm) = model.as_any().downcast_ref::<VecModel<RowItem>>() else { return };
    let Some(mut item) = vm.row_data(row) else { return };
    let old_h = item.h;
    let dh = p.h - old_h;
    let row_top: f32 = (0..row).filter_map(|i| vm.row_data(i)).map(|r| r.h).sum();
    // Битмап нужен строке в окне (или уже показанной — её заменяем версией
    // с картинками); дальней хватит высоты, битмап подтянет прокрутка.
    let (keep_a, keep_b) = keep_span(ui);
    if item.img.size().width > 0 || (row_top + p.h > keep_a && row_top < keep_b) {
        item.img = Image::from_rgba8(p.buf);
    }
    item.h = p.h;
    vm.set_row_data(row, item);
    SHARED.with(|s| {
        if let Some(sh) = s.borrow().as_ref() {
            sh.rows_requested.borrow_mut().remove(&row);
            if let Some(l) = sh.row_links.borrow_mut().get_mut(row) {
                *l = p.links;
            }
            if let Some(r) = sh.row_text_runs.borrow_mut().get_mut(row) {
                *r = p.runs;
            }
            // Word rects of this row moved — a selection in it would
            // highlight the wrong words.
            if sh.selection.sel_row.get() == row as i32 {
                sh.selection.sel_row.set(-1);
                ui.set_selection_row(-1);
                ui.set_selection_rects(ModelRc::new(VecModel::from(Vec::<SelRect>::new())));
            }
        }
    });
    if dh.abs() < 0.5 {
        return;
    }
    ui.set_chat_content_h(ui.get_chat_content_h() + dh);
    if ui.get_chat_scroll_pending() {
        // The open anchor has not been applied yet: it is a row's top, and a
        // row above it just changed height. "To the end" (1e9) needs nothing.
        let y = ui.get_chat_scroll_y();
        if y < 1.0e8 && row_top < y {
            ui.set_chat_scroll_y(y + dh);
        }
        return;
    }
    // A bubble wholly above the viewport grew or shrank: shift the view by
    // the same amount, or everything on screen would slide by `dh`. A bubble
    // in view or below changes height downwards from its top, which moves
    // nothing above it.
    let vp_top = -ui.get_chat_vp_y();
    if vp_top > 0.0 && row_top + old_h <= vp_top + 0.5 {
        let target = vp_top + dh;
        ui.set_chat_scroll_y(target);
        ui.set_chat_scroll_seq(ui.get_chat_scroll_seq() + 1);
        // The bridge raises chat-scroll-pending to re-apply after the next
        // layout. Here the relayout may already have happened, and a flag
        // left up would yank the view back to `target` on some later,
        // unrelated content change, after the user has scrolled away.
        let weak = ui.as_weak();
        slint::Timer::single_shot(Duration::from_millis(100), move || {
            if let Some(ui) = weak.upgrade() {
                if (-ui.get_chat_vp_y() - target).abs() <= 1.0 {
                    ui.set_chat_scroll_pending(false);
                }
            }
        });
    }
}

/// Запас вокруг экрана в высотах экрана: битмапы просим для
/// [верх − WANT, низ + WANT], выгружаем вне [верх − KEEP, низ + KEEP].
/// Разрыв между ними — чтобы строка на границе не моргала туда-обратно.
const WANT_SCREENS: f32 = 1.0;
const KEEP_SCREENS: f32 = 3.0;

fn view_span(ui: &MainWindow) -> (f32, f32) {
    let top = -ui.get_chat_vp_y();
    let h = ui.get_chat_view_h().max(300.0);
    (top, h)
}

fn keep_span(ui: &MainWindow) -> (f32, f32) {
    let (top, h) = view_span(ui);
    (top - KEEP_SCREENS * h, top + h + KEEP_SCREENS * h)
}

/// Держать битмапы только у строк возле экрана: недостающие попросить у
/// воркера, далёкие выгрузить (высота и геометрия ссылок остаются). Зовётся
/// на каждое движение прокрутки и после установки строк — проход по модели
/// дешёвый, сотни строк — это микросекунды.
pub(crate) fn sync_chat_window(ui: &MainWindow) {
    // Анкор открытия ещё не применён — вьюпорт показывает не то место, куда
    // встанет прокрутка, и просьбы по нему были бы мимо.
    if ui.get_chat_scroll_pending() {
        return;
    }
    let model = ui.get_messages();
    let Some(vm) = model.as_any().downcast_ref::<VecModel<RowItem>>() else { return };
    let (top, h) = view_span(ui);
    let (want_a, want_b) = (top - WANT_SCREENS * h, top + h + WANT_SCREENS * h);
    let (keep_a, keep_b) = keep_span(ui);
    SHARED.with(|s| {
        let Some(sh) = s.borrow().as_ref().cloned() else { return };
        let mut want = Vec::new();
        {
            let mut requested = sh.rows_requested.borrow_mut();
            let mut y = 0.0;
            for i in 0..vm.row_count() {
                let Some(mut r) = vm.row_data(i) else { continue };
                let (a, b) = (y, y + r.h);
                y = b;
                let has = r.img.size().width > 0;
                if b > want_a && a < want_b {
                    if !has && requested.insert(i) {
                        want.push(i);
                    }
                } else if has && !(b > keep_a && a < keep_b) {
                    r.img = Image::default();
                    vm.set_row_data(i, r);
                }
            }
        }
        if !want.is_empty() {
            let _ = sh.tx.send(crate::Job::LoadRows { seq: sh.rows_seq.get(), rows: want });
        }
    });
}

/// Render the viewer text the same way as a bubble: a bitmap + word rects.
/// The modal then selects via the fast Rust text-run layer, not Slint's
/// TextInput.
fn render_source(ctx: &Ctx, text: &str, width: u32, scale: f32) {
    let html = build_source_html(text);
    let p = Packed::from_render(render::render(&html, width, scale, &render::no_remote));
    println!("[perf] source render {}x{} runs={}", p.buf.width(), p.buf.height(), p.runs.len());
    let _ = ctx.ui.upgrade_in_event_loop(move |ui| {
        SHARED.with(|s| {
            if let Some(sh) = s.borrow().as_ref() {
                *sh.selection.src_runs.borrow_mut() = p.runs;
                sh.selection.src_sel_moved.set(false);
                sh.selection.src_sel_dragging.set(false);
            }
        });
        ui.set_source_img(Image::from_rgba8(p.buf));
        ui.set_source_img_h(p.h);
        ui.set_source_selection_rects(ModelRc::new(VecModel::from(Vec::<SelRect>::new())));
    });
}

#[cfg(test)]
mod window_tests {
    use super::window_rows;

    /// Открытие «в конец»: экран покрывают последние строки, плюс одна сверху
    /// на дробную прокрутку. Хвост длинного диалога не тянет за собой начало.
    #[test]
    fn bottom_anchor_takes_only_the_tail() {
        let h = vec![300.0; 500];
        assert_eq!(window_rows(&h, Some(-1), 0.0, 900.0), vec![496, 497, 498, 499]);
    }

    /// Анкор на строку: она встаёт наверх, окно идёт вниз от неё.
    #[test]
    fn row_anchor_starts_at_that_row() {
        let h = vec![300.0; 500];
        assert_eq!(window_rows(&h, Some(100), 0.0, 900.0), vec![99, 100, 101, 102, 103]);
    }

    /// Без анкора — текущее окно; верх за пределами содержимого клампится.
    #[test]
    fn no_anchor_follows_the_viewport() {
        let h = vec![100.0, 500.0, 100.0, 100.0];
        assert_eq!(window_rows(&h, None, 120.0, 200.0), vec![0, 1, 2]);
        assert_eq!(window_rows(&h, None, 1.0e9, 200.0), vec![1, 2, 3]);
    }

    #[test]
    fn short_and_empty_conversations() {
        assert_eq!(window_rows(&[50.0, 50.0], Some(-1), 0.0, 900.0), vec![0, 1]);
        assert!(window_rows(&[], Some(-1), 0.0, 900.0).is_empty());
    }
}
