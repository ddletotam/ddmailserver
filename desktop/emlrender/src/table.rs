//! Table layout — the grid real email is actually built out of.
//!
//! Almost every mail in the corpus is nested `<table>`s: a 600 px wrapper, a
//! row of columns, a spacer. Laying those out as plain blocks (what the first
//! vertical slice did) linearises everything and drops `cellpadding` on the
//! floor, which is why text ended up glued to the bubble's edge.
//!
//! Two rules carry over from the block path:
//!
//! * a table is **never** laid out wider than the width handed to it;
//! * when the columns genuinely cannot fit — their minimum content widths add
//!   up to more than we have — the table **linearises**: every cell becomes a
//!   full-width block, stacked. Squeezing four columns into a 420 px bubble
//!   produces unreadable slivers, and mail that wide is desktop-only anyway.
//!
//! Column widths need each cell's intrinsic (min, max) width *before* layout,
//! so this module also carries the measuring walk. It mirrors the layout walk
//! but emits nothing, and its results are cached per node.

use std::rc::Rc;

use markup5ever_rcdom::Handle;

use crate::dom::{attr, children, is_dropped, tag, text as node_text};
use crate::layout::{align_offset, is_inline, Cmd, Ctx, Flow, MAX_DEPTH};
use crate::style::{Display, Edges, Len, Rgba, Style, VAlign};
use crate::text::{self, Span};

/// Ceilings. A table past these is broken or hostile, and we want a truncated
/// render rather than a hung worker.
const MAX_COLS: usize = 64;
const MAX_ROWS: usize = 512;
const MAX_SPAN: usize = 512;

struct Cell {
    node: Handle,
    style: Style,
    col: usize,
    colspan: usize,
    /// How many rows this cell occupies. Its height belongs to the *last* of
    /// them, not the first.
    rowspan: usize,
}

struct Row {
    style: Style,
    cells: Vec<Cell>,
}

struct Grid {
    rows: Vec<Row>,
    cols: usize,
    /// `cellspacing`, in device px, applied between and around cells.
    spacing: f32,
}

/// One column's width demands, gathered from every cell that sits in it.
#[derive(Default, Clone, Copy)]
struct Col {
    /// Widest thing that cannot be broken (longest word, widest fixed child).
    min: f32,
    /// Width the content would take if nothing wrapped.
    max: f32,
    /// A declared `width="120"` — a preference, shrinkable down to `min`.
    fixed: Option<f32>,
    /// A declared `width="30%"` — resolved against the table, not the bubble.
    pct: Option<f32>,
}

/// A cell whose box geometry is still waiting on its row's final height.
struct Placed {
    bg: (Option<usize>, Option<usize>),
    border: Option<usize>,
    content: std::ops::Range<usize>,
    x: f32,
    /// Top of the cell box. A cell spanning rows is finished long after the
    /// row it started in has scrolled out of the loop.
    y: f32,
    w: f32,
    box_h: f32,
    valign: VAlign,
    /// Kept whole because the background may be a gradient or an image, not
    /// just a colour, and none of it is known until the row height is.
    style: Style,
    border_width: Edges,
    border_color: Rgba,
    radius: f32,
}

impl Ctx<'_> {
    /// Lay out a `<table>` at `(x, y)`. Returns the height it consumed,
    /// margins included — same contract as [`Ctx::block`].
    pub(crate) fn table(
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
        let grid = self.build_grid(node, style);
        if grid.cols == 0 {
            // `display: table` on a styling wrapper with no rows in it. Nothing
            // here is a grid; the block path renders it correctly.
            return self.block(node, style, x, y, avail_w, link, depth);
        }
        let cols = self.columns(&grid, depth);

        let (m, border, pad) = (style.mar(avail_w), style.border_width, style.pad(avail_w));
        let gaps = grid.spacing * (grid.cols as f32 + 1.0);
        let frame = border.horizontal() + pad.horizontal() + gaps;
        let max_outer = (avail_w - m.horizontal()).max(1.0);

        // No declared width means shrink-to-fit, which is what makes a narrow
        // centred logo table sit centred instead of stretching.
        let natural = cols.iter().map(|c| c.max).sum::<f32>() + frame;
        let outer = match style.width {
            Some(len) => len.resolve(avail_w),
            None => natural,
        }
        .clamp(1.0, max_outer);
        let outer = match style.max_width {
            Some(len) => outer.min(len.resolve(avail_w)).max(1.0),
            None => outer,
        };

        let content_w = (outer - border.horizontal() - pad.horizontal()).max(1.0);
        let usable = (content_w - gaps).max(1.0);
        let min_sum: f32 = cols.iter().map(|c| c.min).sum();
        let linear = grid.cols >= 2 && min_sum > usable;

        // `align="center"` places the box; an inherited alignment (from
        // `<center>`, the way half the corpus centres things) does too.
        let box_align = style.box_align.unwrap_or(style.align);
        let box_x = align_offset(box_align, x + m.left, max_outer, outer);
        let box_y = y + m.top;
        let content_x = box_x + border.left + pad.left;
        let content_y = box_y + border.top + pad.top;

        let bg_slots = self.reserve_bg(style);
        let border_slot = self.reserve(
            border.vertical() + border.horizontal() > 0.0 && style.border_color.is_visible(),
        );

        let content_h = if linear {
            self.linear_rows(&grid, content_x, content_y, content_w, link, depth)
        } else {
            let widths = distribute(&cols, usable);
            self.grid_rows(&grid, &widths, content_x, content_y, content_w, link, depth)
        };
        let content_h = match style.height {
            Some(len) => content_h.max(len.resolve(0.0)),
            None => content_h,
        };

        let box_h = content_h + border.vertical() + pad.vertical();
        self.patch_bg(bg_slots, style, box_x, box_y, outer, box_h);
        if let Some(i) = border_slot {
            self.cmds[i] = Cmd::Border {
                x: box_x,
                y: box_y,
                w: outer,
                h: box_h,
                radius: style.radius,
                widths: border,
                color: style.border_color,
            };
        }
        box_h + m.vertical()
    }

    // ------------------------------------------------------------------ grid

    fn build_grid(&self, node: &Handle, style: &Style) -> Grid {
        // `cellpadding` is declared on the table but belongs to every cell —
        // this is the attribute whose loss left text touching the bubble edge.
        let cellpadding = attr_px(node, "cellpadding", self.scale, 200.0);
        let spacing = attr_px(node, "cellspacing", self.scale, 100.0).unwrap_or(0.0);
        // `border="1"` frames the table *and* rules every cell, 1 px each.
        let cell_border = attr(node, "border")
            .and_then(|v| v.trim().parse::<f32>().ok())
            .filter(|v| *v > 0.0)
            .map(|_| self.scale);

        let mut rows = Vec::new();
        let mut occupied: Vec<usize> = Vec::new();
        self.collect_rows(
            node,
            style,
            &mut rows,
            &mut occupied,
            cellpadding,
            cell_border,
            style.border_color,
        );
        let cols = rows
            .iter()
            .flat_map(|r| r.cells.iter())
            .map(|c| c.col + c.colspan)
            .max()
            .unwrap_or(0);
        Grid { rows, cols, spacing }
    }

    /// Gather rows, descending through row groups. `occupied` carries the rows
    /// still owed to a `rowspan` from above, so later rows skip those columns.
    #[allow(clippy::too_many_arguments)]
    fn collect_rows(
        &self,
        node: &Handle,
        parent: &Style,
        rows: &mut Vec<Row>,
        occupied: &mut Vec<usize>,
        cellpadding: Option<f32>,
        cell_border: Option<f32>,
        border_color: Rgba,
    ) {
        for child in children(node) {
            if rows.len() >= MAX_ROWS {
                return;
            }
            let t = tag(&child);
            if t.is_empty() || is_dropped(t) {
                continue;
            }
            let cs = self.res.resolve(&child, parent);
            if cs.display == Display::None || cs.hidden {
                continue;
            }
            if matches!(t, "tbody" | "thead" | "tfoot") {
                self.collect_rows(
                    &child,
                    &cs,
                    rows,
                    occupied,
                    cellpadding,
                    cell_border,
                    border_color,
                );
                continue;
            }
            if cs.display != Display::TableRow {
                // Stray content directly under a table: html5ever foster-parents
                // it out during parsing, so anything left here is furniture.
                continue;
            }

            let mut cells = Vec::new();
            let mut col = 0usize;
            for kid in children(&child) {
                let kt = tag(&kid);
                if kt.is_empty() || is_dropped(kt) {
                    continue;
                }
                let mut cst = self.res.resolve(&kid, &cs);
                if cst.display == Display::None || cst.hidden || cst.display != Display::TableCell {
                    continue;
                }
                while col < occupied.len() && occupied[col] > 0 {
                    col += 1;
                }
                if col >= MAX_COLS {
                    break;
                }
                let colspan = span_attr(&kid, "colspan").min(MAX_COLS - col);
                let rowspan = span_attr(&kid, "rowspan");
                if occupied.len() < col + colspan {
                    occupied.resize(col + colspan, 0);
                }
                for slot in &mut occupied[col..col + colspan] {
                    *slot = rowspan;
                }
                if let Some(p) = cellpadding {
                    if is_zero(&cst.padding) && is_zero(&cst.padding_pct) {
                        cst.padding = Edges::all(p);
                    }
                }
                if let Some(b) = cell_border {
                    if is_zero(&cst.border_width) {
                        cst.border_width = Edges::all(b);
                        if !cst.border_color.is_visible() {
                            cst.border_color = border_color;
                        }
                    }
                }
                cells.push(Cell { node: kid.clone(), style: cst, col, colspan, rowspan });
                col += colspan;
            }
            for slot in occupied.iter_mut() {
                *slot = slot.saturating_sub(1);
            }
            rows.push(Row { style: cs, cells });
        }
    }

    /// Width demands per column, folded from every cell's intrinsic widths.
    fn columns(&mut self, grid: &Grid, depth: usize) -> Vec<Col> {
        let mut cols = vec![Col::default(); grid.cols];
        let mut spanning: Vec<(&Cell, f32, f32)> = Vec::new();

        for row in &grid.rows {
            for cell in &row.cells {
                let (mut cmin, mut cmax) = self.intrinsic(&cell.node, &cell.style, depth + 1);
                let frame = cell.style.padding.horizontal() + cell.style.border_width.horizontal();
                cmin += frame;
                cmax += frame;
                if cell.colspan != 1 {
                    spanning.push((cell, cmin, cmax));
                    continue;
                }
                let c = &mut cols[cell.col];
                c.min = c.min.max(cmin);
                c.max = c.max.max(cmax);
                match cell.style.width {
                    Some(Len::Px(w)) => c.fixed = Some(c.fixed.unwrap_or(0.0).max(w)),
                    Some(Len::Pct(p)) => c.pct = Some(c.pct.unwrap_or(0.0).max(p)),
                    None => {}
                }
            }
        }

        // A spanning cell only raises the columns it covers, and only when the
        // cover is already too narrow — otherwise one colspan inflates them all.
        for (cell, cmin, cmax) in spanning {
            let range = cell.col..(cell.col + cell.colspan).min(cols.len());
            if range.is_empty() {
                continue;
            }
            let share = range.len() as f32;
            let have: f32 = cols[range.clone()].iter().map(|c| c.min).sum();
            if cmin > have {
                let add = (cmin - have) / share;
                for c in &mut cols[range.clone()] {
                    c.min += add;
                }
            }
            let have: f32 = cols[range.clone()].iter().map(|c| c.max).sum();
            if cmax > have {
                let add = (cmax - have) / share;
                for c in &mut cols[range] {
                    c.max += add;
                }
            }
        }
        for c in &mut cols {
            // A declared cell width is a stated preference, so it counts toward
            // what the table naturally wants to be. Without this a shrink-to-fit
            // table whose only content sizes itself in percentages — a logo at
            // `width:100%` inside `<td style="width:112px">` — measures as zero
            // and collapses to a sliver.
            c.max = c.max.max(c.min).max(c.fixed.unwrap_or(0.0));
        }
        cols
    }

    // ---------------------------------------------------------------- rows

    fn grid_rows(
        &mut self,
        grid: &Grid,
        widths: &[f32],
        x: f32,
        y: f32,
        content_w: f32,
        link: Option<usize>,
        depth: usize,
    ) -> f32 {
        let sp = grid.spacing;
        // Column edges, spacing folded in: column `i` starts at `offs[i]` and a
        // span of `n` columns runs to `offs[i + n]` minus the trailing gap.
        let mut offs = Vec::with_capacity(widths.len() + 1);
        let mut acc = sp;
        for w in widths {
            offs.push(acc);
            acc += w + sp;
        }
        offs.push(acc);

        let mut cursor = y + sp;
        // Cells still spanning down from an earlier row, with the index of the
        // last row each one covers.
        let mut spanning: Vec<(Placed, usize)> = Vec::new();

        for (index, row) in grid.rows.iter().enumerate() {
            if self.over_budget(cursor) {
                break;
            }
            let row_top = cursor;
            let row_bg = self.reserve_bg(&row.style);
            let mut placed = Vec::with_capacity(row.cells.len());
            let mut row_h = 0.0f32;

            for cell in &row.cells {
                let end = (cell.col + cell.colspan).min(widths.len());
                if cell.col >= widths.len() {
                    continue;
                }
                let cx = x + offs[cell.col];
                let cw = (offs[end] - offs[cell.col] - sp).max(1.0);
                let p = self.cell(cell, cx, row_top, cw, content_w, link, depth);
                if cell.rowspan > 1 {
                    // Its height is not this row's problem — it is owed to the
                    // last row it covers, and paying it here would inflate a
                    // row that has nothing tall in it.
                    spanning.push((p, index + cell.rowspan - 1));
                } else {
                    row_h = row_h.max(p.box_h);
                    placed.push(p);
                }
            }
            if let Some(len) = row.style.height {
                row_h = row_h.max(len.resolve(0.0));
            }
            // Anything ending here has to fit by now; the shortfall (if any)
            // grows this row, which is where a browser puts it too.
            for (p, _) in spanning.iter().filter(|(_, last)| *last == index) {
                row_h += (p.box_h - (row_top + row_h - p.y)).max(0.0);
            }

            let row_bottom = row_top + row_h;
            for p in placed {
                self.finish_cell(p, row_h);
            }
            let (ending, carried): (Vec<_>, Vec<_>) =
                spanning.into_iter().partition(|(_, last)| *last <= index);
            spanning = carried;
            for (p, _) in ending {
                let height = (row_bottom - p.y).max(p.box_h);
                self.finish_cell(p, height);
            }

            self.patch_bg(row_bg, &row.style, x, row_top, content_w, row_h);
            cursor = row_bottom + sp;
        }

        // A `rowspan` larger than the rows that follow it, or a run cut short
        // by the height budget: give the cell what there is.
        let bottom = (cursor - sp).max(y);
        for (p, _) in spanning {
            let height = (bottom - p.y).max(p.box_h);
            self.finish_cell(p, height);
        }
        (cursor - y).max(0.0)
    }

    /// The escape hatch: every cell full width, stacked in reading order.
    /// Declared cell widths are ignored — honouring them is exactly what does
    /// not fit.
    fn linear_rows(
        &mut self,
        grid: &Grid,
        x: f32,
        y: f32,
        content_w: f32,
        link: Option<usize>,
        depth: usize,
    ) -> f32 {
        let mut cursor = y;
        for row in &grid.rows {
            for cell in &row.cells {
                if self.over_budget(cursor) {
                    return (cursor - y).max(0.0);
                }
                let p = self.cell(cell, x, cursor, content_w, content_w, link, depth);
                let h = p.box_h;
                self.finish_cell(p, h);
                cursor += h;
            }
        }
        (cursor - y).max(0.0)
    }

    /// Lay out one cell's content into a box of exactly `w`. The box's own
    /// background and border wait for the row height, so they get reserved
    /// slots and the content range is remembered for vertical alignment.
    ///
    /// `base` is the table's content width — a cell's percentage padding is a
    /// percentage of the table it sits in, not of its own column.
    fn cell(
        &mut self,
        cell: &Cell,
        x: f32,
        y: f32,
        w: f32,
        base: f32,
        link: Option<usize>,
        depth: usize,
    ) -> Placed {
        let s = &cell.style;
        let (border, pad) = (s.border_width, s.pad(base));
        let bg = self.reserve_bg(s);
        let border_slot = self.reserve(
            border.vertical() + border.horizontal() > 0.0 && s.border_color.is_visible(),
        );
        let from = self.cmds.len();

        let inner_w = (w - border.horizontal() - pad.horizontal()).max(1.0);
        let inner_x = x + border.left + pad.left;
        let inner_y = y + border.top + pad.top;
        let style = cell.style.clone();
        let mut h = self.block_children(&cell.node, &style, inner_x, inner_y, inner_w, link, depth + 1);
        if let Some(len) = style.height {
            h = h.max(len.resolve(0.0));
        }

        Placed {
            bg,
            border: border_slot,
            content: from..self.cmds.len(),
            x,
            y,
            w,
            box_h: h + border.vertical() + pad.vertical(),
            valign: style.valign,
            border_width: border,
            border_color: style.border_color,
            radius: style.radius,
            style,
        }
    }

    /// Stretch a cell's box to `height` and align its content inside it.
    fn finish_cell(&mut self, mut p: Placed, height: f32) {
        let dy = match p.valign {
            VAlign::Top => 0.0,
            VAlign::Middle => (height - p.box_h) / 2.0,
            VAlign::Bottom => height - p.box_h,
        }
        .max(0.0);
        if dy > 0.5 {
            self.shift(p.content, dy);
        }
        let style = std::mem::replace(&mut p.style, Style::root(1.0));
        self.patch_bg(p.bg, &style, p.x, p.y, p.w, height);
        if let Some(i) = p.border {
            self.cmds[i] = Cmd::Border {
                x: p.x,
                y: p.y,
                w: p.w,
                h: height,
                radius: p.radius,
                widths: p.border_width,
                color: p.border_color,
            };
        }
    }

    // --------------------------------------------------------- measurement

    /// Intrinsic (min, max) width of a node's **content**, excluding its own
    /// padding, border and margin. Cached: a node's computed style depends only
    /// on its ancestors, so the answer never changes, and nested tables would
    /// otherwise re-measure the same subtree once per nesting level.
    pub(crate) fn intrinsic(&mut self, node: &Handle, style: &Style, depth: usize) -> (f32, f32) {
        let key = Rc::as_ptr(node) as usize;
        if let Some(hit) = self.intrinsic_cache.get(&key) {
            return *hit;
        }
        // Measuring walks the same subtree layout will walk, so it must not
        // spend layout's node budget, nor leave its `<a href>`s behind.
        let (nodes, hrefs) = (self.nodes, self.hrefs.len());
        let measured = if style.display == Display::Table {
            self.table_intrinsic(node, style, depth)
        } else {
            self.intrinsic_children(node, style, depth)
        };
        self.nodes = nodes;
        self.hrefs.truncate(hrefs);
        self.intrinsic_cache.insert(key, measured);
        measured
    }

    /// A table's min is its **widest single cell**, not the sum of its columns:
    /// when the columns don't fit, the table linearises, and every cell gets
    /// the full width. Claiming the sum would cascade linearisation upward.
    fn table_intrinsic(&mut self, node: &Handle, style: &Style, depth: usize) -> (f32, f32) {
        if depth > MAX_DEPTH {
            return (0.0, 0.0);
        }
        let grid = self.build_grid(node, style);
        if grid.cols == 0 {
            return self.intrinsic_children(node, style, depth);
        }
        let cols = self.columns(&grid, depth);
        let gaps = grid.spacing * (grid.cols as f32 + 1.0);
        let min = cols.iter().map(|c| c.min).fold(0.0f32, f32::max);
        let max = cols.iter().map(|c| c.max).sum::<f32>() + gaps;
        (min.min(max), max)
    }

    /// The measuring counterpart of [`Ctx::block_children`]: same walk, same
    /// inline gathering, but it folds widths instead of emitting commands.
    fn intrinsic_children(&mut self, node: &Handle, style: &Style, depth: usize) -> (f32, f32) {
        if depth > MAX_DEPTH || self.measure_left == 0 {
            return (0.0, 0.0);
        }
        let (mut min, mut max) = (0.0f32, 0.0f32);
        let mut inline: Vec<Span> = Vec::new();

        for child in children(node) {
            if self.measure_left == 0 {
                break;
            }
            self.measure_left -= 1;
            if let Some(t) = node_text(&child) {
                self.push_text(&mut inline, &t, style, None);
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
            match t {
                "br" => {
                    self.push_raw(&mut inline, "\n", &cs, None);
                    continue;
                }
                "hr" => continue,
                "img" => {
                    self.flush_measure(&mut inline, style, &mut min, &mut max);
                    // Only an absolute width says anything about intrinsic size;
                    // a percentage is a demand on the container, not a demand
                    // for space.
                    if let Some(Len::Px(w)) = cs.width {
                        let w = w + cs.margin.horizontal();
                        min = min.max(w);
                        max = max.max(w);
                    }
                    continue;
                }
                _ => {}
            }

            if cs.display == Display::InlineBlock {
                // Measured as a box, not walked into as text: an inline-level
                // box has its own padding and border, and delegating to the
                // inline walk would measure only what is inside it. A button
                // is `padding: 14px 36px` around six characters — lose the
                // frame and it lays out at a width its own label cannot fit.
                let (bmin, bmax) = self.intrinsic(&child, &cs, depth + 1);
                let frame = cs.padding.horizontal()
                    + cs.border_width.horizontal()
                    + cs.margin.horizontal();
                min = min.max(bmin + frame);
                max = max.max(bmax + frame);
                continue;
            }
            if is_inline(cs.display) {
                // The same walk layout uses, with the image branch folding a
                // width in instead of placing a box.
                let mut flow = Flow::Measure { min: 0.0, max: 0.0 };
                self.inline_subtree(&child, &cs, &mut inline, None, depth + 1, &mut flow);
                if let Flow::Measure { min: imin, max: imax } = flow {
                    min = min.max(imin);
                    max = max.max(imax);
                }
                continue;
            }
            self.flush_measure(&mut inline, style, &mut min, &mut max);
            let (mut cmin, mut cmax) = self.intrinsic(&child, &cs, depth + 1);
            let frame =
                cs.padding.horizontal() + cs.border_width.horizontal() + cs.margin.horizontal();
            cmin += frame;
            cmax += frame;
            // A declared width is what the box *wants*; it still shrinks when
            // the container is narrower, so it raises max but never min.
            if let Some(Len::Px(w)) = cs.width {
                cmax = cmax.max(w);
            }
            min = min.max(cmin);
            max = max.max(cmax);
        }
        self.flush_measure(&mut inline, style, &mut min, &mut max);
        (min, max)
    }

    fn flush_measure(&mut self, spans: &mut Vec<Span>, style: &Style, min: &mut f32, max: &mut f32) {
        if spans.is_empty() {
            return;
        }
        let taken = std::mem::take(spans);
        if taken.iter().all(|s| s.text.trim().is_empty()) {
            return;
        }
        let (w0, w1) = text::measure_min_max(self.eng, &taken, style.font_size, style.line_px());
        *min = min.max(w0);
        *max = max.max(w1);
    }
}

/// Hand `usable` px out to the columns.
///
/// Every column starts at what it asked for, then the result is pulled toward
/// the minimums (when there is too little room) or padded out from the flexible
/// columns (when there is too much). The one invariant that matters: the widths
/// returned never sum to more than `usable`.
fn distribute(cols: &[Col], usable: f32) -> Vec<f32> {
    let mut w: Vec<f32> = cols
        .iter()
        .map(|c| {
            let want = match (c.pct, c.fixed) {
                (Some(p), _) => usable * p / 100.0,
                (None, Some(f)) => f,
                (None, None) => c.max,
            };
            want.max(c.min).max(0.0)
        })
        .collect();
    let total: f32 = w.iter().sum();

    if total > usable + 0.01 {
        // Shrink each column in proportion to how much it *can* give up.
        let slack: f32 = w.iter().zip(cols).map(|(x, c)| (x - c.min).max(0.0)).sum();
        if slack > 0.01 {
            let k = ((total - usable) / slack).min(1.0);
            for (x, c) in w.iter_mut().zip(cols) {
                *x -= (*x - c.min).max(0.0) * k;
            }
        }
    } else if total < usable - 0.01 {
        let extra = usable - total;
        // Grow the auto columns; a declared width is a promise we keep while
        // there is room to keep it.
        let flex: f32 = cols
            .iter()
            .filter(|c| c.pct.is_none() && c.fixed.is_none())
            .map(|c| c.max.max(1.0))
            .sum();
        if flex > 0.01 {
            for (x, c) in w.iter_mut().zip(cols) {
                if c.pct.is_none() && c.fixed.is_none() {
                    *x += extra * c.max.max(1.0) / flex;
                }
            }
        } else if !w.is_empty() {
            let share = extra / w.len() as f32;
            for x in w.iter_mut() {
                *x += share;
            }
        }
    }

    for x in w.iter_mut() {
        *x = x.max(1.0);
    }
    // The width invariant is absolute: if the columns still don't fit — every
    // one at its minimum, or 64 of them in a 420 px bubble — scale them down
    // together. Text breaks mid-word inside them, which never overflows.
    let total: f32 = w.iter().sum();
    if total > usable && total > 0.0 {
        let k = usable / total;
        for x in w.iter_mut() {
            *x *= k;
        }
    } else if let Some(i) = widest(&w) {
        // Absorb rounding drift so the row fills its width exactly.
        w[i] += usable - total;
    }
    w
}

fn widest(w: &[f32]) -> Option<usize> {
    w.iter()
        .enumerate()
        .fold(None, |best, (i, v)| match best {
            Some((_, bv)) if bv >= *v => best,
            _ => Some((i, *v)),
        })
        .map(|(i, _)| i)
}

fn is_zero(e: &Edges) -> bool {
    e.horizontal() + e.vertical() <= 0.0
}

fn span_attr(node: &Handle, name: &str) -> usize {
    attr(node, name)
        .and_then(|v| v.trim().parse::<usize>().ok())
        .unwrap_or(1)
        .clamp(1, MAX_SPAN)
}

fn attr_px(node: &Handle, name: &str, scale: f32, limit: f32) -> Option<f32> {
    attr(node, name)
        .and_then(|v| v.trim().trim_end_matches("px").trim().parse::<f32>().ok())
        .map(|v| (v * scale).clamp(0.0, limit * scale))
}
