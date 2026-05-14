//! Pieces shared by every platform-specific tray implementation: the menu,
//! the click/menu event handlers, and the "show main window" helper. Each
//! `linux.rs` / `macos.rs` / `windows.rs` calls into these and adds the
//! platform-specific icon + click semantics on top.

use tauri::{
    menu::{Menu, MenuItem},
    tray::{MouseButton, MouseButtonState, TrayIconEvent},
    AppHandle, Manager, Runtime,
};

#[allow(dead_code)]
pub(super) fn build_menu<R: Runtime>(app: &AppHandle<R>) -> tauri::Result<Menu<R>> {
    // Single "Show/Hide" item that toggles the main window. Phrased as a
    // toggle rather than two separate Show + Hide entries because:
    //   * Linux/libayatana opens the menu on every click — there's no way
    //     to bind a different action to the icon itself, so this entry is
    //     effectively the primary action.
    //   * Updating the label live ("Show…" vs "Hide…") would need
    //     re-keying the menu on every visibility change; the static text
    //     is unambiguous enough.
    let toggle = MenuItem::with_id(app, "toggle", "Show / Hide DDMail", true, None::<&str>)?;
    let quit = MenuItem::with_id(app, "quit", "Quit", true, None::<&str>)?;
    Menu::with_items(app, &[&toggle, &quit])
}

#[allow(dead_code)]
pub(super) fn handle_menu_event<R: Runtime>(app: &AppHandle<R>, id: &str) {
    match id {
        "toggle" => toggle_main_window(app),
        "show" => show_main_window(app),
        "hide" => {
            if let Some(window) = app.get_webview_window("main") {
                window.hide().ok();
            }
        }
        "quit" => app.exit(0),
        _ => {}
    }
}

/// Left-click handler — toggles the main window. Hidden window comes back
/// to the foreground; visible & focused window is fully hidden (not
/// minimised — minimise leaves a taskbar entry the user already has via
/// the tray, so hide is the cleaner state).
///
/// Used by every platform that distinguishes left from right-click. macOS
/// ignores this and routes any click to the menu, per HIG.
#[allow(dead_code)]
pub(super) fn handle_left_click<R: Runtime>(app: &AppHandle<R>, event: &TrayIconEvent) {
    if let TrayIconEvent::Click {
        button: MouseButton::Left,
        button_state: MouseButtonState::Up,
        ..
    } = event
    {
        toggle_main_window(app);
    }
}

#[allow(dead_code)]
pub(super) fn show_main_window<R: Runtime>(app: &AppHandle<R>) {
    if let Some(window) = app.get_webview_window("main") {
        window.show().ok();
        window.unminimize().ok();
        window.set_focus().ok();
    }
}

#[allow(dead_code)]
pub(super) fn toggle_main_window<R: Runtime>(app: &AppHandle<R>) {
    let Some(window) = app.get_webview_window("main") else { return };
    // "Visible AND focused" is the only state we treat as "user actively
    // sees it". A background-but-visible window (covered by other windows
    // on the same workspace) should still come to the front instead of
    // disappearing — otherwise a single mis-aimed click eats the only
    // way to find the window again.
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
