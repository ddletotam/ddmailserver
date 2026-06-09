//! System-tray icon (Windows). Created on the UI thread before `ui.run()`;
//! tray + menu events are polled from a Slint timer so we never touch a second
//! event loop. Linux/macOS trays are deferred (Linux needs gtk on the main
//! thread, which fights the render worker's gtk).

#[cfg(windows)]
pub struct Tray {
    _icon: tray_icon::TrayIcon,
    _timer: slint::Timer,
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
        _icon: tray,
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
