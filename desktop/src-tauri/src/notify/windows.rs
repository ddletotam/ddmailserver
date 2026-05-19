//! Windows reminder toast — full WinRT toast surface.
//!
//! Mirrors the Linux notifier (notify/linux.rs) feature-for-feature:
//!   * `default`        — body click; opens the event in-app.
//!   * `ack`            — "Игнорировать".
//!   * `snooze-window`  — "Отложить…"; spawns the snooze-config webview.
//!
//! We bypass `tauri-plugin-notification` (which exposes no callback path)
//! and talk to WinRT directly via `tauri-winrt-notification`. The
//! `on_activated` event is delivered in-process through a
//! `TypedEventHandler` — no COM activator class needed because the app
//! is always running when its own reminder fires.
//!
//! AppUserModelID: we use `Toast::POWERSHELL_APP_ID`. The same fallback
//! `notify-rust` uses for unpackaged Windows apps. A bundle-specific
//! AUMID would only matter for cross-process activation (toast clicked
//! while the app is closed) — that's not how the reminder loop works.
//! When the app ships through MSIX / WiX, the installer can register a
//! proper AUMID and we'll swap it in here.

use std::sync::Arc;
use tauri::{AppHandle, Runtime};
use tauri_winrt_notification::{Duration as ToastDuration, Toast};

use crate::cache::{Cache, ReminderRow};
use crate::reminders::handle_action;

pub(super) fn show_reminder<R: Runtime>(
    app: &AppHandle<R>,
    cache: &Arc<Cache>,
    row: &ReminderRow,
) -> Result<(), String> {
    let title = if row.summary.trim().is_empty() {
        "Событие".to_string()
    } else {
        row.summary.clone()
    };
    let body = format_body(row);

    // The callback must be `FnMut + Send + 'static`, so capture owned
    // clones of cache + app handle. Both are Arc-backed.
    let cb_cache = cache.clone();
    let cb_app = app.clone();
    let event_id = row.event_id;
    let occ = row.occurrence_start_ms;

    Toast::new(Toast::POWERSHELL_APP_ID)
        .title(&title)
        .text1(&body)
        .duration(ToastDuration::Long)
        // Two-button layout: minimal native chrome, rich snooze UX
        // lives in a dedicated webview spawned by `snooze-window`.
        // Body click stays as the "open the event" gesture (None ↦
        // "default" in on_activated).
        .add_button("Игнорировать", "ack")
        .add_button("Отложить…", "snooze-window")
        .on_activated(move |action| {
            // `None` = body click (no argument); per the shared
            // contract that maps to the `default` action.
            let id = action.as_deref().unwrap_or("default");
            handle_action(&cb_cache, &cb_app, event_id, occ, id);
            Ok(())
        })
        .show()
        .map_err(|e| format!("toast show: {e}"))?;

    Ok(())
}

/// Two-line body, same wording as Linux for cross-platform consistency.
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
