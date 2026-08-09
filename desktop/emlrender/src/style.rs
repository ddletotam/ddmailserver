//! Computed style: the CSS subset that email actually uses.
//!
//! Three sources feed a computed style, in ascending priority: presentational
//! attributes (`bgcolor`, `align`, `width`, …), `<style>` rules matched by a
//! flat tag/class/id selector, and the inline `style=` attribute. Anything
//! beyond that — combinators, pseudo-classes, `@media` — is ignored rather
//! than half-supported, because a wrong match looks worse than no match.
//!
//! All lengths are stored in **device px** (CSS px × scale): the renderer works
//! in a single coordinate space and only converts back at the API boundary.
//! Percentages are the exception — they need a containing block, so they stay
//! symbolic until layout.

use crate::dom::{attr, tag};
use markup5ever_rcdom::Handle;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Display {
    None,
    Block,
    Inline,
    InlineBlock,
    Table,
    TableRow,
    TableCell,
    ListItem,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Align {
    Left,
    Center,
    Right,
    Justify,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum VAlign {
    Top,
    Middle,
    Bottom,
}

/// A length that may still depend on the containing block.
#[derive(Clone, Copy, Debug)]
pub enum Len {
    /// Already in device px.
    Px(f32),
    /// Percent of the containing block's content width.
    Pct(f32),
}

impl Len {
    pub fn resolve(self, base: f32) -> f32 {
        match self {
            Len::Px(v) => v,
            Len::Pct(p) => base * p / 100.0,
        }
    }
}

#[derive(Clone, Copy, Debug, Default, PartialEq)]
pub struct Rgba {
    pub r: u8,
    pub g: u8,
    pub b: u8,
    pub a: u8,
}

impl Rgba {
    pub const fn rgb(r: u8, g: u8, b: u8) -> Self {
        Self { r, g, b, a: 255 }
    }
    pub const BLACK: Rgba = Rgba::rgb(0, 0, 0);
    pub fn is_visible(&self) -> bool {
        self.a > 0
    }
}

#[derive(Clone, Copy, Debug, Default)]
pub struct Edges {
    pub top: f32,
    pub right: f32,
    pub bottom: f32,
    pub left: f32,
}

impl Edges {
    pub fn all(v: f32) -> Self {
        Self { top: v, right: v, bottom: v, left: v }
    }
    pub fn horizontal(&self) -> f32 {
        self.left + self.right
    }
    pub fn vertical(&self) -> f32 {
        self.top + self.bottom
    }
}

/// A `background-image`. Mail uses exactly two kinds and ignores the rest.
#[derive(Clone, Debug)]
pub enum BgImage {
    /// Angle in CSS degrees (0 = up, clockwise) and stops already resolved to
    /// explicit 0..1 positions.
    Linear { angle: f32, stops: Vec<(Rgba, f32)> },
    /// Centre as a fraction of the box, and the same resolved stops. Ellipses
    /// are drawn as circles — tiny-skia has no elliptical gradient, and a
    /// circle through the same corner is far closer than a flat fill.
    Radial { cx: f32, cy: f32, stops: Vec<(Rgba, f32)> },
    Url(String),
}

/// `background-position`, as a fraction of the free space in each axis —
/// which is exactly what CSS percentages mean here (0 = flush left/top,
/// 1 = flush right/bottom).
#[derive(Clone, Copy, Debug)]
pub struct BgPos {
    pub x: f32,
    pub y: f32,
}

impl Default for BgPos {
    fn default() -> Self {
        Self { x: 0.0, y: 0.0 }
    }
}

/// `background-size`. `Auto` is the CSS default: natural size, tiled.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum BgSize {
    Auto,
    Cover,
    Contain,
    Stretch,
}

/// Which generic family to hand cosmic-text when the declared families are all
/// unavailable — the distinction survives font fallback, the exact face rarely does.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Generic {
    Sans,
    Serif,
    Mono,
}

#[derive(Clone, Debug)]
pub struct Style {
    pub display: Display,
    // --- inherited ---
    pub font_size: f32,
    pub font_weight: u16,
    pub italic: bool,
    pub underline: bool,
    pub strike: bool,
    pub color: Rgba,
    pub family: Option<String>,
    pub generic: Generic,
    pub align: Align,
    pub valign: VAlign,
    pub line_height: Option<f32>,
    /// Extra advance per glyph, device px. Inherited, like the rest of this
    /// half — mail sets it on a wrapper and expects the headline inside to
    /// track wide.
    pub letter_spacing: f32,
    pub pre: bool,
    // --- not inherited ---
    pub background: Option<Rgba>,
    /// Painted over [`Style::background`], under the content.
    pub background_image: Option<BgImage>,
    pub bg_size: BgSize,
    /// `background-repeat` — tile when the image is smaller than the box.
    pub bg_repeat: bool,
    pub bg_pos: BgPos,
    pub padding: Edges,
    /// Padding declared in percent, kept symbolic: `padding: 0 5%` is the
    /// standard way mail insets a full-width cell, and the percentage is of the
    /// containing block — which only layout knows. Added to [`Style::padding`]
    /// by [`Style::pad`].
    pub padding_pct: Edges,
    pub margin: Edges,
    /// Percentage margins, kept symbolic for the same reason as
    /// [`Style::padding_pct`]. Rarer than percentage padding, but `margin: 0 5%`
    /// insets a block the same way, and silently collapsing it to zero is the
    /// same bug.
    pub margin_pct: Edges,
    /// `margin-left: auto` / `margin-right: auto`. The standard way to push a
    /// block right or centre it — and the way mail centres a fixed-width
    /// wrapper table, so this is not a niche.
    pub margin_auto_left: bool,
    pub margin_auto_right: bool,
    pub border_width: Edges,
    pub border_color: Rgba,
    pub radius: f32,
    pub width: Option<Len>,
    pub height: Option<Len>,
    pub max_width: Option<Len>,
    pub hidden: bool,
    /// `<table align>` — where the table *box* sits, which is not the same
    /// thing as where its text sits. HTML maps this attribute to auto margins;
    /// letting it reach `align` instead would centre every cell's text too.
    pub box_align: Option<Align>,
}

impl Style {
    /// Root style: 16 CSS px black sans-serif, the browser default email is written against.
    pub fn root(scale: f32) -> Self {
        Self {
            display: Display::Block,
            font_size: 16.0 * scale,
            font_weight: 400,
            italic: false,
            underline: false,
            strike: false,
            color: Rgba::BLACK,
            family: None,
            generic: Generic::Sans,
            align: Align::Left,
            // Browsers baseline-align table cells, which for the single-font
            // rows mail is made of is indistinguishable from top. Middle would
            // stagger the columns of a multi-cell row against each other.
            valign: VAlign::Top,
            line_height: None,
            letter_spacing: 0.0,
            pre: false,
            background: None,
            background_image: None,
            bg_size: BgSize::Auto,
            bg_repeat: true,
            bg_pos: BgPos::default(),
            padding: Edges::default(),
            padding_pct: Edges::default(),
            margin: Edges::default(),
            margin_pct: Edges::default(),
            margin_auto_left: false,
            margin_auto_right: false,
            border_width: Edges::default(),
            border_color: Rgba::default(),
            radius: 0.0,
            width: None,
            height: None,
            max_width: None,
            hidden: false,
            box_align: None,
        }
    }

    /// Start a child style: keep the inherited half, reset the box half.
    fn inherit(&self) -> Self {
        Self {
            display: Display::Inline,
            box_align: None,
            background: None,
            background_image: None,
            bg_size: BgSize::Auto,
            bg_repeat: true,
            bg_pos: BgPos::default(),
            padding: Edges::default(),
            padding_pct: Edges::default(),
            margin: Edges::default(),
            margin_pct: Edges::default(),
            margin_auto_left: false,
            margin_auto_right: false,
            border_width: Edges::default(),
            border_color: Rgba::default(),
            radius: 0.0,
            width: None,
            height: None,
            max_width: None,
            hidden: false,
            ..self.clone()
        }
    }

    /// Padding in device px inside a containing block `base` px wide. Per CSS,
    /// a percentage resolves against the container's **width** on every side,
    /// vertical ones included.
    pub fn pad(&self, base: f32) -> Edges {
        resolve_pct(self.padding, self.padding_pct, base)
    }

    /// Margins in device px inside a containing block `base` px wide.
    pub fn mar(&self, base: f32) -> Edges {
        resolve_pct(self.margin, self.margin_pct, base)
    }

    /// Line box height, never zero. `line-height:0` is a standard trick for
    /// collapsing preheader text, and cosmic-text asserts on a zero metric —
    /// one such declaration would otherwise blank the whole mail.
    pub fn line_px(&self) -> f32 {
        self.line_height.unwrap_or(self.font_size * 1.35).max(1.0)
    }
}

// ---------------------------------------------------------------- resolution

pub struct Resolver {
    pub scale: f32,
    pub sheet: Stylesheet,
}

impl Resolver {
    /// Compute an element's style from its parent's.
    pub fn resolve(&self, node: &Handle, parent: &Style) -> Style {
        let t = tag(node);
        let mut s = parent.inherit();
        s.display = default_display(t);
        apply_tag_defaults(t, &mut s, self.scale);
        apply_presentational(node, t, &mut s, self.scale);

        for decls in self.sheet.matching(node, t) {
            self.apply_decls(decls, &mut s, parent);
        }
        if let Some(inline) = attr(node, "style") {
            self.apply_decls(&parse_decls(&inline), &mut s, parent);
        }
        s
    }

    fn apply_decls(&self, decls: &[(String, String)], s: &mut Style, parent: &Style) {
        for (prop, val) in decls {
            self.apply_one(prop, val, s, parent);
        }
    }

    fn apply_one(&self, prop: &str, val: &str, s: &mut Style, parent: &Style) {
        let v = val.trim();
        let lower = v.to_ascii_lowercase();
        // Percent font sizes and em lengths resolve against the *parent* size,
        // so every em conversion below uses it rather than the running value.
        let em = parent.font_size;
        match prop {
            "display" => {
                s.display = match lower.as_str() {
                    "none" => Display::None,
                    "block" => Display::Block,
                    "inline" => Display::Inline,
                    // An inline-level box with a block inside. MJML builds
                    // every multi-column layout out of these, so treating one
                    // as a plain inline (and flattening its tables into text)
                    // wrecks a large fraction of modern mail.
                    "inline-block" | "inline-table" | "inline-flex" | "inline-grid" => {
                        Display::InlineBlock
                    }
                    "table" => Display::Table,
                    "table-row" => Display::TableRow,
                    "table-cell" => Display::TableCell,
                    "list-item" => Display::ListItem,
                    _ => s.display,
                }
            }
            "visibility" => s.hidden = lower == "hidden",
            "font-size" => {
                if let Some(px) = self.len_px(&lower, em, Some(em)) {
                    s.font_size = px.clamp(1.0 * self.scale, 200.0 * self.scale);
                }
            }
            "font-weight" => {
                s.font_weight = snap_weight(match lower.as_str() {
                    "bold" | "bolder" => 700,
                    "normal" | "lighter" => 400,
                    other => other.parse().unwrap_or(s.font_weight),
                })
            }
            "font-style" => s.italic = lower == "italic" || lower == "oblique",
            "font-family" => {
                let (name, generic) = parse_family(&lower);
                s.family = name;
                s.generic = generic;
            }
            "font" => {
                // Shorthand: only the size/family tail is worth salvaging.
                if let Some(px) = lower.split_whitespace().find_map(|w| self.len_px(w, em, Some(em)))
                {
                    s.font_size = px.clamp(1.0 * self.scale, 200.0 * self.scale);
                }
                if lower.contains("bold") {
                    s.font_weight = 700;
                }
                if lower.contains("italic") {
                    s.italic = true;
                }
            }
            "text-decoration" | "text-decoration-line" => {
                s.underline = lower.contains("underline");
                s.strike = lower.contains("line-through");
            }
            "color" => {
                if let Some(c) = parse_color(&lower) {
                    s.color = c;
                }
            }
            "background-color" | "background" | "background-image" => {
                // `background` is a shorthand carrying a colour, an image and
                // the painting options at once; take whichever parts are there
                // and leave the rest alone.
                if prop != "background-image" {
                    if let Some(c) = parse_color(&lower) {
                        s.background = Some(c);
                    } else if let Some(c) = lower.split_whitespace().find_map(parse_color) {
                        s.background = Some(c);
                    }
                }
                // The *raw* value: a `url(data:…;base64,…)` payload is
                // case-sensitive, and lowercasing it turns the image to noise.
                if let Some(img) = parse_bg_image(v) {
                    s.background_image = Some(img);
                }
                if prop == "background" {
                    if lower.contains("no-repeat") {
                        s.bg_repeat = false;
                    }
                    if let Some(pos) = parse_bg_pos(&lower) {
                        s.bg_pos = pos;
                    }
                    if lower.contains("cover") {
                        s.bg_size = BgSize::Cover;
                    } else if lower.contains("contain") {
                        s.bg_size = BgSize::Contain;
                    }
                }
            }
            "background-repeat" => s.bg_repeat = !lower.contains("no-repeat"),
            "background-position" => {
                if let Some(pos) = parse_bg_pos(&lower) {
                    s.bg_pos = pos;
                }
            }
            "background-size" => {
                s.bg_size = if lower.contains("cover") {
                    BgSize::Cover
                } else if lower.contains("contain") {
                    BgSize::Contain
                } else if lower.ends_with('%') || lower.split_whitespace().count() == 2 {
                    // `100% 100%` / `100%` — the only explicit sizing mail uses
                    // in practice, and it means "fill the box".
                    BgSize::Stretch
                } else {
                    BgSize::Auto
                }
            }
            "text-align" => {
                s.align = match lower.as_str() {
                    "center" => Align::Center,
                    "right" | "end" => Align::Right,
                    "justify" => Align::Justify,
                    _ => Align::Left,
                }
            }
            "vertical-align" => {
                s.valign = match lower.as_str() {
                    "top" => VAlign::Top,
                    "bottom" => VAlign::Bottom,
                    _ => VAlign::Middle,
                }
            }
            "line-height" => {
                // Unitless line-height is a multiplier, not a length.
                if let Ok(mult) = lower.parse::<f32>() {
                    s.line_height = Some(s.font_size * mult);
                } else if let Some(px) = self.len_px(&lower, em, Some(s.font_size)) {
                    s.line_height = Some(px);
                }
            }
            "letter-spacing" => {
                s.letter_spacing = if lower == "normal" {
                    0.0
                } else {
                    self.len_px(&lower, em, None).unwrap_or(s.letter_spacing)
                }
            }
            "white-space" => s.pre = lower.starts_with("pre"),
            // A value we cannot parse — `auto` above all — leaves the declared
            // size alone rather than clearing it. `<img width="130"
            // style="width:auto">` is standard mail boilerplate, and for a
            // blocked remote image the attribute is the only size we will ever
            // have; honouring `auto` there means drawing nothing at all.
            "width" => s.width = self.len(&lower, em).or(s.width),
            "height" => s.height = self.len(&lower, em).or(s.height),
            "max-width" => s.max_width = self.len(&lower, em).or(s.max_width),
            "border-radius" => {
                if let Some(px) = self.len_px(first_word(&lower), em, None) {
                    s.radius = px;
                }
            }
            "padding" => {
                let (px, pct) = self.edges_split(&lower, em);
                s.padding = px;
                s.padding_pct = pct;
            }
            "padding-top" => (s.padding.top, s.padding_pct.top) = self.len_or_pct(&lower, em),
            "padding-right" => (s.padding.right, s.padding_pct.right) = self.len_or_pct(&lower, em),
            "padding-bottom" => {
                (s.padding.bottom, s.padding_pct.bottom) = self.len_or_pct(&lower, em)
            }
            "padding-left" => (s.padding.left, s.padding_pct.left) = self.len_or_pct(&lower, em),
            "margin" => {
                let (px, pct) = self.edges_split(&lower, em);
                s.margin = px;
                s.margin_pct = pct;
                // `margin: 0 auto` — the shorthand's horizontal slot.
                let words: Vec<&str> = lower.split_whitespace().collect();
                let horizontal_auto = match words.len() {
                    1 => words[0] == "auto",
                    _ => words.get(1).is_some_and(|w| *w == "auto"),
                };
                s.margin_auto_left = horizontal_auto;
                s.margin_auto_right = horizontal_auto;
            }
            "margin-top" => (s.margin.top, s.margin_pct.top) = self.len_or_pct(&lower, em),
            "margin-right" => {
                s.margin_auto_right = lower == "auto";
                (s.margin.right, s.margin_pct.right) = self.len_or_pct(&lower, em);
            }
            "margin-bottom" => (s.margin.bottom, s.margin_pct.bottom) = self.len_or_pct(&lower, em),
            "margin-left" => {
                s.margin_auto_left = lower == "auto";
                (s.margin.left, s.margin_pct.left) = self.len_or_pct(&lower, em);
            }
            "border" | "border-top" | "border-right" | "border-bottom" | "border-left" => {
                let w = lower
                    .split_whitespace()
                    .find_map(|p| self.len_px(p, em, None))
                    .unwrap_or(1.0 * self.scale);
                let w = if lower.contains("none") || lower.contains("hidden") { 0.0 } else { w };
                if let Some(c) = lower.split_whitespace().find_map(parse_color) {
                    s.border_color = c;
                }
                match prop {
                    "border-top" => s.border_width.top = w,
                    "border-right" => s.border_width.right = w,
                    "border-bottom" => s.border_width.bottom = w,
                    "border-left" => s.border_width.left = w,
                    _ => s.border_width = Edges::all(w),
                }
            }
            "border-color" => {
                if let Some(c) = parse_color(&lower) {
                    s.border_color = c;
                }
            }
            "border-width" => s.border_width = self.edges(&lower, em),
            _ => {}
        }
    }

    fn len(&self, v: &str, em: f32) -> Option<Len> {
        let v = v.trim();
        if let Some(p) = v.strip_suffix('%') {
            return p.trim().parse::<f32>().ok().map(Len::Pct);
        }
        self.len_px(v, em, None).map(Len::Px)
    }

    /// Absolute length → device px. `pct_base` enables `%` (font-size only).
    fn len_px(&self, v: &str, em: f32, pct_base: Option<f32>) -> Option<f32> {
        let v = v.trim();
        if v.is_empty() || v == "0" {
            return if v == "0" { Some(0.0) } else { None };
        }
        let num = |s: &str| s.trim().parse::<f32>().ok();
        if let Some(n) = v.strip_suffix("px").and_then(num) {
            Some(n * self.scale)
        } else if let Some(n) = v.strip_suffix("pt").and_then(num) {
            Some(n * 4.0 / 3.0 * self.scale)
        } else if let Some(n) = v.strip_suffix("rem").and_then(num) {
            Some(n * 16.0 * self.scale)
        } else if let Some(n) = v.strip_suffix("em").and_then(num) {
            Some(n * em)
        } else if let Some(n) = v.strip_suffix("ex").or_else(|| v.strip_suffix("ch")).and_then(num) {
            // Gmail writes its quote rule as `padding-left: 1ex`. Without these
            // the padding silently became 0 and quoted text sat on the rule.
            // Half an em is close enough for both on the fonts mail uses.
            Some(n * em * 0.5)
        } else if let Some(n) = v.strip_suffix('%').and_then(num) {
            pct_base.map(|b| b * n / 100.0)
        } else {
            // Bare numbers appear constantly in mail (`width="600"`, `padding:10`).
            num(v).map(|n| n * self.scale)
        }
    }

    fn edges(&self, v: &str, em: f32) -> Edges {
        self.edges_split(v, em).0
    }

    /// A box shorthand split into its absolute and its percentage halves.
    fn edges_split(&self, v: &str, em: f32) -> (Edges, Edges) {
        let parts: Vec<(f32, f32)> =
            v.split_whitespace().map(|p| self.len_or_pct(p, em)).collect();
        (shorthand(&parts, |p| p.0), shorthand(&parts, |p| p.1))
    }

    /// One length as (device px, percent). A percentage can't become px here —
    /// the containing block is not known until layout.
    fn len_or_pct(&self, v: &str, em: f32) -> (f32, f32) {
        match v.trim().strip_suffix('%').and_then(|n| n.trim().parse::<f32>().ok()) {
            Some(pct) => (0.0, pct),
            None => (self.len_px(v, em, None).unwrap_or(0.0), 0.0),
        }
    }
}

/// Fold a percentage half back into an absolute one. Percentages resolve
/// against the container's **width** on every side, per CSS — including the
/// vertical ones, which surprises people but is what mail is written against.
fn resolve_pct(px: Edges, pct: Edges, base: f32) -> Edges {
    if pct.horizontal() + pct.vertical() <= 0.0 {
        return px;
    }
    let add = |a: f32, p: f32| a + (base * p / 100.0).max(0.0);
    Edges {
        top: add(px.top, pct.top),
        right: add(px.right, pct.right),
        bottom: add(px.bottom, pct.bottom),
        left: add(px.left, pct.left),
    }
}

/// The 1/2/3/4-value box shorthand, over whichever half of the parse we want.
fn shorthand(parts: &[(f32, f32)], pick: fn(&(f32, f32)) -> f32) -> Edges {
    let v: Vec<f32> = parts.iter().map(pick).collect();
    match v.len() {
        1 => Edges::all(v[0]),
        2 => Edges { top: v[0], bottom: v[0], left: v[1], right: v[1] },
        3 => Edges { top: v[0], left: v[1], right: v[1], bottom: v[2] },
        4 => Edges { top: v[0], right: v[1], bottom: v[2], left: v[3] },
        _ => Edges::default(),
    }
}

fn first_word(s: &str) -> &str {
    s.split_whitespace().next().unwrap_or("")
}

/// Collapse the weight axis to regular/bold.
///
/// Mail asks for 500 and 600 constantly. Matching those against a system font
/// set that has no such face does not fall back gracefully here: shaping comes
/// back with one em of advance per glyph, so a button label renders as
/// `П е р е й т и`. Regular and bold are the two weights we can count on, and
/// the visual difference from 500 is not worth a broken line.
fn snap_weight(w: u16) -> u16 {
    if w >= 600 {
        700
    } else {
        400
    }
}

fn default_display(t: &str) -> Display {
    match t {
        "" => Display::Inline, // text node
        "div" | "p" | "h1" | "h2" | "h3" | "h4" | "h5" | "h6" | "ul" | "ol" | "dl" | "dd"
        | "dt" | "blockquote" | "pre" | "hr" | "section" | "article" | "header" | "footer"
        | "main" | "nav" | "aside" | "figure" | "figcaption" | "address" | "center" | "body"
        | "html" | "tbody" | "thead" | "tfoot" | "caption" | "fieldset" | "legend" => {
            Display::Block
        }
        "table" => Display::Table,
        "tr" => Display::TableRow,
        "td" | "th" => Display::TableCell,
        "li" => Display::ListItem,
        _ => Display::Inline,
    }
}

/// The bits of the UA stylesheet that visibly matter in mail.
fn apply_tag_defaults(t: &str, s: &mut Style, scale: f32) {
    let em = s.font_size;
    match t {
        "b" | "strong" | "th" => s.font_weight = 700,
        "i" | "em" | "cite" | "address" => s.italic = true,
        "u" | "ins" => s.underline = true,
        "s" | "strike" | "del" => s.strike = true,
        "a" => {
            s.color = Rgba::rgb(0x11, 0x55, 0xcc);
            s.underline = true;
        }
        "small" => s.font_size = em * 0.85,
        "big" => s.font_size = em * 1.15,
        "code" | "kbd" | "samp" | "tt" | "pre" => {
            s.generic = Generic::Mono;
            s.family = None;
            if t == "pre" {
                s.pre = true;
                s.margin = Edges { top: em * 0.6, bottom: em * 0.6, ..Default::default() };
            }
        }
        "p" => s.margin = Edges { top: em * 0.6, bottom: em * 0.6, ..Default::default() },
        "h1" => heading(s, 2.0, 0.67),
        "h2" => heading(s, 1.5, 0.75),
        "h3" => heading(s, 1.25, 0.83),
        "h4" => heading(s, 1.0, 1.12),
        "h5" => heading(s, 0.85, 1.5),
        "h6" => heading(s, 0.7, 1.67),
        "ul" | "ol" => {
            s.margin = Edges { top: em * 0.5, bottom: em * 0.5, ..Default::default() };
            s.padding.left = 24.0 * scale;
        }
        "blockquote" => {
            s.margin = Edges { top: em * 0.5, bottom: em * 0.5, ..Default::default() };
            s.padding.left = 12.0 * scale;
            s.border_width.left = 3.0 * scale;
            s.border_color = Rgba::rgb(0xcc, 0xcc, 0xcc);
        }
        "center" => s.align = Align::Center,
        "hr" => {
            s.margin = Edges { top: em * 0.5, bottom: em * 0.5, ..Default::default() };
            s.border_width.top = 1.0 * scale;
            s.border_color = Rgba::rgb(0xdd, 0xdd, 0xdd);
        }
        "td" | "caption" => {}
        _ => {}
    }
}

fn heading(s: &mut Style, size_mult: f32, margin_em: f32) {
    s.font_size *= size_mult;
    s.font_weight = 700;
    let m = s.font_size * margin_em;
    s.margin = Edges { top: m, bottom: m, ..Default::default() };
}

/// Presentational attributes — still the backbone of table-based mail layout.
fn apply_presentational(node: &Handle, t: &str, s: &mut Style, scale: f32) {
    if let Some(v) = attr(node, "bgcolor").as_deref().and_then(parse_color) {
        s.background = Some(v);
    }
    if let Some(v) = attr(node, "color").as_deref().and_then(parse_color) {
        s.color = v;
    }
    if let Some(v) = attr(node, "align") {
        let align = match v.to_ascii_lowercase().as_str() {
            "center" => Some(Align::Center),
            "right" => Some(Align::Right),
            "left" => Some(Align::Left),
            _ => None,
        };
        if let Some(align) = align {
            // On a table the attribute places the box; everywhere else it is
            // plain text alignment, inherited by the subtree.
            if t == "table" {
                s.box_align = Some(align);
            } else {
                s.align = align;
            }
        }
    }
    if let Some(v) = attr(node, "valign") {
        match v.to_ascii_lowercase().as_str() {
            "top" => s.valign = VAlign::Top,
            "bottom" => s.valign = VAlign::Bottom,
            _ => s.valign = VAlign::Middle,
        }
    }
    if let Some(v) = attr(node, "width") {
        s.width = parse_attr_len(&v, scale);
    }
    if let Some(v) = attr(node, "height") {
        s.height = parse_attr_len(&v, scale);
    }
    if t == "table" {
        // `cellpadding` and `cellspacing` are deliberately *not* handled here:
        // they describe the grid's interior, not the table box, so table layout
        // reads them straight off the element (see `crate::table`).
        if let Some(v) = attr(node, "border").and_then(|v| v.trim().parse::<f32>().ok()) {
            if v > 0.0 {
                s.border_width = Edges::all(v * scale);
                if !s.border_color.is_visible() {
                    s.border_color = Rgba::rgb(0xcc, 0xcc, 0xcc);
                }
            }
        }
    }
    if attr(node, "hidden").is_some() {
        s.display = Display::None;
    }
}

fn parse_attr_len(v: &str, scale: f32) -> Option<Len> {
    let v = v.trim();
    if let Some(p) = v.strip_suffix('%') {
        return p.trim().parse::<f32>().ok().map(Len::Pct);
    }
    v.trim_end_matches("px").trim().parse::<f32>().ok().map(|n| Len::Px(n * scale))
}

fn parse_family(list: &str) -> (Option<String>, Generic) {
    let mut generic = Generic::Sans;
    let mut first_named = None;
    for raw in list.split(',') {
        let name = raw.trim().trim_matches(['"', '\'']).trim();
        match name {
            "serif" | "georgia" | "times" | "times new roman" | "garamond" => {
                generic = Generic::Serif
            }
            "monospace" | "courier" | "courier new" | "consolas" | "menlo" => {
                generic = Generic::Mono
            }
            "sans-serif" | "system-ui" | "-apple-system" | "" => {}
            _ => {
                if first_named.is_none() {
                    first_named = Some(name.to_string());
                }
            }
        }
    }
    (first_named, generic)
}

// -------------------------------------------------------------------- colour

pub fn parse_color(v: &str) -> Option<Rgba> {
    let v = v.trim().trim_end_matches(&[';', ' '][..]);
    if let Some(hex) = v.strip_prefix('#') {
        return parse_hex(hex);
    }
    let lower = v.to_ascii_lowercase();
    if let Some(inner) = lower.strip_prefix("rgb(").and_then(|s| s.strip_suffix(')')) {
        return parse_rgb_fn(inner, false);
    }
    if let Some(inner) = lower.strip_prefix("rgba(").and_then(|s| s.strip_suffix(')')) {
        return parse_rgb_fn(inner, true);
    }
    named_color(&lower)
}

fn parse_hex(hex: &str) -> Option<Rgba> {
    let h = hex.trim();
    let b = |s: &str| u8::from_str_radix(s, 16).ok();
    match h.len() {
        3 => {
            let d: Vec<char> = h.chars().collect();
            Some(Rgba::rgb(
                b(&format!("{}{}", d[0], d[0]))?,
                b(&format!("{}{}", d[1], d[1]))?,
                b(&format!("{}{}", d[2], d[2]))?,
            ))
        }
        6 => Some(Rgba::rgb(b(&h[0..2])?, b(&h[2..4])?, b(&h[4..6])?)),
        8 => Some(Rgba { r: b(&h[0..2])?, g: b(&h[2..4])?, b: b(&h[4..6])?, a: b(&h[6..8])? }),
        _ => None,
    }
}

fn parse_rgb_fn(inner: &str, alpha: bool) -> Option<Rgba> {
    let parts: Vec<&str> = inner.split(&[',', '/', ' '][..]).filter(|p| !p.trim().is_empty()).collect();
    if parts.len() < 3 {
        return None;
    }
    let ch = |s: &str| -> Option<u8> {
        let s = s.trim();
        if let Some(p) = s.strip_suffix('%') {
            p.trim().parse::<f32>().ok().map(|v| (v * 2.55).clamp(0.0, 255.0) as u8)
        } else {
            s.parse::<f32>().ok().map(|v| v.clamp(0.0, 255.0) as u8)
        }
    };
    let a = if alpha && parts.len() >= 4 {
        parts[3].trim().parse::<f32>().ok().map(|v| (v * 255.0).clamp(0.0, 255.0) as u8).unwrap_or(255)
    } else {
        255
    };
    Some(Rgba { r: ch(parts[0])?, g: ch(parts[1])?, b: ch(parts[2])?, a })
}

fn named_color(name: &str) -> Option<Rgba> {
    let c = match name {
        "transparent" => Rgba { r: 0, g: 0, b: 0, a: 0 },
        "black" => Rgba::rgb(0, 0, 0),
        "white" => Rgba::rgb(255, 255, 255),
        "red" => Rgba::rgb(255, 0, 0),
        "green" => Rgba::rgb(0, 128, 0),
        "lime" => Rgba::rgb(0, 255, 0),
        "blue" => Rgba::rgb(0, 0, 255),
        "yellow" => Rgba::rgb(255, 255, 0),
        "orange" => Rgba::rgb(255, 165, 0),
        "purple" => Rgba::rgb(128, 0, 128),
        "gray" | "grey" => Rgba::rgb(128, 128, 128),
        "lightgray" | "lightgrey" => Rgba::rgb(211, 211, 211),
        "darkgray" | "darkgrey" => Rgba::rgb(169, 169, 169),
        "silver" => Rgba::rgb(192, 192, 192),
        "whitesmoke" => Rgba::rgb(245, 245, 245),
        "gainsboro" => Rgba::rgb(220, 220, 220),
        "navy" => Rgba::rgb(0, 0, 128),
        "teal" => Rgba::rgb(0, 128, 128),
        "aqua" | "cyan" => Rgba::rgb(0, 255, 255),
        "maroon" => Rgba::rgb(128, 0, 0),
        "olive" => Rgba::rgb(128, 128, 0),
        "fuchsia" | "magenta" => Rgba::rgb(255, 0, 255),
        "pink" => Rgba::rgb(255, 192, 203),
        "brown" => Rgba::rgb(165, 42, 42),
        "gold" => Rgba::rgb(255, 215, 0),
        "beige" => Rgba::rgb(245, 245, 220),
        "ivory" => Rgba::rgb(255, 255, 240),
        "coral" => Rgba::rgb(255, 127, 80),
        "salmon" => Rgba::rgb(250, 128, 114),
        "crimson" => Rgba::rgb(220, 20, 60),
        "indigo" => Rgba::rgb(75, 0, 130),
        "violet" => Rgba::rgb(238, 130, 238),
        "tomato" => Rgba::rgb(255, 99, 71),
        "steelblue" => Rgba::rgb(70, 130, 180),
        "dodgerblue" => Rgba::rgb(30, 144, 255),
        "royalblue" => Rgba::rgb(65, 105, 225),
        "cornflowerblue" => Rgba::rgb(100, 149, 237),
        "lightblue" => Rgba::rgb(173, 216, 230),
        "skyblue" => Rgba::rgb(135, 206, 235),
        "seagreen" => Rgba::rgb(46, 139, 87),
        "forestgreen" => Rgba::rgb(34, 139, 34),
        "darkgreen" => Rgba::rgb(0, 100, 0),
        "lightgreen" => Rgba::rgb(144, 238, 144),
        "khaki" => Rgba::rgb(240, 230, 140),
        "azure" => Rgba::rgb(240, 255, 255),
        "aliceblue" => Rgba::rgb(240, 248, 255),
        "snow" => Rgba::rgb(255, 250, 250),
        _ => return None,
    };
    Some(c)
}

// -------------------------------------------------------------- declarations

/// Split a declaration block into `(property, value)` pairs.
///
/// Naive splitting breaks on `url(data:image/png;base64,…)`, which is
/// everywhere in mail, so `;` and `:` inside parentheses or quotes are ignored.
pub fn parse_decls(src: &str) -> Vec<(String, String)> {
    let mut out = Vec::new();
    let mut depth = 0usize;
    let mut quote: Option<char> = None;
    let mut start = 0usize;
    let bytes: Vec<char> = src.chars().collect();
    let push = |from: usize, to: usize, out: &mut Vec<(String, String)>| {
        let decl: String = bytes[from..to].iter().collect();
        if let Some((p, v)) = split_decl(&decl) {
            out.push((p, v));
        }
    };
    for i in 0..bytes.len() {
        let c = bytes[i];
        match c {
            '"' | '\'' => {
                quote = match quote {
                    Some(q) if q == c => None,
                    Some(q) => Some(q),
                    None => Some(c),
                }
            }
            '(' if quote.is_none() => depth += 1,
            ')' if quote.is_none() => depth = depth.saturating_sub(1),
            ';' if quote.is_none() && depth == 0 => {
                push(start, i, &mut out);
                start = i + 1;
            }
            _ => {}
        }
    }
    push(start, bytes.len(), &mut out);
    out
}

fn split_decl(decl: &str) -> Option<(String, String)> {
    let mut depth = 0usize;
    for (i, c) in decl.char_indices() {
        match c {
            '(' => depth += 1,
            ')' => depth = depth.saturating_sub(1),
            ':' if depth == 0 => {
                let prop = decl[..i].trim().to_ascii_lowercase();
                let val = decl[i + 1..].trim().trim_end_matches("!important").trim().to_string();
                if prop.is_empty() || val.is_empty() {
                    return None;
                }
                return Some((prop, val));
            }
            _ => {}
        }
    }
    None
}

// ---------------------------------------------------------------- stylesheet

struct SimpleSel {
    tag: Option<String>,
    class: Option<String>,
    id: Option<String>,
}

struct Rule {
    sel: SimpleSel,
    decls: Vec<(String, String)>,
    /// id/class/tag specificity, then document order — the usual cascade.
    spec: u32,
    order: usize,
}

#[derive(Default)]
pub struct Stylesheet {
    rules: Vec<Rule>,
}

impl Stylesheet {
    /// Parse `<style>` text. Unsupported selectors are skipped whole; at-rules
    /// (`@media`, `@font-face`) are skipped along with their block.
    pub fn parse(src: &str) -> Self {
        let src = strip_css_comments(src);
        let mut rules = Vec::new();
        let chars: Vec<char> = src.chars().collect();
        let mut i = 0usize;
        let mut order = 0usize;
        while i < chars.len() {
            // selector text up to '{'
            let sel_start = i;
            while i < chars.len() && chars[i] != '{' {
                i += 1;
            }
            if i >= chars.len() {
                break;
            }
            let sel_text: String = chars[sel_start..i].iter().collect();
            i += 1; // past '{'
            let body_start = i;
            let mut depth = 1usize;
            while i < chars.len() && depth > 0 {
                match chars[i] {
                    '{' => depth += 1,
                    '}' => depth -= 1,
                    _ => {}
                }
                i += 1;
            }
            let body: String = chars[body_start..i.saturating_sub(1)].iter().collect();
            let sel_text = sel_text.trim().to_string();
            if sel_text.starts_with('@') {
                continue; // whole at-rule block skipped
            }
            let decls = parse_decls(&body);
            if decls.is_empty() {
                continue;
            }
            for one in sel_text.split(',') {
                if let Some((sel, spec)) = parse_simple_selector(one.trim()) {
                    rules.push(Rule { sel, decls: decls.clone(), spec, order });
                    order += 1;
                }
            }
        }
        rules.sort_by_key(|r| (r.spec, r.order));
        Self { rules }
    }

    fn matching(&self, node: &Handle, t: &str) -> Vec<&Vec<(String, String)>> {
        if self.rules.is_empty() {
            return Vec::new();
        }
        let classes = attr(node, "class").unwrap_or_default().to_ascii_lowercase();
        let id = attr(node, "id").unwrap_or_default().to_ascii_lowercase();
        self.rules
            .iter()
            .filter(|r| {
                r.sel.tag.as_deref().is_none_or(|s| s == t)
                    && r.sel.class.as_deref().is_none_or(|c| {
                        classes.split_whitespace().any(|have| have == c)
                    })
                    && r.sel.id.as_deref().is_none_or(|i| i == id)
            })
            .map(|r| &r.decls)
            .collect()
    }
}

/// Accept `tag`, `.class`, `#id`, `tag.class`, `*`. Reject everything with a
/// combinator, attribute test or pseudo — a wrong match is worse than none.
fn parse_simple_selector(sel: &str) -> Option<(SimpleSel, u32)> {
    let sel = sel.trim();
    if sel.is_empty()
        || sel.contains([' ', '>', '+', '~', '[', ':', '(', '*'])
    {
        return if sel == "*" { Some((SimpleSel { tag: None, class: None, id: None }, 0)) } else { None };
    }
    let lower = sel.to_ascii_lowercase();
    let mut out = SimpleSel { tag: None, class: None, id: None };
    let mut spec = 0u32;
    let mut rest = lower.as_str();
    // leading tag, if any
    let split = rest.find(['.', '#']).unwrap_or(rest.len());
    if split > 0 {
        out.tag = Some(rest[..split].to_string());
        spec += 1;
    }
    rest = &rest[split..];
    while !rest.is_empty() {
        let kind = rest.as_bytes()[0] as char;
        rest = &rest[1..];
        let end = rest.find(['.', '#']).unwrap_or(rest.len());
        let name = &rest[..end];
        if name.is_empty() {
            return None;
        }
        match kind {
            '.' => {
                if out.class.is_some() {
                    return None; // multi-class: rare, and we'd match too loosely
                }
                out.class = Some(name.to_string());
                spec += 10;
            }
            '#' => {
                out.id = Some(name.to_string());
                spec += 100;
            }
            _ => return None,
        }
        rest = &rest[end..];
    }
    Some((out, spec))
}

fn strip_css_comments(src: &str) -> String {
    let mut out = String::with_capacity(src.len());
    let mut rest = src;
    while let Some(start) = rest.find("/*") {
        out.push_str(&rest[..start]);
        match rest[start + 2..].find("*/") {
            Some(end) => rest = &rest[start + 2 + end + 2..],
            None => return out,
        }
    }
    out.push_str(rest);
    out
}

// ------------------------------------------------------------ background art

/// `linear-gradient(...)` or `url(...)` out of a `background` value.
///
/// Radial and conic gradients are not parsed: they are rare in mail and a
/// wrong guess paints a worse lie than a flat colour does.
pub fn parse_bg_image(v: &str) -> Option<BgImage> {
    if let Some(inner) = fn_arg(v, "linear-gradient") {
        return parse_linear(inner);
    }
    if let Some(inner) = fn_arg(v, "radial-gradient") {
        return parse_radial(inner);
    }
    if let Some(inner) = fn_arg(v, "url") {
        let url = inner.trim().trim_matches(['"', '\'']).trim();
        return if url.is_empty() { None } else { Some(BgImage::Url(url.to_string())) };
    }
    None
}

/// The argument text of the first `name(...)` in `v`, parens balanced.
fn fn_arg<'a>(v: &'a str, name: &str) -> Option<&'a str> {
    let lower = v.to_ascii_lowercase();
    let at = lower.find(name)?;
    // `-webkit-linear-gradient` must not be mistaken for the plain one only to
    // then be parsed identically — it is the same syntax, so accept it, but the
    // character before still has to be a boundary for `url` inside `blurl(`.
    let rest = &v[at + name.len()..];
    let inner = rest.strip_prefix('(')?;
    let mut depth = 1usize;
    for (i, c) in inner.char_indices() {
        match c {
            '(' => depth += 1,
            ')' => {
                depth -= 1;
                if depth == 0 {
                    return Some(&inner[..i]);
                }
            }
            _ => {}
        }
    }
    None
}

fn parse_linear(inner: &str) -> Option<BgImage> {
    let parts = split_commas(inner);
    if parts.is_empty() {
        return None;
    }
    // CSS default direction is `to bottom`, which is 180°.
    let mut angle = 180.0f32;
    let mut first_stop = 0usize;
    let head = parts[0].trim().to_ascii_lowercase();
    if let Some(a) = parse_angle(&head) {
        angle = a;
        first_stop = 1;
    } else if let Some(sides) = head.strip_prefix("to ") {
        angle = side_angle(sides);
        first_stop = 1;
    }

    let stops = resolve_stops(&parts[first_stop..])?;
    Some(BgImage::Linear { angle, stops })
}

fn parse_angle(v: &str) -> Option<f32> {
    let v = v.trim();
    if let Some(n) = v.strip_suffix("deg") {
        n.trim().parse().ok()
    } else if let Some(n) = v.strip_suffix("turn") {
        n.trim().parse::<f32>().ok().map(|t| t * 360.0)
    } else if let Some(n) = v.strip_suffix("rad") {
        n.trim().parse::<f32>().ok().map(|r| r.to_degrees())
    } else {
        None
    }
}

/// `to bottom` → 180°, `to right` → 90°, and the corners in between.
fn side_angle(sides: &str) -> f32 {
    let (mut y, mut x) = (0.0f32, 0.0f32);
    let mut vertical = false;
    let mut horizontal = false;
    for word in sides.split_whitespace() {
        match word {
            "top" => (y, vertical) = (0.0, true),
            "bottom" => (y, vertical) = (180.0, true),
            "left" => (x, horizontal) = (270.0, true),
            "right" => (x, horizontal) = (90.0, true),
            _ => {}
        }
    }
    match (vertical, horizontal) {
        (true, true) => {
            // Corner: halfway between the two sides, the short way round.
            let (a, b) = (y, x);
            let diff = ((b - a + 540.0) % 360.0) - 180.0;
            (a + diff / 2.0 + 360.0) % 360.0
        }
        (true, false) => y,
        (false, true) => x,
        (false, false) => 180.0,
    }
}

/// Split on top-level commas — `rgba(0,0,0,.5)` must survive intact.
fn split_commas(v: &str) -> Vec<&str> {
    let mut out = Vec::new();
    let mut depth = 0usize;
    let mut start = 0usize;
    for (i, c) in v.char_indices() {
        match c {
            '(' => depth += 1,
            ')' => depth = depth.saturating_sub(1),
            ',' if depth == 0 => {
                out.push(&v[start..i]);
                start = i + 1;
            }
            _ => {}
        }
    }
    out.push(&v[start..]);
    out.into_iter().filter(|p| !p.trim().is_empty()).collect()
}

/// `radial-gradient([shape] [size] [at <position>,] <stops>)`.
///
/// The shape and size keywords are read and then mostly ignored: what changes
/// the picture is the centre and the stops, and guessing a farthest-corner
/// radius from the box is the browser default anyway.
fn parse_radial(inner: &str) -> Option<BgImage> {
    let parts = split_commas(inner);
    if parts.is_empty() {
        return None;
    }
    let head = parts[0].trim().to_ascii_lowercase();
    let is_config = head.contains("circle")
        || head.contains("ellipse")
        || head.contains(" at ")
        || head.starts_with("at ")
        || head.contains("closest")
        || head.contains("farthest");
    let first_stop = usize::from(is_config);
    let centre = head
        .split(" at ")
        .nth(1)
        .or_else(|| head.strip_prefix("at "))
        .and_then(parse_bg_pos)
        .unwrap_or(BgPos { x: 0.5, y: 0.5 });

    let stops = resolve_stops(&parts[first_stop..])?;
    Some(BgImage::Radial { cx: centre.x, cy: centre.y, stops })
}

/// `left top`, `center`, `50% 20%`, `right` — as fractions of the free space.
///
/// Lengths are deliberately not handled: as a fraction of free space they need
/// the box, and mail that positions a backdrop in pixels is vanishingly rare
/// next to mail that says `center`.
fn parse_bg_pos(v: &str) -> Option<BgPos> {
    let mut pos = BgPos { x: 0.5, y: 0.5 };
    let mut seen = false;
    let mut axis_free: Vec<f32> = Vec::new();
    for word in v.split_whitespace() {
        match word.trim_end_matches(',') {
            "left" => (pos.x, seen) = (0.0, true),
            "right" => (pos.x, seen) = (1.0, true),
            "top" => (pos.y, seen) = (0.0, true),
            "bottom" => (pos.y, seen) = (1.0, true),
            "center" => seen = true,
            other => {
                if let Some(p) = other.strip_suffix('%').and_then(|n| n.parse::<f32>().ok()) {
                    axis_free.push((p / 100.0).clamp(0.0, 1.0));
                    seen = true;
                }
            }
        }
    }
    // Bare percentages are x then y, per CSS.
    if let Some(x) = axis_free.first() {
        pos.x = *x;
    }
    if let Some(y) = axis_free.get(1) {
        pos.y = *y;
    }
    seen.then_some(pos)
}

/// Colour stops with the unpositioned ones spread evenly between them.
fn resolve_stops(parts: &[&str]) -> Option<Vec<(Rgba, f32)>> {
    let raw: Vec<(Rgba, Option<f32>)> = parts
        .iter()
        .filter_map(|p| {
            let p = p.trim();
            let color = parse_color(p).or_else(|| p.split_whitespace().find_map(parse_color))?;
            let pos = p
                .split_whitespace()
                .find_map(|w| w.strip_suffix('%'))
                .and_then(|n| n.trim().parse::<f32>().ok())
                .map(|n| n / 100.0);
            Some((color, pos))
        })
        .collect();
    if raw.len() < 2 {
        return None;
    }
    let last = raw.len() - 1;
    Some(
        raw.iter()
            .enumerate()
            .map(|(i, (c, p))| (*c, p.unwrap_or(i as f32 / last as f32).clamp(0.0, 1.0)))
            .collect(),
    )
}
