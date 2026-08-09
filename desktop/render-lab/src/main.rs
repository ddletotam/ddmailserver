//! render-lab — the bubble panel, fed by the client's own cache.
//!
//! What this proves, in the order the brief asks for it: real mail out of
//! `cache.db` goes through `emlrender`, comes back as a bitmap plus geometry,
//! and lands in one scrolling panel of bubbles with working text selection and
//! link clicks. No controls, no chrome — see `ui/lab.slint`.
//!
//! Selection is the PDF-viewer trick: the bubble is a raster page, and the
//! "text layer" is `Rendered::runs` — one rect per word, in reading order. A
//! selection is a contiguous slice of that layer, drawn as translucent rects on
//! top. This is deliberately the same shape the client already implements over
//! its WebView2 captures (`render_common::{LinkRect, TextRun}`), so swapping
//! the renderer underneath it is a drop-in.
//!
//! Resizing re-runs layout rather than stretching pixels: a mail reflows into
//! the width it is given, which is the whole point of owning the renderer. See
//! [`Pass`] for how that is kept off the hot path.

use std::cell::RefCell;
use std::path::PathBuf;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;
use std::time::Duration;

use emlrender::{LinkRect, RenderOptions, TextRun};
use rusqlite::{Connection, OpenFlags};
use slint::{ComponentHandle, Model, ModelRc, SharedPixelBuffer, VecModel};

slint::include_modules!();

/// HiDPI: bitmap px = CSS px × this.
const SCALE: f32 = 2.0;

/// How many of the most recent mails to render.
///
/// Every bubble keeps its pixels: 420 × ~1500 CSS px at 2× is around 5 MB, so
/// the whole 778-message cache would be some three gigabytes. The client does
/// not have this problem — it caches by row with a bounded FIFO over a disk
/// cache, and shows one conversation rather than an inbox — so this ceiling is
/// the harness being honest, not a design.
const DEFAULT_LIMIT: usize = 60;

/// Hard ceiling on retained bubble pixels, so `render-lab all` degrades into a
/// short panel instead of a swap storm. Whatever it drops, it says so.
const PIXEL_BUDGET: usize = 1_200 * 1024 * 1024;

/// How often the width watcher looks, and how many quiet looks in a row mean
/// the width has settled. A resize drag emits a new width every frame, and
/// re-laying out sixty mails on each one throws away hundreds of milliseconds
/// per frame — so the pass waits.
///
/// Waiting on the *pointer* is the real signal (see [`pointer_held`]); the
/// quiet count is what catches width changes that no drag produced — a
/// maximise, a tiling window manager, a keyboard resize.
const TICK: Duration = Duration::from_millis(150);
const QUIET_TICKS: u32 = 1;

struct Mail {
    html: String,
    outgoing: bool,
}

/// Generation of the current render pass.
///
/// Bumped when a new width settles. The worker checks it between mails and the
/// UI checks it before applying a result, so a pass that has been overtaken
/// stops rather than filling the panel with bitmaps of the wrong width.
static PASS: AtomicU64 = AtomicU64::new(0);

/// Everything the pointer handlers need, on the UI thread only. A thread_local
/// rather than an `Rc` passed around: the render worker hands its results back
/// through `invoke_from_event_loop`, whose closure must be `Send`.
#[derive(Default)]
struct Lab {
    links: Vec<Vec<LinkRect>>,
    runs: Vec<Vec<TextRun>>,
    /// Selection: which bubble, and the anchor/head word in its text layer.
    row: i32,
    anchor: usize,
    head: usize,
    /// Where the press landed, to tell a click from a drag.
    press: (f32, f32),
    /// Width the current pass was started for; 0 before the first one.
    rendered_w: f32,
    /// Width last seen from the UI, and how many ticks it has held still.
    seen_w: f32,
    quiet: u32,
}

thread_local! {
    static LAB: RefCell<Lab> = RefCell::new(Lab::default());
}

fn main() {
    let limit = std::env::args()
        .nth(1)
        .and_then(|a| if a == "all" { Some(usize::MAX) } else { a.parse().ok() })
        .unwrap_or(DEFAULT_LIMIT);

    let mails = match load_mails(limit) {
        Ok(m) if !m.is_empty() => Arc::new(m),
        Ok(_) => {
            eprintln!("cache has no html bodies");
            return;
        }
        Err(e) => {
            eprintln!("cannot read cache: {e}\nset RENDER_LAB_DB to a cache.db path");
            return;
        }
    };
    eprintln!("{} mails", mails.len());

    let ui = match LabWindow::new() {
        Ok(ui) => ui,
        Err(e) => {
            eprintln!("cannot create window: {e}");
            return;
        }
    };
    ui.set_bubbles(ModelRc::new(VecModel::<Bubble>::default()));
    // The width band is the host's policy, so the harness lets you move it —
    // that is the point of it being a property rather than a constant.
    if let Some(w) = env_len("RENDER_LAB_MIN_W") {
        ui.set_min_content_w(w);
    }
    if let Some(w) = env_len("RENDER_LAB_MAX_W") {
        ui.set_max_content_w(w);
    }
    wire_pointer(&ui);

    // The first render is started by the same watcher that handles resizes —
    // one code path, and it means the first pass already uses the real window
    // width rather than a guess made before the window was shown.
    let watcher = slint::Timer::default();
    {
        let weak = ui.as_weak();
        let mails = Arc::clone(&mails);
        watcher.start(slint::TimerMode::Repeated, TICK, move || {
            let Some(ui) = weak.upgrade() else { return };
            let width = ui.get_content_w().round();
            // Mid-drag: the window frame is the OS's to move, and the panel
            // never sees those events. A drag can pause for seconds and resume,
            // so a quiet period alone would fire a full re-layout in the middle
            // of one. Nothing happens until the button is let go.
            if width < 1.0 || pointer_held() {
                return;
            }
            let go = LAB.with(|lab| {
                let mut lab = lab.borrow_mut();
                if (width - lab.seen_w).abs() > 0.5 {
                    lab.seen_w = width;
                    lab.quiet = 0;
                    return false;
                }
                if (width - lab.rendered_w).abs() < 0.5 {
                    return false; // already rendered at this width
                }
                // Nothing on screen yet: don't make the user wait out the
                // quiet period for the first paint.
                let first = lab.rendered_w == 0.0;
                lab.quiet = lab.quiet.saturating_add(1);
                if !first && lab.quiet < QUIET_TICKS {
                    return false;
                }
                lab.rendered_w = width;
                true
            });
            if go {
                // Word indices are about to be replaced wholesale.
                ui.set_sel_row(-1);
                start_pass(Arc::clone(&mails), ui.as_weak(), width as u32);
            }
        });
    }

    if let Err(e) = ui.run() {
        eprintln!("event loop: {e}");
    }
}

/// Render every mail at `width` and stream the bubbles in as they finish, so
/// the panel stays scrollable while the tail is still rasterising.
fn start_pass(mails: Arc<Vec<Mail>>, weak: slint::Weak<LabWindow>, width: u32) {
    let pass = PASS.fetch_add(1, Ordering::SeqCst) + 1;
    eprintln!("pass {pass}: laying out at {width} px");
    std::thread::spawn(move || {
        let total = mails.len();
        let mut spent = 0usize;
        for (index, mail) in mails.iter().enumerate() {
            if PASS.load(Ordering::SeqCst) != pass {
                return; // a wider window overtook us
            }
            let opts = RenderOptions { width, scale: SCALE, block_remote: false };
            let images = emlrender::net::HttpResources::prefetch(&mail.html);
            let r = emlrender::render_with(&mail.html, &opts, &images);
            spent += r.rgba.len();
            if spent > PIXEL_BUDGET {
                // The panel reads oldest-first, so this cuts the *newest*
                // mails. Pass a count instead of `all` to get the recent ones.
                eprintln!(
                    "stopped at {index} of {total} mails — {} MB of bubble pixels is the \
                     budget; run with a count (e.g. `render-lab 60`) to see the newest",
                    PIXEL_BUDGET / (1024 * 1024)
                );
                return;
            }
            let (cw, ch) = r.css_size();
            let outgoing = mail.outgoing;
            let (rgba, w_px, h_px) = (r.rgba, r.width_px, r.height_px);
            let (links, runs) = (r.links, r.runs);
            let weak = weak.clone();
            let posted = slint::invoke_from_event_loop(move || {
                if PASS.load(Ordering::SeqCst) != pass {
                    return;
                }
                let Some(ui) = weak.upgrade() else { return };
                let mut buf = SharedPixelBuffer::new(w_px, h_px);
                buf.make_mut_bytes().copy_from_slice(&rgba);
                let bubble = Bubble { img: slint::Image::from_rgba8(buf), w: cw, h: ch, outgoing };
                LAB.with(|lab| {
                    let mut lab = lab.borrow_mut();
                    if lab.links.len() <= index {
                        lab.links.resize_with(index + 1, Vec::new);
                        lab.runs.resize_with(index + 1, Vec::new);
                    }
                    lab.links[index] = links;
                    lab.runs[index] = runs;
                });
                let model = ui.get_bubbles();
                if let Some(v) = model.as_any().downcast_ref::<VecModel<Bubble>>() {
                    // A re-render replaces in place; the first pass appends.
                    if index < v.row_count() {
                        v.set_row_data(index, bubble);
                    } else {
                        v.push(bubble);
                    }
                }
            });
            if posted.is_err() {
                return; // window closed under us
            }
        }
    });
}

// ------------------------------------------------------------------ pointer

fn wire_pointer(ui: &LabWindow) {
    let weak = ui.as_weak();
    ui.on_press(move |row, x, y| {
        let Some(ui) = weak.upgrade() else { return };
        let (x, y) = as_rendered(&ui, row, x, y);
        LAB.with(|lab| {
            let mut lab = lab.borrow_mut();
            lab.press = (x, y);
            lab.row = row;
            let hit = lab.runs.get(row as usize).and_then(|r| nearest_run(r, x, y));
            lab.anchor = hit.unwrap_or(0);
            lab.head = lab.anchor;
        });
        // A press with no drag yet selects nothing — otherwise every click
        // would leave a stray word highlighted.
        ui.set_sel_row(-1);
        ui.set_sel_rects(ModelRc::new(VecModel::<SelRect>::default()));
    });

    let weak = ui.as_weak();
    ui.on_drag(move |row, x, y| {
        let Some(ui) = weak.upgrade() else { return };
        let (x, y) = as_rendered(&ui, row, x, y);
        let rects = LAB.with(|lab| {
            let mut lab = lab.borrow_mut();
            if lab.row != row {
                return Vec::new();
            }
            let anchor = lab.anchor;
            let Some(runs) = lab.runs.get(row as usize) else { return Vec::new() };
            let Some(head) = nearest_run(runs, x, y) else { return Vec::new() };
            let rects = selection_rects(runs, anchor, head);
            lab.head = head;
            rects
        });
        ui.set_sel_row(if rects.is_empty() { -1 } else { row });
        ui.set_sel_rects(ModelRc::new(VecModel::from(rects)));
    });

    let weak = ui.as_weak();
    ui.on_release(move |row, x, y| {
        let Some(ui) = weak.upgrade() else { return };
        let (x, y) = as_rendered(&ui, row, x, y);
        let href = LAB.with(|lab| {
            let lab = lab.borrow();
            let (px, py) = lab.press;
            // Anything that moved is a selection, not a click.
            if (px - x).abs() > 4.0 || (py - y).abs() > 4.0 {
                return None;
            }
            // Topmost wins: later boxes are painted over earlier ones.
            let hit = lab.links.get(row as usize)?.iter().rev().find(|l| l.contains(x, y))?;
            Some(hit.href.clone())
        });
        if let Some(href) = href {
            println!("open {href}");
            open_url(&href);
        }
    });

    let weak = ui.as_weak();
    ui.on_hover(move |row, x, y| {
        let Some(ui) = weak.upgrade() else { return };
        let (x, y) = as_rendered(&ui, row, x, y);
        let kind = LAB.with(|lab| {
            let lab = lab.borrow();
            if lab.links.get(row as usize).is_some_and(|ls| ls.iter().any(|l| l.contains(x, y))) {
                return 1; // link
            }
            if lab.runs.get(row as usize).is_some_and(|rs| rs.iter().any(|r| r.contains(x, y))) {
                return 2; // selectable text
            }
            0
        });
        if ui.get_hover_kind() != kind {
            ui.set_hover_kind(kind);
        }
    });

    ui.on_copy_selection(|| {
        let text = LAB.with(|lab| {
            let lab = lab.borrow();
            let runs = lab.runs.get(usize::try_from(lab.row).ok()?)?;
            let text = selection_text(runs, lab.anchor, lab.head);
            if text.is_empty() {
                None
            } else {
                Some(text)
            }
        });
        if let Some(text) = text {
            match arboard::Clipboard::new().and_then(|mut c| c.set_text(text.clone())) {
                Ok(()) => println!("copied {} chars", text.chars().count()),
                Err(e) => eprintln!("clipboard: {e}"),
            }
        }
    });
}

/// Pointer position in the coordinates the bubble's geometry uses.
///
/// Between a resize and the re-render that follows it, a bubble is drawn at
/// `content-w` while its links and words still describe the width it was
/// rendered at. Undoing that scale here keeps the hit-test honest throughout —
/// and it is a no-op the rest of the time.
fn as_rendered(ui: &LabWindow, row: i32, x: f32, y: f32) -> (f32, f32) {
    let rendered = usize::try_from(row)
        .ok()
        .and_then(|r| ui.get_bubbles().row_data(r))
        .map_or(0.0, |b| b.w);
    let display = ui.get_content_w();
    if rendered <= 0.5 || display <= 0.5 {
        return (x, y);
    }
    let zoom = display / rendered;
    (x / zoom, y / zoom)
}

/// Index of the word at `(x, y)`, or the closest one.
///
/// Falling back to the closest is what makes dragging through the margin feel
/// right: a pointer in the empty space left of line 5 should extend the
/// selection to line 5, not to whatever word happens to be nearest in a
/// straight line.
fn nearest_run(runs: &[TextRun], x: f32, y: f32) -> Option<usize> {
    if runs.is_empty() {
        return None;
    }
    if let Some(i) = runs.iter().position(|r| r.contains(x, y)) {
        return Some(i);
    }
    let mut best = (0usize, f32::MAX);
    for (i, r) in runs.iter().enumerate() {
        let dx = (r.x - x).max(x - (r.x + r.w)).max(0.0);
        let dy = (r.y - y).max(y - (r.y + r.h)).max(0.0);
        // Vertical distance dominates: pick the line first, the word second.
        let d = dy * 4.0 + dx;
        if d < best.1 {
            best = (i, d);
        }
    }
    Some(best.0)
}

/// Highlight rects for the runs between `a` and `b`, merged per line.
fn selection_rects(runs: &[TextRun], a: usize, b: usize) -> Vec<SelRect> {
    let (lo, hi) = (a.min(b), a.max(b));
    let mut out: Vec<SelRect> = Vec::new();
    for r in runs.get(lo..=hi).unwrap_or_default() {
        match out.last_mut() {
            Some(p) if same_line(p.y, p.h, r) => {
                let right = (p.x + p.w).max(r.x + r.w);
                p.x = p.x.min(r.x);
                p.w = right - p.x;
            }
            _ => out.push(SelRect { x: r.x, y: r.y, w: r.w, h: r.h }),
        }
    }
    out
}

/// Selected text, with the line structure the reader saw put back in.
fn selection_text(runs: &[TextRun], a: usize, b: usize) -> String {
    let (lo, hi) = (a.min(b), a.max(b));
    let mut out = String::new();
    let mut prev: Option<&TextRun> = None;
    for r in runs.get(lo..=hi).unwrap_or_default() {
        if let Some(p) = prev {
            if !same_line(p.y, p.h, r) {
                out.push('\n');
            } else if !r.cont {
                // `cont` marks a word split across a wrap: joining those with a
                // space would paste a broken URL.
                out.push(' ');
            }
        }
        out.push_str(&r.text);
        prev = Some(r);
    }
    out
}

fn same_line(y: f32, h: f32, r: &TextRun) -> bool {
    (y - r.y).abs() < 0.5 && (h - r.h).abs() < 0.5
}

fn open_url(url: &str) {
    let lower = url.to_ascii_lowercase();
    if !lower.starts_with("http://") && !lower.starts_with("https://") {
        return;
    }
    #[cfg(target_os = "windows")]
    // Not `cmd /C start`: an `&` in a tracking URL ends the command there.
    let spawned = std::process::Command::new("rundll32.exe")
        .args(["url.dll,FileProtocolHandler", url])
        .spawn();
    #[cfg(not(target_os = "windows"))]
    let spawned = std::process::Command::new("xdg-open").arg(url).spawn();
    if let Err(e) = spawned {
        eprintln!("open {url}: {e}");
    }
}

// --------------------------------------------------------------------- cache

fn load_mails(limit: usize) -> rusqlite::Result<Vec<Mail>> {
    let path = cache_path();
    // `immutable=1`: the client may be running and holding the WAL. We only
    // read, and a lab that blocks on someone else's write lock is useless.
    let uri = format!("file:{}?mode=ro&immutable=1", path.display().to_string().replace('\\', "/"));
    let conn = Connection::open_with_flags(
        uri,
        OpenFlags::SQLITE_OPEN_READ_ONLY | OpenFlags::SQLITE_OPEN_URI,
    )?;
    let mut stmt = conn.prepare(
        "SELECT html, COALESCE(is_outgoing, 0) FROM message_bodies
         WHERE html IS NOT NULL AND length(html) > 0
         ORDER BY date_ts DESC LIMIT ?1",
    )?;
    let rows = stmt.query_map([limit.min(i64::MAX as usize) as i64], |row| {
        Ok(Mail { html: row.get(0)?, outgoing: row.get::<_, i64>(1)? != 0 })
    })?;
    let mut mails: Vec<Mail> = rows.filter_map(Result::ok).collect();
    // Newest-first out of SQL so LIMIT takes the recent ones; oldest-first in
    // the panel, because that is the direction a thread reads.
    mails.reverse();
    Ok(mails)
}

/// Is the left mouse button down right now?
///
/// Asked of the OS rather than of the window, because a resize drag belongs to
/// the window manager and produces no events inside the application at all.
#[cfg(windows)]
fn pointer_held() -> bool {
    // One call into user32, rather than a dependency for one predicate.
    unsafe extern "system" {
        fn GetAsyncKeyState(key: i32) -> i16;
    }
    const VK_LBUTTON: i32 = 0x01;
    // The high bit is "currently down"; the low bit is "was pressed since the
    // last call" and would latch on a stale click.
    (unsafe { GetAsyncKeyState(VK_LBUTTON) } as u16 & 0x8000) != 0
}

/// Elsewhere the quiet period is the only signal — X11 would need a display
/// connection of its own for `XQueryPointer`, and a resize there is usually one
/// motion rather than the multi-stage drag Windows produces.
#[cfg(not(windows))]
fn pointer_held() -> bool {
    false
}

fn env_len(name: &str) -> Option<f32> {
    std::env::var(name).ok()?.trim().parse::<f32>().ok().filter(|v| *v > 0.0)
}

fn cache_path() -> PathBuf {
    if let Some(p) = std::env::var_os("RENDER_LAB_DB") {
        return PathBuf::from(p);
    }
    let dir = if cfg!(windows) {
        std::env::var_os("APPDATA").map(PathBuf::from)
    } else {
        std::env::var_os("XDG_DATA_HOME")
            .map(PathBuf::from)
            .or_else(|| std::env::var_os("HOME").map(|h| PathBuf::from(h).join(".local/share")))
    };
    dir.unwrap_or_default().join("ru.letotam.ddmail").join("cache.db")
}
