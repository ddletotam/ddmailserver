//! Email-body renderer backed by WebKitGTK.
//!
//! Replaces the Ultralight-based renderer that was losing bubbles on
//! real-world HTML. We hold a single `WebView` inside an off-screen
//! `GtkOffscreenWindow`, drive `load_html`/snapshot/JS through GLib's
//! default `MainContext` (we own the thread and pump the main loop
//! manually instead of running `gtk::main()`).
//!
//! Threading: every call into GTK / WebKit must happen on the OS
//! thread that called `gtk::init()`. The caller (main.rs Job worker)
//! is a dedicated thread, so `Engine::new` initialises GTK there and
//! all subsequent renders stay on that thread. The Slint UI thread
//! never touches GTK. The only data crossing thread boundaries is the
//! raw RGBA buffer wrapped in `SharedPixelBuffer`, which is `Send`.
//!
//! Hit-testing (link clicks inside bubbles) is currently disabled
//! after this migration — re-adding it needs per-row WebView retention
//! and is tracked separately.

use std::cell::{Cell, RefCell};
use std::rc::Rc;
use std::time::Instant;

use cairo::ImageSurface;
use gtk::prelude::*;
use javascriptcore::ValueExt;
use webkit2gtk::{LoadEvent, SnapshotOptions, SnapshotRegion, WebView, WebViewExt};

use crate::render_common::{
    parse_link_rects, parse_text_runs, LinkRect, TextRun, HIDE_SCROLLBARS_JS, LINK_RECTS_JS,
    TEXT_RUNS_JS,
};

/// Per-bubble canvas cap. WebKitGTK happily renders the whole document
/// regardless, but downstream we still want a sane upper bound on the
/// bitmap dimensions we paint into.
/// Per-bubble height cap. GBM buffers on Intel/AMD/Mesa drivers commonly
/// max out at 8192px and start failing at ~8000px on Wayland. 6000
/// stays comfortably under that ceiling while still letting big
/// notification emails fit; anything taller gets cropped at the bottom
/// (rare in actual mail).
const MAX_H: u32 = 6000;
const LOAD_TIMEOUT_MS: u128 = 5000;

pub struct Bitmap {
    pub rgba: Vec<u8>,
    pub width: u32,
    pub height: u32,
}

/// One render attempt's outcome. `view_ready` is false when WebKit
/// didn't fire `LoadEvent::Finished` inside the timeout; `painted_height`
/// is the document content height we measured via JS (0 means the
/// document had nothing to paint). Caller uses both to decide whether
/// to fall back to a text-only re-render.
pub struct RenderResult {
    pub bitmap: Bitmap,
    pub view_ready: bool,
    pub painted_height: u32,
    pub links: Vec<LinkRect>,
    /// Per-word text layer for mouse selection (PDF-viewer style).
    pub runs: Vec<TextRun>,
    /// Bitmap px per CSS px. This backend always snapshots at 1x (WebKitGTK
    /// off-screen snapshots ignore rasterization scale) — kept for API parity
    /// with the WebView2 backend.
    pub scale: f32,
}

impl RenderResult {
    pub fn successful(&self) -> bool {
        self.view_ready && self.painted_height > 0
    }
}

pub struct Engine {
    // Keep the off-screen window alive so the WebView has a parent and
    // GTK gives it a real rendering surface.
    _offscreen: gtk::OffscreenWindow,
    view: WebView,
}

impl Engine {
    pub fn new() -> Self {
        gtk::init().expect("gtk::init failed");
        let view = WebView::new();
        let offscreen = gtk::OffscreenWindow::new();
        offscreen.add(&view);
        offscreen.set_default_size(800, MAX_H as i32);
        offscreen.show_all();
        Engine {
            _offscreen: offscreen,
            view,
        }
    }

    /// Panic-guarded wrapper — see the WebView2 backend for the rationale.
    /// A render panic must not kill the worker thread (permanent loader
    /// spinner). Returns (result, panicked); the worker rebuilds the engine
    /// when panicked is true.
    pub fn render_one_guarded(&mut self, html: &str, width: u32, scale: f32) -> (RenderResult, bool) {
        let r = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
            self.render_one(html, width, scale)
        }));
        match r {
            Ok(res) => (res, false),
            Err(_) => {
                eprintln!("render: WebKit render panicked — empty bubble, engine will rebuild");
                (
                    RenderResult {
                        bitmap: empty_bitmap(width),
                        view_ready: false,
                        painted_height: 0,
                        links: Vec::new(),
                        runs: Vec::new(),
                        scale: 1.0,
                    },
                    true,
                )
            }
        }
    }

    pub fn render_one(&mut self, html: &str, width: u32, _scale: f32) -> RenderResult {
        let w = width as i32;
        // Start with a deliberately *small* viewport. WebKit treats
        // `document.body.scrollHeight` as the larger of (content,
        // viewport) — so a tall viewport hides real content height
        // behind its own minimum. With a 1px-tall viewport, scrollHeight
        // reflects only what the content actually needs.
        self.view.set_size_request(w, 1);
        self._offscreen.set_default_size(w, 1);

        let view_ready = self.load_html_sync(html);
        if !view_ready {
            return RenderResult {
                bitmap: empty_bitmap(width),
                view_ready: false,
                painted_height: 0,
                links: Vec::new(),
                runs: Vec::new(),
                scale: 1.0,
            };
        }

        // Kill scrollbars before measuring: at the 1px-tall viewport the
        // vertical scrollbar steals layout width that the full-height
        // viewport won't have — height and rects must describe the final
        // layout, not the transiently narrower one.
        self.run_js_blind(HIDE_SCROLLBARS_JS);

        // Force a layout pass so scrollHeight reflects the freshly
        // loaded content.
        pump_iterations(8);

        let painted_height = self.measure_content_height();
        if painted_height == 0 {
            return RenderResult {
                bitmap: empty_bitmap(width),
                view_ready: true,
                painted_height: 0,
                links: Vec::new(),
                runs: Vec::new(),
                scale: 1.0,
            };
        }

        // Grow the viewport BEFORE extracting geometry: rects and the
        // snapshot must come from the same layout (late images / viewport-
        // height dependent CSS shifted the old pre-grow coordinates).
        // Cap at MAX_H to keep GBM happy.
        let render_h = painted_height.min(MAX_H) as i32;
        self.view.set_size_request(w, render_h);
        self._offscreen.set_default_size(w, render_h);
        pump_iterations(4);

        let links = self.extract_links();
        let runs = self.extract_text_runs();

        let bitmap = self
            .snapshot_to_rgba(width, painted_height)
            .unwrap_or_else(|| empty_bitmap(width));

        RenderResult {
            bitmap,
            view_ready,
            painted_height,
            links,
            runs,
            scale: 1.0,
        }
    }

    /// Run a JS snippet for its side effect only (result ignored).
    fn run_js_blind(&self, js: &str) {
        let done = Rc::new(Cell::new(false));
        let done_cb = done.clone();
        self.view
            .run_javascript(js, None::<&gio::Cancellable>, move |_| done_cb.set(true));
        wait_until(&done, 1000);
    }

    /// Extract `<a href>` rects (document-relative CSS px) via JS. Mirrors
    /// measure_content_height: run the script, wait, read the value as JSON.
    fn extract_links(&self) -> Vec<LinkRect> {
        let done = Rc::new(Cell::new(false));
        let json = Rc::new(RefCell::new(String::new()));
        let done_cb = done.clone();
        let json_cb = json.clone();
        self.view.run_javascript(
            LINK_RECTS_JS,
            None::<&gio::Cancellable>,
            move |res| {
                if let Ok(jsr) = res {
                    if let Some(value) = jsr.js_value() {
                        if let Some(s) = value.to_json(0) {
                            *json_cb.borrow_mut() = s.to_string();
                        }
                    }
                }
                done_cb.set(true);
            },
        );
        wait_until(&done, 2000);
        let s = json.borrow();
        parse_link_rects(&s)
    }

    /// Extract the per-word text layer (selection support). Same JS-and-wait
    /// dance as extract_links.
    fn extract_text_runs(&self) -> Vec<TextRun> {
        let done = Rc::new(Cell::new(false));
        let json = Rc::new(RefCell::new(String::new()));
        let done_cb = done.clone();
        let json_cb = json.clone();
        self.view.run_javascript(
            TEXT_RUNS_JS,
            None::<&gio::Cancellable>,
            move |res| {
                if let Ok(jsr) = res {
                    if let Some(value) = jsr.js_value() {
                        if let Some(s) = value.to_json(0) {
                            *json_cb.borrow_mut() = s.to_string();
                        }
                    }
                }
                done_cb.set(true);
            },
        );
        wait_until(&done, 2000);
        let s = json.borrow();
        parse_text_runs(&s)
    }

    /// Reset the cached `View` state — placeholder kept for API parity
    /// with the previous Ultralight engine. Currently a no-op because
    /// we re-use a single WebView across renders.
    pub fn clear_views(&mut self) {}

    /// Per-row hit-test. Disabled in the WebKitGTK migration; bubbles
    /// re-rendered from cache never had a live view either, so this is
    /// just where that limitation now applies to fresh renders too.
    pub fn hit(&self, _row: usize, _x: f32, _y: f32) -> Option<String> {
        None
    }

    fn load_html_sync(&self, html: &str) -> bool {
        let done = Rc::new(Cell::new(false));
        let failed = Rc::new(Cell::new(false));

        let done_cb = done.clone();
        let failed_cb = failed.clone();
        let handler_id = self.view.connect_load_changed(move |_v, event| {
            match event {
                LoadEvent::Finished => done_cb.set(true),
                _ => {}
            }
        });
        let failed_id = self.view.connect_load_failed(move |_v, _evt, _uri, _err| {
            failed_cb.set(true);
            false
        });

        self.view.load_html(html, None);

        let start = Instant::now();
        while !done.get() && !failed.get() && start.elapsed().as_millis() < LOAD_TIMEOUT_MS {
            glib::MainContext::default().iteration(false);
        }

        self.view.disconnect(handler_id);
        self.view.disconnect(failed_id);

        done.get() && !failed.get()
    }

    fn measure_content_height(&self) -> u32 {
        let done = Rc::new(Cell::new(false));
        let height = Rc::new(Cell::new(0u32));
        let done_cb = done.clone();
        let height_cb = height.clone();
        self.view.run_javascript(
            "(function(){ \
               var h = Math.max( \
                   document.documentElement ? document.documentElement.scrollHeight : 0, \
                   document.body ? document.body.scrollHeight : 0); \
               return h; \
             })()",
            None::<&gio::Cancellable>,
            move |res| {
                if let Ok(jsr) = res {
                    if let Some(value) = jsr.js_value() {
                        let n = value.to_int32();
                        if n > 0 {
                            height_cb.set(n as u32);
                        }
                    }
                }
                done_cb.set(true);
            },
        );
        wait_until(&done, 2000);
        height.get().min(MAX_H)
    }

    fn snapshot_to_rgba(&self, width: u32, height: u32) -> Option<Bitmap> {
        let done = Rc::new(Cell::new(false));
        let surface = Rc::new(RefCell::new(None::<cairo::Surface>));
        let done_cb = done.clone();
        let surface_cb = surface.clone();
        self.view.snapshot(
            SnapshotRegion::FullDocument,
            SnapshotOptions::NONE,
            None::<&gio::Cancellable>,
            move |res| {
                if let Ok(s) = res {
                    *surface_cb.borrow_mut() = Some(s);
                }
                done_cb.set(true);
            },
        );
        wait_until(&done, 4000);
        let surface = surface.borrow_mut().take()?;

        let mut img = ImageSurface::try_from(surface).ok()?;
        let sw = img.width() as u32;
        let sh = img.height() as u32;
        // Cap to the measured content height — WebKit may snapshot the
        // full document including white space below the painted area.
        let h = sh.min(height.max(1)).min(MAX_H);
        let w = sw.min(width).max(1);
        let stride = img.stride() as usize;
        let data = img.data().ok()?;
        let mut rgba = Vec::with_capacity(w as usize * h as usize * 4);
        for y in 0..h as usize {
            let row_start = y * stride;
            for x in 0..w as usize {
                let off = row_start + x * 4;
                if off + 3 >= data.len() {
                    rgba.extend_from_slice(&[255, 255, 255, 255]);
                    continue;
                }
                // Cairo's ARGB32 is BGRA in memory on little-endian.
                let b = data[off];
                let g = data[off + 1];
                let r = data[off + 2];
                let a = data[off + 3];
                rgba.push(r);
                rgba.push(g);
                rgba.push(b);
                rgba.push(a);
            }
        }
        Some(Bitmap {
            rgba,
            width: w,
            height: h,
        })
    }
}

fn empty_bitmap(width: u32) -> Bitmap {
    Bitmap {
        rgba: vec![255, 255, 255, 255],
        width: width.max(1),
        height: 1,
    }
}

fn pump_iterations(n: usize) {
    let ctx = glib::MainContext::default();
    for _ in 0..n {
        ctx.iteration(false);
    }
}

fn wait_until(flag: &Rc<Cell<bool>>, timeout_ms: u128) {
    let start = Instant::now();
    let ctx = glib::MainContext::default();
    while !flag.get() && start.elapsed().as_millis() < timeout_ms {
        ctx.iteration(true);
    }
}
