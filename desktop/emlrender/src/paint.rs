//! Paint: display list → pixels, plus the geometry the UI needs on top.
//!
//! Link boxes and the word-level text layer are produced here rather than
//! during layout because both are read off the *shaped* glyph positions — the
//! only place that knows where a word actually landed after wrapping.

use cosmic_text::Color as CtColor;
use image::{ImageBuffer, Rgba as Rgba8};
use tiny_skia::{
    Color, FilterQuality, FillRule, GradientStop, LinearGradient, Paint, Pattern,
    PathBuilder, Pixmap, PremultipliedColorU8, Point, RadialGradient, Rect, SpreadMode, Transform,
};

use crate::image::Bitmap;
use crate::layout::{Cmd, Layout, Ramp};
use crate::style::{BgPos, BgSize, Edges, Rgba};
use crate::text::{Span, TextEngine};
use crate::{LinkRect, TextRun};

pub struct Painted {
    pub pixmap: Pixmap,
    pub links: Vec<LinkRect>,
    pub runs: Vec<TextRun>,
}

pub fn paint(layout: &Layout, eng: &mut TextEngine, w_px: u32, h_px: u32, scale: f32) -> Painted {
    let mut pixmap = Pixmap::new(w_px.max(1), h_px.max(1)).unwrap_or_else(|| {
        // Only fails on a zero/overflowing size, both already clamped.
        Pixmap::new(1, 1).expect("1x1 pixmap")
    });
    let mut links = Vec::new();
    let mut runs = Vec::new();

    for cmd in &layout.cmds {
        match cmd {
            Cmd::Rect { x, y, w, h, radius, color } => {
                fill_round_rect(&mut pixmap, *x, *y, *w, *h, *radius, *color);
            }
            Cmd::Border { x, y, w, h, radius, widths, color } => {
                stroke_border(&mut pixmap, *x, *y, *w, *h, *radius, *widths, *color);
            }
            Cmd::Image { x, y, w, h, radius, bitmap, link } => {
                match bitmap {
                    Some(b) => draw_bitmap(&mut pixmap, b, *x, *y, *w, *h, *radius),
                    None => paint_placeholder(&mut pixmap, *x, *y, *w, *h),
                }
                if let Some(i) = link {
                    if let Some(href) = layout.hrefs.get(*i) {
                        links.push(LinkRect {
                            x: x / scale,
                            y: y / scale,
                            w: w / scale,
                            h: h / scale,
                            href: href.clone(),
                        });
                    }
                }
            }
            Cmd::Gradient { x, y, w, h, radius, kind, stops } => {
                fill_gradient(&mut pixmap, *x, *y, *w, *h, *radius, *kind, stops);
            }
            Cmd::Backdrop { x, y, w, h, bitmap, size, repeat, pos } => {
                fill_backdrop(&mut pixmap, bitmap, *x, *y, *w, *h, *size, *repeat, *pos);
            }
            Cmd::Text { x, y, buffer, spans } => {
                paint_text(
                    &mut pixmap,
                    eng,
                    buffer,
                    spans,
                    &layout.hrefs,
                    *x,
                    *y,
                    scale,
                    &mut links,
                    &mut runs,
                );
            }
        }
    }
    Painted { pixmap, links, runs }
}

// ----------------------------------------------------------------- geometry

fn round_rect_path(x: f32, y: f32, w: f32, h: f32, r: f32) -> Option<tiny_skia::Path> {
    if w <= 0.0 || h <= 0.0 {
        return None;
    }
    let r = r.min(w / 2.0).min(h / 2.0).max(0.0);
    let mut pb = PathBuilder::new();
    if r <= 0.5 {
        pb.push_rect(Rect::from_xywh(x, y, w, h)?);
        return pb.finish();
    }
    let (x1, y1) = (x + w, y + h);
    // Cubic, not quadratic: a quadratic cannot approximate a quarter circle
    // well, and at `r == w/2` — a round avatar, which mail is full of — the
    // difference is a visible squircle instead of a circle. 0.5523 is the
    // standard control-point offset for a circular arc.
    let k = r * 0.552_284_75;
    pb.move_to(x + r, y);
    pb.line_to(x1 - r, y);
    pb.cubic_to(x1 - r + k, y, x1, y + r - k, x1, y + r);
    pb.line_to(x1, y1 - r);
    pb.cubic_to(x1, y1 - r + k, x1 - r + k, y1, x1 - r, y1);
    pb.line_to(x + r, y1);
    pb.cubic_to(x + r - k, y1, x, y1 - r + k, x, y1 - r);
    pb.line_to(x, y + r);
    pb.cubic_to(x, y + r - k, x + r - k, y, x + r, y);
    pb.close();
    pb.finish()
}

fn paint_of(c: Rgba) -> Paint<'static> {
    let mut p = Paint::default();
    p.set_color_rgba8(c.r, c.g, c.b, c.a);
    p.anti_alias = true;
    p
}

fn fill_round_rect(pm: &mut Pixmap, x: f32, y: f32, w: f32, h: f32, r: f32, c: Rgba) {
    if !c.is_visible() {
        return;
    }
    if let Some(path) = round_rect_path(x, y, w, h, r) {
        pm.fill_path(&path, &paint_of(c), FillRule::Winding, Transform::identity(), None);
    }
}

/// Borders are painted as up to four filled bars rather than a stroked path:
/// mail uses per-side borders (a single left rule on a blockquote, a bottom
/// hairline on a row) far more often than a uniform box.
fn stroke_border(pm: &mut Pixmap, x: f32, y: f32, w: f32, h: f32, r: f32, e: Edges, c: Rgba) {
    if !c.is_visible() {
        return;
    }
    let uniform = e.top > 0.0 && (e.top - e.right).abs() < 0.01 && (e.top - e.bottom).abs() < 0.01
        && (e.top - e.left).abs() < 0.01;
    if uniform && r > 0.5 {
        // Rounded and uniform: ring = outer path minus inset path.
        if let (Some(outer), Some(inner)) = (
            round_rect_path(x, y, w, h, r),
            round_rect_path(x + e.top, y + e.top, w - 2.0 * e.top, h - 2.0 * e.top, (r - e.top).max(0.0)),
        ) {
            let mut pb = PathBuilder::new();
            pb.push_path(&outer);
            pb.push_path(&inner);
            if let Some(ring) = pb.finish() {
                pm.fill_path(&ring, &paint_of(c), FillRule::EvenOdd, Transform::identity(), None);
            }
        }
        return;
    }
    let bar = |pm: &mut Pixmap, bx: f32, by: f32, bw: f32, bh: f32| {
        if bw > 0.0 && bh > 0.0 {
            if let Some(rect) = Rect::from_xywh(bx, by, bw, bh) {
                pm.fill_rect(rect, &paint_of(c), Transform::identity(), None);
            }
        }
    };
    bar(pm, x, y, w, e.top);
    bar(pm, x, y + h - e.bottom, w, e.bottom);
    bar(pm, x, y, e.left, h);
    bar(pm, x + w - e.right, y, e.right, h);
}

/// Neutral stand-in for an image we did not (or would not) load. Layout writes
/// the `alt` text over it separately, when there is one worth showing.
fn paint_placeholder(pm: &mut Pixmap, x: f32, y: f32, w: f32, h: f32) {
    fill_round_rect(pm, x, y, w, h, 2.0, Rgba::rgb(0xef, 0xf1, 0xf4));
    stroke_border(
        pm,
        x,
        y,
        w,
        h,
        2.0,
        Edges::all(1.0),
        Rgba { r: 0xc8, g: 0xcd, b: 0xd4, a: 0xff },
    );
}

/// Paint a CSS linear gradient across a box.
///
/// CSS measures the angle clockwise from "up", and the gradient line is the one
/// through the centre whose length makes the corners land exactly on the first
/// and last stop — hence `|w·sin| + |h·cos|` rather than the box diagonal.
fn fill_gradient(
    pm: &mut Pixmap,
    x: f32,
    y: f32,
    w: f32,
    h: f32,
    radius: f32,
    kind: Ramp,
    stops: &[(Rgba, f32)],
) {
    if stops.len() < 2 || w <= 0.0 || h <= 0.0 {
        return;
    }
    let ramp: Vec<GradientStop> = stops
        .iter()
        .map(|(c, p)| {
            GradientStop::new(p.clamp(0.0, 1.0), Color::from_rgba8(c.r, c.g, c.b, c.a))
        })
        .collect();
    let shader = match kind {
        Ramp::Linear(angle) => {
            let rad = angle.to_radians();
            let (sin, cos) = (rad.sin(), rad.cos());
            let len = (w * sin).abs() + (h * cos).abs();
            let (cx, cy) = (x + w / 2.0, y + h / 2.0);
            // Screen y grows downward, so "up" is negative cos.
            let (dx, dy) = (sin * len / 2.0, -cos * len / 2.0);
            LinearGradient::new(
                Point::from_xy(cx - dx, cy - dy),
                Point::from_xy(cx + dx, cy + dy),
                ramp,
                SpreadMode::Pad,
                Transform::identity(),
            )
        }
        Ramp::Radial(fx, fy) => {
            let (cx, cy) = (x + w * fx, y + h * fy);
            // CSS default is farthest-corner: the last stop lands on whichever
            // corner is furthest from the centre.
            let r = [(x, y), (x + w, y), (x, y + h), (x + w, y + h)]
                .iter()
                .map(|(px, py)| ((px - cx).powi(2) + (py - cy).powi(2)).sqrt())
                .fold(1.0f32, f32::max);
            RadialGradient::new(
                Point::from_xy(cx, cy),
                0.0,
                Point::from_xy(cx, cy),
                r,
                ramp,
                SpreadMode::Pad,
                Transform::identity(),
            )
        }
    };
    let Some(shader) = shader else { return };
    let Some(path) = round_rect_path(x, y, w, h, radius) else { return };
    let paint = Paint { shader, anti_alias: true, ..Default::default() };
    pm.fill_path(&path, &paint, FillRule::Winding, Transform::identity(), None);
}

/// Paint a `background-image: url(...)` across a box, sized and tiled per CSS.
fn fill_backdrop(
    pm: &mut Pixmap,
    bmp: &Bitmap,
    x: f32,
    y: f32,
    w: f32,
    h: f32,
    size: BgSize,
    repeat: bool,
    pos: BgPos,
) {
    if w <= 0.0 || h <= 0.0 || bmp.w == 0 || bmp.h == 0 {
        return;
    }
    let (nw, nh) = (bmp.w as f32, bmp.h as f32);
    let (tw, th) = match size {
        BgSize::Stretch => (w, h),
        BgSize::Cover => {
            let k = (w / nw).max(h / nh);
            (nw * k, nh * k)
        }
        BgSize::Contain => {
            let k = (w / nw).min(h / nh);
            (nw * k, nh * k)
        }
        BgSize::Auto => (nw, nh),
    };
    let (tw, th) = (tw.round().max(1.0) as u32, th.round().max(1.0) as u32);
    let Some(src) = ImageBuffer::<Rgba8<u8>, _>::from_raw(bmp.w, bmp.h, bmp.rgba.as_slice()) else {
        return;
    };
    let scaled;
    let tile: &dyn Fn(u32, u32) -> [u8; 4] = if (tw, th) == (bmp.w, bmp.h) {
        &|px, py| src.get_pixel(px, py).0
    } else {
        scaled = image::imageops::resize(&src, tw, th, image::imageops::FilterType::Triangle);
        &|px, py| scaled.get_pixel(px, py).0
    };

    // `background-position` is a fraction of the *free* space, so a tile as
    // large as the box cannot move at all — which is what CSS says too.
    let (ox, oy) = (x.round() as i32, y.round() as i32);
    let (bw, bh) = (w.round().max(1.0) as u32, h.round().max(1.0) as u32);
    let offset = |free: i64, at: f32| (free.max(0) as f32 * at).round() as i64;
    let px0 = offset(bw as i64 - tw as i64, pos.x);
    let py0 = offset(bh as i64 - th as i64, pos.y);

    for row in 0..bh {
        for col in 0..bw {
            let (dx, dy) = (col as i64 - px0, row as i64 - py0);
            let (sx, sy) = if repeat {
                // Modulo that stays positive to the left of the origin.
                (
                    dx.rem_euclid(tw as i64) as u32,
                    dy.rem_euclid(th as i64) as u32,
                )
            } else if dx < 0 || dy < 0 {
                continue;
            } else {
                (dx as u32, dy as u32)
            };
            if sx >= tw || sy >= th {
                continue;
            }
            let p = tile(sx, sy);
            blend_pixel(pm, ox + col as i32, oy + row as i32, p[0], p[1], p[2], p[3]);
        }
    }
}

/// Blit a decoded image into its box.
///
/// Resampling happens on the CPU through `image` rather than as a pattern
/// transform: mail ships 1200 px hero images that land in a 400 px box, and a
/// bilinear tap at that ratio drops most of the pixels on the floor and aliases
/// hard on text-in-images, which mail is unfortunately full of.
fn draw_bitmap(pm: &mut Pixmap, bmp: &Bitmap, x: f32, y: f32, w: f32, h: f32, radius: f32) {
    let (tw, th) = (w.round().max(1.0) as u32, h.round().max(1.0) as u32);
    let Some(src) = ImageBuffer::<Rgba8<u8>, _>::from_raw(bmp.w, bmp.h, bmp.rgba.as_slice()) else {
        return;
    };
    let scaled;
    let pixels: &dyn Fn(u32, u32) -> [u8; 4] = if (tw, th) == (bmp.w, bmp.h) {
        &|px, py| src.get_pixel(px, py).0
    } else {
        scaled = image::imageops::resize(&src, tw, th, image::imageops::FilterType::Triangle);
        &|px, py| scaled.get_pixel(px, py).0
    };

    if radius <= 0.5 {
        let (ox, oy) = (x.round() as i32, y.round() as i32);
        for py in 0..th {
            for px in 0..tw {
                let p = pixels(px, py);
                blend_pixel(pm, ox + px as i32, oy + py as i32, p[0], p[1], p[2], p[3]);
            }
        }
        return;
    }

    // Rounded: hand the pixels to tiny-skia as a pattern and fill the rounded
    // path with it, so the corners are clipped *and* antialiased. Mail rounds
    // avatars and product tiles constantly, and a square avatar reads as broken.
    let Some(mut tile) = Pixmap::new(tw, th) else { return };
    for (i, px) in tile.pixels_mut().iter_mut().enumerate() {
        let p = pixels(i as u32 % tw, i as u32 / tw);
        // The pattern shader wants premultiplied; `Bitmap` is straight alpha.
        let a = p[3] as u32;
        let mul = |c: u8| ((c as u32 * a + 127) / 255) as u8;
        *px = PremultipliedColorU8::from_rgba(mul(p[0]), mul(p[1]), mul(p[2]), p[3])
            .unwrap_or(*px);
    }
    let Some(path) = round_rect_path(x, y, w, h, radius) else { return };
    let paint = Paint {
        shader: Pattern::new(
            tile.as_ref(),
            SpreadMode::Pad,
            FilterQuality::Nearest, // already resampled to the exact box
            1.0,
            Transform::from_translate(x.round(), y.round()),
        ),
        anti_alias: true,
        ..Default::default()
    };
    pm.fill_path(&path, &paint, FillRule::Winding, Transform::identity(), None);
}

// --------------------------------------------------------------------- text

#[allow(clippy::too_many_arguments)]
fn paint_text(
    pm: &mut Pixmap,
    eng: &mut TextEngine,
    buffer: &cosmic_text::Buffer,
    spans: &[Span],
    hrefs: &[String],
    ox: f32,
    oy: f32,
    scale: f32,
    links: &mut Vec<LinkRect>,
    runs_out: &mut Vec<TextRun>,
) {
    let TextEngine { fonts, swash } = eng;
    let mut prev_line: Option<usize> = None;

    for run in buffer.layout_runs() {
        let baseline = oy + run.line_y;
        // --- pixels ---
        for glyph in run.glyphs {
            let span = spans.get(glyph.metadata);
            let color = span.map(|s| s.color).unwrap_or(Rgba::BLACK);
            let phys = glyph.physical((ox, baseline), 1.0);
            let base = CtColor::rgba(color.r, color.g, color.b, color.a);
            swash.with_pixels(fonts, phys.cache_key, base, |dx, dy, c| {
                blend_pixel(pm, phys.x + dx, phys.y + dy, c.r(), c.g(), c.b(), c.a());
            });
        }

        // --- decorations, links, selection words: all glyph-run grouping ---
        let line_top = oy + run.line_top;
        let line_h = run.line_height;

        group_by(run.glyphs, |g| spans.get(g.metadata).and_then(|s| s.link), |link, x0, x1| {
            if let Some(href) = link.and_then(|i| hrefs.get(i)) {
                links.push(LinkRect {
                    x: (ox + x0) / scale,
                    y: line_top / scale,
                    w: (x1 - x0) / scale,
                    h: line_h / scale,
                    href: href.clone(),
                });
            }
        });

        group_by(
            run.glyphs,
            |g| spans.get(g.metadata).map(|s| (s.underline, s.strike, s.color, s.size)),
            |deco, x0, x1| {
                let Some((underline, strike, color, size)) = deco else { return };
                let thickness = (size * 0.06).max(1.0);
                if underline {
                    fill_round_rect(pm, ox + x0, baseline + size * 0.12, x1 - x0, thickness, 0.0, color);
                }
                if strike {
                    fill_round_rect(pm, ox + x0, baseline - size * 0.28, x1 - x0, thickness, 0.0, color);
                }
            },
        );

        // Words for the selection layer. A word is a maximal glyph span with no
        // whitespace between the cluster boundaries.
        let mut word_start: Option<(f32, f32, usize)> = None;
        let flush = |word: Option<(f32, f32, usize)>, runs_out: &mut Vec<TextRun>, cont: bool| {
            if let Some((x0, x1, start)) = word {
                let text: String = run.text[start..].chars().take_while(|c| !c.is_whitespace()).collect();
                if !text.is_empty() {
                    runs_out.push(TextRun {
                        x: (ox + x0) / scale,
                        y: line_top / scale,
                        w: (x1 - x0) / scale,
                        h: line_h / scale,
                        text,
                        cont,
                    });
                }
            }
        };
        let mut first_word = true;
        for glyph in run.glyphs {
            let cluster = run.text.get(glyph.start..glyph.end).unwrap_or("");
            let is_space = cluster.chars().all(char::is_whitespace);
            if is_space {
                let cont = first_word && continues_previous(&run, prev_line);
                flush(word_start.take(), runs_out, cont);
                first_word = false;
            } else {
                match &mut word_start {
                    Some((_, x1, _)) => *x1 = glyph.x + glyph.w,
                    None => word_start = Some((glyph.x, glyph.x + glyph.w, glyph.start)),
                }
            }
        }
        let cont = first_word && continues_previous(&run, prev_line);
        flush(word_start.take(), runs_out, cont);
        prev_line = Some(run.line_i);
    }
}

/// True when this layout run is a wrap continuation of the previous one *and*
/// the break fell mid-word (no whitespace before the first glyph).
fn continues_previous(run: &cosmic_text::LayoutRun, prev_line: Option<usize>) -> bool {
    if prev_line != Some(run.line_i) {
        return false;
    }
    let Some(first) = run.glyphs.first() else { return false };
    run.text[..first.start].chars().next_back().is_some_and(|c| !c.is_whitespace())
}

/// Fold consecutive glyphs sharing a key into one x-span.
fn group_by<K: PartialEq, F, G>(glyphs: &[cosmic_text::LayoutGlyph], key: F, mut emit: G)
where
    F: Fn(&cosmic_text::LayoutGlyph) -> K,
    G: FnMut(K, f32, f32),
{
    let mut current: Option<(K, f32, f32)> = None;
    for g in glyphs {
        let k = key(g);
        match &mut current {
            Some((ck, _, x1)) if *ck == k => *x1 = g.x + g.w,
            Some(_) => {
                let (ck, x0, x1) = current.take().expect("checked");
                emit(ck, x0, x1);
                current = Some((k, g.x, g.x + g.w));
            }
            None => current = Some((k, g.x, g.x + g.w)),
        }
    }
    if let Some((ck, x0, x1)) = current {
        emit(ck, x0, x1);
    }
}

/// Source-over blend of a straight-alpha pixel into the premultiplied pixmap.
fn blend_pixel(pm: &mut Pixmap, x: i32, y: i32, r: u8, g: u8, b: u8, a: u8) {
    if a == 0 || x < 0 || y < 0 {
        return;
    }
    let (w, h) = (pm.width() as i32, pm.height() as i32);
    if x >= w || y >= h {
        return;
    }
    let idx = (y as usize) * (w as usize) + (x as usize);
    let px = pm.pixels_mut();
    let dst = px[idx];
    let sa = a as u32;
    let inv = 255 - sa;
    let pm_ch = |c: u8| (c as u32 * sa + 127) / 255;
    let out = |s: u32, d: u8| -> u8 { (s + (d as u32 * inv + 127) / 255).min(255) as u8 };
    let (nr, ng, nb) = (
        out(pm_ch(r), dst.red()),
        out(pm_ch(g), dst.green()),
        out(pm_ch(b), dst.blue()),
    );
    let na = out(sa, dst.alpha());
    // Premultiplied invariant: channels can't exceed alpha.
    px[idx] = PremultipliedColorU8::from_rgba(nr.min(na), ng.min(na), nb.min(na), na)
        .unwrap_or(dst);
}
