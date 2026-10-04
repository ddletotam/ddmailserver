//! The render backend, built on `emlrender` — no browser engine at all, on
//! either platform. There is no second backend and no flag that selects one.
//!
//! Coordinate contract: bitmap px = CSS px × `scale`, links and runs in CSS px.
//! Everything above this file — the bubble bitmaps, the PDF-style selection
//! layer, the link hit-test against stored `LinkRect`s — works on that
//! geometry alone; nothing keeps a live document around after a render.
//!
//! Why the browser engines went: WebView2 and WebKitGTK each lay out to their
//! own idea of a viewport and overflow horizontally out of the bubble, and no
//! setting makes them stop. `emlrender` treats "never wider than the width you
//! were given" as its one inviolable rule. Dropping them also took WebKitGTK's
//! system dependencies out of the Linux build and WebView2 out of the Windows
//! installer.

use crate::render_common::{LinkRect, TextRun};

pub struct Bitmap {
    pub rgba: Vec<u8>,
    pub width: u32,
    pub height: u32,
}

pub struct RenderResult {
    pub bitmap: Bitmap,
    pub links: Vec<LinkRect>,
    /// Per-word text layer for mouse selection (PDF-viewer style).
    pub runs: Vec<TextRun>,
    /// Rasterization scale the bitmap was produced at.
    pub scale: f32,
}

impl RenderResult {
    /// Did the layout actually produce a page? `emlrender::render_with` never
    /// fails outright — a panic inside the pipeline is caught there and comes
    /// back as a blank strip exactly one pixel high. Every real bubble is
    /// taller than that (the chrome alone has padding and a timestamp line),
    /// so a 1-px result is the crate's "this mail broke me" signal: the caller
    /// retries with the text-only bubble and must not cache the strip.
    pub fn successful(&self) -> bool {
        self.bitmap.height > 1
    }
}

/// Per-host permission for remote images, as `HttpResources::prefetch` takes it.
pub type RemoteGate = dyn Fn(&str) -> bool + Sync;

/// The gate for HTML we built ourselves (text fallback, source viewer): no
/// remote image of ours exists, so nothing may load.
pub fn no_remote(_host: &str) -> bool {
    false
}

/// Remote images of one document, ready for [`render_with`].
pub type Images = emlrender::net::HttpResources;

/// Download this document's remote images — the ones `allow_host` accepts —
/// blocking until they are in or the loader's batch deadline passes (seconds
/// on a slow CDN). Never call this on a path something interactive waits on.
///
/// `allow_host` decides, per host, whether this message's remote images
/// may be fetched — `Policy::media_gate` for a mail body, [`no_remote`]
/// for anything we generated ourselves. It is the security boundary:
/// `sanitize::block_external` blanks what it recognises for the «Медиа…»
/// menu, but a spelling its regexes miss (unquoted `src`, entities, CSS)
/// still reaches the loader, which parses the same DOM layout does and
/// asks `allow_host` about every URL and every redirect hop.
pub fn fetch_images(html: &str, allow_host: &RemoteGate) -> Images {
    Images::prefetch(html, allow_host)
}

/// The images [`fetch_images`] would return that are already in memory,
/// without touching the network, and whether that is all of them. `false`
/// means a render now paints placeholders for some — the caller fetches and
/// renders again.
pub fn cached_images(html: &str, allow_host: &RemoteGate) -> (Images, bool) {
    Images::cached(html, allow_host)
}

/// Lay out and rasterize one document, fetching its remote images first
/// (blocking, see [`fetch_images`]). For documents of our own that have none
/// (`no_remote`), this is plain layout.
pub fn render(html: &str, width: u32, scale: f32, allow_host: &RemoteGate) -> RenderResult {
    render_with(html, width, scale, &fetch_images(html, allow_host))
}

/// Lay out and rasterize one document with the images at hand. Stateless and
/// synchronous, safe to call from several threads at once: each render
/// borrows its own text engine from emlrender's pool.
pub fn render_with(html: &str, width: u32, scale: f32, images: &Images) -> RenderResult {
    let opts = emlrender::RenderOptions { width, scale, block_remote: false };
    let r = emlrender::render_with(html, &opts, images);
    RenderResult {
        bitmap: Bitmap { rgba: r.rgba, width: r.width_px, height: r.height_px },
        links: r.links.into_iter().map(into_link).collect(),
        runs: r.runs.into_iter().map(into_run).collect(),
        scale: r.scale,
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
