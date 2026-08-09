//! Layout: DOM + computed styles → a flat display list in absolute device px.
//!
//! Block-in-block with an inline formatting context per block. The one rule
//! that overrides everything else: a box is **never** laid out wider than the
//! width handed down to it. Declared widths are clamped, not honoured — an
//! email that says `width="600"` inside a 420 px bubble gets 420.

use std::collections::HashMap;
use std::hash::{Hash, Hasher};
use std::ops::Range;
use std::rc::Rc;

use cosmic_text::Buffer;
use markup5ever_rcdom::Handle;

use crate::dom::{attr, children, is_dropped, tag, text as node_text};
use crate::image::{self, Bitmap};
use crate::style::{Align, BgImage, BgPos, BgSize, Display, Edges, Len, Resolver, Rgba, Style};
use crate::text::{self, Span, TextEngine};

pub enum Cmd {
    Rect { x: f32, y: f32, w: f32, h: f32, radius: f32, color: Rgba },
    Border { x: f32, y: f32, w: f32, h: f32, radius: f32, widths: Edges, color: Rgba },
    Text { x: f32, y: f32, buffer: Buffer, spans: Vec<Span> },
    Image { x: f32, y: f32, w: f32, h: f32, radius: f32, bitmap: Option<Rc<Bitmap>>, link: Option<usize> },
    /// `background-image: <gradient>` over a box.
    Gradient { x: f32, y: f32, w: f32, h: f32, radius: f32, kind: Ramp, stops: Rc<Vec<(Rgba, f32)>> },
    /// `background-image: url(...)` over a box, sized, placed and repeated per CSS.
    Backdrop {
        x: f32,
        y: f32,
        w: f32,
        h: f32,
        bitmap: Rc<Bitmap>,
        size: BgSize,
        repeat: bool,
        pos: BgPos,
    },
}

pub struct Layout {
    pub cmds: Vec<Cmd>,
    pub hrefs: Vec<String>,
    pub height: f32,
}

/// Hard ceilings. A mail that trips one of these is broken or hostile; we want
/// a truncated render, not a hung worker.
const MAX_HEIGHT_CSS: f32 = 8000.0;
const MAX_NODES: usize = 60_000;
pub(crate) const MAX_DEPTH: usize = 96;
/// Nodes the table column measurer may visit across the whole document. Sizing
/// a column means walking the cell subtree ahead of laying it out, and deeply
/// nested tables would otherwise multiply that walk by their nesting depth.
const MAX_MEASURED: usize = 200_000;

pub struct Ctx<'a> {
    pub(crate) res: &'a Resolver,
    pub(crate) eng: &'a mut TextEngine,
    pub(crate) host: Host<'a>,
    pub(crate) scale: f32,
    pub(crate) max_h: f32,
    pub(crate) nodes: usize,
    pub(crate) cmds: Vec<Cmd>,
    pub(crate) hrefs: Vec<String>,
    /// Intrinsic (min, max) content width per node, keyed by node address. A
    /// node's computed style depends only on its ancestors, so its intrinsic
    /// widths are the same every time we ask — and nested tables ask often.
    pub(crate) intrinsic_cache: HashMap<usize, (f32, f32)>,
    pub(crate) measure_left: usize,
    /// Decoded images, keyed by a hash of `src`. Mail repeats the same logo in
    /// header and footer, and a `data:` URI can be a megabyte of base64 — both
    /// the decode and the pixels are worth sharing.
    images: HashMap<u64, Option<Rc<Bitmap>>>,
    /// Images reserved on the line of the paragraph currently being gathered.
    /// Placed once shaping says where their reservation actually landed.
    objects: Vec<InlineObject>,
    /// Marker for the list item about to be laid out. The marker has to open
    /// the item's own first line, so it is emitted inside the item — but only
    /// the parent list knows whether it is a bullet or a number, and which.
    marker: Option<String>,
}

/// How a gradient's ramp runs across its box.
#[derive(Clone, Copy, Debug)]
pub enum Ramp {
    /// CSS degrees: 0 is up, growing clockwise.
    Linear(f32),
    /// Centre as a fraction of the box.
    Radial(f32, f32),
}

/// Something on the line that is not a glyph, waiting for shaping to say where
/// its reservation landed.
struct InlineObject {
    /// Left margin to leave before the box inside its reservation.
    lead: f32,
    /// Commands already emitted at the origin; placing the object is a
    /// translation of that run. Everything on a line goes through this — an
    /// image, a blocked image with its `alt` label, a whole inline-level box —
    /// so nothing gets left behind when the object moves.
    cmds: Range<usize>,
    w: f32,
    h: f32,
}

/// A decoded, sized image, ready to be placed.
struct ImageBox {
    bitmap: Option<Rc<Bitmap>>,
    w: f32,
    h: f32,
    alt: String,
}

/// What an `<img>` turns out to be worth.
enum Placement {
    /// A spacer, a tracker, or a decorative image we cannot show: nothing.
    Nothing,
    /// No size and no pixels, but it said what it was — render that.
    Alt(String),
    Image(ImageBox),
}

/// Where an inline walk puts something that is not text.
///
/// An image cannot sit inside a shaped line, so it has to interrupt the
/// paragraph. Layout knows where that paragraph is on the page and can break
/// it, place the image and resume; measurement only needs the width the thing
/// would take. Without this the whole inline element had to be taken down the
/// block path, which cut `<a>text <img> text</a>` into three unrelated
/// paragraphs — and mail puts icons inside links constantly.
pub(crate) enum Flow<'a> {
    Layout { x: f32, cursor: f32, avail_w: f32, style: &'a Style },
    Measure { min: f32, max: f32 },
}

/// What the caller of the crate brings to a render: the resource resolver and
/// the policy for using it.
pub struct Host<'a> {
    pub block_remote: bool,
    pub resources: &'a dyn crate::Resources,
}

pub fn run(
    root: &Handle,
    res: &Resolver,
    eng: &mut TextEngine,
    width: f32,
    host: Host<'_>,
) -> Layout {
    let scale = res.scale;
    let mut ctx = Ctx {
        res,
        eng,
        host,
        scale,
        max_h: MAX_HEIGHT_CSS * scale,
        nodes: 0,
        cmds: Vec::new(),
        hrefs: Vec::new(),
        intrinsic_cache: HashMap::new(),
        measure_left: MAX_MEASURED,
        images: HashMap::new(),
        objects: Vec::new(),
        marker: None,
    };
    let root_style = Style::root(scale);
    let height = ctx.block_children(root, &root_style, 0.0, 0.0, width, None, 0);
    Layout { cmds: ctx.cmds, hrefs: ctx.hrefs, height: height.min(ctx.max_h) }
}

impl Ctx<'_> {
    /// Lay out one element as a block box at `(x, y)` with `avail_w` available.
    /// Returns the height it consumed, margins included.
    pub(crate) fn block(
        &mut self,
        node: &Handle,
        style: &Style,
        x: f32,
        y: f32,
        avail_w: f32,
        link: Option<usize>,
        depth: usize,
    ) -> f32 {
        let m = style.mar(avail_w);
        let border = style.border_width;
        let pad = style.pad(avail_w);

        // Own width: declared, but never wider than what the parent offers.
        let outer_w = match style.width {
            Some(len) => len.resolve(avail_w).clamp(0.0, avail_w),
            None => avail_w - m.horizontal(),
        }
        .max(1.0);
        let outer_w = match style.max_width {
            Some(len) => outer_w.min(len.resolve(avail_w)),
            None => outer_w,
        }
        .clamp(1.0, avail_w);

        let content_w = (outer_w - border.horizontal() - pad.horizontal()).max(1.0);
        // Auto margins eat the leftover width: both auto centres the box, left
        // alone pushes it right. This is how a block is aligned in CSS — there
        // is no other mechanism short of flex.
        let free = (avail_w - outer_w - m.horizontal()).max(0.0);
        let slide = match (style.margin_auto_left, style.margin_auto_right) {
            (true, true) => free / 2.0,
            (true, false) => free,
            _ => 0.0,
        };
        let box_x = x + m.left + slide;
        let box_y = y + m.top;
        let content_x = box_x + border.left + pad.left;
        let content_y = box_y + border.top + pad.top;

        // Background and border are emitted before the children so they paint
        // underneath, but their height is only known afterwards — reserve the
        // slots now and patch them once the content is measured.
        let bg_slots = self.reserve_bg(style);
        let border_slot = self.reserve(
            border.vertical() + border.horizontal() > 0.0 && style.border_color.is_visible(),
        );

        let content_h = self.block_children(node, style, content_x, content_y, content_w, link, depth);
        let content_h = match style.height {
            // A declared height is a floor, never a clip: mail routinely
            // under-declares and we would cut text off.
            Some(len) => content_h.max(len.resolve(0.0)),
            None => content_h,
        };

        let box_h = content_h + border.vertical() + pad.vertical();
        self.patch_bg(bg_slots, style, box_x, box_y, outer_w, box_h);
        if let Some(i) = border_slot {
            self.cmds[i] = Cmd::Border {
                x: box_x,
                y: box_y,
                w: outer_w,
                h: box_h,
                radius: style.radius,
                widths: border,
                color: style.border_color,
            };
        }
        box_h + m.vertical()
    }

    /// Reserve the two slots a box's background needs: the colour, and the
    /// image or gradient painted over it. Both are emitted before the box's
    /// children — their height is only known afterwards.
    pub(crate) fn reserve_bg(&mut self, style: &Style) -> (Option<usize>, Option<usize>) {
        let color = self.reserve(style.background.filter(Rgba::is_visible).is_some());
        let art = self.reserve(style.background_image.is_some());
        (color, art)
    }

    /// Fill in what [`Ctx::reserve_bg`] put aside, now that the box is measured.
    pub(crate) fn patch_bg(
        &mut self,
        slots: (Option<usize>, Option<usize>),
        style: &Style,
        x: f32,
        y: f32,
        w: f32,
        h: f32,
    ) {
        if let Some(i) = slots.0 {
            self.cmds[i] = Cmd::Rect {
                x,
                y,
                w,
                h,
                radius: style.radius,
                color: style.background.unwrap_or_default(),
            };
        }
        let Some(i) = slots.1 else { return };
        match style.background_image.as_ref() {
            Some(BgImage::Linear { angle, stops }) => {
                self.cmds[i] = Cmd::Gradient {
                    x,
                    y,
                    w,
                    h,
                    radius: style.radius,
                    kind: Ramp::Linear(*angle),
                    stops: Rc::new(stops.clone()),
                };
            }
            Some(BgImage::Radial { cx, cy, stops }) => {
                self.cmds[i] = Cmd::Gradient {
                    x,
                    y,
                    w,
                    h,
                    radius: style.radius,
                    kind: Ramp::Radial(*cx, *cy),
                    stops: Rc::new(stops.clone()),
                };
            }
            Some(BgImage::Url(url)) => {
                // No pixels (remote and blocked, or undecodable) means no
                // backdrop — the colour underneath is the honest fallback.
                if let Some(bitmap) = self.decode_image(&url.clone()) {
                    self.cmds[i] = Cmd::Backdrop {
                        x,
                        y,
                        w,
                        h,
                        bitmap,
                        size: style.bg_size,
                        repeat: style.bg_repeat,
                        pos: style.bg_pos,
                    };
                }
            }
            None => {}
        }
    }

    /// Push a placeholder command whose geometry is patched in later.
    pub(crate) fn reserve(&mut self, needed: bool) -> Option<usize> {
        if !needed {
            return None;
        }
        self.cmds.push(Cmd::Rect {
            x: 0.0,
            y: 0.0,
            w: 0.0,
            h: 0.0,
            radius: 0.0,
            color: Rgba::default(),
        });
        Some(self.cmds.len() - 1)
    }

    /// Move every command in `range` down by `dy`.
    ///
    /// Cells are laid out before their row's height is known, so vertical
    /// alignment can only be applied afterwards — by which point the cell's
    /// content is already a run of absolutely-positioned commands.
    pub(crate) fn shift(&mut self, range: Range<usize>, dy: f32) {
        self.translate(range, 0.0, dy);
    }

    /// Move a run of already-emitted commands. Inline-level boxes are laid out
    /// at the origin and only then placed, because where they go depends on
    /// where shaping put their reservation.
    pub(crate) fn translate(&mut self, range: Range<usize>, dx: f32, dy: f32) {
        for cmd in &mut self.cmds[range] {
            match cmd {
                Cmd::Rect { x, y, .. }
                | Cmd::Border { x, y, .. }
                | Cmd::Text { x, y, .. }
                | Cmd::Image { x, y, .. }
                | Cmd::Gradient { x, y, .. }
                | Cmd::Backdrop { x, y, .. } => {
                    *x += dx;
                    *y += dy;
                }
            }
        }
    }

    /// Lay out a block's children: a vertical stack of block boxes, with runs
    /// of inline content gathered into one shaped paragraph each.
    pub(crate) fn block_children(
        &mut self,
        node: &Handle,
        style: &Style,
        x: f32,
        y: f32,
        avail_w: f32,
        link: Option<usize>,
        depth: usize,
    ) -> f32 {
        if depth > MAX_DEPTH {
            return 0.0;
        }
        let mut cursor = y;
        let mut inline: Vec<Span> = Vec::new();
        // The marker belongs at the head of the item's own first line; pushing
        // it from the parent would flush it *after* the item's content.
        if style.display == Display::ListItem {
            let marker = self.marker.take().unwrap_or_else(|| "• ".to_string());
            self.push_raw(&mut inline, &marker, style, link);
        }
        // Numbering is the list's business, not the item's. `<ol start>` is
        // how a mail continues a list broken across a quote.
        let ordered = tag(node) == "ol";
        let mut ordinal: i64 = attr(node, "start")
            .and_then(|v| v.trim().parse().ok())
            .unwrap_or(1);

        for child in children(node) {
            if self.over_budget(cursor) {
                break;
            }
            self.nodes += 1;

            // Text node: pure inline content.
            if let Some(t) = node_text(&child) {
                self.push_text(&mut inline, &t, style, link);
                continue;
            }
            let t = tag(&child);
            if t.is_empty() || is_dropped(t) {
                continue;
            }
            let cs = self.res.resolve(&child, style);
            if cs.display == Display::None || cs.hidden {
                continue;
            }
            let clink = match t {
                "a" => match attr(&child, "href").filter(|h| is_useful_href(h)) {
                    Some(href) => {
                        self.hrefs.push(href);
                        Some(self.hrefs.len() - 1)
                    }
                    None => link,
                },
                _ => link,
            };

            match t {
                "br" => {
                    self.push_raw(&mut inline, "\n", &cs, clink);
                    continue;
                }
                "img" => {
                    // `<img>` is inline-level by default, so it belongs on the
                    // line — but mail sets `display:block` on images constantly
                    // (to kill the descender gap), and that has to be honoured
                    // or every deliberate block image rejoins the text.
                    let on_line = is_inline(cs.display)
                        && self.inline_image(&child, &cs, &mut inline, avail_w, clink);
                    if !on_line {
                        cursor += self.flush_inline(&mut inline, style, x, cursor, avail_w);
                        cursor += self.image(&child, &cs, x, cursor, avail_w, clink);
                    }
                    continue;
                }
                "hr" => {
                    cursor += self.flush_inline(&mut inline, style, x, cursor, avail_w);
                    let h = cs.border_width.top.max(1.0);
                    self.cmds.push(Cmd::Rect {
                        x,
                        y: cursor + cs.margin.top,
                        w: avail_w,
                        h,
                        radius: 0.0,
                        color: cs.border_color,
                    });
                    cursor += h + cs.margin.vertical();
                    continue;
                }
                _ => {}
            }

            if cs.display == Display::InlineBlock {
                // An inline-level box: put it on the line if it fits there,
                // otherwise fall through and treat it as an ordinary block.
                if self.inline_box(&child, &cs, &mut inline, avail_w, clink, depth + 1) {
                    continue;
                }
                cursor += self.flush_inline(&mut inline, style, x, cursor, avail_w);
                cursor += self.block(&child, &cs, x, cursor, avail_w, clink, depth + 1);
            } else if is_inline(cs.display) {
                let mut flow = Flow::Layout { x, cursor, avail_w, style };
                self.inline_subtree(&child, &cs, &mut inline, clink, depth + 1, &mut flow);
                if let Flow::Layout { cursor: moved, .. } = flow {
                    // An image inside the run flushed the paragraph and took
                    // vertical space of its own; the block cursor follows it.
                    cursor = moved;
                }
            } else if cs.display == Display::Table {
                cursor += self.flush_inline(&mut inline, style, x, cursor, avail_w);
                cursor += self.table(&child, &cs, x, cursor, avail_w, clink, depth + 1);
            } else {
                cursor += self.flush_inline(&mut inline, style, x, cursor, avail_w);
                if cs.display == Display::ListItem {
                    self.marker = Some(if ordered {
                        format!("{ordinal}. ")
                    } else {
                        "• ".to_string()
                    });
                    ordinal += 1;
                }
                cursor += self.block(&child, &cs, x, cursor, avail_w, clink, depth + 1);
            }
        }
        cursor += self.flush_inline(&mut inline, style, x, cursor, avail_w);
        (cursor - y).max(0.0)
    }

    /// Walk an inline element, appending its text to the current paragraph.
    pub(crate) fn inline_subtree(
        &mut self,
        node: &Handle,
        style: &Style,
        out: &mut Vec<Span>,
        link: Option<usize>,
        depth: usize,
        flow: &mut Flow<'_>,
    ) {
        if depth > MAX_DEPTH || self.nodes > MAX_NODES {
            return;
        }
        for child in children(node) {
            self.nodes += 1;
            if let Some(t) = node_text(&child) {
                self.push_text(out, &t, style, link);
                continue;
            }
            let t = tag(&child);
            if t.is_empty() || is_dropped(t) {
                continue;
            }
            let cs = self.res.resolve(&child, style);
            if cs.display == Display::None || cs.hidden {
                continue;
            }
            if t == "br" {
                self.push_raw(out, "\n", &cs, link);
                continue;
            }
            let clink = match t {
                "a" => match attr(&child, "href").filter(|h| is_useful_href(h)) {
                    Some(href) => {
                        self.hrefs.push(href);
                        Some(self.hrefs.len() - 1)
                    }
                    None => link,
                },
                _ => link,
            };
            if t == "img" {
                match flow {
                    Flow::Layout { x, cursor, avail_w, style } => {
                        let (bx, bw, block_style) = (*x, *avail_w, *style);
                        // An icon belongs on the line it was written on. Only
                        // when it cannot share one — or when the author asked
                        // for a block — does the paragraph break.
                        let on_line = is_inline(cs.display)
                            && self.inline_image(&child, &cs, out, bw, clink);
                        if !on_line {
                            *cursor += self.flush_inline(out, block_style, bx, *cursor, bw);
                            *cursor += self.image(&child, &cs, bx, *cursor, bw, clink);
                        }
                    }
                    Flow::Measure { min, max } => {
                        // Only an absolute width says anything about intrinsic
                        // size; a percentage is a demand on the container.
                        if let Some(Len::Px(w)) = cs.width {
                            let w = w + cs.margin.horizontal();
                            *min = min.max(w);
                            *max = max.max(w);
                        }
                    }
                }
                continue;
            }
            if cs.display == Display::InlineBlock {
                match flow {
                    Flow::Layout { x, cursor, avail_w, style } => {
                        let (bx, bw, block_style) = (*x, *avail_w, *style);
                        if !self.inline_box(&child, &cs, out, bw, clink, depth + 1) {
                            *cursor += self.flush_inline(out, block_style, bx, *cursor, bw);
                            *cursor += self.block(&child, &cs, bx, *cursor, bw, clink, depth + 1);
                        }
                    }
                    Flow::Measure { min, max } => {
                        let (bmin, bmax) = self.intrinsic(&child, &cs, depth + 1);
                        let frame = cs.padding.horizontal()
                            + cs.border_width.horizontal()
                            + cs.margin.horizontal();
                        *min = min.max(bmin + frame);
                        *max = max.max(bmax + frame);
                    }
                }
                continue;
            }
            // A block inside an inline run is malformed but common — mail wraps
            // whole `<p>` stacks in a styling `<span>`. Keep the content inline
            // (dropping it would lose the message) but force a line break, or
            // consecutive paragraphs run together into one word.
            let breaks = !is_inline(cs.display);
            if breaks {
                self.break_line(out, &cs, link);
            }
            self.inline_subtree(&child, &cs, out, clink, depth + 1, flow);
            if breaks {
                self.break_line(out, &cs, link);
            }
        }
    }

    /// Append text with HTML whitespace collapsing applied.
    pub(crate) fn push_text(
        &mut self,
        out: &mut Vec<Span>,
        raw: &str,
        style: &Style,
        link: Option<usize>,
    ) {
        if style.pre {
            if !raw.is_empty() {
                out.push(Span::from_style(raw.to_string(), style, link));
            }
            return;
        }
        let mut s = String::with_capacity(raw.len());
        let mut space = false;
        for c in raw.chars() {
            if c.is_whitespace() {
                space = true;
            } else {
                if space && !s.is_empty() {
                    s.push(' ');
                }
                // A leading space still matters when text follows an earlier
                // span (`<b>bold</b> tail`), so keep it unless we're at the
                // very start of the paragraph.
                if space && s.is_empty() && ends_with_word(out) {
                    s.push(' ');
                }
                space = false;
                s.push(c);
            }
        }
        if space && !s.is_empty() {
            s.push(' ');
        }
        if s.is_empty() {
            // Whitespace-only node between two words is still a word gap.
            if ends_with_word(out) {
                out.push(Span::from_style(" ".to_string(), style, link));
            }
            return;
        }
        out.push(Span::from_style(s, style, link));
    }

    pub(crate) fn push_raw(
        &mut self,
        out: &mut Vec<Span>,
        s: &str,
        style: &Style,
        link: Option<usize>,
    ) {
        out.push(Span::from_style(s.to_string(), style, link));
    }

    /// End the current line, unless the paragraph is empty or already broken.
    fn break_line(&mut self, out: &mut Vec<Span>, style: &Style, link: Option<usize>) {
        let already = out
            .last()
            .is_none_or(|s| s.text.is_empty() || s.text.ends_with('\n'));
        if !already {
            self.push_raw(out, "\n", style, link);
        }
    }

    /// Shape and emit the pending inline content. Returns the height it took.
    pub(crate) fn flush_inline(
        &mut self,
        spans: &mut Vec<Span>,
        style: &Style,
        x: f32,
        y: f32,
        avail_w: f32,
    ) -> f32 {
        if spans.is_empty() {
            return 0.0;
        }
        let taken = std::mem::take(spans);
        // A paragraph of nothing but a reserved image is still a paragraph:
        // no-break space is whitespace, so `trim` alone would drop the icon.
        if taken.iter().all(|s| s.object.is_none() && s.text.trim().is_empty()) {
            self.objects.clear();
            return 0.0;
        }
        let base_size = style.font_size;
        let base_line = style.line_px();
        let buffer =
            text::shape(self.eng, &taken, avail_w, style.align, base_size, base_line);
        let (_, h) = text::measure(&buffer);
        self.place_objects(&buffer, &taken, x, y);
        self.cmds.push(Cmd::Text { x, y, buffer, spans: taken });
        h
    }

    /// Draw the paragraph's reserved images where shaping put their runs.
    ///
    /// Reading the position back off the shaped glyphs is the whole point: the
    /// line may have been centred, justified or wrapped, and only the buffer
    /// knows where the reservation actually ended up.
    fn place_objects(&mut self, buffer: &Buffer, spans: &[Span], x: f32, y: f32) {
        let objects = std::mem::take(&mut self.objects);
        if objects.is_empty() {
            return;
        }
        // Where each object's reservation actually landed.
        //
        // A run of no-break spaces should never be split, but shaping is
        // allowed to break one by glyph when it does not fit the space left on
        // the current line — and then its first fragment is a sliver at the end
        // of that line. Placing the box there drops it on top of its
        // neighbour, which is exactly what overlapping attachment chips were.
        // So every fragment is collected and the **widest** one wins: that is
        // the slot the object really got.
        let mut slots: Vec<Option<(f32, f32, f32)>> = vec![None; objects.len()];
        for run in buffer.layout_runs() {
            // Glyphs of one reservation are contiguous; a different index, or
            // the end of the run, closes the group.
            let mut group: Option<(usize, f32, f32)> = None;
            let centre = run.line_top + run.line_height / 2.0;
            let mut close = |group: Option<(usize, f32, f32)>,
                             slots: &mut Vec<Option<(f32, f32, f32)>>| {
                let Some((index, x0, x1)) = group else { return };
                if index >= slots.len() {
                    return;
                }
                let wider = slots[index].is_none_or(|(sx0, sx1, _)| sx1 - sx0 < x1 - x0);
                if wider {
                    slots[index] = Some((x0, x1, centre));
                }
            };
            for glyph in run.glyphs {
                let obj = spans.get(glyph.metadata).and_then(|s| s.object);
                match (&mut group, obj) {
                    (Some((index, _, x1)), Some(next)) if *index == next => {
                        *x1 = glyph.x + glyph.w;
                    }
                    _ => {
                        close(group.take(), &mut slots);
                        group = obj.map(|i| (i, glyph.x, glyph.x + glyph.w));
                    }
                }
            }
            close(group.take(), &mut slots);
        }

        // Boxes are already in the display list; they only need moving, and
        // that cannot happen while `objects` is borrowed.
        let mut moves: Vec<(Range<usize>, f32, f32)> = Vec::new();
        for (object, slot) in objects.iter().zip(slots) {
            let Some((x0, _, centre)) = slot else { continue };
            // Left-aligned in its reservation, after the author's left margin;
            // the rounding slack lands on the right, where a gap between
            // inline things belongs.
            moves.push((
                object.cmds.clone(),
                x + x0 + object.lead,
                y + centre - object.h / 2.0,
            ));
        }
        for (cmds, px, py) in moves {
            self.translate(cmds, px, py);
        }
    }

    /// Decode an `<img>` and work out the box it wants.
    ///
    /// Sizing follows the author when they said something and the image's own
    /// aspect ratio when they said half of it. An image we could not decode
    /// *and* were given no size for is not drawn as a grey rectangle of
    /// invented dimensions — it becomes its `alt` text, or nothing.
    fn image_box(&mut self, node: &Handle, style: &Style, avail_w: f32) -> Placement {
        let bitmap = self.decode_image(&attr(node, "src").unwrap_or_default());
        let natural = bitmap
            .as_ref()
            .map(|b| (b.w as f32 * self.scale, b.h as f32 * self.scale));

        let declared_w = style.width.map(|l| l.resolve(avail_w));
        let declared_h = style.height.map(|l| l.resolve(0.0));
        // Spacer gifs and 1×1 trackers are furniture, not content.
        let tiny = 3.0 * self.scale;
        if declared_w.is_some_and(|w| w <= tiny)
            || declared_h.is_some_and(|h| h <= tiny)
            || natural.is_some_and(|(w, h)| w <= tiny && h <= tiny)
        {
            return Placement::Nothing;
        }

        let alt = attr(node, "alt").unwrap_or_default();
        let ratio = natural.map(|(w, h)| if h > 0.5 { w / h } else { 1.0 });
        let size = match (declared_w, declared_h) {
            (Some(w), Some(h)) => Some((w, h)),
            (Some(w), None) => Some((w, ratio.map_or(w * 0.6, |r| w / r.max(0.01)))),
            (None, Some(h)) => Some((ratio.map_or(h * 1.6, |r| h * r), h)),
            (None, None) => natural,
        };
        let Some((w, h)) = size else {
            // A decorative image with no alt text is exactly what it says.
            return if alt.trim().is_empty() { Placement::Nothing } else { Placement::Alt(alt) };
        };

        // Wider than the room it has: scale down, keeping the aspect. This is
        // the width invariant reaching all the way into image sizing.
        let limit = style
            .max_width
            .map_or(avail_w, |l| l.resolve(avail_w))
            .clamp(1.0, avail_w);
        let (w, h) = if w > limit { (limit, h * limit / w.max(0.01)) } else { (w, h) };
        Placement::Image(ImageBox {
            bitmap,
            w: w.clamp(1.0, avail_w),
            h: h.clamp(1.0, self.max_h),
            alt,
        })
    }

    /// Place an `<img>` as a block of its own. Returns the height it consumed.
    pub(crate) fn image(
        &mut self,
        node: &Handle,
        style: &Style,
        x: f32,
        y: f32,
        avail_w: f32,
        link: Option<usize>,
    ) -> f32 {
        match self.image_box(node, style, avail_w) {
            Placement::Nothing => 0.0,
            Placement::Alt(alt) => {
                let mut spans = vec![Span::from_style(alt, style, link)];
                self.flush_inline(&mut spans, style, x, y, avail_w)
            }
            Placement::Image(b) => {
                let ix = align_offset(style.align, x, avail_w, b.w);
                let iy = y + style.margin.top;
                let blocked = b.bitmap.is_none();
                self.cmds.push(Cmd::Image {
                    x: ix,
                    y: iy,
                    w: b.w,
                    h: b.h,
                    radius: style.radius,
                    bitmap: b.bitmap,
                    link,
                });
                if blocked {
                    self.alt_label(&b.alt, style, ix, iy, b.w, b.h, link);
                }
                b.h + style.margin.vertical()
            }
        }
    }

    /// Reserve an `<img>` on the current line instead of breaking the paragraph
    /// for it. Returns false when it will not fit and has to be a block.
    ///
    /// The reservation is a run of no-break spaces as wide as the image; once
    /// the paragraph is shaped, [`Ctx::flush_inline`] reads back where that run
    /// landed and draws the image over it. This is the only way to get an icon
    /// onto a line: a shaped run holds glyphs, and an image is not one.
    fn inline_image(
        &mut self,
        node: &Handle,
        style: &Style,
        out: &mut Vec<Span>,
        avail_w: f32,
        link: Option<usize>,
    ) -> bool {
        let Placement::Image(b) = self.image_box(node, style, avail_w) else {
            return false;
        };
        // Too wide to share a line with anything — and a reservation that has
        // to be glyph-broken would land the image on two lines at once.
        if b.w > avail_w * 0.9 {
            return false;
        }
        // Emitted at the origin — picture first, then the `alt` label over it
        // when there are no pixels — and moved onto the line later. Placing it
        // as a run of commands rather than a lone bitmap is what keeps the
        // label travelling with the picture it describes.
        let start = self.cmds.len();
        let blocked = b.bitmap.is_none();
        self.cmds.push(Cmd::Image {
            x: 0.0,
            y: 0.0,
            w: b.w,
            h: b.h,
            radius: style.radius,
            bitmap: b.bitmap,
            link,
        });
        if blocked {
            self.alt_label(&b.alt, style, 0.0, 0.0, b.w, b.h, link);
        }
        self.reserve_object(out, style, link, start..self.cmds.len(), b.w, b.h, 0.0);
        true
    }

    /// Reserve `w × h` on the current line for commands already emitted at the
    /// origin.
    ///
    /// No-break spaces: a reservation split by a line break would place the
    /// same thing twice. The span also carries the object's height as its line
    /// height, because otherwise the object overlaps the lines around it.
    fn reserve_object(
        &mut self,
        out: &mut Vec<Span>,
        style: &Style,
        link: Option<usize>,
        cmds: Range<usize>,
        w: f32,
        h: f32,
        // Left margin, kept inside the reservation so the gap lands where the
        // author put it instead of being split evenly around the box.
        lead: f32,
    ) {
        let mut span = Span::from_style(String::new(), style, link);
        span.line_height = span.line_height.max(h + 2.0 * self.scale);
        span.underline = false;
        span.strike = false;
        // A run of no-break spaces as wide as the object. One glyph widened by
        // letter spacing would be tidier — and unbreakable — but a glyph's ink
        // width is not its advance, and the placement below reads glyph extents.
        let nbsp = text::nbsp_advance(self.eng, &span).max(0.5);
        let count = ((w / nbsp).ceil() as usize).clamp(1, 600);
        span.text = std::iter::repeat_n(text::NBSP, count).collect();
        span.object = Some(self.objects.len());
        self.objects.push(InlineObject { cmds, w, h, lead });
        out.push(span);
    }

    /// Label a blocked image's placeholder with its `alt` text, centred, when
    /// there is one and it fits. A named box beats an anonymous grey one.
    fn alt_label(
        &mut self,
        alt: &str,
        style: &Style,
        x: f32,
        y: f32,
        w: f32,
        h: f32,
        link: Option<usize>,
    ) {
        let alt = alt.trim();
        let pad = 6.0 * self.scale;
        if alt.is_empty() || w < 8.0 * pad || h < 4.0 * pad {
            return;
        }
        let size = style.font_size.min(13.0 * self.scale);
        let span = Span {
            text: alt.to_string(),
            size,
            line_height: size * 1.3,
            color: Rgba::rgb(0x6b, 0x74, 0x80),
            underline: false,
            ..Span::from_style(String::new(), style, link)
        };
        let buffer = text::shape(
            self.eng,
            std::slice::from_ref(&span),
            w - 2.0 * pad,
            Align::Center,
            size,
            size * 1.3,
        );
        let (_, th) = text::measure(&buffer);
        if th > h - pad {
            return;
        }
        self.cmds.push(Cmd::Text {
            x: x + pad,
            y: y + (h - th) / 2.0,
            buffer,
            spans: vec![span],
        });
    }

    /// Lay out an inline-level box (`display: inline-block` / `inline-table`)
    /// and reserve it on the current line. Returns false when it is too wide to
    /// share a line and should be a block instead.
    ///
    /// The box is laid out at the origin into the display list, then moved into
    /// place once shaping says where its reservation landed — the same trick as
    /// an inline image, except the payload is a run of commands rather than a
    /// bitmap. Walking into such a box as if it were inline text (which is what
    /// happened before) flattens its tables and loses every declared width:
    /// MJML builds all of its columns this way.
    fn inline_box(
        &mut self,
        node: &Handle,
        style: &Style,
        out: &mut Vec<Span>,
        avail_w: f32,
        link: Option<usize>,
        depth: usize,
    ) -> bool {
        if depth > MAX_DEPTH || self.over_budget(0.0) {
            return false;
        }
        // Shrink-to-fit, the inline-level sizing rule: as wide as the content
        // wants, never wider than the line.
        let (min, max) = self.intrinsic(node, style, depth);
        let frame = style.pad(avail_w).horizontal()
            + style.border_width.horizontal()
            + style.mar(avail_w).horizontal();
        let want = match style.width {
            Some(len) => len.resolve(avail_w),
            None => {
                // Shrink-to-fit, with the floor itself capped: a box whose
                // min-content is wider than the line still may not overflow it,
                // and an uncapped floor makes `clamp` panic on min > max.
                let floor = (min + frame).min(avail_w);
                (max + frame).clamp(floor, avail_w.max(floor))
            }
        };
        let w = want.clamp(1.0, avail_w.max(1.0));
        // Fills the line anyway: nothing can sit beside it, so a block box is
        // both simpler and identical on screen.
        if w > avail_w * 0.95 {
            return false;
        }

        // The box lays out whole paragraphs of its own, and each of those
        // flushes — which would otherwise consume *this* paragraph's pending
        // objects and leave every reservation index dangling.
        let outer_objects = std::mem::take(&mut self.objects);
        let start = self.cmds.len();
        let mut inner = style.clone();
        inner.margin = Edges::default();
        inner.margin_pct = Edges::default();
        let lead = style.mar(avail_w).left;
        let h = if tag(node) == "table" || style.display == Display::Table {
            inner.display = Display::Table;
            self.table(node, &inner, 0.0, 0.0, w, link, depth)
        } else {
            inner.display = Display::Block;
            self.block(node, &inner, 0.0, 0.0, w, link, depth)
        };
        let cmds = start..self.cmds.len();
        self.objects = outer_objects;
        if cmds.is_empty() && h <= 0.5 {
            return true; // nothing to show, and nothing to reserve for
        }

        self.reserve_object(out, style, link, cmds, w + lead, h, lead);
        true
    }

    /// Decode `src`, sharing the result with every other use of the same URI.
    fn decode_image(&mut self, src: &str) -> Option<Rc<Bitmap>> {
        if src.trim().is_empty() {
            return None;
        }
        let mut hasher = std::collections::hash_map::DefaultHasher::new();
        src.hash(&mut hasher);
        let key = hasher.finish();
        if let Some(hit) = self.images.get(&key) {
            return hit.clone();
        }
        let decoded = image::decode(src, self.scale)
            .or_else(|| {
                // Not a `data:` URI. Ask the host — which is the only party
                // allowed to know whether this sender's media is permitted,
                // and the only one that can reach a network or a MIME part.
                if self.resolvable(src) {
                    self.host
                        .resources
                        .fetch(src)
                        .as_deref()
                        .and_then(|b| image::decode_bytes(b, self.scale))
                } else {
                    None
                }
            })
            .map(Rc::new);
        self.images.insert(key, decoded.clone());
        decoded
    }

    /// Whether the host is worth asking about this `src`.
    ///
    /// `cid:` is a part of the message we already have, so `block_remote` does
    /// not apply to it — that flag is about the network, not about attachments.
    fn resolvable(&self, src: &str) -> bool {
        let src = src.trim();
        let is = |p: &str| src.len() >= p.len() && src[..p.len()].eq_ignore_ascii_case(p);
        if is("cid:") {
            return true;
        }
        !self.host.block_remote && (is("http://") || is("https://") || is("//"))
    }

    pub(crate) fn over_budget(&self, cursor: f32) -> bool {
        self.nodes > MAX_NODES || cursor > self.max_h
    }
}

pub(crate) fn align_offset(align: Align, x: f32, avail_w: f32, w: f32) -> f32 {
    match align {
        Align::Center => x + (avail_w - w).max(0.0) / 2.0,
        Align::Right => x + (avail_w - w).max(0.0),
        _ => x,
    }
}

pub(crate) fn is_inline(d: Display) -> bool {
    matches!(d, Display::Inline | Display::InlineBlock)
}

fn ends_with_word(spans: &[Span]) -> bool {
    spans.last().is_some_and(|s| s.text.chars().last().is_some_and(|c| !c.is_whitespace()))
}

/// Links that can't go anywhere are not worth a hit-box.
fn is_useful_href(h: &str) -> bool {
    let h = h.trim();
    !h.is_empty() && h != "#" && !h.to_ascii_lowercase().starts_with("javascript:")
}
