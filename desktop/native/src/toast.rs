//! Clickable mail toast.
//!
//! Windows: `tauri-winrt-notification` — its `on_activated` is delivered
//! in-process (the app is always running when its own toast fires, hidden
//! to tray included), so body clicks work WITHOUT an AUMID/COM activator.
//! Same mechanism the Tauri build used for calendar reminders.
//!
//! Elsewhere it is a freedesktop notification whose body click is the
//! "default" action (`notify::notify_clickable`).
//!
//! Calendar reminders do not come through here: they are our own
//! always-on-top windows (`toast_window`), with buttons and a timer.

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
pub fn mail_toast(from: &str, subject: &str, on_click: impl Fn() + Send + Sync + 'static) {
    // Клик по плашке обязан открывать письмо. Раньше обработчик здесь
    // отбрасывался (`_on_click`), и уведомление на Linux не реагировало
    // вообще ни на что — при том что весь путь «открыть письмо из тоста»
    // уже был написан и работал на Windows.
    crate::notify::notify_clickable(from, subject, move || on_click());
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
