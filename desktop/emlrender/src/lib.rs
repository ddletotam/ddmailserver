//! emlrender — pure-Rust email HTML → RGBA bitmap renderer (NO browser engine).
//!
//! Goal: render real marketing/transactional email HTML into a bitmap that is
//! **constrained to `width`** (must never overflow horizontally — that overflow
//! is the whole reason we're replacing WebView2/Ultralight) and looks GOOD.
//!
//! This file fixes the PUBLIC API only. Internals (parse → style → layout →
//! paint) are implemented by the renderer author. Do not change these
//! signatures without updating the harness (`render-lab`).
//!
//! The geometry types below intentionally mirror `render_common::{LinkRect,
//! TextRun}` in `desktop/native`: the client already implements link hit-tests
//! and PDF-viewer-style selection on top of a bitmap using exactly these
//! shapes, so matching them makes the eventual swap mechanical.

/// What to render and how wide.
#[derive(Clone, Copy, Debug)]
pub struct RenderOptions {
    /// Target content width in CSS px (the bubble's inner width). The output
    /// bitmap is `width * scale` px wide. Content MUST be laid out to fit this
    /// width — wider designs are scaled/reflowed down, never clipped-with-overflow.
    pub width: u32,
    /// Device scale factor (HiDPI). Paint at this scale for crisp text.
    pub scale: f32,
    /// If true, do NOT fetch remote (http/https) images — draw a neutral
    /// placeholder box of the declared size. `cid:` / `data:` inline images
    /// still render.
    pub block_remote: bool,
}

impl Default for RenderOptions {
    fn default() -> Self {
        Self { width: 420, scale: 1.0, block_remote: true }
    }
}

/// A clickable `<a href>` box, in **CSS px** relative to the bitmap's top-left
/// (bitmap px = CSS px × `Rendered::scale`).
#[derive(Clone, Debug)]
pub struct LinkRect {
    pub x: f32,
    pub y: f32,
    pub w: f32,
    pub h: f32,
    pub href: String,
}

impl LinkRect {
    pub fn contains(&self, x: f32, y: f32) -> bool {
        x >= self.x && x <= self.x + self.w && y >= self.y && y <= self.y + self.h
    }
}

/// One laid-out piece of text — the selection layer drawn over the bitmap.
/// Coordinates are **CSS px** relative to the bitmap's top-left.
///
/// Granularity is per-word (or finer): the harness picks runs by rectangle
/// intersection to build a selection, so oversized runs make selection coarse.
#[derive(Clone, Debug)]
pub struct TextRun {
    pub x: f32,
    pub y: f32,
    pub w: f32,
    pub h: f32,
    pub text: String,
    /// True when this run continues the previous one mid-word (a word wrapped
    /// across lines yields one run per line fragment). Copy joins such runs
    /// with no separator, so a wrapped URL pastes back in one piece.
    pub cont: bool,
}

impl TextRun {
    pub fn contains(&self, x: f32, y: f32) -> bool {
        x >= self.x && x <= self.x + self.w && y >= self.y && y <= self.y + self.h
    }
}

/// A rendered email: tightly-cropped RGBA8 bitmap (non-premultiplied, row-major,
/// `width_px * height_px * 4` bytes) plus its pixel dimensions and the geometry
/// the UI needs for links and text selection.
pub struct Rendered {
    pub rgba: Vec<u8>,
    pub width_px: u32,
    pub height_px: u32,
    /// Clickable link boxes, CSS px.
    pub links: Vec<LinkRect>,
    /// Text layer for mouse selection, CSS px, in reading order (a selection
    /// spanning two runs takes everything between them in this order).
    pub runs: Vec<TextRun>,
    /// The scale the bitmap was painted at: bitmap px = CSS px × scale.
    /// Echoes `RenderOptions::scale`.
    pub scale: f32,
}

impl Rendered {
    /// Logical (CSS px) size of the render — what the UI lays out with.
    pub fn css_size(&self) -> (f32, f32) {
        (self.width_px as f32 / self.scale, self.height_px as f32 / self.scale)
    }

    /// href of the topmost link under a CSS-px point, if any.
    pub fn hit(&self, x: f32, y: f32) -> Option<&str> {
        self.links.iter().rev().find(|l| l.contains(x, y)).map(|l| l.href.as_str())
    }
}

/// Bytes for a resource this crate will not go and get itself.
///
/// The renderer opens no sockets and reads no files, by design — a renderer
/// that fetches is one that leaks read receipts and stalls a scroll panel on
/// someone else's CDN, and neither belongs behind a layout API. `data:` images
/// decode without help; everything else (`https:` when the user has allowed
/// that sender's media, `cid:` parts out of the MIME tree) is the host's to
/// resolve, against its own permission policy, cache and attachment store.
///
/// Called from the layout thread, once per distinct `src`. Return `None` for
/// anything unavailable and the image falls back to its placeholder.
pub trait Resources {
    fn fetch(&self, src: &str) -> Option<Vec<u8>>;
}

/// The default: nothing resolves, everything remote is a placeholder.
struct NoResources;

impl Resources for NoResources {
    fn fetch(&self, _src: &str) -> Option<Vec<u8>> {
        None
    }
}

mod dom;
mod image;
mod layout;
#[cfg(feature = "net")]
pub mod net;
mod paint;
mod style;
mod table;
mod text;

/// Render one email body (the `html` column from cache.db) to a bitmap.
///
/// Never panics on malformed HTML; degrade gracefully. Never returns a bitmap
/// wider than `opts.width * opts.scale`.
pub fn render(html: &str, opts: &RenderOptions) -> Rendered {
    render_with(html, opts, &NoResources)
}

/// Same, with a host-supplied resolver for images the crate cannot decode on
/// its own. See [`Resources`].
pub fn render_with(html: &str, opts: &RenderOptions, resources: &dyn Resources) -> Rendered {
    let scale = opts.scale.clamp(0.5, 4.0);
    let width_px = ((opts.width as f32 * scale).round() as u32).clamp(1, 8192);
    // The pipeline is written not to panic, but it runs on bytes that arrived
    // from strangers: a bug here must cost one blank bubble, not the client.
    std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
        render_inner(html, scale, width_px, opts.block_remote, resources)
    }))
    .unwrap_or_else(|_| {
        text::reset_engine();
        Rendered {
            rgba: vec![0; (width_px as usize) * 4],
            width_px,
            height_px: 1,
            links: Vec::new(),
            runs: Vec::new(),
            scale,
        }
    })
}

fn render_inner(
    html: &str,
    scale: f32,
    width_px: u32,
    block_remote: bool,
    resources: &dyn Resources,
) -> Rendered {
    // `EMLRENDER_DEBUG=1` prints per-phase timings — the only cheap way to tell
    // a slow mail from a stuck one.
    let trace = std::env::var_os("EMLRENDER_DEBUG").is_some();
    let mut mark = std::time::Instant::now();
    let mut phase = |name: &str| {
        if trace {
            eprintln!("  [{name}] {} ms", mark.elapsed().as_millis());
            mark = std::time::Instant::now();
        }
    };

    let root = dom::parse(html);
    phase("parse");
    let sheet = style::Stylesheet::parse(&dom::collect_style_text(&root));
    phase("css");
    let resolver = style::Resolver { scale, sheet };

    let mut engine = text::engine();
    phase("fonts");
    let laid = layout::run(
        &root,
        &resolver,
        &mut engine,
        width_px as f32,
        layout::Host { block_remote, resources },
    );
    phase("layout");
    let height_px = (laid.height.ceil() as u32).clamp(1, 32_768);
    let painted = paint::paint(&laid, &mut engine, width_px, height_px, scale);
    phase("paint");
    drop(engine);

    Rendered {
        rgba: unpremultiply(&painted.pixmap),
        width_px,
        height_px,
        links: painted.links,
        runs: painted.runs,
        scale,
    }
}

/// tiny-skia works premultiplied; the API promises straight alpha.
fn unpremultiply(pixmap: &tiny_skia::Pixmap) -> Vec<u8> {
    let mut out = Vec::with_capacity(pixmap.pixels().len() * 4);
    for px in pixmap.pixels() {
        let c = px.demultiply();
        out.extend_from_slice(&[c.red(), c.green(), c.blue(), c.alpha()]);
    }
    out
}

#[cfg(test)]
mod tests {
    // `::image` throughout: this crate has a module by that name.
    use super::*;

    fn opts(width: u32) -> RenderOptions {
        RenderOptions { width, scale: 1.0, block_remote: true }
    }

    /// Straight-alpha pixel at (x, y).
    fn pixel(r: &Rendered, x: u32, y: u32) -> [u8; 4] {
        let i = ((y * r.width_px + x) * 4) as usize;
        r.rgba[i..i + 4].try_into().unwrap_or([0; 4])
    }

    fn png(w: u32, h: u32, rgba: [u8; 4]) -> Vec<u8> {
        let img = ::image::RgbaImage::from_pixel(w, h, ::image::Rgba(rgba));
        let mut out = std::io::Cursor::new(Vec::new());
        img.write_to(&mut out, ::image::ImageFormat::Png).expect("encode png");
        out.into_inner()
    }

    struct OneImage(Vec<u8>);

    impl Resources for OneImage {
        fn fetch(&self, _src: &str) -> Option<Vec<u8>> {
            Some(self.0.clone())
        }
    }

    /// The rule the whole crate exists for: nothing is ever wider than asked.
    #[test]
    fn unbreakable_text_does_not_widen_the_bitmap() {
        let url = "x".repeat(400);
        let r = render(&format!("<p>{url}</p>"), &opts(200));
        assert_eq!(r.width_px, 200);
        assert!(r.height_px > 20, "400 chars must wrap onto many lines");
    }

    /// Declared widths are clamped, not honoured — including across a table's
    /// columns, which are solved independently of the block path.
    #[test]
    fn oversized_table_stays_inside_the_width() {
        let html = r#"<table width="1200"><tr>
            <td width="600">left column text</td><td width="600">right column text</td>
        </tr></table>"#;
        let r = render(html, &opts(300));
        assert_eq!(r.width_px, 300);
        for run in &r.runs {
            assert!(run.x + run.w <= 300.5, "run {:?} escapes the width", run.text);
        }
        let text: String = r.runs.iter().map(|t| t.text.as_str()).collect::<Vec<_>>().join(" ");
        assert!(text.contains("left"), "left cell lost: {text}");
        assert!(text.contains("right"), "right cell lost: {text}");
    }

    /// A remote image is a placeholder by default and real pixels when the host
    /// resolves it — the whole point of [`Resources`].
    #[test]
    fn resolver_supplies_remote_image_pixels() {
        const RED: [u8; 4] = [220, 20, 60, 255];
        let html = r#"<img src="https://example.invalid/p.png" width="40" height="40" alt="">"#;
        let open = RenderOptions { block_remote: false, ..opts(60) };

        let blocked = render(html, &opts(60));
        assert_ne!(pixel(&blocked, 20, 20)[0..3], RED[0..3], "must not fetch by itself");

        let loaded = render_with(html, &open, &OneImage(png(4, 4, RED)));
        assert_eq!(pixel(&loaded, 20, 20)[0..3], RED[0..3], "resolver bytes must be drawn");
    }

    /// `cid:` is a part of the message, not the network: `block_remote` must
    /// not gate it, or attachments never render.
    #[test]
    fn cid_is_resolved_even_when_remote_is_blocked() {
        const BLUE: [u8; 4] = [30, 144, 255, 255];
        let html = r#"<img src="cid:part1@mail" width="40" height="40" alt="">"#;
        let r = render_with(html, &opts(60), &OneImage(png(4, 4, BLUE)));
        assert_eq!(pixel(&r, 20, 20)[0..3], BLUE[0..3]);
    }

    /// A blocked image says what it was instead of being an anonymous grey box.
    #[test]
    fn blocked_image_shows_its_alt_text() {
        let html = r#"<img src="https://example.invalid/logo.png" width="200" height="80"
                           alt="Логотип">"#;
        let r = render(html, &opts(300));
        assert!(
            r.runs.iter().any(|t| t.text.contains("Логотип")),
            "alt text missing from the text layer"
        );
    }

    /// Links come back as boxes over the words that carry them.
    #[test]
    fn link_boxes_cover_their_text() {
        let r = render(r#"<p>go <a href="https://example.invalid/x">here</a> now</p>"#, &opts(300));
        let link = r.links.first().expect("one link");
        assert_eq!(link.href, "https://example.invalid/x");
        let word = r.runs.iter().find(|t| t.text.contains("here")).expect("word");
        assert!(link.contains(word.x + word.w / 2.0, word.y + word.h / 2.0));
    }

    /// A tall `rowspan` cell owes its height to the last row it covers, not
    /// the first — otherwise row one inflates and the table is mostly air.
    #[test]
    fn rowspan_does_not_inflate_the_row_it_starts_in() {
        let tall = "tall ".repeat(40);
        let html = format!(
            r#"<table><tr><td rowspan="2">{tall}</td><td>alpha</td></tr>
               <tr><td>beta</td></tr></table>"#
        );
        let r = render(&html, &opts(200));
        let y = |w: &str| r.runs.iter().find(|t| t.text == w).map(|t| t.y).unwrap_or(-1.0);
        let (a, b) = (y("alpha"), y("beta"));
        assert!(a >= 0.0 && b >= 0.0, "both rows must render");
        assert!(b > a, "second row goes under the first");
        assert!(b - a < 40.0, "row one grew to the spanning cell: {a} → {b}");
    }

    /// `letter-spacing` widens a line by roughly what it says, in px.
    ///
    /// The unit is the whole test: cosmic-text wants a fraction of the font
    /// size, and feeding it px instead spaces a button label out to one em per
    /// glyph — which reads as a font-fallback bug and is nearly invisible in a
    /// diff.
    #[test]
    fn letter_spacing_is_in_pixels_not_ems() {
        let word = "iiiiiiiiii"; // ten glyphs, ten gaps added
        let plain = render(&format!("<p>{word}</p>"), &opts(400));
        let spaced =
            render(&format!(r#"<p style="letter-spacing:2px">{word}</p>"#), &opts(400));
        let width = |r: &Rendered| r.runs.first().map_or(0.0, |t| t.w);
        let grew = width(&spaced) - width(&plain);
        assert!(grew > 10.0, "spacing had no effect: {grew}");
        assert!(grew < 40.0, "spacing applied in the wrong unit: {grew}");
    }

    /// An icon inside a sentence stays inside the sentence.
    ///
    /// A shaped run holds glyphs, not pictures, so the image is reserved as a
    /// run of no-break spaces and painted over where shaping put it. If that
    /// ever regresses, the line breaks around every icon in every mail.
    #[test]
    fn inline_image_sits_on_the_line() {
        const RED: [u8; 4] = [220, 20, 60, 255];
        let html = r#"<p>before <a href="https://example.invalid/x">alpha
             <img src="cid:icon" width="16" height="16" alt=""> beta</a> after</p>"#;
        let r = render_with(html, &opts(400), &OneImage(png(4, 4, RED)));

        let run = |w: &str| r.runs.iter().find(|t| t.text == w).cloned().expect("word");
        let (before, alpha, beta, after) =
            (run("before"), run("alpha"), run("beta"), run("after"));
        assert_eq!(before.y, alpha.y, "text before the icon left the line");
        assert_eq!(alpha.y, beta.y, "the icon broke the line");
        assert_eq!(beta.y, after.y, "text after the icon left the line");

        // The icon is painted, and painted between the words it was written
        // between — not at the start of some line of its own.
        let mut bounds: Option<(u32, u32)> = None;
        let mut painted = 0;
        for i in (0..r.rgba.len()).step_by(4) {
            if r.rgba[i..i + 3] == RED[0..3] {
                painted += 1;
                let px = (i as u32 / 4) % r.width_px;
                bounds = Some(match bounds {
                    Some((lo, hi)) => (lo.min(px), hi.max(px)),
                    None => (px, px),
                });
            }
        }
        assert!(painted > 150, "icon barely drawn: {painted} px");
        let (x0, x1) = bounds.expect("icon pixels");
        assert!(x0 as f32 >= alpha.x + alpha.w - 2.0, "icon drawn before its word");
        assert!((x1 as f32) <= beta.x + 2.0, "icon drawn past its word");
    }

    /// …but an image too wide to share a line still gets one of its own.
    #[test]
    fn oversized_inline_image_breaks_the_line() {
        let html = r#"<p>alpha <img src="cid:hero" width="380" height="80" alt=""> beta</p>"#;
        let r = render(html, &opts(400));
        let y = |w: &str| r.runs.iter().find(|t| t.text == w).map(|t| t.y).unwrap_or(-1.0);
        assert!(y("beta") > y("alpha") + 40.0, "a 380 px image cannot share a 400 px line");
    }

    /// A linear gradient runs in the direction CSS says it does.
    ///
    /// `to right` is the easy one to get backwards: CSS measures the angle from
    /// "up", clockwise, while screen y grows downward.
    #[test]
    fn linear_gradient_runs_the_right_way() {
        let html = r#"<div style="background:linear-gradient(to right,#ff0000,#0000ff);
                                  height:20px">&nbsp;</div>"#;
        let r = render(html, &opts(100));
        let (left, right) = (pixel(&r, 2, 10), pixel(&r, 97, 10));
        assert!(left[0] > 200 && left[2] < 60, "left end is not red: {left:?}");
        assert!(right[2] > 200 && right[0] < 60, "right end is not blue: {right:?}");
    }

    /// `background-image: url(data:…)` paints, and its base64 survives the
    /// journey — the value must not be lower-cased on the way in.
    #[test]
    fn background_image_paints_from_a_data_uri() {
        use base64::Engine as _;
        let data = base64::engine::general_purpose::STANDARD.encode(png(4, 4, [0, 128, 0, 255]));
        let html = format!(
            r#"<div style="background-image:url(data:image/png;base64,{data});
                           background-size:cover;height:20px">&nbsp;</div>"#
        );
        let r = render(&html, &opts(100));
        assert_eq!(pixel(&r, 50, 10)[0..3], [0, 128, 0], "backdrop not painted");
    }

    /// A radial gradient is bright where its centre is, not where the box is.
    #[test]
    fn radial_gradient_centres_where_told() {
        let html = r#"<div style="background:radial-gradient(circle at left top,#ffffff,#000000);
                                  height:60px">&nbsp;</div>"#;
        let r = render(html, &opts(60));
        let corner = pixel(&r, 2, 2)[0] as i32;
        let far = pixel(&r, 57, 57)[0] as i32;
        assert!(corner > far + 100, "centre not at the top-left: {corner} vs {far}");
    }

    /// `background-position` moves a non-repeating backdrop.
    #[test]
    fn background_position_places_the_backdrop() {
        use base64::Engine as _;
        let tile = base64::engine::general_purpose::STANDARD.encode(png(8, 8, [255, 0, 0, 255]));
        let at = |pos: &str| {
            let html = format!(
                r#"<div style="background:#ffffff url(data:image/png;base64,{tile}) no-repeat
                               {pos};height:60px">&nbsp;</div>"#
            );
            let r = render(&html, &opts(60));
            // Centre of the drawn tile, as a fraction of the box.
            let reds: Vec<(u32, u32)> = (0..r.rgba.len() / 4)
                .filter(|i| r.rgba[i * 4] > 200 && r.rgba[i * 4 + 1] < 60)
                .map(|i| (i as u32 % r.width_px, i as u32 / r.width_px))
                .collect();
            assert!(!reds.is_empty(), "backdrop missing for `{pos}`");
            let n = reds.len() as u32;
            (reds.iter().map(|p| p.0).sum::<u32>() / n, reds.iter().map(|p| p.1).sum::<u32>() / n)
        };
        let (lx, ly) = at("left top");
        let (rx, ry) = at("right bottom");
        assert!(rx > lx + 30 && ry > ly + 30, "tile did not move: {lx},{ly} → {rx},{ry}");
    }

    /// An inline-level box is measured as a box, frame and all.
    ///
    /// Walking into it as text loses its padding, so it lays out narrower than
    /// its own label and the label then wraps a glyph at a time — which is what
    /// a bulletproof button (`padding: 14px 36px` around six characters) did.
    #[test]
    fn inline_block_keeps_its_own_padding() {
        let html = r#"<p><a href="https://example.invalid/x"
             style="display:inline-block;padding:14px 36px;background:#141413;color:#fff">
             Sign in</a></p>"#;
        let r = render(html, &opts(400));
        let word = |w: &str| r.runs.iter().find(|t| t.text == w).cloned().expect("label word");
        assert_eq!(word("Sign").y, word("in").y, "the button broke its own label");
    }

    /// Inline-level boxes share a line instead of stacking.
    #[test]
    fn inline_blocks_sit_side_by_side() {
        let cell = r#"<table style="display:inline-table"><tr><td>%</td></tr></table>"#;
        let html = format!("<div>{}</div>", cell.replace('%', "A") + &cell.replace('%', "B"));
        let r = render(&html, &opts(400));
        let y = |w: &str| r.runs.iter().find(|t| t.text == w).map(|t| t.y).expect("cell");
        assert_eq!(y("A"), y("B"), "inline-tables stacked instead of lining up");
    }

    /// A declared cell width counts toward what a shrink-to-fit table wants.
    ///
    /// The MJML logo: `<td style="width:112px">` holding an image sized
    /// `width:100%`. Nothing there states an intrinsic width, so a table that
    /// only measures content collapses to a sliver — or, before tables, the
    /// percentage resolved against the whole bubble and the logo filled it.
    #[test]
    fn declared_cell_width_survives_a_percentage_child() {
        const RED: [u8; 4] = [220, 20, 60, 255];
        let html = r#"<table><tr><td style="width:112px"><img src="cid:logo"
             width="112" height="33" style="width:100%;height:33px" alt="L"></td></tr></table>"#;
        let r = render_with(html, &opts(400), &OneImage(png(4, 4, RED)));
        let xs: Vec<u32> = (0..r.rgba.len() / 4)
            .filter(|i| r.rgba[i * 4..i * 4 + 3] == RED[0..3])
            .map(|i| i as u32 % r.width_px)
            .collect();
        let (lo, hi) = (*xs.iter().min().expect("logo drawn"), *xs.iter().max().unwrap());
        let drawn = hi - lo + 1;
        assert!((100..=125).contains(&drawn), "logo is {drawn} px wide, wanted about 112");
    }

    /// SVG is text, not a raster format: no decoder sniffs it, and mail puts
    /// logos in it constantly.
    #[test]
    fn svg_is_rasterised() {
        use base64::Engine as _;
        // Two hashes: the fill colour contains `"#`, which would end a `r#""#`.
        let svg = br##"<svg xmlns="http://www.w3.org/2000/svg" width="40" height="20">
            <rect width="40" height="20" fill="#00ff00"/></svg>"##;
        let data = base64::engine::general_purpose::STANDARD.encode(svg);
        let html = format!(r#"<img src="data:image/svg+xml;base64,{data}" alt="v">"#);
        let r = render(&html, &opts(100));
        assert_eq!(pixel(&r, 20, 10)[0..3], [0, 255, 0], "svg not rasterised");
    }

    /// `border-radius` clips the picture, not just the box around it. Mail
    /// rounds avatars, and a square one reads as broken.
    #[test]
    fn border_radius_clips_an_image() {
        use base64::Engine as _;
        let data = base64::engine::general_purpose::STANDARD.encode(png(8, 8, [220, 20, 60, 255]));
        let html = format!(
            r#"<img src="data:image/png;base64,{data}" width="40" height="40"
                 style="border-radius:20px" alt="a">"#
        );
        let r = render(&html, &opts(60));
        assert_eq!(pixel(&r, 20, 20)[0..3], [220, 20, 60], "middle should be the image");
        assert!(pixel(&r, 1, 1)[3] < 40, "corner should have been clipped away");
    }

    /// Malformed input must cost a bad-looking bubble, never a panic.
    #[test]
    fn hostile_input_does_not_panic() {
        for html in [
            "",
            "<table><tr><td colspan=99999 rowspan=99999>x",
            &"<div>".repeat(500),
            r#"<img src="data:image/png;base64,!!!!" width="10" height="10">"#,
            "<p style='line-height:0;font-size:0'>preheader</p>",
            // An inline-level box whose minimum content is wider than the line.
            "<div style='display:inline-block'><div style='width:900px'>x</div></div>",
        ] {
            let r = render(html, &opts(200));
            assert_eq!(r.width_px, 200);
        }
    }
}
