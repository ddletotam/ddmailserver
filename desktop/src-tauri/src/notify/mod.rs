//! Cross-platform calendar-reminder notifier.
//!
//! Per-OS implementations live in their own files (same layout as the
//! `tray` module) — Linux uses libnotify-style D-Bus, macOS will use the
//! UserNotifications framework, Windows will use WinRT toast templates.
//! The shared API is one function: render the reminder, wire up the
//! action callback, return.

#[cfg(target_os = "linux")]
mod linux;
#[cfg(target_os = "macos")]
mod macos;
#[cfg(target_os = "windows")]
mod windows;

use std::sync::Arc;
use tauri::{AppHandle, Runtime};

use crate::cache::{Cache, ReminderRow};

/// Show the reminder toast for `row`. Action callbacks are routed back
/// to the reminder state machine via the shared cache + app handle.
///
/// macOS and Windows implementations land in their own files when their
/// platform code is wired; for now they no-op so the rest of the
/// scheduler compiles and runs everywhere.
pub fn show_reminder<R: Runtime>(
    app: &AppHandle<R>,
    cache: &Arc<Cache>,
    row: &ReminderRow,
) -> Result<(), String> {
    #[cfg(target_os = "linux")]
    return linux::show_reminder(app, cache, row);
    #[cfg(target_os = "macos")]
    return macos::show_reminder(app, cache, row);
    #[cfg(target_os = "windows")]
    return windows::show_reminder(app, cache, row);
    #[cfg(not(any(target_os = "linux", target_os = "macos", target_os = "windows")))]
    {
        let _ = (app, cache, row);
        Ok(())
    }
}
