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
