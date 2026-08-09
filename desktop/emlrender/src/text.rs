//! Text shaping: styled inline spans → a laid-out cosmic-text buffer.
//!
//! cosmic-text carries three things we would otherwise have to build by hand:
//! shaping (so Cyrillic renders as more than boxes), line breaking, and system
//! font fallback when a face is missing a script. We give it rich-text spans
//! tagged with `metadata` = index into our span table, and read that tag back
//! off each glyph at paint time to recover colour, underline and link.

use std::sync::{Mutex, MutexGuard, OnceLock};

use cosmic_text::{
    Attrs, Buffer, Family, FontSystem, Metrics, Shaping, Stretch, Style as FontStyle, SwashCache,
    Weight, Wrap,
};

use crate::style::{Align, Generic, Rgba, Style};

/// U+00A0. The reservation character for an inline image (see [`nbsp_advance`]).
pub const NBSP: char = '\u{00A0}';

/// One run of text sharing a single computed style.
#[derive(Clone, Debug)]
pub struct Span {
    pub text: String,
    pub size: f32,
    pub line_height: f32,
    pub weight: u16,
    pub italic: bool,
    pub underline: bool,
    pub strike: bool,
    pub color: Rgba,
    pub family: Option<String>,
    pub generic: Generic,
    pub letter_spacing: f32,
    /// Index into the paragraph's inline-object list, when this span is not
    /// text at all but a reservation for an image sitting on the line.
    pub object: Option<usize>,
    /// Index into the render's href table, when this text sits inside an `<a>`.
    pub link: Option<usize>,
}

impl Span {
    pub fn from_style(text: String, s: &Style, link: Option<usize>) -> Self {
        Self {
            text,
            size: s.font_size,
            line_height: s.line_px(),
            weight: s.font_weight,
            italic: s.italic,
            underline: s.underline,
            strike: s.strike,
            color: s.color,
            family: s.family.clone(),
            generic: s.generic,
            letter_spacing: s.letter_spacing,
            object: None,
            link,
        }
    }
}

/// Font database plus glyph raster cache. Scanning system fonts costs a few
/// hundred ms, and we render hundreds of mails in a row, so both live for the
/// lifetime of the process behind one lock.
pub struct TextEngine {
    pub fonts: FontSystem,
    pub swash: SwashCache,
}

fn shared() -> &'static Mutex<TextEngine> {
    static ENGINE: OnceLock<Mutex<TextEngine>> = OnceLock::new();
    ENGINE.get_or_init(|| {
        Mutex::new(TextEngine { fonts: FontSystem::new(), swash: SwashCache::new() })
    })
}

pub fn engine() -> MutexGuard<'static, TextEngine> {
    shared().lock().unwrap_or_else(|poisoned| poisoned.into_inner())
}

/// Throw the shared engine away and build a fresh one.
///
/// Call this after a panic escapes a render. Unwinding out of the middle of
/// shaping leaves cosmic-text's internal caches half-updated, and reusing them
/// does not merely produce a wrong glyph — it has been observed to spin
/// forever on the *next* mail. Rescanning system fonts costs a few hundred ms,
/// which is the right price for a state we can no longer trust.
pub fn reset_engine() {
    let mut guard = engine();
    *guard = TextEngine { fonts: FontSystem::new(), swash: SwashCache::new() };
}

fn attrs_for(span: &Span, index: usize) -> Attrs<'_> {
    let family = match (&span.family, span.generic) {
        (Some(name), _) => Family::Name(name.as_str()),
        (None, Generic::Serif) => Family::Serif,
        (None, Generic::Mono) => Family::Monospace,
        (None, Generic::Sans) => Family::SansSerif,
    };
    let attrs = Attrs::new()
        .family(family)
        .weight(Weight(span.weight))
        .stretch(Stretch::Normal)
        .style(if span.italic { FontStyle::Italic } else { FontStyle::Normal })
        .metrics(Metrics::new(span.size.max(1.0), span.line_height.max(1.0)))
        .metadata(index);
    // cosmic-text adds this to a glyph advance that is still normalised by
    // units-per-em, so the value is a **fraction of the font size**, not px.
    // Handing it device px stretches every glyph by about a full em — a button
    // label comes out as `О т к р ы т ь`.
    let em_fraction = span.letter_spacing / span.size.max(1.0);
    if em_fraction.abs() > 0.001 {
        attrs.letter_spacing(em_fraction)
    } else {
        attrs
    }
}

/// Shape `spans` into at most `max_w` device px. The buffer is unbounded
/// vertically — the caller measures the result and grows its box to fit.
pub fn shape(
    engine: &mut TextEngine,
    spans: &[Span],
    max_w: f32,
    align: Align,
    base_size: f32,
    base_line: f32,
) -> Buffer {
    let (base_size, base_line) = (base_size.max(1.0), base_line.max(1.0));
    let mut buffer = Buffer::new(&mut engine.fonts, Metrics::new(base_size, base_line));
    buffer.set_size(Some(max_w.max(1.0)), None);
    // WordOrGlyph is what keeps the width invariant honest: a 300-char
    // tracking URL with no spaces in it breaks mid-token instead of running
    // off the right edge.
    buffer.set_wrap(Wrap::WordOrGlyph);

    let cosmic_align = match align {
        Align::Left => None, // None == start, and avoids a needless re-layout pass
        Align::Center => Some(cosmic_text::Align::Center),
        Align::Right => Some(cosmic_text::Align::Right),
        Align::Justify => Some(cosmic_text::Align::Justified),
    };

    let default = Attrs::new().metrics(Metrics::new(base_size, base_line));
    let rich: Vec<(&str, Attrs)> =
        spans.iter().enumerate().map(|(i, s)| (s.text.as_str(), attrs_for(s, i))).collect();
    buffer.set_rich_text(rich, &default, Shaping::Advanced, cosmic_align);
    buffer.shape_until_scroll(&mut engine.fonts, false);
    buffer
}

/// Advance of one no-break space in a span's font, device px.
///
/// Inline images are reserved as a run of no-break spaces, so this says how
/// many of them a given width is worth. No-break specifically: a plain space is
/// a break opportunity, and a reservation split across two lines would place
/// the image twice.
pub fn nbsp_advance(engine: &mut TextEngine, span: &Span) -> f32 {
    let probe = Span { text: NBSP.to_string(), ..span.clone() };
    let buffer = shape(engine, &[probe], 10_000.0, Align::Left, span.size, span.line_height);
    buffer
        .layout_runs()
        .flat_map(|run| run.glyphs.iter())
        .map(|g| g.w)
        .find(|w| *w > 0.1)
        .unwrap_or(span.size * 0.3)
}

/// Min/max content width of a paragraph, in device px: the widest unbreakable
/// word, and the width the whole thing would take on one line.
///
/// Both fall out of a **single** shaping pass at effectively unbounded width —
/// the min comes from walking glyph advances between whitespace clusters. Table
/// column sizing asks this of every cell, and a second shaping pass per cell is
/// visible in the corpus timings.
pub fn measure_min_max(
    engine: &mut TextEngine,
    spans: &[Span],
    base_size: f32,
    base_line: f32,
) -> (f32, f32) {
    const UNBOUNDED: f32 = 100_000.0;
    let buffer = shape(engine, spans, UNBOUNDED, Align::Left, base_size, base_line);
    let (mut min, mut max) = (0.0f32, 0.0f32);
    for run in buffer.layout_runs() {
        max = max.max(run.line_w);
        let mut word = 0.0f32;
        for glyph in run.glyphs {
            let cluster = run.text.get(glyph.start..glyph.end).unwrap_or("");
            if cluster.chars().all(char::is_whitespace) {
                min = min.max(word);
                word = 0.0;
            } else {
                word += glyph.w;
            }
        }
        min = min.max(word);
    }
    (min.min(max), max)
}

/// Laid-out size of a shaped buffer, in device px.
pub fn measure(buffer: &Buffer) -> (f32, f32) {
    let mut w: f32 = 0.0;
    let mut h: f32 = 0.0;
    for run in buffer.layout_runs() {
        w = w.max(run.line_w);
        h = h.max(run.line_top + run.line_height);
    }
    (w, h)
}
