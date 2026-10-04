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
    build_source_html, build_text_only_html, nudge_chat_scroll, policy, recipients_tip, render,
    sanitize,
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
    },
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

/// Finished bubbles in RAM. Bitmaps are megabytes each, so the entry count is
/// capped and the oldest insert goes first (the disk layer still has it —
/// eviction only costs a PNG decode).
struct RamCache {
    map: HashMap<Key, Packed>,
    order: VecDeque<Key>,
}

const RAM_CAP: usize = 400;

impl RamCache {
    fn get(&self, k: &Key) -> Option<Packed> {
        self.map.get(k).cloned()
    }

    fn insert(&mut self, k: Key, p: Packed) {
        if self.map.insert(k.clone(), p).is_none() {
            self.order.push_back(k);
        }
        while self.order.len() > RAM_CAP {
            if let Some(old) = self.order.pop_front() {
                self.map.remove(&old);
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
}

impl Ctx {
    fn stale(&self, seq: u64) -> bool {
        seq < self.latest_seq.load(Ordering::SeqCst)
    }

    /// Remember a finished bubble: RAM now, disk in the background. Pending-
    /// send stubs are transient — never persist their textures (the synthetic
    /// uid restarts every session and would collide).
    fn remember(&self, key: Key, p: &Packed) {
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
        ram: Mutex::new(RamCache { map: HashMap::new(), order: VecDeque::new() }),
        disk,
        to_disk: Mutex::new(disk_tx),
        to_fetch: Mutex::new(fetch_tx),
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
        for job in rx {
            match job {
                Job::SetConversation {
                    bodies,
                    width,
                    policy,
                    policy_gen,
                    seq,
                    scroll_to,
                    modes,
                    scale,
                    merged,
                } => {
                    if ctx.stale(seq) {
                        println!("[perf] render job seq={seq} superseded — skipped");
                        continue;
                    }
                    let job = ConvJob {
                        bodies: &bodies,
                        width,
                        policy: &policy,
                        policy_gen,
                        seq,
                        modes: &modes,
                        scale,
                        merged,
                    };
                    render_conversation(&ctx, &job, scroll_to);
                }
                Job::RenderSource { text, width, scale } => {
                    render_source(&ctx, &text, width, scale)
                }
            }
        }
    });
    tx
}

/// One `Job::SetConversation`, borrowed for the pool.
struct ConvJob<'a> {
    bodies: &'a [MessageBody],
    width: u32,
    policy: &'a policy::Policy,
    policy_gen: u64,
    seq: u64,
    modes: &'a [u8],
    scale: f32,
    merged: bool,
}

impl ConvJob<'_> {
    fn mode(&self, i: usize) -> u8 {
        // Mode 1 = «Текстовая версия» override for this body.
        self.modes.get(i).copied().unwrap_or(0)
    }

    fn key(&self, i: usize) -> Key {
        let body = &self.bodies[i];
        let fp = fingerprint(body, self.scale, self.merged);
        (body.folder.clone(), body.uid, self.width, self.policy_gen, self.mode(i), fp)
    }

    /// Context-menu data: per-sender / per-host checkbox states reflect the
    /// policy this job rendered under (a toggle re-renders anyway).
    fn meta(&self, i: usize) -> RowMeta {
        let body = &self.bodies[i];
        let policy = self.policy;
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
fn first_pass(ctx: &Ctx, job: &ConvJob, i: usize) -> (Packed, Source, Option<ImageJob>) {
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
        build_body_html(body, job.policy, job.merged)
    };
    // Текстовой версии картинки не положены вовсе.
    let gate: Gate = if force_text {
        Box::new(render::no_remote)
    } else {
        Box::new(job.policy.media_gate(&body.from_addr))
    };
    let (images, complete) = render::cached_images(&html, &*gate);
    let result = render::render_with(&html, job.width, job.scale, &images);
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
        Some(ImageJob { seq: job.seq, row: i, key, html, gate, width: job.width, scale: job.scale })
    };
    (p, source, fetch)
}

fn render_conversation(ctx: &Ctx, job: &ConvJob, scroll_to: Option<i32>) {
    let t_wall = Instant::now();
    let n = job.bodies.len();
    let seq = job.seq;

    // Tell the UI to show the progress bar.
    let n_total = n as i32;
    let _ = ctx.ui.upgrade_in_event_loop(move |ui| {
        ui.set_render_total(n_total);
        ui.set_render_progress(0);
    });

    // RAM hits first, on this thread: they cost a refcount bump.
    let mut slots: Vec<Option<(Packed, Source)>> = {
        let ram = ctx.ram.lock().unwrap_or_else(|e| e.into_inner());
        (0..n).map(|i| ram.get(&job.key(i)).map(|p| (p, Source::Ram))).collect()
    };
    let misses: Vec<usize> = (0..n).filter(|&i| slots[i].is_none()).collect();
    let done = AtomicUsize::new(n - misses.len());
    let report = |d: usize| {
        let d = d as i32;
        let _ = ctx.ui.upgrade_in_event_loop(move |ui| ui.set_render_progress(d));
    };
    report(n - misses.len());

    // The misses, in parallel. Each thread pulls the next index, so a heavy
    // newsletter does not leave the others idle; each checks `seq` before
    // taking work — a newer job (conversation switch, next resize step) stops
    // the pool within one bubble per thread.
    let next = AtomicUsize::new(0);
    let results: Mutex<Vec<(usize, Packed, Source, Option<ImageJob>)>> = Mutex::new(Vec::new());
    let t_layout = Instant::now();
    let threads = layout_threads().min(misses.len());
    std::thread::scope(|s| {
        for _ in 0..threads {
            s.spawn(|| {
                loop {
                    if ctx.stale(seq) {
                        return;
                    }
                    let k = next.fetch_add(1, Ordering::SeqCst);
                    let Some(&i) = misses.get(k) else { return };
                    let (p, source, fetch) = first_pass(ctx, job, i);
                    results.lock().unwrap_or_else(|e| e.into_inner()).push((i, p, source, fetch));
                    report(done.fetch_add(1, Ordering::SeqCst) + 1);
                }
            });
        }
    });
    let layout_ms = t_layout.elapsed().as_millis();

    if ctx.stale(seq) {
        println!("[perf] render job seq={seq} aborted mid-render — newer job queued");
        // Hide the progress bar; the superseding job re-seeds it with its
        // own totals.
        let _ = ctx.ui.upgrade_in_event_loop(move |ui| {
            ui.set_render_total(0);
            ui.set_render_progress(0);
        });
        return;
    }

    let mut fetches: Vec<ImageJob> = Vec::new();
    for (i, p, source, fetch) in results.into_inner().unwrap_or_else(|e| e.into_inner()) {
        slots[i] = Some((p, source));
        fetches.extend(fetch);
    }
    let count = |want: Source| slots.iter().flatten().filter(|(_, s)| *s == want).count();
    println!(
        "[perf] render N={n} width={}px threads={threads} cache_hits={} disk_hits={} \
         fallback={} images_pending={} layout={layout_ms}ms total_job={}ms",
        job.width,
        count(Source::Ram),
        count(Source::Disk),
        count(Source::Fallback),
        fetches.len(),
        t_wall.elapsed().as_millis()
    );

    let rows: Vec<(Packed, RowMeta)> = slots
        .into_iter()
        .enumerate()
        .filter_map(|(i, s)| s.map(|(p, _)| (p, job.meta(i))))
        .collect();
    install_rows(ctx, rows, job.width, scroll_to);

    // Only now: the rows are queued for the UI ahead of any update a fetcher
    // could send for them (one event loop, first in first out).
    let to_fetch = ctx.to_fetch.lock().unwrap_or_else(|e| e.into_inner());
    for f in fetches {
        let _ = to_fetch.send(f);
    }
}

/// Hand a finished first pass to the UI thread: one model swap for the whole
/// conversation, so bubbles appear together and in order, and the scroll
/// anchor is computed from final row heights.
fn install_rows(ctx: &Ctx, rows: Vec<(Packed, RowMeta)>, width: u32, scroll_to: Option<i32>) {
    let rects_width = width as f32;
    let _ = ctx.ui.upgrade_in_event_loop(move |ui| {
        // The width these rows' link/text rects were extracted at — Slint
        // maps mouse coords into (and highlight rects out of) this space, so
        // hits stay exact even while the column width drifts from the render
        // width (resize-debounce window).
        ui.set_body_rects_width(rects_width);
        let (links, runs): (Vec<_>, Vec<_>) =
            rows.iter().map(|(p, _)| (p.links.clone(), p.runs.clone())).unzip();
        SHARED.with(|s| {
            if let Some(sh) = s.borrow().as_ref() {
                *sh.row_links.borrow_mut() = links;
                *sh.row_text_runs.borrow_mut() = runs;
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
            .map(|(p, m)| RowItem {
                img: Image::from_rgba8(p.buf),
                h: p.h,
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
        ui.set_messages(ModelRc::new(VecModel::from(rows)));
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
    let result = render::render_with(&job.html, job.width, job.scale, &images);
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
    item.img = Image::from_rgba8(p.buf);
    item.h = p.h;
    vm.set_row_data(row, item);
    SHARED.with(|s| {
        if let Some(sh) = s.borrow().as_ref() {
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
