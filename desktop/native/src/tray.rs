//! System-tray icon (Windows). Created on the UI thread before `ui.run()`;
//! tray + menu events are polled from a Slint timer so we never touch a second
//! event loop. Linux/macOS trays are deferred (Linux needs gtk on the main
//! thread, which fights the render worker's gtk).

#[cfg(windows)]
pub struct Tray {
    icon: tray_icon::TrayIcon,
    _timer: slint::Timer,
}

#[cfg(windows)]
impl Tray {
    /// Toggle the unread dot in the icon's bottom-right corner. Idempotent —
    /// callers don't need to track the current state.
    pub fn set_unread_dot(&self, on: bool) {
        use tray_icon::Icon;
        let rgba = if on { dotted_icon_rgba() } else { solid_icon_rgba() };
        if let Ok(icon) = Icon::from_rgba(rgba, 32, 32) {
            let _ = self.icon.set_icon(Some(icon));
        }
    }
}

/// Build the tray. `on_open` fires on left-click or the "Открыть" item;
/// `on_quit` on the "Выход" item. Both run on the UI thread (from the timer).
#[cfg(windows)]
pub fn setup(
    on_open: impl Fn() + 'static,
    on_quit: impl Fn() + 'static,
) -> Option<Tray> {
    use tray_icon::menu::{Menu, MenuEvent, MenuItem};
    use tray_icon::{Icon, TrayIconBuilder, TrayIconEvent};

    let menu = Menu::new();
    let open_item = MenuItem::new("Открыть ddmail", true, None);
    let quit_item = MenuItem::new("Выход", true, None);
    menu.append(&open_item).ok()?;
    menu.append(&quit_item).ok()?;
    let open_id = open_item.id().clone();
    let quit_id = quit_item.id().clone();

    let icon = Icon::from_rgba(solid_icon_rgba(), 32, 32).ok()?;
    let tray = TrayIconBuilder::new()
        .with_tooltip("ddmail")
        .with_menu(Box::new(menu))
        .with_icon(icon)
        .build()
        .ok()?;

    let timer = slint::Timer::default();
    timer.start(
        slint::TimerMode::Repeated,
        std::time::Duration::from_millis(300),
        move || {
            while let Ok(ev) = MenuEvent::receiver().try_recv() {
                if ev.id == open_id {
                    on_open();
                } else if ev.id == quit_id {
                    on_quit();
                }
            }
            while let Ok(ev) = TrayIconEvent::receiver().try_recv() {
                if let TrayIconEvent::Click { .. } = ev {
                    on_open();
                }
            }
        },
    );

    Some(Tray {
        icon: tray,
        _timer: timer,
    })
}

/// 32×32 solid brand-blue (#2f80ed) RGBA icon.
#[cfg(windows)]
fn solid_icon_rgba() -> Vec<u8> {
    let mut v = Vec::with_capacity(32 * 32 * 4);
    for _ in 0..32 * 32 {
        v.extend_from_slice(&[0x2f, 0x80, 0xed, 0xff]);
    }
    v
}

/// Solid icon + unread dot bottom-right: a white disc (so the badge reads
/// against the blue base) with a blue core — «синий кружочек».
#[cfg(windows)]
fn dotted_icon_rgba() -> Vec<u8> {
    let mut v = solid_icon_rgba();
    let (cx, cy) = (24.0f32, 24.0f32);
    for y in 0..32 {
        for x in 0..32 {
            let dx = x as f32 - cx;
            let dy = y as f32 - cy;
            let d2 = dx * dx + dy * dy;
            let px = (y * 32 + x) * 4;
            if d2 <= 4.5 * 4.5 {
                // Blue core.
                v[px..px + 4].copy_from_slice(&[0x15, 0x56, 0xc8, 0xff]);
            } else if d2 <= 7.0 * 7.0 {
                // White ring around it.
                v[px..px + 4].copy_from_slice(&[0xff, 0xff, 0xff, 0xff]);
            }
        }
    }
    v
}
