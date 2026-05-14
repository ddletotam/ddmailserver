//! Windows tray (Shell_NotifyIcon). Full-color icon, regular click semantics:
//! left-click activates the main window, right-click opens the context menu.
//! Tauri picks the right size automatically from the bundled .ico — both
//! 16×16 and 32×32 are present in the app's icon set.

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

    Ok(())
}
