//! Operating-system integration around the main window: its icon, raising
//! it from the tray or a toast, the tray's unread dot, native file dialogs,
//! and the stdout logger.

use super::*;

#[cfg(any(windows, target_os = "linux"))]
thread_local! {
    /// The tray handle — kept here so the new-mail path can flip the
    /// unread dot from engine-result handlers.
    pub(crate) static TRAY: RefCell<Option<tray::Tray>> = const { RefCell::new(None) };
    /// Startup ticker that retries `set_window_icon` until the native window
    /// exists; parked here so the icon-setter callback can stop and drop it.
    pub(crate) static ICON_TIMER: RefCell<Option<slint::Timer>> = const { RefCell::new(None) };
}

/// Build an `HICON` at `size`×`size` from the bundled icon PNG. Slint/winit
/// doesn't read the exe's embedded .ico for the window icon, so we set it
/// ourselves via WM_SETICON. Returns None if decoding or GDI allocation fails.
#[cfg(windows)]
pub(crate) fn hicon_from_png(size: u32) -> Option<windows::Win32::UI::WindowsAndMessaging::HICON> {
    use image::imageops::FilterType;
    use windows::Win32::Foundation::HANDLE;
    use windows::Win32::Graphics::Gdi::{
        BI_RGB, BITMAPINFO, BITMAPINFOHEADER, CreateBitmap, CreateDIBSection, DIB_RGB_COLORS,
        DeleteObject, HDC,
    };
    use windows::Win32::UI::WindowsAndMessaging::{CreateIconIndirect, ICONINFO};

    let rgba = image::load_from_memory(ICON_PNG)
        .ok()?
        .resize_exact(size, size, FilterType::Lanczos3)
        .to_rgba8();
    let (w, h) = (size as i32, size as i32);

    let bi = BITMAPINFO {
        bmiHeader: BITMAPINFOHEADER {
            biSize: core::mem::size_of::<BITMAPINFOHEADER>() as u32,
            biWidth: w,
            biHeight: -h, // negative → top-down
            biPlanes: 1,
            biBitCount: 32,
            biCompression: BI_RGB.0 as u32,
            ..Default::default()
        },
        ..Default::default()
    };
    let mut bits: *mut core::ffi::c_void = core::ptr::null_mut();
    let hbm_color = unsafe {
        CreateDIBSection(
            HDC(core::ptr::null_mut()),
            &bi,
            DIB_RGB_COLORS,
            &mut bits,
            HANDLE(core::ptr::null_mut()),
            0,
        )
        .ok()?
    };
    if bits.is_null() {
        unsafe {
            let _ = DeleteObject(hbm_color);
        }
        return None;
    }
    // RGBA → BGRA (what a 32bpp DIB expects).
    unsafe {
        let dst = bits as *mut u8;
        for (i, px) in rgba.pixels().enumerate() {
            let o = i * 4;
            *dst.add(o) = px[2];
            *dst.add(o + 1) = px[1];
            *dst.add(o + 2) = px[0];
            *dst.add(o + 3) = px[3];
        }
    }
    let hbm_mask = unsafe { CreateBitmap(w, h, 1, 1, None) };
    let mut ii = ICONINFO {
        fIcon: true.into(),
        xHotspot: 0,
        yHotspot: 0,
        hbmMask: hbm_mask,
        hbmColor: hbm_color,
    };
    let hicon = unsafe { CreateIconIndirect(&mut ii).ok() };
    unsafe {
        let _ = DeleteObject(hbm_color);
        let _ = DeleteObject(hbm_mask);
    }
    hicon
}

/// Set the window's title-bar + taskbar icon (Windows). Must run after the
/// native window exists (call from a single-shot timer once the loop starts).
/// The leaked HICONs live for the app's lifetime — the shell keeps referencing
/// them, and there's exactly one window, so this is a bounded, one-time cost.
#[cfg(windows)]
pub(crate) fn set_window_icon(ui: &MainWindow) -> bool {
    use raw_window_handle::{HasWindowHandle, RawWindowHandle};
    use windows::Win32::Foundation::{HWND, LPARAM, WPARAM};
    use windows::Win32::UI::WindowsAndMessaging::{SendMessageW, WM_SETICON};
    const ICON_SMALL: usize = 0;
    const ICON_BIG: usize = 1;

    let handle = ui.window().window_handle();
    let Ok(wh) = handle.window_handle() else { return false };
    let RawWindowHandle::Win32(h) = wh.as_raw() else { return false };
    let hwnd = HWND(h.hwnd.get() as *mut core::ffi::c_void);
    for (size, which) in [(32u32, ICON_BIG), (16u32, ICON_SMALL)] {
        if let Some(hicon) = hicon_from_png(size) {
            unsafe {
                SendMessageW(hwnd, WM_SETICON, WPARAM(which), LPARAM(hicon.0 as isize));
            }
        }
    }
    true
}

/// Set the window's title-bar + taskbar icon (Linux/X11) — the analogue of
/// the Windows WM_SETICON path above. Writes `_NET_WM_ICON` (EWMH) on the
/// Xlib window from a throwaway display connection, so we never race the
/// winit/Slint event loop's own Xlib connection (same pattern as
/// `activate_window_x11`). Must run after the native window exists. On a
/// Wayland session there is no per-window icon protocol — the icon comes
/// from the .desktop entry instead (see installer/install_linux.sh) — and
/// the non-Xlib handle makes this a silent no-op.
#[cfg(target_os = "linux")]
pub(crate) fn set_window_icon(ui: &MainWindow) -> bool {
    use image::imageops::FilterType;
    use raw_window_handle::{HasWindowHandle, RawWindowHandle};

    let handle = ui.window().window_handle();
    // No native window yet — tell the startup ticker to try again.
    let Ok(wh) = handle.window_handle() else { return false };
    let RawWindowHandle::Xlib(h) = wh.as_raw() else {
        // Wayland (or anything non-Xlib): no per-window icon protocol; the
        // icon comes from the .desktop entry. Nothing to retry.
        return true;
    };
    let window = h.window;

    // A decode failure is permanent — report done so the ticker stops.
    let Ok(img) = image::load_from_memory(ICON_PNG) else { return true };
    // _NET_WM_ICON payload: [w, h, w*h ARGB pixels] per size, each element a
    // CARDINAL carried in a c_ulong (that's how format-32 properties travel
    // through Xlib on 64-bit). The WM picks the closest size itself.
    let mut data: Vec<std::os::raw::c_ulong> = Vec::new();
    for size in [16u32, 32, 48, 64, 128] {
        let rgba = img.resize_exact(size, size, FilterType::Lanczos3).to_rgba8();
        data.push(size as std::os::raw::c_ulong);
        data.push(size as std::os::raw::c_ulong);
        for px in rgba.pixels() {
            let [r, g, b, a] = px.0;
            let argb = ((a as u32) << 24) | ((r as u32) << 16) | ((g as u32) << 8) | (b as u32);
            data.push(argb as std::os::raw::c_ulong);
        }
    }

    use std::ptr;
    use x11::xlib;
    unsafe {
        let dpy = xlib::XOpenDisplay(ptr::null());
        if dpy.is_null() {
            return true;
        }
        let net_wm_icon = xlib::XInternAtom(dpy, c"_NET_WM_ICON".as_ptr(), xlib::False);
        xlib::XChangeProperty(
            dpy,
            window,
            net_wm_icon,
            xlib::XA_CARDINAL,
            32,
            xlib::PropModeReplace,
            data.as_ptr() as *const u8,
            data.len() as i32,
        );
        xlib::XFlush(dpy);
        xlib::XCloseDisplay(dpy);
    }
    true
}

/// Show + un-minimize + bring to foreground. Plain `ui.show()` is a no-op
/// for a window that is minimized or buried under others — tray and toast
/// clicks must actually surface it. SetForegroundWindow is allowed to
/// succeed here because the click that got us called counts as user input.
pub(crate) fn raise_window(ui: &MainWindow) {
    let _ = ui.show();
    #[cfg(windows)]
    {
        use raw_window_handle::{HasWindowHandle, RawWindowHandle};
        use windows::Win32::UI::WindowsAndMessaging::{
            IsIconic, SW_RESTORE, SetForegroundWindow, ShowWindow,
        };
        let handle = ui.window().window_handle();
        if let Ok(wh) = handle.window_handle() {
            if let RawWindowHandle::Win32(h) = wh.as_raw() {
                let hwnd = windows::Win32::Foundation::HWND(h.hwnd.get() as *mut core::ffi::c_void);
                unsafe {
                    if IsIconic(hwnd).as_bool() {
                        let _ = ShowWindow(hwnd, SW_RESTORE);
                    }
                    let _ = SetForegroundWindow(hwnd);
                }
            }
        }
    }
    #[cfg(target_os = "linux")]
    {
        use raw_window_handle::{HasWindowHandle, RawWindowHandle};
        let handle = ui.window().window_handle();
        if let Ok(wh) = handle.window_handle() {
            if let RawWindowHandle::Xlib(h) = wh.as_raw() {
                activate_window_x11(h.window);
            }
        }
    }
}

/// Raise + focus an X11 window the WM-friendly way: send `_NET_ACTIVE_WINDOW`
/// (EWMH) to the root window. Uses a throwaway display connection so we never
/// race the winit/Slint event loop's own Xlib connection. Best-effort — failures
/// (e.g. a Wayland session or a WM ignoring the hint) are silently no-ops.
#[cfg(target_os = "linux")]
pub(crate) fn activate_window_x11(window: std::os::raw::c_ulong) {
    use std::ptr;
    use x11::xlib;
    unsafe {
        let dpy = xlib::XOpenDisplay(ptr::null());
        if dpy.is_null() {
            return;
        }
        let net_active = xlib::XInternAtom(dpy, c"_NET_ACTIVE_WINDOW".as_ptr(), xlib::False);
        let root = xlib::XDefaultRootWindow(dpy);

        let mut data = xlib::ClientMessageData::new();
        // Source indication 2 (pager): the activation is a direct user action
        // via the tray, so the WM should honor it. Source 1 (application) is
        // subject to focus-stealing prevention — KWin would only flag the
        // window as "demands attention" instead of raising + focusing it.
        data.set_long(0, 2);
        data.set_long(1, xlib::CurrentTime as std::os::raw::c_long);
        data.set_long(2, 0); // no "requestor's currently active window"

        let mut ev = xlib::XEvent {
            client_message: xlib::XClientMessageEvent {
                type_: xlib::ClientMessage,
                serial: 0,
                send_event: xlib::True,
                display: dpy,
                window,
                message_type: net_active,
                format: 32,
                data,
            },
        };
        xlib::XSendEvent(
            dpy,
            root,
            xlib::False,
            xlib::SubstructureRedirectMask | xlib::SubstructureNotifyMask,
            &mut ev,
        );
        xlib::XRaiseWindow(dpy, window);
        xlib::XFlush(dpy);
        xlib::XCloseDisplay(dpy);
    }
}

/// Flip the tray unread dot (no-op where there's no tray backend).
pub(crate) fn tray_set_dot(on: bool) {
    #[cfg(any(windows, target_os = "linux"))]
    TRAY.with(|t| {
        if let Some(tr) = t.borrow().as_ref() {
            tr.set_unread_dot(on);
        }
    });
    #[cfg(not(any(windows, target_os = "linux")))]
    let _ = on;
}

/// Синхронизировать unread-точку трея с фактом: точка ⇔ есть хоть один
/// диалог с непрочитанным. Вызывается везде, где unread_count меняется
/// (открытие диалога, дельты движка) и один раз после создания трея —
/// чтобы письма, пришедшие пока клиент был выключен, зажигали точку.
pub(crate) fn tray_sync_dot(sh: &Shared) {
    tray_set_dot(sh.convs.borrow().iter().any(|c| c.unread_count > 0));
}

/// Minimal stdout logger: ddmail-core (NativeProvider, engine) reports
/// through the `log` crate, and without an installed logger those records
/// vanish — the WebSocket watcher's connect/refresh diagnostics were
/// invisible exactly when they were needed.
pub(crate) struct StdoutLogger;

impl log::Log for StdoutLogger {
    fn enabled(&self, m: &log::Metadata) -> bool {
        m.level() <= log::Level::Info
    }
    fn log(&self, r: &log::Record) {
        if self.enabled(r.metadata()) {
            println!("[{}] {}", r.level(), r.args());
        }
    }
    fn flush(&self) {}
}

pub(crate) static STDOUT_LOGGER: StdoutLogger = StdoutLogger;

/// Open the native file picker and return the chosen attachment paths.
///
/// Windows uses rfd's synchronous Win32 backend, parented to our window. Linux
/// does not get rfd at all: its only sync backend there is gtk3, and this build
/// deliberately links no gtk — that went out with the WebKitGTK renderer. So on
/// Linux we shell out to a native picker (kdialog on KDE, zenity as fallback)
/// in its own process, which also keeps the UI thread free of a second event
/// loop.
#[cfg(not(target_os = "linux"))]
pub(crate) fn pick_attachment_files(ui: &MainWindow) -> Vec<std::path::PathBuf> {
    let handle = ui.window().window_handle();
    rfd::FileDialog::new().set_parent(&handle).pick_files().unwrap_or_default()
}

#[cfg(target_os = "linux")]
pub(crate) fn pick_attachment_files(ui: &MainWindow) -> Vec<std::path::PathBuf> {
    use raw_window_handle::{HasWindowHandle, RawWindowHandle};
    // X11 window id lets the picker open transient-for our window (modal,
    // centered, above). Wayland has no portable id here — the picker just
    // opens unparented, which is fine.
    let xid = ui.window().window_handle().window_handle().ok().and_then(|wh| match wh.as_raw() {
        RawWindowHandle::Xlib(h) => Some(h.window),
        _ => None,
    });

    // kdialog: native on KDE. `--separate-output --multiple` → one path per
    // line, so filenames with spaces parse cleanly.
    if let Ok(kdialog) = which_bin("kdialog") {
        let mut c = std::process::Command::new(kdialog);
        if let Some(xid) = xid {
            c.arg("--attach").arg(xid.to_string());
        }
        c.args(["--getopenfilename", "", "--multiple", "--separate-output"]);
        if let Ok(out) = c.output() {
            if out.status.success() {
                return String::from_utf8_lossy(&out.stdout)
                    .lines()
                    .map(|l| l.trim())
                    .filter(|l| !l.is_empty())
                    .map(std::path::PathBuf::from)
                    .collect();
            }
            return Vec::new(); // user cancelled → non-zero exit, no paths
        }
    }
    // zenity fallback: GTK-themed but works everywhere. Newline separator so
    // multi-select parses even with spaces in names.
    if let Ok(zenity) = which_bin("zenity") {
        if let Ok(out) = std::process::Command::new(zenity)
            .args(["--file-selection", "--multiple", "--separator=\n"])
            .output()
        {
            if out.status.success() {
                return String::from_utf8_lossy(&out.stdout)
                    .lines()
                    .map(|l| l.trim())
                    .filter(|l| !l.is_empty())
                    .map(std::path::PathBuf::from)
                    .collect();
            }
        }
    }
    eprintln!("attach: no native file picker found (install kdialog or zenity)");
    Vec::new()
}

/// Диалог «Сохранить как…» для вложения (None = отмена). Разделение по ОС —
/// то же, что у pick_attachment_files: rfd на Windows; на Linux
/// kdialog/zenity отдельным процессом (gtk в сборке нет вовсе).
#[cfg(not(target_os = "linux"))]
pub(crate) fn pick_save_path(ui: &MainWindow, filename: &str) -> Option<std::path::PathBuf> {
    let handle = ui.window().window_handle();
    rfd::FileDialog::new().set_parent(&handle).set_file_name(filename).save_file()
}

#[cfg(target_os = "linux")]
pub(crate) fn pick_save_path(ui: &MainWindow, filename: &str) -> Option<std::path::PathBuf> {
    use raw_window_handle::{HasWindowHandle, RawWindowHandle};
    let xid = ui.window().window_handle().window_handle().ok().and_then(|wh| match wh.as_raw() {
        RawWindowHandle::Xlib(h) => Some(h.window),
        _ => None,
    });
    if let Ok(kdialog) = which_bin("kdialog") {
        let mut c = std::process::Command::new(kdialog);
        if let Some(xid) = xid {
            c.arg("--attach").arg(xid.to_string());
        }
        c.args(["--getsavefilename", filename]);
        if let Ok(out) = c.output() {
            if out.status.success() {
                let s = String::from_utf8_lossy(&out.stdout).trim().to_string();
                if !s.is_empty() {
                    return Some(std::path::PathBuf::from(s));
                }
            }
            return None; // отмена
        }
    }
    if let Ok(zenity) = which_bin("zenity") {
        if let Ok(out) = std::process::Command::new(zenity)
            .args(["--file-selection", "--save", &format!("--filename={filename}")])
            .output()
        {
            if out.status.success() {
                let s = String::from_utf8_lossy(&out.stdout).trim().to_string();
                if !s.is_empty() {
                    return Some(std::path::PathBuf::from(s));
                }
            }
        }
    }
    eprintln!("save: no native file picker found (install kdialog or zenity)");
    None
}
