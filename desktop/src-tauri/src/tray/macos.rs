//! macOS menu-bar tray. Apple HIG expects template (monochrome+alpha) icons
//! so the OS auto-tints them for light vs. dark menu bar; setting
//! `icon_as_template(true)` is the difference between a recognisable icon
//! and a stretched coloured blob.
//!
//! Both buttons open the menu, per macOS convention. There's no "single
//! left-click activates window" pattern — apps like Slack, Things, etc.
//! all open the menu on either click.

use tauri::{tray::TrayIconBuilder, AppHandle, Runtime};

use super::common;

pub(super) fn create<R: Runtime>(app: &AppHandle<R>) -> Result<(), Box<dyn std::error::Error>> {
    let menu = common::build_menu(app)?;
    let icon = app
        .default_window_icon()
        .cloned()
        .ok_or("default window icon missing — set bundle.icon[] in tauri.conf.json")?;

    let _tray = TrayIconBuilder::new()
        .icon(icon)
        .icon_as_template(true) // macOS menu-bar template icon
        .show_menu_on_left_click(true)
        .menu(&menu)
        .tooltip("DDMail")
        .on_menu_event(|app, event| common::handle_menu_event(app, event.id.as_ref()))
        .build(app)?;

    Ok(())
}
