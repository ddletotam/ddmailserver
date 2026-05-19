//! Windows tray (Shell_NotifyIcon). Full-color icon, regular click semantics:
//! left-click activates the main window, right-click opens the context menu.
//! Tauri picks the right size automatically from the bundled .ico — both
//! 16×16 and 32×32 are present in the app's icon set.
//!
//! Unread badge: when JS pushes a non-zero count, we composite a blue dot
//! onto the bottom-right corner of the icon and swap it in via
//! `TrayIcon::set_icon`. On zero, the original full-color icon is
//! restored. The setter closure lives in a `OnceLock` so the cross-platform
//! `tray::set_unread` Tauri command can reach it without knowing the
//! Tauri runtime parameter.

use std::sync::Mutex;
use std::sync::OnceLock;

use tauri::{image::Image, tray::TrayIconBuilder, AppHandle, Runtime};

use super::common;

/// Boxed setter that owns the TrayIcon + cached original RGBA buffer.
/// Erases the runtime generic so the cross-platform `set_unread`
/// dispatcher can stay non-generic.
type SetUnreadFn = Box<dyn Fn(u32) + Send + Sync>;
static SET_UNREAD: OnceLock<Mutex<Option<SetUnreadFn>>> = OnceLock::new();

pub(super) fn create<R: Runtime>(app: &AppHandle<R>) -> Result<(), Box<dyn std::error::Error>> {
    let menu = common::build_menu(app)?;
    let icon = app
        .default_window_icon()
        .cloned()
        .ok_or("default window icon missing — set bundle.icon[] in tauri.conf.json")?;

    // Capture the original icon's pixel buffer so we can re-render with a
    // badge later. tauri::image::Image::rgba() returns a borrowed slice;
    // own it before the icon moves into the builder.
    let base_rgba: Vec<u8> = icon.rgba().to_vec();
    let base_w = icon.width();
    let base_h = icon.height();

    let tray = TrayIconBuilder::new()
        .icon(icon)
        .icon_as_template(false)
        .show_menu_on_left_click(false) // left = activate, right = menu (the Windows default)
        .menu(&menu)
        .tooltip("DDMail")
        .on_menu_event(|app, event| common::handle_menu_event(app, event.id.as_ref()))
        .on_tray_icon_event(|tray, event| {
            let app = tray.app_handle().clone();
            common::handle_left_click(&app, &event);
        })
        .build(app)?;

    // The closure owns the (cloneable, Arc-backed) TrayIcon along with
    // the original RGBA. Boxing it as a Fn(u32) erases R so the static
    // slot can be a single concrete type.
    let setter: SetUnreadFn = Box::new(move |count: u32| {
        let pixels = if count > 0 {
            let mut buf = base_rgba.clone();
            overlay_unread_dot_rgba(&mut buf, base_w as i32, base_h as i32);
            buf
        } else {
            base_rgba.clone()
        };
        let img = Image::new_owned(pixels, base_w, base_h);
        if let Err(e) = tray.set_icon(Some(img)) {
            log::warn!("[tray] set_icon failed: {e}");
        }
    });

    SET_UNREAD
        .get_or_init(|| Mutex::new(None))
        .lock()
        .unwrap()
        .replace(setter);

    Ok(())
}

/// Cross-platform tray dispatcher hook. Re-renders the icon with a blue
/// unread dot when `count > 0`, restores the original on zero.
pub fn set_unread(count: u32) {
    let Some(slot) = SET_UNREAD.get() else { return };
    let Ok(guard) = slot.lock() else { return };
    let Some(setter) = guard.as_ref() else { return };
    setter(count);
}

/// Composite a solid blue dot onto an existing RGBA8 buffer, bottom-right
/// quadrant. Same geometry/color as the Linux silhouette overlay but in
/// RGBA order to match `tauri::image::Image`. Sized at ~28% of the
/// smaller side so the dot stays legible at 16×16 and 32×32.
fn overlay_unread_dot_rgba(pixels: &mut [u8], w: i32, h: i32) {
    // Mattermost / Discord-blue; reads as "notification" at a glance.
    const DOT_RGB: [u8; 3] = [0x3B, 0x82, 0xF6];
    let side = w.min(h);
    let radius = (side as f32 * 0.28) as i32;
    if radius <= 0 {
        return;
    }
    let inset = radius / 4;
    let cx = w - radius - inset;
    let cy = h - radius - inset;
    let r2 = radius * radius;
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
            let alpha: u16 = if dist_sq <= inner_r2 {
                255
            } else {
                let span = (r2 - inner_r2).max(1);
                ((255 * (r2 - dist_sq)) / span) as u16
            };
            let inv = 255 - alpha;
            let i = (y as usize) * stride + (x as usize) * 4;
            // RGBA order; source-over blend: out = src·α + dst·(1-α).
            pixels[i] = ((DOT_RGB[0] as u16 * alpha + pixels[i] as u16 * inv) / 255) as u8;
            pixels[i + 1] = ((DOT_RGB[1] as u16 * alpha + pixels[i + 1] as u16 * inv) / 255) as u8;
            pixels[i + 2] = ((DOT_RGB[2] as u16 * alpha + pixels[i + 2] as u16 * inv) / 255) as u8;
            let da = pixels[i + 3] as u16;
            pixels[i + 3] = ((alpha * 255 + da * inv) / 255).min(255) as u8;
        }
    }
}
