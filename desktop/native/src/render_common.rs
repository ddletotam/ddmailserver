//! Geometry the renderer hands up to main.rs, plus the JSON round-trip the
//! texture cache stores it as. Kept tiny and dependency-free.

/// A clickable `<a href>` rectangle extracted at render time, in CSS px
/// relative to the rendered bubble (same coordinate space as the Slint
/// Image's logical box, so a click's (x, y) maps directly). The click is
/// resolved by a pure point-in-rect test — no live DOM needed.
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

/// Parse LinkRects back out of the texture cache's sidecar JSON.
/// Shape: `[{"x":..,"y":..,"w":..,"h":..,"href":".."}, ...]`.
pub fn parse_link_rects(json: &str) -> Vec<LinkRect> {
    let v: serde_json::Value = match serde_json::from_str(json) {
        Ok(v) => v,
        Err(_) => return Vec::new(),
    };
    let Some(arr) = v.as_array() else { return Vec::new() };
    arr.iter()
        .filter_map(|o| {
            let href = o.get("href")?.as_str()?.to_string();
            if href.is_empty() {
                return None;
            }
            Some(LinkRect {
                x: o.get("x")?.as_f64()? as f32,
                y: o.get("y")?.as_f64()? as f32,
                w: o.get("w")?.as_f64()? as f32,
                h: o.get("h")?.as_f64()? as f32,
                href,
            })
        })
        .collect()
}

/// One selectable word of rendered text: its rect (CSS px, bubble-relative,
/// same space as LinkRect) and the word itself. The set of runs, in DOM
/// order, is the bubble's "text layer" — selection is a contiguous slice of
/// it (the PDF-viewer approach: highlight rects over a raster page).
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

/// Parse TextRuns back out of the texture cache's sidecar JSON.
/// Shape: `[{"x":..,"y":..,"w":..,"h":..,"t":".."}, ...]`.
pub fn parse_text_runs(json: &str) -> Vec<TextRun> {
    let v: serde_json::Value = match serde_json::from_str(json) {
        Ok(v) => v,
        Err(_) => return Vec::new(),
    };
    let Some(arr) = v.as_array() else { return Vec::new() };
    arr.iter()
        .filter_map(|o| {
            let text = o.get("t")?.as_str()?.to_string();
            if text.is_empty() {
                return None;
            }
            Some(TextRun {
                x: o.get("x")?.as_f64()? as f32,
                y: o.get("y")?.as_f64()? as f32,
                w: o.get("w")?.as_f64()? as f32,
                h: o.get("h")?.as_f64()? as f32,
                text,
                cont: o.get("c").and_then(|c| c.as_i64()).unwrap_or(0) != 0,
            })
        })
        .collect()
}

