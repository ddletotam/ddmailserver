//! macOS reminder toast — best-effort via notify-rust (mac-notification-sys).
//!
//! Limitations versus the Linux/Windows path:
//!   * No action buttons. mac-notification-sys only exposes the Reply
//!     input; full button categories require registering a
//!     `UNNotificationCategory` on `UNUserNotificationCenter` at app
//!     launch with an Objective-C delegate to receive callbacks — that's
//!     the proper macOS path and the next step here.
//!   * No body-click callback. The OS default (activate the app) still
//!     applies, which is enough to surface the reminder. Routing the
//!     click to `handle_action(..., "default")` likewise needs the UN
//!     delegate.
//!
//! Untested — no Mac in the dev rotation. The code mirrors the Linux
//! formatter so messages stay consistent across platforms.

use std::sync::Arc;

use notify_rust::Notification;
use tauri::{AppHandle, Runtime};

use crate::cache::{Cache, ReminderRow};

pub(super) fn show_reminder<R: Runtime>(
    _app: &AppHandle<R>,
    _cache: &Arc<Cache>,
    row: &ReminderRow,
) -> Result<(), String> {
    let summary = if row.summary.trim().is_empty() {
        "Событие".to_string()
    } else {
        row.summary.clone()
    };
    let body = format_body(row);

    Notification::new()
        .summary(&summary)
        .body(&body)
        .appname("DDMail")
        .show()
        .map_err(|e| format!("notify show: {e}"))?;

    Ok(())
}

fn format_body(row: &ReminderRow) -> String {
    use chrono::{Local, TimeZone};
    let dt = Local
        .timestamp_millis_opt(row.occurrence_start_ms)
        .single();
    let when = dt
        .map(|d| d.format("%H:%M").to_string())
        .unwrap_or_default();
    if row.lead_min > 0 {
        format!("Через {} мин — начало в {}", row.lead_min, when)
    } else {
        format!("Начало в {}", when)
    }
}
