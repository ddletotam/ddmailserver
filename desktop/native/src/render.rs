//! Ultralight (WebKit) body renderer. Owns one Renderer; renders each message
//! body's HTML to an offscreen RGBA bitmap, cropped to its content height.
//! Lives entirely on the worker thread (Ultralight is single-threaded).

use std::ffi::CString;
use std::ptr::null_mut;

use ultralight::sys::{
    ulCreateString, ulDestroyString, ulViewLoadHTML, JSContextRef, JSEvaluateScript,
    JSStringCreateWithUTF8CString, JSStringGetMaximumUTF8CStringSize, JSStringGetUTF8CString,
    JSStringRelease, JSValueToStringCopy, ULView,
};
use ultralight::{Config, Renderer, View, ViewConfig};

/// Evaluate a JS expression in `ctx` and return its result coerced to a String.
fn eval_string(ctx: JSContextRef, script: &str) -> String {
    let Ok(c) = CString::new(script) else { return String::new() };
    unsafe {
        let js = JSStringCreateWithUTF8CString(c.as_ptr());
        let val = JSEvaluateScript(ctx, js, null_mut(), null_mut(), 0, null_mut());
        JSStringRelease(js);
        if val.is_null() {
            return String::new();
        }
        let sref = JSValueToStringCopy(ctx, val, null_mut());
        if sref.is_null() {
            return String::new();
        }
        let max = JSStringGetMaximumUTF8CStringSize(sref);
        let mut buf = vec![0u8; max];
        let n = JSStringGetUTF8CString(sref, buf.as_mut_ptr() as *mut _, max);
        JSStringRelease(sref);
        let len = n.saturating_sub(1); // drop NUL terminator
        String::from_utf8_lossy(&buf[..len]).into_owned()
    }
}

const MAX_H: u32 = 6000; // per-bubble render canvas height cap

pub struct Bitmap {
    pub rgba: Vec<u8>,
    pub width: u32,
    pub height: u32,
}

pub struct Engine {
    renderer: Renderer,
    // Keep the current conversation's views alive (for future hit-testing).
    views: Vec<View>,
}

impl Engine {
    pub fn new() -> Self {
        let assets = format!("{}/assets", env!("CARGO_MANIFEST_DIR"));
        ultralight::init(assets, None);
        let mut config = Config::default();
        config.set_resource_path_prefix("resources/".to_owned());
        let renderer = Renderer::new(&config);
        Engine {
            renderer,
            views: Vec::new(),
        }
    }

    /// Render each HTML body to a bitmap cropped to its content height.
    pub fn render_bodies(&mut self, htmls: &[String], width: u32) -> Vec<Bitmap> {
        self.views.clear();
        let mut out = Vec::with_capacity(htmls.len());

        for html in htmls {
            let view = self.renderer.create_view(width, MAX_H, &ViewConfig::default());

            let c = CString::new(html.as_str())
                .unwrap_or_else(|_| CString::new("<p>bad html</p>").unwrap());
            unsafe {
                let s = ulCreateString(c.as_ptr());
                let v: ULView = (&view).into();
                ulViewLoadHTML(v, s);
                ulDestroyString(s);
            }

            let t = std::time::Instant::now();
            loop {
                self.renderer.update();
                if view.is_ready() || t.elapsed().as_millis() > 3000 {
                    break;
                }
            }
            self.renderer.render();

            let (bw, bh) = view.bitmap_size();
            // Content height = bottom of the painted (dirty) region.
            let db = view.dirty_bounds(); // [left, right, top, bottom]
            let content_h = if db[3] > 0 && db[3] <= bh { db[3] } else { bh };

            // get_image() returns full bw×bh RGBA; keep the top content_h rows.
            let raw = view.get_image().into_raw();
            let row_bytes = (bw * 4) as usize;
            let keep = (content_h as usize) * row_bytes;
            let rgba = raw[..keep.min(raw.len())].to_vec();

            out.push(Bitmap {
                rgba,
                width: bw,
                height: content_h,
            });
            self.views.push(view);
        }
        out
    }

    /// Hit-test (x, y) (CSS px in the bubble) against the row's view via JS
    /// `elementFromPoint`; returns the enclosing `<a>` href if any.
    pub fn hit(&self, row: usize, x: f32, y: f32) -> Option<String> {
        let view = self.views.get(row)?;
        let ctx = view.lock_jscontext();
        let ctx_ref: JSContextRef = (&ctx).into();
        let script = format!(
            "(function(){{var e=document.elementFromPoint({x:.0},{y:.0});\
             var a=e&&e.closest?e.closest('a'):null;return (a&&a.href)?a.href:'';}})()"
        );
        let url = eval_string(ctx_ref, &script);
        drop(ctx);
        if url.is_empty() || url == "undefined" || url == "null" {
            None
        } else {
            Some(url)
        }
    }
}
