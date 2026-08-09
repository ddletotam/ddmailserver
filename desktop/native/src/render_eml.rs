//! Render backend built on `emlrender` — no browser engine at all.
//!
//! Drop-in for `render_webview2` / `render_webkit`: same `Engine` surface, same
//! `RenderResult`, same coordinate contract (bitmap px = CSS px × `scale`,
//! links and runs in CSS px). Everything above this file — the bubble bitmaps,
//! the PDF-style selection layer, the link hit-test — is unchanged.
//!
//! Why replace a working WebView2 path: a browser engine lays out to its own
//! idea of a viewport and overflows horizontally out of the bubble, and there
//! is no setting that makes it stop. `emlrender` treats "never wider than the
//! width you were given" as its one inviolable rule.
//!
//! Enable with `--features eml-render`.

use crate::render_common::{LinkRect, TextRun};

pub struct Bitmap {
    pub rgba: Vec<u8>,
    pub width: u32,
    pub height: u32,
}

pub struct RenderResult {
    pub bitmap: Bitmap,
    pub view_ready: bool,
    pub painted_height: u32,
    pub links: Vec<LinkRect>,
    /// Per-word text layer for mouse selection (PDF-viewer style).
    pub runs: Vec<TextRun>,
    /// Rasterization scale the bitmap was produced at.
    pub scale: f32,
}

impl RenderResult {
    pub fn successful(&self) -> bool {
        self.view_ready && self.painted_height > 0
    }
}

/// Stateless: there is no view to own, no message pump to drive, and no
/// navigation to wait for. Kept as a struct so the call sites do not change.
pub struct Engine;

impl Default for Engine {
    fn default() -> Self {
        Self::new()
    }
}

impl Engine {
    pub fn new() -> Self {
        Engine
    }

    /// `emlrender::render` already catches panics internally and rebuilds its
    /// text engine, so the "did it panic" flag the callers use to fall back to
    /// the plain-text body is always false: a failure here is a short bitmap,
    /// not a lost worker.
    pub fn render_one_guarded(&mut self, html: &str, width: u32, scale: f32) -> (RenderResult, bool) {
        (self.render_one(html, width, scale), false)
    }

    pub fn render_one(&mut self, html: &str, width: u32, scale: f32) -> RenderResult {
        // `block_remote: false` is not "load everything": the permission gate
        // already ran. `sanitize::block_external` blanks the `src` of any image
        // this sender is not allowed to load, so every absolute URL still
        // standing in `html` is one the user said yes to — exactly what the
        // browser backends loaded implicitly.
        let opts = emlrender::RenderOptions { width, scale, block_remote: false };
        let images = emlrender::net::HttpResources::prefetch(html);
        let r = emlrender::render_with(html, &opts, &images);
        RenderResult {
            bitmap: Bitmap { rgba: r.rgba, width: r.width_px, height: r.height_px },
            // No view to become ready and nothing asynchronous to wait for.
            view_ready: true,
            painted_height: r.height_px,
            links: r.links.into_iter().map(into_link).collect(),
            runs: r.runs.into_iter().map(into_run).collect(),
            scale: r.scale,
        }
    }

    /// No views to release.
    pub fn clear_views(&mut self) {}

    /// Hit-testing lives in `main.rs` against the stored `LinkRect`s; the
    /// browser backends kept this for their live-DOM path.
    pub fn hit(&self, _row: usize, _x: f32, _y: f32) -> Option<String> {
        None
    }
}

// The two `LinkRect`/`TextRun` pairs are structurally identical by design (see
// the note in `emlrender/src/lib.rs`), but they are distinct types in distinct
// crates, so the boundary gets an explicit conversion rather than a transmute.
fn into_link(l: emlrender::LinkRect) -> LinkRect {
    LinkRect { x: l.x, y: l.y, w: l.w, h: l.h, href: l.href }
}

fn into_run(r: emlrender::TextRun) -> TextRun {
    TextRun { x: r.x, y: r.y, w: r.w, h: r.h, text: r.text, cont: r.cont }
}
