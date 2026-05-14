//! Linux tray via direct StatusNotifierItem (the `ksni` crate). Tauri's
//! bundled tray-icon backend uses libappindicator on Linux, which has the
//! "any click opens the menu" limitation built in. Apps like Telegram /
//! Discord avoid it by speaking SNI directly (via Qt's KStatusNotifierItem);
//! we do the same in Rust with `ksni`.
//!
//! Click semantics — what KDE Plasma / GNOME (with the AppIndicator
//! extension) deliver:
//!   * `Activate` (D-Bus signal, primary click) → toggle main window.
//!   * `SecondaryActivate` (middle click) → same toggle, for parity.
//!   * `ContextMenu` (right click) → show our menu.
//!
//! The tray icon is generated at startup from the app's full-colour window
//! icon, flattened to a white silhouette to match the convention of every
//! other monochrome tray icon on KDE/GNOME panels. When the unread-count
//! pushed from JS is non-zero, an overlay dot is composited on top.

use std::sync::OnceLock;
use std::sync::{Arc, Mutex};

use tauri::{AppHandle, Manager, Runtime};

/// ksni handle stored globally so the `set_unread` Tauri command can push
/// new counts into the tray thread. Locked once when the tray is created
/// and read-only after.
static TRAY_HANDLE: OnceLock<Mutex<Option<ksni::Handle<TrayState>>>> = OnceLock::new();

/// State for the StatusNotifierItem. Non-generic so ksni's spawn signature
/// works without leaking the Tauri runtime type — callbacks are stored as
/// boxed closures that capture the AppHandle.
struct TrayState {
    base_argb: Vec<u8>, // pre-rendered white silhouette in ARGB32
    width: i32,
    height: i32,
    unread: u32,
    toggle: Box<dyn Fn() + Send + Sync>,
    quit: Box<dyn Fn() + Send + Sync>,
}

impl ksni::Tray for TrayState {
    fn id(&self) -> String {
        "ru.letotam.ddmail".into()
    }

    fn title(&self) -> String {
        "DDMail".into()
    }

    fn icon_pixmap(&self) -> Vec<ksni::Icon> {
        let mut data = self.base_argb.clone();
        if self.unread > 0 {
            overlay_unread_dot(&mut data, self.width, self.height);
        }
        vec![ksni::Icon { width: self.width, height: self.height, data }]
    }

    fn activate(&mut self, _x: i32, _y: i32) {
        (self.toggle)();
    }

    fn secondary_activate(&mut self, _x: i32, _y: i32) {
        (self.toggle)();
    }

    fn menu(&self) -> Vec<ksni::MenuItem<Self>> {
        use ksni::menu::{MenuItem, StandardItem};
        vec![
            StandardItem {
                label: "Show / Hide DDMail".into(),
                activate: Box::new(|this: &mut Self| (this.toggle)()),
                ..Default::default()
            }
            .into(),
            MenuItem::Separator,
            StandardItem {
                label: "Quit".into(),
                activate: Box::new(|this: &mut Self| (this.quit)()),
                ..Default::default()
            }
            .into(),
        ]
    }
}

pub(super) fn create<R: Runtime>(app: &AppHandle<R>) -> Result<(), Box<dyn std::error::Error>> {
    let (width, height, base_argb) = build_silhouette(app)
        .ok_or("could not build tray silhouette — default_window_icon missing")?;

    let toggle = {
        let app = app.clone();
        Box::new(move || {
            // GTK calls must run on the main thread; ksni callbacks fire on
            // its own D-Bus thread. Clone once for the dispatch target and
            // once for the closure to side-step the receiver-vs-move-arg
            // borrow conflict.
            let app_for_call = app.clone();
            let _ = app.run_on_main_thread(move || {
                if let Some(window) = app_for_call.get_webview_window("main") {
                    let visible = window.is_visible().unwrap_or(false);
                    let focused = window.is_focused().unwrap_or(false);
                    if visible && focused {
                        window.hide().ok();
                    } else {
                        window.show().ok();
                        window.unminimize().ok();
                        window.set_focus().ok();
                    }
                }
            });
        }) as Box<dyn Fn() + Send + Sync>
    };
    let quit = {
        let app = app.clone();
        Box::new(move || {
            let app_for_call = app.clone();
            let _ = app.run_on_main_thread(move || app_for_call.exit(0));
        }) as Box<dyn Fn() + Send + Sync>
    };

    let state = TrayState {
        base_argb,
        width,
        height,
        unread: 0,
        toggle,
        quit,
    };
    let service = ksni::TrayService::new(state);
    let handle = service.handle();
    service.spawn();

    // Store the handle so the v2_set_tray_unread Tauri command can push
    // new counts in.
    TRAY_HANDLE
        .get_or_init(|| Mutex::new(None))
        .lock()
        .unwrap()
        .replace(handle);

    // Keep an Arc anchor on the heap to make extra sure nothing
    // optimises the thread away in release builds.
    static ANCHOR: OnceLock<Arc<()>> = OnceLock::new();
    ANCHOR.set(Arc::new(())).ok();

    Ok(())
}

/// Externally visible setter for the unread count. Other tray
/// implementations (macOS / Windows) will need their own implementation —
/// this one is Linux-specific.
pub fn set_unread(count: u32) {
    let Some(slot) = TRAY_HANDLE.get() else { return };
    let Ok(guard) = slot.lock() else { return };
    let Some(handle) = guard.as_ref() else { return };
    let handle = handle.clone();
    handle.update(move |t: &mut TrayState| {
        t.unread = count;
    });
}

/// Turn the app's full-colour window icon into a white silhouette by
/// finding the background colour at the corners and mapping each pixel's
/// distance from that colour to its silhouette opacity. Result is ARGB32
/// in network byte order, ready for ksni.
fn build_silhouette<R: Runtime>(app: &AppHandle<R>) -> Option<(i32, i32, Vec<u8>)> {
    let image = app.default_window_icon()?;
    let w = image.width() as i32;
    let h = image.height() as i32;
    let rgba = image.rgba();
    let stride = (w as usize) * 4;
    let pix = |x: usize, y: usize| -> (u8, u8, u8) {
        let i = y * stride + x * 4;
        (rgba[i], rgba[i + 1], rgba[i + 2])
    };
    let last_x = (w as usize).saturating_sub(1);
    let last_y = (h as usize).saturating_sub(1);
    let (mut br, mut bg, mut bb) = (0u32, 0u32, 0u32);
    for (x, y) in [(0, 0), (last_x, 0), (0, last_y), (last_x, last_y)] {
        let (r, g, b) = pix(x, y);
        br += r as u32;
        bg += g as u32;
        bb += b as u32;
    }
    let (br, bg, bb) = ((br / 4) as i32, (bg / 4) as i32, (bb / 4) as i32);
    // Two thresholds: dist ≤ DEAD → transparent (kill the gradient halo);
    // dist ≥ FULL → fully opaque (the actual glyph). Distances are
    // squared so we skip the sqrt.
    const DEAD_SQ: i32 = 32 * 32;
    const FULL_SQ: i32 = 96 * 96;
    let mut argb = Vec::with_capacity(rgba.len());
    for px in rgba.chunks_exact(4) {
        let dr = px[0] as i32 - br;
        let dg = px[1] as i32 - bg;
        let db = px[2] as i32 - bb;
        let dist_sq = dr * dr + dg * dg + db * db;
        let alpha = if dist_sq <= DEAD_SQ {
            0
        } else if dist_sq >= FULL_SQ {
            255
        } else {
            (((dist_sq - DEAD_SQ) * 255) / (FULL_SQ - DEAD_SQ)) as u8
        };
        let alpha = ((alpha as u16 * px[3] as u16) / 255) as u8;
        argb.push(alpha);
        argb.push(0xFF);
        argb.push(0xFF);
        argb.push(0xFF);
    }
    Some((w, h, argb))
}

/// Composite a solid blue dot onto an existing ARGB32 buffer, bottom-right
/// quadrant. The dot is sized at ~28% of the smaller side, which scales
/// gracefully across the 22-48 px Plasma/GNOME tray sizes.
fn overlay_unread_dot(pixels: &mut [u8], w: i32, h: i32) {
    // Mattermost / Discord use roughly the same medium-blue; stay in that
    // bucket so the dot reads as "notification" out of the box.
    const DOT_RGB: [u8; 3] = [0x3B, 0x82, 0xF6]; // #3B82F6
    let side = w.min(h);
    let radius = (side as f32 * 0.28) as i32;
    if radius <= 0 {
        return;
    }
    // Anchor the dot to the bottom-right with a small inset so it doesn't
    // get clipped by the panel's icon-bounds clamping.
    let inset = radius / 4;
    let cx = w - radius - inset;
    let cy = h - radius - inset;
    let r2 = radius * radius;
    // 2-px feather for anti-aliased edge.
    let inner_r = (radius - 2).max(0);
    let inner_r2 = inner_r * inner_r;

    let stride = (w as usize) * 4;
    for y in (cy - radius).max(0)..(cy + radius + 1).min(h) {
        let dy = y - cy;
        for x in (cx - radius).max(0)..(cx + radius + 1).min(w) {
            let dx = x - cx;
            let dist_sq = dx * dx + dy * dy;
            if dist_sq > r2 {
                continue;
            }
            // Linear edge feather between inner_r and radius.
            let alpha: u16 = if dist_sq <= inner_r2 {
                255
            } else {
                let span = (r2 - inner_r2).max(1);
                ((255 * (r2 - dist_sq)) / span) as u16
            };
            let inv = 255 - alpha;
            let i = (y as usize) * stride + (x as usize) * 4;
            // ARGB packed (A, R, G, B). Source-over: out = src·α + dst·(1-α).
            let da = pixels[i] as u16;
            pixels[i] = ((alpha * 255 + da * inv) / 255).min(255) as u8;
            pixels[i + 1] = ((DOT_RGB[0] as u16 * alpha + pixels[i + 1] as u16 * inv) / 255) as u8;
            pixels[i + 2] = ((DOT_RGB[1] as u16 * alpha + pixels[i + 2] as u16 * inv) / 255) as u8;
            pixels[i + 3] = ((DOT_RGB[2] as u16 * alpha + pixels[i + 3] as u16 * inv) / 255) as u8;
        }
    }
}
