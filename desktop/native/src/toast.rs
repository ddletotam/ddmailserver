//! Clickable mail toast.
//!
//! Windows: `tauri-winrt-notification` — its `on_activated` is delivered
//! in-process (the app is always running when its own toast fires, hidden
//! to tray included), so body clicks work WITHOUT an AUMID/COM activator.
//! Same mechanism the Tauri build used for calendar reminders.
//!
//! Non-Windows falls back to a plain fire-and-forget notification.

/// Show «отправитель — тема» for ~7–10 s; `on_click` fires on a body
/// click (from the WinRT callback thread — the caller must hop to the UI
/// loop itself, e.g. via `Weak::upgrade_in_event_loop`).
#[cfg(windows)]
pub fn mail_toast(from: &str, subject: &str, on_click: impl Fn() + Send + Sync + 'static) {
    use tauri_winrt_notification::{Duration as ToastDuration, Toast};
    let r = Toast::new(Toast::POWERSHELL_APP_ID)
        .title(from)
        .text1(subject)
        .duration(ToastDuration::Short)
        .on_activated(move |_action| {
            on_click();
            Ok(())
        })
        .show();
    if let Err(e) = r {
        eprintln!("mail toast: {e}");
    }
}

#[cfg(not(windows))]
pub fn mail_toast(from: &str, subject: &str, _on_click: impl Fn() + Send + Sync + 'static) {
    crate::notify::notify(from, subject);
}

/// Calendar-reminder toast, restored from the Tauri build: body click =
/// "default" (open the event), «Игнорировать» = "ack", «Отложить…» =
/// "snooze-window" — shown only while the occurrence hasn't started yet.
/// `on_action` runs on the WinRT callback thread; the caller hops to the
/// UI loop itself.
#[cfg(windows)]
pub fn reminder_toast(
    summary: &str,
    body: &str,
    can_snooze: bool,
    on_action: impl Fn(&str) + Send + Sync + 'static,
) {
    use tauri_winrt_notification::{Duration as ToastDuration, Toast};
    let title = if summary.trim().is_empty() { "Событие" } else { summary };
    let mut t = Toast::new(Toast::POWERSHELL_APP_ID)
        .title(title)
        .text1(body)
        .duration(ToastDuration::Long)
        .add_button("Игнорировать", "ack");
    if can_snooze {
        t = t.add_button("Отложить…", "snooze-window");
    }
    let r = t
        .on_activated(move |action| {
            on_action(action.as_deref().unwrap_or("default"));
            Ok(())
        })
        .show();
    if let Err(e) = r {
        eprintln!("reminder toast: {e}");
    }
}

#[cfg(not(windows))]
pub fn reminder_toast(
    summary: &str,
    body: &str,
    _can_snooze: bool,
    _on_action: impl Fn(&str) + Send + Sync + 'static,
) {
    let title = if summary.trim().is_empty() { "Событие" } else { summary };
    crate::notify::notify(title, body);
}

/// New-mail beep, honouring nothing — the caller checks the setting.
#[cfg(windows)]
pub fn beep() {
    use windows::Win32::System::Diagnostics::Debug::MessageBeep;
    use windows::Win32::UI::WindowsAndMessaging::MB_ICONASTERISK;
    unsafe {
        let _ = MessageBeep(MB_ICONASTERISK);
    }
}

#[cfg(not(windows))]
pub fn beep() {}
