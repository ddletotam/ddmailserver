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

/// JS that neutralises scrollbars before measuring/extraction. At the 1px-tall
/// measuring viewport the vertical scrollbar steals ~17px of layout width; the
/// final full-height viewport has none — so any geometry read while it's
/// visible describes a *different* (narrower) layout than the snapshot.
pub const HIDE_SCROLLBARS_JS: &str = "(function(){\
var d=document.documentElement,b=document.body;\
if(d)d.style.overflow='hidden';\
if(b)b.style.overflow='hidden';\
return 1;})()";

/// JS that evaluates to an ARRAY of every `<a href>`'s document-relative
/// rects + href — one entry per line fragment (`getClientRects`), so a link
/// wrapped across lines yields tight per-line rects instead of one union box
/// whose dead corners would swallow clicks on unrelated text. Returning the
/// array (not a stringified string) lets each backend get array JSON
/// directly: WebView2's ExecuteScript JSON-encodes the result; WebKit's
/// JSCValue::to_json does the same. Feed the result to [`parse_link_rects`].
pub const LINK_RECTS_JS: &str = "(function(){\
var out=[],as=document.querySelectorAll('a[href]');\
var sx=window.scrollX||0,sy=window.scrollY||0;\
for(var i=0;i<as.length&&out.length<4000;i++){\
var a=as[i],rs=a.getClientRects();\
for(var j=0;j<rs.length&&out.length<4000;j++){\
var r=rs[j];\
if(r.width>0&&r.height>0)\
out.push({x:r.left+sx,y:r.top+sy,w:r.width,h:r.height,href:a.href});\
}}\
return out;})()";

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
                cont: o.get("c").and_then(|c| c.as_i64()).unwrap_or(0) != 0,
            })
        })
        .collect()
}

/// JS that walks every text node and emits one rect per WORD (whitespace
/// split via Range), document-relative — the bubble's text layer. Capped so
/// a pathological newsletter can't produce megabyte sidecars.
///
/// A word wrapped across lines (long URL, break-word, CJK) gets one run per
/// line fragment with its true substring: `getBoundingClientRect` on such a
/// range is a union box spanning both lines, which made the highlight cover
/// half a paragraph. The per-char split only runs for wrapped words, so the
/// common case stays one Range op per word.
pub const TEXT_RUNS_JS: &str = "(function(){\
var runs=[],walker=document.createTreeWalker(document.body,NodeFilter.SHOW_TEXT),n;\
var range=document.createRange(),re=/\\S+/g,m;\
var sx=window.scrollX||0,sy=window.scrollY||0;\
function push(x,y,w,h,t,c){runs.push({x:x+sx,y:y+sy,w:w,h:h,t:t,c:c});}\
while((n=walker.nextNode())&&runs.length<6000){\
var s=n.textContent;if(!s||!s.trim())continue;\
re.lastIndex=0;\
while((m=re.exec(s))&&runs.length<6000){\
try{\
range.setStart(n,m.index);range.setEnd(n,m.index+m[0].length);\
var rs=range.getClientRects();\
if(rs.length<2||m[0].length>1000){\
var r=rs.length===1?rs[0]:range.getBoundingClientRect();\
if(r.width>0&&r.height>0)push(r.left,r.top,r.width,r.height,m[0],0);\
}else{\
var f=null,t='',fi=0;\
for(var k=0;k<m[0].length;k++){\
range.setStart(n,m.index+k);range.setEnd(n,m.index+k+1);\
var c=range.getBoundingClientRect();\
if(c.width<=0||c.height<=0)continue;\
if(f&&Math.abs(c.top-f.y)<f.h*0.5){\
if(c.left<f.x)f.x=c.left;\
if(c.right>f.r)f.r=c.right;\
if(c.bottom>f.b)f.b=c.bottom;\
t+=m[0][k];\
}else{\
if(f&&t)push(f.x,f.y,f.r-f.x,f.b-f.y,t,fi++?1:0);\
f={x:c.left,y:c.top,r:c.right,b:c.bottom,h:c.height};t=m[0][k];\
}}\
if(f&&t)push(f.x,f.y,f.r-f.x,f.b-f.y,t,fi++?1:0);\
}\
}catch(e){}\
}}\
return runs;})()";
