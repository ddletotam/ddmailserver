//! Ultralight (WebKit) body renderer. Owns one Renderer; renders each message
//! body's HTML to an offscreen RGBA bitmap, cropped to its content height.
//! Lives entirely on the worker thread (Ultralight is single-threaded).

use std::ffi::CString;

use ultralight::sys::{ulCreateString, ulDestroyString, ulViewLoadHTML, ULView};
use ultralight::{Config, Renderer, View, ViewConfig};

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
}
