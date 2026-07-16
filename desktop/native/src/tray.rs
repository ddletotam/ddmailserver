//! System-tray icon.
//!
//! Windows: tray-icon, created on the UI thread before `ui.run()`; tray + menu
//! events are polled from a Slint timer so we never touch a second event loop.
//!
//! Linux: ksni (StatusNotifierItem), which runs its own D-Bus service on a
//! background thread and never touches gtk — so, unlike tray-icon, it can't
//! fight the WebKitGTK render worker's gtk main loop. Its menu/activate
//! callbacks fire on the ksni thread, so the closures handed in from main.rs
//! marshal UI work back to the Slint event loop themselves.
//!
//! macOS tray is still deferred.
//!
//! Both backends expose the same surface: `setup(on_open, on_quit) -> Option<Tray>`
//! and `Tray::set_unread_dot(bool)`.

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

/// 32×32 solid emerald (#10b981) RGBA icon.
#[cfg(windows)]
fn solid_icon_rgba() -> Vec<u8> {
    let mut v = Vec::with_capacity(32 * 32 * 4);
    for _ in 0..32 * 32 {
        v.extend_from_slice(&[0x10, 0xb9, 0x81, 0xff]);
    }
    v
}

/// Solid icon + unread dot bottom-right: a white disc (so the badge reads
/// against the emerald base) with a darker-emerald core — «изумрудный кружочек».
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
                // Darker-emerald core.
                v[px..px + 4].copy_from_slice(&[0x04, 0x78, 0x57, 0xff]);
            } else if d2 <= 7.0 * 7.0 {
                // White ring around it.
                v[px..px + 4].copy_from_slice(&[0xff, 0xff, 0xff, 0xff]);
            }
        }
    }
    v
}

// ─────────────────────────── Linux (ksni / StatusNotifierItem) ──────────────

#[cfg(target_os = "linux")]
pub struct Tray {
    handle: ksni::Handle<DdmailTray>,
}

#[cfg(target_os = "linux")]
impl Tray {
    /// Toggle the unread dot. Idempotent. Safe to call from any thread — ksni
    /// applies the update on its own service thread and redraws the icon.
    pub fn set_unread_dot(&self, on: bool) {
        self.handle.update(|t: &mut DdmailTray| t.unread = on);
    }
}

#[cfg(target_os = "linux")]
struct DdmailTray {
    unread: bool,
    on_open: Box<dyn Fn() + Send>,
    on_quit: Box<dyn Fn() + Send>,
}

#[cfg(target_os = "linux")]
impl ksni::Tray for DdmailTray {
    fn id(&self) -> String {
        "ddmail".into()
    }

    fn title(&self) -> String {
        "ddmail".into()
    }

    fn icon_pixmap(&self) -> Vec<ksni::Icon> {
        vec![ksni::Icon {
            width: 32,
            height: 32,
            data: icon_argb(self.unread),
        }]
    }

    // Left-click on the tray icon.
    fn activate(&mut self, _x: i32, _y: i32) {
        (self.on_open)();
    }

    fn menu(&self) -> Vec<ksni::MenuItem<Self>> {
        use ksni::menu::{MenuItem, StandardItem};
        vec![
            StandardItem {
                label: "Открыть ddmail".into(),
                activate: Box::new(|t: &mut DdmailTray| (t.on_open)()),
                ..Default::default()
            }
            .into(),
            MenuItem::Separator,
            StandardItem {
                label: "Выход".into(),
                activate: Box::new(|t: &mut DdmailTray| (t.on_quit)()),
                ..Default::default()
            }
            .into(),
        ]
    }
}

/// Build the tray. `on_open` fires on left-click or the "Открыть" item;
/// `on_quit` on the "Выход" item. Both run on the ksni service thread, so the
/// closures must be `Send` and marshal any UI work back to the Slint loop.
#[cfg(target_os = "linux")]
pub fn setup(
    on_open: impl Fn() + Send + 'static,
    on_quit: impl Fn() + Send + 'static,
) -> Option<Tray> {
    let service = ksni::TrayService::new(DdmailTray {
        unread: false,
        on_open: Box::new(on_open),
        on_quit: Box::new(on_quit),
    });
    let handle = service.handle();
    service.spawn();
    Some(Tray { handle })
}

/// 32×32 emerald (#10b981) icon in ksni's ARGB32 byte order ([A,R,G,B] per
/// pixel — network byte order, unlike the Windows RGBA above). With `unread`,
/// the same bottom-right badge: white ring around a darker-emerald core.
#[cfg(target_os = "linux")]
fn icon_argb(unread: bool) -> Vec<u8> {
    let mut v = Vec::with_capacity(32 * 32 * 4);
    for _ in 0..32 * 32 {
        v.extend_from_slice(&[0xff, 0x10, 0xb9, 0x81]);
    }
    if unread {
        let (cx, cy) = (24.0f32, 24.0f32);
        for y in 0..32 {
            for x in 0..32 {
                let dx = x as f32 - cx;
                let dy = y as f32 - cy;
                let d2 = dx * dx + dy * dy;
                let px = (y * 32 + x) * 4;
                if d2 <= 4.5 * 4.5 {
                    v[px..px + 4].copy_from_slice(&[0xff, 0x04, 0x78, 0x57]);
                } else if d2 <= 7.0 * 7.0 {
                    v[px..px + 4].copy_from_slice(&[0xff, 0xff, 0xff, 0xff]);
                }
            }
        }
    }
    v
}
