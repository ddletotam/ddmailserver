//! Calendar reminder scheduling for the native client.
//!
//! Lives in the client (not core) because firing a reminder means showing
//! an OS toast — a UI concern. The data, though, is persisted in the core
//! cache's `event_reminders` table so a reminder still fires after a
//! restart that crosses its lead-time window.
//!
//! Two halves:
//!   - [`seed`] — called whenever the calendar view refreshes. Turns the
//!     freshly-fetched events into pending reminder rows (idempotent: the
//!     cache upserts by occurrence+summary).
//!   - [`scan`] — called on a fixed interval by a `slint::Timer` on the UI
//!     thread. Prunes stale rows, then returns the rows that just came due,
//!     marking each `fired` first so a hung toast can't loop-spam it.
//!
//! Unlike the old Tauri build this scheduler runs on the Slint event loop
//! rather than a background Tokio task: `slint::Timer` keeps ticking while
//! the window is hidden to tray, so we don't need a separate thread.

use ddmail_core::cache::{Cache, ReminderRow};
use ddmail_core::types::DesktopCalendarEvent;

/// Lead-time used when an event carries no VALARM (`alarm_lead_min == 0`).
const DEFAULT_LEAD_MIN: i32 = 10;

/// Don't seed reminders for events further out than this — keeps the table
/// bounded. The view re-seeds on every refresh, so events crossing the
/// horizon get picked up well before they fire.
const SEED_HORIZON_MS: i64 = 30 * 24 * 3600 * 1000;

/// Rows older than this are pruned on each scan.
const PRUNE_AFTER_HOURS: i64 = 48;

/// How often the UI-thread timer scans for due reminders.
pub const SCAN_INTERVAL_SECS: u64 = 30;

/// Seed pending reminders from the current calendar view.
///
/// Only future occurrences within the horizon are considered. The cache
/// upsert is keyed on (occurrence_start, summary), so calling this on every
/// refresh neither duplicates rows nor resurrects ones already `fired`.
pub fn seed(cache: &Cache, events: &[DesktopCalendarEvent], now_ms: i64) {
    let horizon = now_ms + SEED_HORIZON_MS;
    for ev in events {
        if ev.dtstart < now_ms || ev.dtstart > horizon {
            continue;
        }
        if ev.summary.trim().is_empty() {
            continue;
        }
        let lead = if ev.alarm_lead_min > 0 {
            ev.alarm_lead_min
        } else {
            DEFAULT_LEAD_MIN
        };
        let fire_at = ev.dtstart - (lead as i64) * 60_000;
        if let Err(e) =
            cache.upsert_pending_reminder(ev.id, ev.dtstart, fire_at, lead, &ev.summary)
        {
            eprintln!("reminders: seed failed for event {}: {e}", ev.id);
        }
    }
}

/// Prune stale rows, then return reminders that have just come due,
/// marking each `fired` before returning so the caller's toast dispatch
/// can never re-trigger the same row.
pub fn scan(cache: &Cache, now_ms: i64) -> Vec<ReminderRow> {
    let cutoff = now_ms - PRUNE_AFTER_HOURS * 3600 * 1000;
    let _ = cache.prune_old_reminders(cutoff);

    let due = match cache.due_reminders(now_ms) {
        Ok(rows) => rows,
        Err(e) => {
            eprintln!("reminders: scan failed: {e}");
            return Vec::new();
        }
    };
    for row in &due {
        if let Err(e) = cache.mark_reminder_fired(row.event_id, row.occurrence_start_ms) {
            eprintln!("reminders: mark_fired failed for {}: {e}", row.event_id);
        }
    }
    due
}

/// Human-readable toast body for a due reminder: how long until the event
/// starts (or that it's starting now) plus the local start time.
pub fn body_for(row: &ReminderRow, now_ms: i64) -> String {
    use chrono::{Local, TimeZone};
    let start_local = Local
        .timestamp_millis_opt(row.occurrence_start_ms)
        .single();
    let hhmm = start_local
        .map(|t| t.format("%H:%M").to_string())
        .unwrap_or_default();

    let mins_until = (row.occurrence_start_ms - now_ms) / 60_000;
    if mins_until > 1 {
        format!("Через {mins_until} мин — начало в {hhmm}")
    } else if mins_until >= 0 {
        format!("Начинается — в {hhmm}")
    } else {
        format!("Началось в {hhmm}")
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::path::PathBuf;

    fn temp_cache() -> Cache {
        // Unique-enough per test process; cargo runs tests in one process so
        // we suffix with a static counter to avoid collisions across tests.
        use std::sync::atomic::{AtomicU32, Ordering};
        static N: AtomicU32 = AtomicU32::new(0);
        let dir = std::env::temp_dir().join(format!(
            "ddmail_rem_test_{}_{}",
            std::process::id(),
            N.fetch_add(1, Ordering::Relaxed)
        ));
        let _ = std::fs::remove_dir_all(&dir);
        Cache::new(PathBuf::from(&dir)).expect("cache")
    }

    fn event(id: i64, summary: &str, dtstart: i64, lead: i32) -> DesktopCalendarEvent {
        serde_json::from_value(serde_json::json!({
            "id": id, "calendar_id": 1, "uid": format!("uid-{id}"),
            "summary": summary, "dtstart": dtstart, "dtend": null,
            "all_day": false, "alarm_lead_min": lead,
        }))
        .expect("event")
    }

    #[test]
    fn seeds_only_future_within_horizon() {
        let cache = temp_cache();
        let now = 1_000_000_000_000;
        let evs = vec![
            event(1, "Past", now - 60_000, 10),           // already started — skip
            event(2, "Soon", now + 5 * 60_000, 10),       // future, fires now (lead 10)
            event(3, "Far", now + SEED_HORIZON_MS + 1, 10), // beyond horizon — skip
            event(4, "", now + 5 * 60_000, 10),           // empty summary — skip
        ];
        seed(&cache, &evs, now);

        // Only event 2 is due (its fire_at = start - 10min is in the past).
        let due = scan(&cache, now);
        assert_eq!(due.len(), 1, "only the in-window future event fires");
        assert_eq!(due[0].event_id, 2);
        assert_eq!(due[0].summary, "Soon");
    }

    #[test]
    fn does_not_refire_after_marked() {
        let cache = temp_cache();
        let now = 2_000_000_000_000;
        seed(&cache, &[event(7, "Daily", now + 60_000, 10)], now);

        assert_eq!(scan(&cache, now).len(), 1, "fires once");
        assert_eq!(scan(&cache, now).len(), 0, "stays fired, no spam");
    }

    #[test]
    fn lead_defaults_when_zero() {
        let cache = temp_cache();
        let now = 3_000_000_000_000;
        // Start DEFAULT_LEAD+1 minutes out, no alarm → fire_at still future.
        let start = now + (DEFAULT_LEAD_MIN as i64 + 1) * 60_000;
        seed(&cache, &[event(9, "NoAlarm", start, 0)], now);
        assert_eq!(scan(&cache, now).len(), 0, "default lead not yet reached");

        // One minute later we cross the default-lead boundary.
        let due = scan(&cache, now + 2 * 60_000);
        assert_eq!(due.len(), 1);
        assert_eq!(due[0].lead_min, DEFAULT_LEAD_MIN);
    }

    #[test]
    fn body_text_phrases() {
        let row = ReminderRow {
            event_id: 1,
            occurrence_start_ms: 0,
            fire_at_ms: 0,
            lead_min: 10,
            summary: "X".into(),
        };
        assert!(body_for(&row, -10 * 60_000).starts_with("Через 10 мин"));
        assert!(body_for(&row, 0).starts_with("Начинается"));
        assert!(body_for(&row, 60_000).starts_with("Началось"));
    }
}
