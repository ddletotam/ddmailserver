//! Blitz helpers: render an HTML row to a fixed-size RGBA texture, and hit-test
//! a point against a row's HTML to find a clicked <a href> (links stay inline).

use std::sync::Arc;

use anyrender::render_to_buffer;
use anyrender_vello_cpu::VelloCpuImageRenderer;
use blitz_dom::{local_name, DocumentConfig};
use blitz_html::HtmlDocument;
use blitz_net::Provider;
use blitz_paint::paint_scene;
use blitz_traits::shell::{ColorScheme, Viewport};

pub struct Rendered {
    pub rgba: Vec<u8>,
    pub width: u32,
    pub height: u32,
}

pub fn render_html_fixed(html: &str, w_logical: u32, h_logical: u32, scale: f64) -> Rendered {
    let net = Arc::new(Provider::new(None));
    let rw = (w_logical as f64 * scale) as u32;
    let rh = (h_logical as f64 * scale) as u32;

    let mut document = HtmlDocument::from_html(
        html,
        DocumentConfig {
            net_provider: Some(Arc::clone(&net) as _),
            viewport: Some(Viewport::new(rw, rh, scale as f32, ColorScheme::Light)),
            ..Default::default()
        },
    );

    let mut tries = 0;
    loop {
        document.as_mut().resolve(0.0);
        if net.is_empty() || tries > 50 {
            break;
        }
        std::thread::sleep(std::time::Duration::from_millis(2));
        tries += 1;
    }

    let buffer = render_to_buffer::<VelloCpuImageRenderer, _>(
        |scene| {
            paint_scene(scene, document.as_mut(), scale, rw, rh, 0, 0);
        },
        rw,
        rh,
    );

    Rendered {
        rgba: buffer,
        width: rw,
        height: rh,
    }
}

/// Resolve `html` at `w_logical` (layout only) and return its content height in
/// CSS px. Used to size message bubbles before the texture is painted.
pub fn measure_height(html: &str, w_logical: u32) -> u32 {
    let net = Arc::new(Provider::new(None));
    let mut document = HtmlDocument::from_html(
        html,
        DocumentConfig {
            net_provider: Some(Arc::clone(&net) as _),
            viewport: Some(Viewport::new(w_logical, 12000, 1.0, ColorScheme::Light)),
            ..Default::default()
        },
    );
    document.as_mut().resolve(0.0);
    document
        .as_ref()
        .root_element()
        .final_layout
        .size
        .height
        .ceil()
        .max(1.0) as u32
}

/// Resolve the row's HTML (layout only, scale 1) and hit-test (x, y) in CSS px.
/// Returns the href of the <a> under the point, walking up the DOM.
pub fn hit_test_link(html: &str, w_logical: u32, h_logical: u32, x: f32, y: f32) -> Option<String> {
    let net = Arc::new(Provider::new(None));
    let mut document = HtmlDocument::from_html(
        html,
        DocumentConfig {
            net_provider: Some(Arc::clone(&net) as _),
            viewport: Some(Viewport::new(w_logical, h_logical, 1.0, ColorScheme::Light)),
            ..Default::default()
        },
    );
    document.as_mut().resolve(0.0);

    let doc = document.as_ref();
    let hit = doc.hit(x, y)?;
    let mut id = Some(hit.node_id);
    while let Some(nid) = id {
        let node = doc.get_node(nid)?;
        if let Some(el) = node.element_data() {
            if el.name.local == local_name!("a") {
                if let Some(href) = el.attr(local_name!("href")) {
                    return Some(href.to_string());
                }
            }
        }
        id = node.parent;
    }
    None
}
