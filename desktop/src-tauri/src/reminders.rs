//! Calendar reminder scheduler.
//!
//! Lives in Rust (not JS) because the WebKitGTK / Webview2 timers go to
//! sleep when the window is hidden — Tokio in a background thread is the
//! only reliable place to keep a reminder ticking. The scheduler owns:
//!
//!   1. An SQLite table (`event_reminders`) of pending/fired/acked rows.
//!      Survives app restarts so a snooze "+15 min" works even if the
//!      user quits the app halfway through that window.
//!   2. A Tokio loop that scans the table every 30 s, fires the toast
//!      for each due row, and updates the row to `fired`.
//!   3. An action handler (`handle_action`) invoked by the per-platform
//!      notifier when the user clicks OK / Snooze / body. It mutates the
//!      reminder row and, for body-click, emits a Tauri event so the
//!      calendar window can open the event.
//!
//! The frontend feeds the scheduler with `schedule_reminders` whenever
//! it refreshes the calendar view — passing pre-expanded occurrences so
//! we don't have to drag an RRULE parser into Rust.

use std::sync::Arc;
use std::time::Duration;

use serde::{Deserialize, Serialize};
use tauri::{AppHandle, Emitter, Manager, Runtime};

use crate::cache::{Cache, ReminderRow};
use crate::notify;

/// Polling interval. Reminders have minute resolution, so a 30 s scan
/// keeps fire-latency below half a minute without burning cycles when
/// nothing's due. Cheaper than maintaining a heap of timers and dealing
/// with the cancellation/re-arm dance on every schedule update.
const SCAN_INTERVAL: Duration = Duration::from_secs(30);

/// Anything older than this is pruned on each scan — keeps the table
/// from growing without bound while still leaving recent rows around in
/// case we want them for "missed reminders" UI later.
const PRUNE_AFTER_HOURS: i64 = 48;

pub struct ReminderScheduler {
    // The scheduler is fire-and-forget once spawned: the Tokio task
    // holds the cache + app handle it needs, and Tauri keeps this
    // wrapper alive for the lifetime of the app via `app.manage`.
    _marker: (),
}

impl ReminderScheduler {
    pub fn spawn<R: Runtime>(cache: Arc<Cache>, app: AppHandle<R>) -> Self {
        tauri::async_runtime::spawn(async move {
            run_loop(cache, app).await;
        });
        Self { _marker: () }
    }
}

async fn run_loop<R: Runtime>(cache: Arc<Cache>, app: AppHandle<R>) {
    loop {
        if let Err(e) = scan_once(&cache, &app) {
            log::warn!("[reminders] scan failed: {e}");
        }
        tokio::time::sleep(SCAN_INTERVAL).await;
    }
}

fn scan_once<R: Runtime>(cache: &Arc<Cache>, app: &AppHandle<R>) -> Result<(), String> {
    let now_ms = chrono::Utc::now().timestamp_millis();
    let cutoff = now_ms - PRUNE_AFTER_HOURS * 3600 * 1000;
    let _ = cache.prune_old_reminders(cutoff);

    let due: Vec<ReminderRow> = cache.due_reminders(now_ms)?;
    for row in due {
        // Mark `fired` BEFORE showing — if the OS notification dispatch
        // hangs or panics, we still won't loop-spam the same toast.
        cache.mark_reminder_fired(row.event_id, row.occurrence_start_ms)?;
        if let Err(e) = notify::show_reminder(app, cache, &row) {
            log::warn!("[reminders] notify failed for event {}: {e}", row.event_id);
        }
    }
    Ok(())
}

// ── Tauri commands ────────────────────────────────────────────────────

/// One reminder slot pushed from JS. The frontend expands RRULEs and
/// filters by lead-time before sending — we just persist whatever we
/// get. The cache UPSERT-IGNORE keeps user state (ack / snooze) intact
/// for rows that already exist.
#[derive(Debug, Clone, Deserialize)]
pub struct ReminderInput {
    pub event_id: i64,
    pub occurrence_start_ms: i64,
    pub fire_at_ms: i64,
    pub lead_min: i32,
    #[serde(default)]
    pub summary: String,
}

/// Bulk-replace pending schedule for the visible window. We don't
/// delete previously-known reminders even when they fall out of the
/// payload — those occurrences may still be relevant (e.g. while the
/// app was offline overnight) and dropping them would silently miss
/// fires. Stale rows age out via the 48 h prune.
#[tauri::command]
pub async fn schedule_reminders<R: Runtime>(
    app: AppHandle<R>,
    reminders: Vec<ReminderInput>,
) -> Result<(), String> {
    let cache = app.state::<Arc<Cache>>().inner().clone();
    for r in reminders {
        cache.upsert_pending_reminder(
            r.event_id,
            r.occurrence_start_ms,
            r.fire_at_ms,
            r.lead_min,
            &r.summary,
        )?;
    }
    Ok(())
}

/// JS-side dispatch for reminder actions. The OS notifier already
/// calls `handle_action` directly for native button clicks; this
/// command lets in-app UI (e.g. an "ack" button in the calendar
/// modal) route through the same state machine.
#[tauri::command]
pub async fn reminder_action<R: Runtime>(
    app: AppHandle<R>,
    event_id: i64,
    occurrence_start_ms: i64,
    action: String,
) -> Result<(), String> {
    let cache = app.state::<Arc<Cache>>().inner().clone();
    handle_action(&cache, &app, event_id, occurrence_start_ms, &action);
    Ok(())
}

// ── Action dispatch ───────────────────────────────────────────────────

#[derive(Debug, Clone, Serialize)]
struct OpenEventPayload {
    event_id: i64,
    occurrence_start_ms: i64,
}

/// Handle one action from a reminder toast. Called from both the
/// platform notifier (D-Bus / UNNotification / WinRT callback) and the
/// in-app `reminder_action` command, so the side-effects are
/// identical regardless of where the click came from.
///
/// Recognised action ids:
///   * `default`       → emit `open-event` so the calendar window opens.
///                       Does NOT mark acked — opening the event is a
///                       softer state than "I handled this".
///   * `ack`           → mark acked; no further notifications.
///   * `snooze-window` → emit `open-snooze-reminder` so the frontend
///                       can spawn the snooze-config webview. Does
///                       NOT change row state; the snooze window
///                       commits via a subsequent `snz:N`/`snz:atstart`.
///   * `snz:N`         → snooze N minutes (5 / 15 / etc.).
///   * `snz:atstart`   → re-fire exactly at the occurrence start.
///   * anything else   → log and ignore (defensive — server can send
///                       e.g. `dismissed` on auto-timeouts).
pub fn handle_action<R: Runtime>(
    cache: &Arc<Cache>,
    app: &AppHandle<R>,
    event_id: i64,
    occurrence_start_ms: i64,
    action: &str,
) {
    match action {
        "default" => {
            let _ = app.emit(
                "open-event",
                OpenEventPayload { event_id, occurrence_start_ms },
            );
        }
        "ack" => {
            if let Err(e) = cache.mark_reminder_acked(event_id, occurrence_start_ms) {
                log::warn!("[reminders] ack failed for event {event_id}: {e}");
            }
        }
        "snooze-window" => {
            let _ = app.emit(
                "open-snooze-reminder",
                OpenEventPayload { event_id, occurrence_start_ms },
            );
        }
        "snz:atstart" => {
            if let Err(e) = cache.snooze_reminder(event_id, occurrence_start_ms, occurrence_start_ms, 0) {
                log::warn!("[reminders] snooze@start failed for event {event_id}: {e}");
            }
        }
        other if other.starts_with("snz:") => {
            let minutes: i64 = other[4..].parse().unwrap_or(0);
            if minutes <= 0 {
                log::warn!("[reminders] bad snooze offset: {other}");
                return;
            }
            let new_fire_at = chrono::Utc::now().timestamp_millis() + minutes * 60_000;
            let lead = ((occurrence_start_ms - new_fire_at) / 60_000).max(0) as i32;
            if let Err(e) = cache.snooze_reminder(event_id, occurrence_start_ms, new_fire_at, lead) {
                log::warn!("[reminders] snooze failed for event {event_id}: {e}");
            }
        }
        _ => {
            log::debug!("[reminders] ignoring action {action}");
        }
    }
}

/// Read a single reminder row. The snooze-config webview calls this on
/// mount to populate its summary / occurrence-time / lead so the URL
/// only has to carry the (event_id, occurrence_start_ms) pair.
#[tauri::command]
pub async fn get_reminder<R: Runtime>(
    app: AppHandle<R>,
    event_id: i64,
    occurrence_start_ms: i64,
) -> Result<Option<crate::cache::ReminderRow>, String> {
    let cache = app.state::<Arc<Cache>>().inner().clone();
    cache.get_reminder(event_id, occurrence_start_ms)
}
