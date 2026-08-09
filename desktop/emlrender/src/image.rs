//! Inline image decoding.
//!
//! The crate is network-free by construction, and that is a feature: a renderer
//! that fetches is a renderer that leaks read receipts and stalls a scroll
//! panel on someone else's CDN. Only `data:` URIs decode here. Remote images —
//! and `cid:` parts, which the frozen API gives us no channel to resolve —
//! come back as `None` and are drawn as a placeholder carrying the `alt` text.

use std::io::Cursor;

use base64::Engine as _;

/// A decoded image: straight-alpha RGBA8, row-major, `w * h * 4` bytes.
pub struct Bitmap {
    pub rgba: Vec<u8>,
    pub w: u32,
    pub h: u32,
}

/// Ceilings for a decode. Mail is not a trusted source: a 30 000 × 30 000 PNG
/// costs a few hundred bytes to send and 3.6 GB to decompress.
const MAX_SIDE: u32 = 8192;
const MAX_ALLOC: u64 = 64 * 1024 * 1024;

/// Decode an `src` attribute, or `None` if it is not something we can decode
/// on our own: remote URLs, `cid:` parts, SVG, and anything malformed. Those
/// go to the host's [`crate::Resources`] and come back here as bytes.
pub fn decode(src: &str, scale: f32) -> Option<Bitmap> {
    decode_bytes(&data_uri_payload(src)?, scale)
}

/// Sniff SVG: an XML declaration, a doctype or the root element, whichever the
/// author happened to start with. No decoder will guess this for us — every
/// raster format is identified by magic bytes, and SVG is just text.
fn looks_like_svg(bytes: &[u8]) -> bool {
    let head = &bytes[..bytes.len().min(512)];
    let text = String::from_utf8_lossy(head);
    let text = text.trim_start().to_ascii_lowercase();
    text.starts_with("<svg") || (text.starts_with("<?xml") || text.starts_with("<!doctype svg"))
        && text.contains("<svg")
}

/// Vector art → pixels at the resolution it will actually be drawn.
fn rasterize_svg(bytes: &[u8], scale: f32) -> Option<Bitmap> {
    let options = resvg::usvg::Options::default();
    let tree = resvg::usvg::Tree::from_data(bytes, &options).ok()?;
    let size = tree.size();
    let scale = scale.clamp(0.5, 4.0);
    let (w, h) = ((size.width() * scale).round(), (size.height() * scale).round());
    if !(w >= 1.0 && h >= 1.0) || w > MAX_SIDE as f32 || h > MAX_SIDE as f32 {
        return None;
    }
    let (w, h) = (w as u32, h as u32);
    let mut pixmap = resvg::tiny_skia::Pixmap::new(w, h)?;
    resvg::render(
        &tree,
        resvg::tiny_skia::Transform::from_scale(scale, scale),
        &mut pixmap.as_mut(),
    );
    // resvg paints premultiplied; the rest of this crate works in straight
    // alpha, so undo it once here rather than special-casing every consumer.
    let mut rgba = Vec::with_capacity((w as usize) * (h as usize) * 4);
    for px in pixmap.pixels() {
        let c = px.demultiply();
        rgba.extend_from_slice(&[c.red(), c.green(), c.blue(), c.alpha()]);
    }
    Some(Bitmap { rgba, w, h })
}

/// Decode image bytes from anywhere — a `data:` payload we unwrapped, or a
/// body the host fetched for us.
///
/// `scale` only matters for vector art: a raster image has the pixels it has,
/// but an SVG has to be rasterised at *some* size, and the right one is the
/// device resolution it will be drawn at.
pub fn decode_bytes(bytes: &[u8], scale: f32) -> Option<Bitmap> {
    if looks_like_svg(bytes) {
        return rasterize_svg(bytes, scale);
    }
    let mut reader = ::image::ImageReader::new(Cursor::new(bytes)).with_guessed_format().ok()?;
    let mut limits = ::image::Limits::default();
    limits.max_image_width = Some(MAX_SIDE);
    limits.max_image_height = Some(MAX_SIDE);
    limits.max_alloc = Some(MAX_ALLOC);
    reader.limits(limits);
    let img = reader.decode().ok()?;
    let rgba = img.to_rgba8();
    let (w, h) = (rgba.width(), rgba.height());
    if w == 0 || h == 0 {
        return None;
    }
    Some(Bitmap { rgba: rgba.into_raw(), w, h })
}

/// The bytes behind a `data:[<mediatype>][;base64],<payload>` URI.
fn data_uri_payload(src: &str) -> Option<Vec<u8>> {
    let src = src.trim();
    // `get`, not indexing: the first five bytes may not be a char boundary.
    if !src.get(..5)?.eq_ignore_ascii_case("data:") {
        return None;
    }
    let (meta, payload) = src[5..].split_once(',')?;
    if !meta.to_ascii_lowercase().contains("base64") {
        return Some(percent_decode(payload));
    }
    // Mail wraps long data URIs across lines, and the padding is routinely
    // mangled by whatever assembled the message.
    let cleaned: String = payload.chars().filter(|c| !c.is_whitespace()).collect();
    let std = base64::engine::general_purpose::STANDARD;
    std.decode(&cleaned).ok().or_else(|| {
        let trimmed = cleaned.trim_end_matches('=');
        base64::engine::general_purpose::STANDARD_NO_PAD.decode(trimmed).ok()
    })
}

fn percent_decode(s: &str) -> Vec<u8> {
    let raw = s.as_bytes();
    let mut out = Vec::with_capacity(raw.len());
    let mut i = 0;
    while i < raw.len() {
        if raw[i] == b'%' && i + 2 < raw.len() {
            let hex = std::str::from_utf8(&raw[i + 1..i + 3]).ok();
            if let Some(b) = hex.and_then(|h| u8::from_str_radix(h, 16).ok()) {
                out.push(b);
                i += 3;
                continue;
            }
        }
        out.push(raw[i]);
        i += 1;
    }
    out
}
