//! Linux reminder toast via `org.freedesktop.Notifications` (D-Bus).
//!
//! We use `notify-rust`, which speaks the fdo spec directly. The
//! reminder is rendered as a critical-urgency, resident notification with
//! three actions:
//!   * `default`        — body-click; opens the event in-app.
//!   * `ack`            — "Игнорировать".
//!   * `snooze-window`  — "Отложить…"; spawns the snooze-config webview.
//!
//! `default` is part of the fdo contract — every server invokes it on
//! body clicks even when it isn't rendered as a button. The explicit
//! action buttons are rendered on KDE Plasma / dunst / xfce4-notifyd; on
//! GNOME Shell only the body-click default works (their toast style is
//! intentional). That's a graceful degradation: ack + snooze are still
//! reachable via the in-app dialog the body-click opens.

use std::sync::Arc;
use std::thread;

use notify_rust::{Hint, Notification, Urgency};
use tauri::{AppHandle, Runtime};

use crate::cache::{Cache, ReminderRow};
use crate::reminders::handle_action;

pub(super) fn show_reminder<R: Runtime>(
    app: &AppHandle<R>,
    cache: &Arc<Cache>,
    row: &ReminderRow,
) -> Result<(), String> {
    let summary = if row.summary.trim().is_empty() {
        "Событие".to_string()
    } else {
        row.summary.clone()
    };
    let body = format_body(row);

    // `notify-rust` consumes the builder into a NotificationHandle on
    // .show(); the handle holds the connection we need for
    // wait_for_action.
    let handle = Notification::new()
        .summary(&summary)
        .body(&body)
        .appname("DDMail")
        .icon("x-office-calendar")
        .urgency(Urgency::Critical)
        // Resident keeps the toast on screen on servers that honour it —
        // KDE Plasma in particular — so the user has time to choose a
        // snooze even if they were AFK when it fired.
        .hint(Hint::Resident(true))
        .hint(Hint::Category("im.received".into()))
        // Two real actions; the rich snooze UX lives in a dedicated
        // webview spawned by `snooze-window`. `default` is kept as the
        // implicit body-click that opens the event — every fdo server
        // honours it even without rendering it as a button.
        .action("default", "Открыть событие")
        .action("ack", "Игнорировать")
        .action("snooze-window", "Отложить…")
        .show()
        .map_err(|e| format!("notify show: {e}"))?;

    // `wait_for_action` blocks until the user clicks something OR the
    // server emits NotificationClosed. We pin it to its own OS thread —
    // the Tokio runtime must not be blocked, and each reminder has its
    // own short-lived D-Bus subscription anyway.
    let cache = cache.clone();
    let app = app.clone();
    let event_id = row.event_id;
    let occ = row.occurrence_start_ms;
    thread::spawn(move || {
        handle.wait_for_action(|action_id| {
            handle_action(&cache, &app, event_id, occ, action_id);
        });
    });

    Ok(())
}

/// Two-line body: a friendly "in N minutes" lead, plus the absolute
/// HH:MM start time of the occurrence so the user can sanity-check what
/// the reminder is about without opening the app.
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
