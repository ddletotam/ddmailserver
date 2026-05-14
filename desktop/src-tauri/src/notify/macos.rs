//! macOS reminder toast — stub.
//!
//! Real implementation will use `UNUserNotificationCenter` with a
//! pre-registered `UNNotificationCategory` ("calendar-reminder") that
//! carries the 4-action set (OK / +5 / +15 / при начале). Category
//! registration has to happen at app startup, before any toast fires.

use std::sync::Arc;
use tauri::{AppHandle, Runtime};

use crate::cache::{Cache, ReminderRow};

pub(super) fn show_reminder<R: Runtime>(
    _app: &AppHandle<R>,
    _cache: &Arc<Cache>,
    row: &ReminderRow,
) -> Result<(), String> {
    log::info!(
        "[reminders] macOS notifier not yet implemented; would have shown event {} at {}",
        row.event_id, row.occurrence_start_ms
    );
    Ok(())
}
