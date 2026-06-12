//! Shared types between the platform render backends (WebKitGTK / WebView2)
//! and main.rs. Kept tiny and dependency-free so both `#[cfg]` paths use it.

/// A clickable `<a href>` rectangle extracted at render time, in CSS px
/// relative to the rendered bubble (same coordinate space as the Slint
/// Image's logical box, so a click's (x, y) maps directly). Renderer-agnostic:
/// both backends fill this via a `getBoundingClientRect` JS pass, and the click
/// is resolved by a pure point-in-rect test — no live DOM needed.
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

/// Parse the JSON array emitted by `LINK_RECTS_JS` into LinkRects.
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

/// JS that evaluates to an ARRAY of every `<a href>`'s document-relative rect +
/// href. Returning the array (not a stringified string) lets each backend get
/// array JSON directly: WebView2's ExecuteScript JSON-encodes the result;
/// WebKit's JSCValue::to_json does the same. Feed the result to
/// [`parse_link_rects`].
pub const LINK_RECTS_JS: &str = "Array.prototype.slice.call(\
document.querySelectorAll('a[href]')).map(function(a){\
var r=a.getBoundingClientRect();\
return {x:r.left+(window.scrollX||0),y:r.top+(window.scrollY||0),\
w:r.width,h:r.height,href:a.href};\
})";

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
}

impl TextRun {
    pub fn contains(&self, x: f32, y: f32) -> bool {
        x >= self.x && x <= self.x + self.w && y >= self.y && y <= self.y + self.h
    }
}

/// Parse the JSON array emitted by `TEXT_RUNS_JS`.
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
            })
        })
        .collect()
}

/// JS that walks every text node and emits one rect per WORD (whitespace
/// split via Range), document-relative — the bubble's text layer. Capped so
/// a pathological newsletter can't produce megabyte sidecars.
pub const TEXT_RUNS_JS: &str = "(function(){\
var runs=[],walker=document.createTreeWalker(document.body,NodeFilter.SHOW_TEXT),n;\
var range=document.createRange(),re=/\\S+/g,m;\
while((n=walker.nextNode())&&runs.length<6000){\
var s=n.textContent;if(!s||!s.trim())continue;\
re.lastIndex=0;\
while((m=re.exec(s))&&runs.length<6000){\
try{\
range.setStart(n,m.index);range.setEnd(n,m.index+m[0].length);\
var r=range.getBoundingClientRect();\
if(r.width>0&&r.height>0){\
runs.push({x:r.left+(window.scrollX||0),y:r.top+(window.scrollY||0),\
w:r.width,h:r.height,t:m[0]});}\
}catch(e){}\
}}\
return runs;})()";
