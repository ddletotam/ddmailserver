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

/// Reminder kinds (mirror the cache `kind` column / `toast_window`).
pub const KIND_SOON: i32 = 1; // «скоро случится» — at dtstart − lead
pub const KIND_STARTED: i32 = 2; // «наступило» — at dtstart
pub const KIND_MANUAL: i32 = 3; // user «напомнить позже» (fires as a «скоро»)

/// A «наступило» toast is only meaningful right around the start; if the
/// client was offline and we cross it well after the fact, suppress it.
pub const STARTED_GRACE_MS: i64 = 3 * 60 * 1000;

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
/// Each future occurrence gets TWO rows: «скоро случится» (kind 1, at
/// dtstart − lead) and «наступило» (kind 2, at dtstart). Occurrences the user
/// has taken manual control of (a kind-3 row exists) are left untouched, so a
/// routine refresh can't clobber a «напомнить позже». Idempotent: the cache
/// upserts on (occurrence, summary, kind) and never resurrects fired rows.
pub fn seed(cache: &Cache, events: &[DesktopCalendarEvent], now_ms: i64) {
    let horizon = now_ms + SEED_HORIZON_MS;
    for ev in events {
        // Only future starts within the horizon, with a real title.
        if ev.dtstart < now_ms || ev.dtstart > horizon {
            continue;
        }
        if ev.summary.trim().is_empty() {
            continue;
        }
        // The user neutralised the auto reminders for this occurrence — respect it.
        if cache.has_manual_reminder(ev.dtstart, &ev.summary) {
            continue;
        }
        let lead = if ev.alarm_lead_min > 0 {
            ev.alarm_lead_min
        } else {
            DEFAULT_LEAD_MIN
        };
        let fire_soon = ev.dtstart - (lead as i64) * 60_000;
        if let Err(e) =
            cache.upsert_pending_reminder(ev.id, ev.dtstart, fire_soon, lead, &ev.summary, KIND_SOON)
        {
            eprintln!("reminders: seed A failed for event {}: {e}", ev.id);
        }
        if let Err(e) = cache.upsert_pending_reminder(
            ev.id,
            ev.dtstart,
            ev.dtstart,
            0,
            &ev.summary,
            KIND_STARTED,
        ) {
            eprintln!("reminders: seed B failed for event {}: {e}", ev.id);
        }
    }
}

/// Regenerate reminders for one event after it was edited (or wipe them after
/// a delete): purge every row for the event, then re-seed from the current
/// view. Per spec, an edit/delete drops all prior reminders and rebuilds from
/// the event's settings — including any manual snooze.
pub fn reseed_event(cache: &Cache, event_id: i64, events: &[DesktopCalendarEvent], now_ms: i64) {
    if let Err(e) = cache.purge_event_reminders(event_id) {
        eprintln!("reminders: purge {event_id} failed: {e}");
    }
    // Re-seed only the surviving occurrences of this event (delete → none).
    let mine: Vec<DesktopCalendarEvent> =
        events.iter().filter(|e| e.id == event_id).cloned().collect();
    seed(cache, &mine, now_ms);
}

/// Prune stale rows, then return reminders that have just come due, marking
/// each `fired` (by its logical (occurrence, summary, kind) key, so A and B
/// for one occurrence are tracked independently) before returning — the
/// caller can never re-trigger the same row.
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
        if let Err(e) = cache.mark_reminder_fired(row.occurrence_start_ms, &row.summary, row.kind) {
            eprintln!("reminders: mark_fired failed for {}: {e}", row.event_id);
        }
    }
    due
}

/// Should a due row actually be shown? «скоро» (kind 1) is pointless once the
/// event has started; «наступило» (kind 2) is stale long after the start. A
/// manual snooze (kind 3) always shows — the user asked for it explicitly.
pub fn should_show(row: &ReminderRow, now_ms: i64) -> bool {
    match row.kind {
        KIND_SOON => now_ms < row.occurrence_start_ms,
        KIND_STARTED => now_ms <= row.occurrence_start_ms + STARTED_GRACE_MS,
        _ => true,
    }
}

/// Toast title: «Скоро: …» / «Наступило: …».
pub fn title_for(row: &ReminderRow) -> String {
    let s = if row.summary.trim().is_empty() {
        "Событие"
    } else {
        row.summary.trim()
    };
    match row.kind {
        KIND_STARTED => format!("Наступило: {s}"),
        _ => format!("Скоро: {s}"),
    }
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
            kind: KIND_SOON,
        };
        assert!(body_for(&row, -10 * 60_000).starts_with("Через 10 мин"));
        assert!(body_for(&row, 0).starts_with("Начинается"));
        assert!(body_for(&row, 60_000).starts_with("Началось"));
    }
}
