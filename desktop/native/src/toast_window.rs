//! Calendar reminder toasts as their own frameless, always-on-top windows.
//!
//! Spec: calendar notifications must show regardless of the main window's state
//! (even hidden to tray), can only be dismissed by the «✕», don't steal focus,
//! and a body click opens the event card WITHOUT closing the toast. None of
//! that is possible with OS/KDE notifications, so each toast is its own
//! `ToastWindow` (frameless + always-on-top via Slint `Window` props; X11
//! NOTIFICATION type so KWin keeps it above, off the taskbar and unfocused).
//!
//! This module OWNS the window handles — keeping a `ToastWindow` alive is what
//! keeps its window open — and stacks live toasts from the bottom-right corner.
//! Everything here runs on the Slint UI thread.

use std::cell::RefCell;

use slint::ComponentHandle;

use crate::ToastWindow;

pub const KIND_SOON: i32 = 1; // «скоро случится»
pub const KIND_STARTED: i32 = 2; // «наступило»

struct Entry {
    id: u64,
    event_id: i64, // which calendar event this toast belongs to (0 = none)
    win: ToastWindow,
    created: std::time::Instant,
    timeout_ms: u64, // 0 = no auto-close / no progress bar
}

#[derive(Default)]
struct Reg {
    items: Vec<Entry>,
    next_id: u64,
    // One shared 200ms ticker drives every toast's progress bar and auto-close.
    // Started lazily on the first toast; left running (idle ticks are cheap and
    // dropping a Timer from inside its own callback is unsafe).
    ticker: Option<slint::Timer>,
}

thread_local! {
    static REG: RefCell<Reg> = RefCell::new(Reg::default());
}

const MARGIN: f32 = 16.0;
const GAP: f32 = 10.0;
const TOAST_W: f32 = 360.0;
const TOAST_H: f32 = 128.0;

/// Spawn a toast window. Callbacks run on the UI thread:
///   - `on_close`  : «✕» pressed (the window is closed for you afterwards).
///   - `on_body`   : card body clicked (does NOT close — caller decides).
///   - `on_action` : action button («Напомнить позже») pressed.
/// `timeout_secs > 0` auto-closes after that long (no `on_close` callback —
/// a silent expiry). Returns the toast id (0 on failure).
#[allow(clippy::too_many_arguments)]
pub fn show(
    kind: i32,
    event_id: i64,
    title: &str,
    body: &str,
    show_action: bool,
    timeout_secs: u64,
    on_close: impl Fn() + 'static,
    on_body: impl Fn() + 'static,
    on_action: impl Fn() + 'static,
) -> u64 {
    let win = match ToastWindow::new() {
        Ok(w) => w,
        Err(e) => {
            eprintln!("toast window create: {e}");
            return 0;
        }
    };
    win.set_kind(kind);
    win.set_toast_title(title.into());
    win.set_toast_body(body.into());
    win.set_show_action(show_action);
    win.set_timeout_ms((timeout_secs as i32).saturating_mul(1000));

    let id = REG.with(|r| {
        let mut r = r.borrow_mut();
        r.next_id += 1;
        r.next_id
    });

    win.on_close_clicked(move || {
        on_close();
        // Defer the actual close out of this event callback so we never drop
        // the window/timer from inside their own dispatch.
        let _ = slint::invoke_from_event_loop(move || close(id));
    });
    win.on_body_clicked(move || on_body());
    win.on_action_clicked(move || on_action());

    win.show().ok();

    #[cfg(target_os = "linux")]
    set_x11_notification(&win);

    REG.with(|r| {
        let mut r = r.borrow_mut();
        r.items.push(Entry {
            id,
            event_id,
            win,
            created: std::time::Instant::now(),
            timeout_ms: timeout_secs.saturating_mul(1000),
        });
        if r.ticker.is_none() {
            let t = slint::Timer::default();
            t.start(
                slint::TimerMode::Repeated,
                std::time::Duration::from_millis(200),
                tick,
            );
            r.ticker = Some(t);
        }
    });
    reposition();
    id
}

/// Drive every toast's progress bar; auto-close the expired ones. Runs on the
/// UI thread off the shared ticker.
fn tick() {
    let now = std::time::Instant::now();
    let mut due: Vec<u64> = Vec::new();
    REG.with(|r| {
        let r = r.borrow();
        for e in &r.items {
            if e.timeout_ms == 0 {
                continue;
            }
            let elapsed = now.duration_since(e.created).as_millis() as u64;
            let p = (elapsed as f32 / e.timeout_ms as f32).clamp(0.0, 1.0);
            e.win.set_progress(p);
            if elapsed >= e.timeout_ms {
                due.push(e.id);
            }
        }
    });
    for id in due {
        close(id);
    }
}

/// Close a toast by id (no-op if already gone). Re-stacks the rest.
pub fn close(id: u64) {
    let removed = REG.with(|r| {
        let mut r = r.borrow_mut();
        if let Some(pos) = r.items.iter().position(|e| e.id == id) {
            let e = r.items.remove(pos);
            e.win.hide().ok();
            true
        } else {
            false
        }
    });
    if removed {
        reposition();
    }
}

/// Close every toast belonging to a given (event, occurrence) — used when the
/// «✕» on a «наступило» toast must wipe all notifications for that occurrence.
/// (Mapping id→event is the caller's job in the full feature; for now this is
/// the primitive the caller composes.)
pub fn close_all() {
    let ids: Vec<u64> = REG.with(|r| r.borrow().items.iter().map(|e| e.id).collect());
    for id in ids {
        close(id);
    }
}

/// Is a toast for this event currently on screen? Used to dedup — spec: don't
/// raise a second «скоро» for an event while one is already showing (e.g. a
/// burst of repeats right after the client starts).
pub fn has_for_event(event_id: i64) -> bool {
    REG.with(|r| r.borrow().items.iter().any(|e| e.event_id == event_id))
}

/// Close every toast for an event — «✕» on a «наступило» toast wipes the
/// occurrence's notifications, so any sibling toast for it must go too.
pub fn close_for_event(event_id: i64) {
    let ids: Vec<u64> =
        REG.with(|r| r.borrow().items.iter().filter(|e| e.event_id == event_id).map(|e| e.id).collect());
    for id in ids {
        close(id);
    }
}

fn reposition() {
    let (sw, sh) = screen_size();
    REG.with(|r| {
        let r = r.borrow();
        let mut y = sh;
        for e in &r.items {
            let scale = e.win.window().scale_factor().max(0.01);
            let wpx = TOAST_W * scale;
            let hpx = TOAST_H * scale;
            let m = MARGIN * scale;
            y -= hpx + m;
            let x = sw - wpx - m;
            e.win
                .window()
                .set_position(slint::PhysicalPosition::new(x as i32, y as i32));
            y -= GAP * scale;
        }
    });
}

#[cfg(target_os = "linux")]
fn screen_size() -> (f32, f32) {
    use x11::xlib;
    unsafe {
        let dpy = xlib::XOpenDisplay(std::ptr::null());
        if dpy.is_null() {
            return (1920.0, 1080.0);
        }
        let s = xlib::XDefaultScreen(dpy);
        let w = xlib::XDisplayWidth(dpy, s) as f32;
        let h = xlib::XDisplayHeight(dpy, s) as f32;
        xlib::XCloseDisplay(dpy);
        (w, h)
    }
}

#[cfg(not(target_os = "linux"))]
fn screen_size() -> (f32, f32) {
    (1920.0, 1080.0)
}

/// Tag the toast's X11 window as `_NET_WM_WINDOW_TYPE_NOTIFICATION`. On KWin
/// that single hint keeps it above other windows, off the taskbar, and stops
/// it stealing focus. Best-effort: a throwaway display connection (never race
/// Slint's own), silent no-op on Wayland / unsupported WMs.
#[cfg(target_os = "linux")]
fn set_x11_notification(win: &ToastWindow) {
    use raw_window_handle::{HasWindowHandle, RawWindowHandle};
    use x11::xlib;

    // slint::Window::window_handle() → slint::WindowHandle, which impls
    // raw_window_handle::HasWindowHandle (the trait method, same name).
    let slint_handle = win.window().window_handle();
    let xid = match slint_handle.window_handle() {
        Ok(h) => match h.as_raw() {
            RawWindowHandle::Xlib(x) => x.window,
            _ => return, // Wayland / other → nothing to do
        },
        Err(_) => return,
    };

    unsafe {
        let dpy = xlib::XOpenDisplay(std::ptr::null());
        if dpy.is_null() {
            return;
        }

        // Window type NOTIFICATION → above, undecorated-ish, unfocused.
        let wt = xlib::XInternAtom(dpy, c"_NET_WM_WINDOW_TYPE".as_ptr(), xlib::False);
        let wt_notif = xlib::XInternAtom(
            dpy,
            c"_NET_WM_WINDOW_TYPE_NOTIFICATION".as_ptr(),
            xlib::False,
        );
        let wt_arr: [std::os::raw::c_ulong; 1] = [wt_notif];
        xlib::XChangeProperty(
            dpy,
            xid,
            wt,
            xlib::XA_ATOM,
            32,
            xlib::PropModeReplace,
            wt_arr.as_ptr() as *const std::os::raw::c_uchar,
            1,
        );

        // Explicit states: keep it out of the pager AND the taskbar, and above
        // everything. NOTIFICATION type doesn't reliably skip the pager on
        // KWin, so we set these directly. Set the property (honoured before/at
        // map) and also send the client message (the EWMH-correct path for an
        // already-mapped window).
        let st = xlib::XInternAtom(dpy, c"_NET_WM_STATE".as_ptr(), xlib::False);
        let skip_pager =
            xlib::XInternAtom(dpy, c"_NET_WM_STATE_SKIP_PAGER".as_ptr(), xlib::False);
        let skip_taskbar =
            xlib::XInternAtom(dpy, c"_NET_WM_STATE_SKIP_TASKBAR".as_ptr(), xlib::False);
        let above = xlib::XInternAtom(dpy, c"_NET_WM_STATE_ABOVE".as_ptr(), xlib::False);

        let st_arr: [std::os::raw::c_ulong; 3] = [skip_pager, skip_taskbar, above];
        xlib::XChangeProperty(
            dpy,
            xid,
            st,
            xlib::XA_ATOM,
            32,
            xlib::PropModeReplace,
            st_arr.as_ptr() as *const std::os::raw::c_uchar,
            3,
        );

        // Client-message ADD for the mapped window (action=1 _ADD, source=1).
        let root = xlib::XDefaultRootWindow(dpy);
        let send_state = |a1: std::os::raw::c_ulong, a2: std::os::raw::c_ulong| {
            let mut data = xlib::ClientMessageData::new();
            data.set_long(0, 1); // _NET_WM_STATE_ADD
            data.set_long(1, a1 as std::os::raw::c_long);
            data.set_long(2, a2 as std::os::raw::c_long);
            data.set_long(3, 1); // source: application
            let mut ev = xlib::XEvent {
                client_message: xlib::XClientMessageEvent {
                    type_: xlib::ClientMessage,
                    serial: 0,
                    send_event: xlib::True,
                    display: dpy,
                    window: xid,
                    message_type: st,
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
        };
        send_state(skip_pager, skip_taskbar);
        send_state(above, 0);

        xlib::XFlush(dpy);
        xlib::XCloseDisplay(dpy);
    }
}
