//! Calendar reminder scheduling — user spec of 2026-07-11.
//!
//! Model: every occurrence carries a CASCADE of alarms taken from the
//! event's VALARMs (element 0 = primary, the rest = secondary). A link of
//! the cascade fires its toast; what happens next depends on how the toast
//! dies:
//!   - TIMEOUT (untouched)  → the next link arms;
//!   - «✕»                  → the whole occurrence is silenced forever;
//!   - «напомнить позже»    → cascade replaced by ONE user-chosen reminder;
//!   - body click           → toast stays, its timer stops (no outcome yet).
//!
//! Data lives in the core cache (`reminders2`); this module owns seeding
//! from the fetched events and the scan-side state machine. Toast plumbing
//! (windows, buttons, navigation) stays in main.rs.

use ddmail_core::cache::{Cache, ReminderRow};
use ddmail_core::types::DesktopCalendarEvent;

use crate::recurrence;

/// Don't seed occurrences further out than this. The view re-seeds on every
/// refetch, so events crossing the horizon are picked up well before firing.
const SEED_HORIZON_MS: i64 = 30 * 24 * 3600 * 1000;

/// Occurrence with no dtend counts as running for this long (spec answer #6).
pub const NO_END_RUNNING_MS: i64 = 30 * 60 * 1000;

/// Rows whose occurrence is older than this are pruned on each scan.
const PRUNE_AFTER_HOURS: i64 = 48;

/// How often the UI-thread timer scans for due reminders.
pub const SCAN_INTERVAL_SECS: u64 = 15;

/// Toast lifetimes.
pub const SOON_TIMEOUT_SECS: u64 = 30;
pub const AT_START_TIMEOUT_SECS: u64 = 180;

/// How a due reminder should present itself.
#[derive(Debug, Clone, Copy, PartialEq)]
pub enum ToastMode {
    /// «Скоро: …» — ✕ / body / «Напомнить позже», timeout advances cascade.
    Soon,
    /// «Наступило: …» — ✕ + body only, closes on body click.
    AtStart,
    /// Startup catch-up: the event is in progress. Same shape as AtStart
    /// with the «Событие уже идёт!» marker.
    AlreadyRunning,
}

/// A reminder the scanner decided to show right now.
#[derive(Debug, Clone)]
pub struct DueToast {
    pub row: ReminderRow,
    pub mode: ToastMode,
}

/// Effective end of an occurrence for "is it still running" decisions.
pub fn occurrence_end(row: &ReminderRow) -> i64 {
    if row.occurrence_end_ms > row.occurrence_start_ms {
        row.occurrence_end_ms
    } else {
        row.occurrence_start_ms + NO_END_RUNNING_MS
    }
}

/// Seed the alarm cascades from the current calendar view.
///
/// `hidden_calendars` are skipped outright (spec answer #9: выключено —
/// значит выключено). Recurring masters are expanded client-side so their
/// occurrences within the horizon get reminders too; one-off events and
/// server-side override rows seed directly. Signature covers everything
/// reminder-relevant, so any event change (times, alarms, title) resets the
/// occurrence's cascade — spec answer #8.
pub fn seed(
    cache: &Cache,
    events: &[DesktopCalendarEvent],
    hidden_calendars: &dyn Fn(i64) -> bool,
    now_ms: i64,
) {
    let horizon = now_ms + SEED_HORIZON_MS;
    for ev in events {
        if ev.summary.trim().is_empty() || ev.all_day {
            continue;
        }
        if hidden_calendars(ev.calendar_id) {
            continue;
        }
        let leads: Vec<i32> = if !ev.alarm_leads.is_empty() {
            ev.alarm_leads.clone()
        } else if ev.alarm_lead_min > 0 {
            vec![ev.alarm_lead_min]
        } else {
            vec![10] // pre-alarm_leads server fallback
        };
        let signature = format!(
            "{}|{}|{:?}|{}",
            ev.dtstart,
            ev.dtend.unwrap_or(0),
            leads,
            ev.summary.trim()
        );
        let duration = ev
            .dtend
            .map(|e| (e - ev.dtstart).max(0))
            .unwrap_or(0);

        // Occurrences: rrule masters expand within [now, horizon]; plain
        // events and override rows are their own single occurrence.
        let starts: Vec<i64> = if ev.rrule.is_empty() {
            vec![ev.dtstart]
        } else {
            recurrence::expand(ev.dtstart, ev.dtend, &ev.rrule, &ev.exdates, now_ms, horizon)
                .into_iter()
                .map(|o| o.start_ms)
                .collect()
        };
        for start in starts {
            if start <= now_ms || start > horizon {
                continue;
            }
            let end = if duration > 0 { start + duration } else { 0 };
            if let Err(e) =
                cache.seed_occurrence(ev.id, start, end, ev.summary.trim(), &leads, &signature)
            {
                eprintln!("reminders: seed failed for event {}: {e}", ev.id);
            }
        }
    }
}

/// Prune stale rows, then run the state machine over the due set:
///   - occurrence over        → expire silently (spec: «прошло — тихо удаляем»);
///   - occurrence in progress → ONE «уже идёт» toast per occurrence, the
///     rest of its cascade is cancelled;
///   - otherwise              → show as Soon (lead > 0) or AtStart (lead 0).
/// Every returned row is already marked `shown`.
pub fn scan(cache: &Cache, now_ms: i64) -> Vec<DueToast> {
    let cutoff = now_ms - PRUNE_AFTER_HOURS * 3600 * 1000;
    let _ = cache.prune_old_reminders(cutoff);

    let due = match cache.due_reminders(now_ms) {
        Ok(rows) => rows,
        Err(e) => {
            eprintln!("reminders: scan failed: {e}");
            return Vec::new();
        }
    };

    let mut out: Vec<DueToast> = Vec::new();
    let mut running_seen: std::collections::HashSet<(i64, i64)> = std::collections::HashSet::new();
    for row in due {
        let key = (row.event_id, row.occurrence_start_ms);
        if occurrence_end(&row) <= now_ms {
            let _ = cache.expire_occurrence_reminders(row.event_id, row.occurrence_start_ms);
            continue;
        }
        if row.occurrence_start_ms <= now_ms {
            // In progress. One catch-up toast per occurrence; the rest of
            // the cascade is moot — cancel it so nothing else fires later.
            if !running_seen.insert(key) {
                continue;
            }
            let _ = cache.mark_reminder_shown(row.event_id, row.occurrence_start_ms, row.seq);
            let _ = cache.cancel_occurrence_reminders(row.event_id, row.occurrence_start_ms);
            out.push(DueToast { row, mode: ToastMode::AlreadyRunning });
            continue;
        }
        let _ = cache.mark_reminder_shown(row.event_id, row.occurrence_start_ms, row.seq);
        let mode = if row.at_start || row.lead_min == 0 {
            ToastMode::AtStart
        } else {
            ToastMode::Soon
        };
        out.push(DueToast { row, mode });
    }
    out
}

/// Toast title per mode.
pub fn title_for(t: &DueToast) -> String {
    let s = if t.row.summary.trim().is_empty() {
        "Событие"
    } else {
        t.row.summary.trim()
    };
    match t.mode {
        ToastMode::Soon => format!("Скоро: {s}"),
        ToastMode::AtStart => format!("Наступило: {s}"),
        ToastMode::AlreadyRunning => format!("Событие уже идёт! {s}"),
    }
}

/// Toast body: time until start + the local start time.
pub fn body_for(t: &DueToast, now_ms: i64) -> String {
    use chrono::{Local, TimeZone};
    let hhmm = Local
        .timestamp_millis_opt(t.row.occurrence_start_ms)
        .single()
        .map(|x| x.format("%H:%M").to_string())
        .unwrap_or_default();
    let mins_until = (t.row.occurrence_start_ms - now_ms) / 60_000;
    match t.mode {
        ToastMode::AlreadyRunning => format!("Началось в {hhmm}"),
        _ if mins_until > 1 => format!("Через {mins_until} мин — начало в {hhmm}"),
        _ => format!("Начинается — в {hhmm}"),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::path::PathBuf;

    fn temp_cache() -> Cache {
        use std::sync::atomic::{AtomicU32, Ordering};
        static N: AtomicU32 = AtomicU32::new(0);
        let dir = std::env::temp_dir().join(format!(
            "ddmail_rem2_test_{}_{}",
            std::process::id(),
            N.fetch_add(1, Ordering::Relaxed)
        ));
        let _ = std::fs::remove_dir_all(&dir);
        Cache::new(PathBuf::from(&dir)).expect("cache")
    }

    fn event(id: i64, summary: &str, dtstart: i64, leads: &[i32]) -> DesktopCalendarEvent {
        serde_json::from_value(serde_json::json!({
            "id": id, "calendar_id": 1, "uid": format!("uid-{id}"),
            "summary": summary, "dtstart": dtstart, "dtend": dtstart + 3_600_000,
            "all_day": false, "alarm_leads": leads,
        }))
        .expect("event")
    }

    fn no_hidden(_: i64) -> bool {
        false
    }

    #[test]
    fn cascade_fires_in_order_on_timeouts() {
        let cache = temp_cache();
        let now = 1_000_000_000_000;
        // Two alarms: -10 min (primary) and at-start (secondary).
        let start = now + 5 * 60_000;
        seed(&cache, &[event(1, "Meet", start, &[10, 0])], &no_hidden, now);

        // Primary due immediately (fire_at = start - 10m < now).
        let due = scan(&cache, now);
        assert_eq!(due.len(), 1);
        assert_eq!(due[0].row.seq, 0);
        assert_eq!(due[0].mode, ToastMode::Soon);
        // Secondary stays chained until the primary's toast TIMES OUT.
        assert_eq!(scan(&cache, now).len(), 0);
        cache.reminder_timeout(1, start, 0).unwrap();
        // Now armed; due at start.
        assert_eq!(scan(&cache, now).len(), 0, "at-start not due yet");
        let due2 = scan(&cache, start);
        assert_eq!(due2.len(), 1);
        assert_eq!(due2[0].row.seq, 1);
    }

    #[test]
    fn cross_kills_the_whole_cascade() {
        let cache = temp_cache();
        let now = 2_000_000_000_000;
        let start = now + 5 * 60_000;
        seed(&cache, &[event(2, "Kill", start, &[10, 5, 0])], &no_hidden, now);
        assert_eq!(scan(&cache, now).len(), 1);
        cache.cancel_occurrence_reminders(2, start).unwrap();
        cache.reminder_timeout(2, start, 0).unwrap(); // stray timeout after ✕
        assert_eq!(scan(&cache, start).len(), 0, "✕ is irreversible");
    }

    #[test]
    fn user_choice_replaces_cascade() {
        let cache = temp_cache();
        let now = 3_000_000_000_000;
        let start = now + 30 * 60_000;
        seed(&cache, &[event(3, "Snooze", start, &[25, 0])], &no_hidden, now);
        assert_eq!(scan(&cache, now + 5 * 60_000).len(), 1); // primary at -25m
        cache
            .user_choice_reminder(3, start, start + 3_600_000, now + 10 * 60_000, false, "Snooze")
            .unwrap();
        let due = scan(&cache, now + 10 * 60_000);
        assert_eq!(due.len(), 1);
        assert_eq!(due[0].row.seq, 100);
        // The event-defined at-start alarm never fires afterwards.
        cache.reminder_timeout(3, start, 100).unwrap();
        assert_eq!(scan(&cache, start).len(), 0);
    }

    #[test]
    fn startup_running_and_past() {
        let cache = temp_cache();
        let now = 4_000_000_000_000;
        // Seed while both events are future…
        let running_start = now + 60_000;
        let past_start = now + 120_000;
        seed(&cache, &[event(4, "Running", running_start, &[10])], &no_hidden, now);
        seed(&cache, &[event(5, "Past", past_start, &[10])], &no_hidden, now);
        // …then "wake up" long after: event 4 is mid-run, event 5 is over.
        let wake = running_start + 30 * 60_000; // event4 (1h long) still running
        let due = scan(&cache, wake);
        let modes: Vec<_> = due.iter().map(|d| (d.row.event_id, d.mode)).collect();
        assert!(modes.contains(&(4, ToastMode::AlreadyRunning)));
        // Event 5 (1h long) is also still running at wake… make it truly past:
        let wake2 = past_start + 2 * 3_600_000;
        let due2 = scan(&cache, wake2);
        assert!(due2.iter().all(|d| d.row.event_id != 5), "ended events stay silent");
    }

    #[test]
    fn event_change_resets_cascade() {
        let cache = temp_cache();
        let now = 5_000_000_000_000;
        let start = now + 20 * 60_000;
        seed(&cache, &[event(6, "Move", start, &[10])], &no_hidden, now);
        cache.cancel_occurrence_reminders(6, start).unwrap(); // user said ✕
        // Same event, unchanged: reseed keeps the cancellation.
        seed(&cache, &[event(6, "Move", start, &[10])], &no_hidden, now);
        assert_eq!(scan(&cache, start - 60_000).len(), 0);
        // The event moved → full reset, reminders live again.
        let start2 = start + 3_600_000;
        seed(&cache, &[event(6, "Move", start2, &[10])], &no_hidden, now);
        let due = scan(&cache, start2 - 60_000);
        assert_eq!(due.len(), 1, "changed event re-arms as if new");
    }
}
