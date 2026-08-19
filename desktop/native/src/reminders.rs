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
            if let Err(e) = cache.seed_occurrence(
                ev.id,
                ev.calendar_id,
                start,
                end,
                ev.summary.trim(),
                &leads,
                &signature,
            ) {
                eprintln!("reminders: seed failed for event {}: {e}", ev.id);
            }
        }
    }
}

/// Occurrence starts each event actually produces inside [from_ms, to_ms) —
/// та же деривация, что у seed(): plain event = свой dtstart, rrule-мастер =
/// разворот в окне. Скармливается в `cache.prune_moved_reminders`: строка
/// напоминания, чьё вхождение событие больше НЕ производит (перенос 12→13),
/// иначе остаётся взведённой и стреляет по старому времени.
pub fn valid_starts(
    events: &[DesktopCalendarEvent],
    from_ms: i64,
    to_ms: i64,
) -> std::collections::HashMap<i64, std::collections::HashSet<i64>> {
    let mut valid: std::collections::HashMap<i64, std::collections::HashSet<i64>> =
        std::collections::HashMap::new();
    for ev in events {
        let entry = valid.entry(ev.id).or_default();
        if ev.rrule.is_empty() {
            // Plain event / override row: единственное вхождение — dtstart
            // (вне окна — безвредно, window-фильтр cull'а такие строки
            // не рассматривает).
            entry.insert(ev.dtstart);
        } else {
            // Мастер: только реальные развороты (exdates учтены). Перенос
            // одного вхождения (EXDATE + override) валидирует старый слот
            // ТОЛЬКО если разворот его всё ещё производит — как в seed().
            for o in recurrence::expand(ev.dtstart, ev.dtend, &ev.rrule, &ev.exdates, from_ms, to_ms)
            {
                entry.insert(o.start_ms);
            }
        }
    }
    valid
}

/// Prune stale rows, then run the state machine over the due set:
///   - calendar hidden RIGHT NOW → пропускаем, строку не трогаем;
///   - occurrence over        → expire silently (spec: «прошло — тихо удаляем»);
///   - occurrence in progress → ONE «уже идёт» toast per occurrence, the
///     rest of its cascade is cancelled;
///   - otherwise              → show as Soon (lead > 0) or AtStart (lead 0).
/// Every returned row is already marked `shown`.
///
/// `hidden` — последняя линия обороны: посев скрытые календари пропускает, а
/// переключатель видимости снимает уже взведённое, но любая щель в этой паре
/// (строка вне загруженного окна, событие, переехавшее между календарями,
/// выключение до первого фетча) выпускала тост из выключенного календаря.
/// Проверка в момент выстрела закрывает класс целиком. Строка при этом
/// остаётся взведённой: включат календарь обратно — зазвонит сама, без
/// пересева. Отбраковка идёт ДО дедупа: иначе строка скрытого календаря
/// «съедала» бы логическую оккурренцию и глушила ту же встречу в видимом
/// календаре.
pub fn scan(cache: &Cache, now_ms: i64, hidden: &dyn Fn(&ReminderRow) -> bool) -> Vec<DueToast> {
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
    // A logical occurrence is (start, summary): the same meeting can carry
    // two event_ids (mirrored across calendars, or master + override). The
    // first time such an occurrence comes due we retire every other id's
    // rows so only one cascade — and one toast — survives.
    let mut logical_seen: std::collections::HashSet<(i64, String)> = std::collections::HashSet::new();
    for row in due {
        if hidden(&row) {
            continue;
        }
        let logical = (row.occurrence_start_ms, row.summary.clone());
        if logical_seen.insert(logical.clone()) {
            let _ = cache.dedup_occurrence(row.event_id, row.occurrence_start_ms, &row.summary);
        } else {
            // A duplicate that dedup_occurrence just retired but this scan had
            // already fetched — skip it (its row is now 'done').
            continue;
        }
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
        event_in(id, 1, summary, dtstart, leads)
    }

    fn event_in(
        id: i64,
        calendar_id: i64,
        summary: &str,
        dtstart: i64,
        leads: &[i32],
    ) -> DesktopCalendarEvent {
        serde_json::from_value(serde_json::json!({
            "id": id, "calendar_id": calendar_id, "uid": format!("uid-{id}"),
            "summary": summary, "dtstart": dtstart, "dtend": dtstart + 3_600_000,
            "all_day": false, "alarm_leads": leads,
        }))
        .expect("event")
    }

    fn no_hidden(_: i64) -> bool {
        false
    }

    /// Скан без фильтра видимости — «все календари включены».
    fn nothing_hidden(_: &ReminderRow) -> bool {
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
        let due = scan(&cache, now, &nothing_hidden);
        assert_eq!(due.len(), 1);
        assert_eq!(due[0].row.seq, 0);
        assert_eq!(due[0].mode, ToastMode::Soon);
        // Secondary stays chained until the primary's toast TIMES OUT.
        assert_eq!(scan(&cache, now, &nothing_hidden).len(), 0);
        cache.reminder_timeout(1, start, 0).unwrap();
        // Now armed; due at start.
        assert_eq!(scan(&cache, now, &nothing_hidden).len(), 0, "at-start not due yet");
        let due2 = scan(&cache, start, &nothing_hidden);
        assert_eq!(due2.len(), 1);
        assert_eq!(due2[0].row.seq, 1);
    }

    #[test]
    fn cross_kills_the_whole_cascade() {
        let cache = temp_cache();
        let now = 2_000_000_000_000;
        let start = now + 5 * 60_000;
        seed(&cache, &[event(2, "Kill", start, &[10, 5, 0])], &no_hidden, now);
        assert_eq!(scan(&cache, now, &nothing_hidden).len(), 1);
        cache.cancel_occurrence_reminders(2, start).unwrap();
        cache.reminder_timeout(2, start, 0).unwrap(); // stray timeout after ✕
        assert_eq!(scan(&cache, start, &nothing_hidden).len(), 0, "✕ is irreversible");
    }

    #[test]
    fn user_choice_replaces_cascade() {
        let cache = temp_cache();
        let now = 3_000_000_000_000;
        let start = now + 30 * 60_000;
        seed(&cache, &[event(3, "Snooze", start, &[25, 0])], &no_hidden, now);
        assert_eq!(scan(&cache, now + 5 * 60_000, &nothing_hidden).len(), 1); // primary at -25m
        cache
            .user_choice_reminder(3, start, start + 3_600_000, now + 10 * 60_000, false, "Snooze")
            .unwrap();
        let due = scan(&cache, now + 10 * 60_000, &nothing_hidden);
        assert_eq!(due.len(), 1);
        assert_eq!(due[0].row.seq, 100);
        // The event-defined at-start alarm never fires afterwards.
        cache.reminder_timeout(3, start, 100).unwrap();
        assert_eq!(scan(&cache, start, &nothing_hidden).len(), 0);
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
        let due = scan(&cache, wake, &nothing_hidden);
        let modes: Vec<_> = due.iter().map(|d| (d.row.event_id, d.mode)).collect();
        assert!(modes.contains(&(4, ToastMode::AlreadyRunning)));
        // Event 5 (1h long) is also still running at wake… make it truly past:
        let wake2 = past_start + 2 * 3_600_000;
        let due2 = scan(&cache, wake2, &nothing_hidden);
        assert!(due2.iter().all(|d| d.row.event_id != 5), "ended events stay silent");
    }

    #[test]
    fn same_meeting_two_event_ids_fires_once() {
        let cache = temp_cache();
        let now = 6_500_000_000_000;
        let start = now + 5 * 60_000;
        // Same meeting mirrored across two calendars → two event_ids, same
        // title + start. Both seed an armed seq-0.
        seed(&cache, &[event(10, "Синк", start, &[10])], &no_hidden, now);
        seed(&cache, &[event(20, "Синк", start, &[10])], &no_hidden, now);
        let due = scan(&cache, now, &nothing_hidden);
        assert_eq!(due.len(), 1, "one logical occurrence → one toast");
        // The retired duplicate never resurfaces on later scans either.
        assert_eq!(scan(&cache, now, &nothing_hidden).len(), 0);
    }

    #[test]
    fn moved_event_drops_stale_occurrence() {
        let cache = temp_cache();
        let now = 7_000_000_000_000;
        let start12 = now + 60 * 60_000;
        // Посеяли на «12:00»…
        seed(&cache, &[event(7, "Move12to13", start12, &[10])], &no_hidden, now);
        // …событие перенесли на час позже; пришёл полный фетч окна.
        let start13 = start12 + 60 * 60_000;
        let moved = event(7, "Move12to13", start13, &[10]);
        seed(&cache, &[moved.clone()], &no_hidden, now);
        let valid = valid_starts(&[moved], now, now + 7 * 24 * 3_600_000);
        let affected = cache
            .prune_moved_reminders(now, now + 7 * 24 * 3_600_000, &valid)
            .unwrap();
        assert_eq!(affected, vec![7], "stale occurrence culled");
        // Старое время молчит, новое стреляет.
        assert_eq!(scan(&cache, start12 - 10 * 60_000, &nothing_hidden).len(), 0, "old slot silent");
        let due = scan(&cache, start13 - 10 * 60_000, &nothing_hidden);
        assert_eq!(due.len(), 1, "new slot fires");
        assert_eq!(due[0].row.occurrence_start_ms, start13);
    }

    #[test]
    fn user_choice_survives_event_id_churn() {
        let cache = temp_cache();
        let now = 8_000_000_000_000;
        let start = now + 10 * 60_000; // созвон через 10 минут
        // Встреча под id 30: штатный -10 срабатывает сразу.
        seed(&cache, &[event(30, "Синк", start, &[10])], &no_hidden, now);
        assert_eq!(scan(&cache, now, &nothing_hidden).len(), 1);
        // «В момент начала»: ручная строка seq=100 на start.
        cache
            .user_choice_reminder(30, start, start + 3_600_000, start, true, "Синк")
            .unwrap();
        assert_eq!(scan(&cache, now + 4 * 60_000, &nothing_hidden).len(), 0, "тихо после снуза");

        // Ресинк сервера пересоздал ту же встречу под НОВЫМ id 31 (churn).
        seed(&cache, &[event(31, "Синк", start, &[10])], &no_hidden, now + 4 * 60_000);
        // Новый id НЕ должен переармить просроченный -10.
        assert_eq!(
            scan(&cache, now + 4 * 60_000, &nothing_hidden).len(),
            0,
            "churned id не воскрешает пре-аларм"
        );

        // Настоящий ресинк ещё и удаляет исчезнувший старый id — решение уже
        // должно жить под новым id к этому моменту.
        let keep: std::collections::HashSet<i64> = [31].into_iter().collect();
        cache
            .prune_orphan_reminders(now, now + 30 * 24 * 3_600_000, &keep)
            .unwrap();

        // Выбор пользователя срабатывает ровно один раз, в момент начала
        // (в этот момент occurrence_start ≤ now → презентация «уже идёт»,
        // как и у штатного at-start будильника).
        let due = scan(&cache, start, &nothing_hidden);
        assert_eq!(due.len(), 1, "ручное напоминание пережило churn + prune");
        assert_eq!(due[0].row.occurrence_start_ms, start);
        assert_eq!(due[0].mode, ToastMode::AlreadyRunning);
        // И только один раз — повторный скан молчит.
        assert_eq!(scan(&cache, start, &nothing_hidden).len(), 0, "без повторов");
    }

    #[test]
    fn dismissal_survives_event_id_churn() {
        let cache = temp_cache();
        let now = 9_000_000_000_000;
        let start = now + 10 * 60_000;
        seed(&cache, &[event(40, "Спам-инвайт", start, &[10])], &no_hidden, now);
        assert_eq!(scan(&cache, now, &nothing_hidden).len(), 1);
        // ✕ — убить всю оккурренцию.
        cache.cancel_occurrence_reminders(40, start).unwrap();
        // Новый id той же встречи не должен воскресить напоминание.
        seed(&cache, &[event(41, "Спам-инвайт", start, &[10])], &no_hidden, now + 60_000);
        assert_eq!(scan(&cache, start - 60_000, &nothing_hidden).len(), 0, "✕ переживает churn");
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
        assert_eq!(scan(&cache, start - 60_000, &nothing_hidden).len(), 0);
        // The event moved → full reset, reminders live again.
        let start2 = start + 3_600_000;
        seed(&cache, &[event(6, "Move", start2, &[10])], &no_hidden, now);
        let due = scan(&cache, start2 - 60_000, &nothing_hidden);
        assert_eq!(due.len(), 1, "changed event re-arms as if new");
    }

    #[test]
    fn disabling_a_calendar_silences_what_was_already_armed() {
        let cache = temp_cache();
        let now = 6_000_000_000_000;
        let start = now + 20 * 60_000;
        // Два календаря, по событию в каждом — оба взведены.
        let events = vec![
            event_in(70, 7, "Планёрка", start, &[10]),
            event_in(80, 8, "Созвон", start + 60_000, &[10]),
        ];
        seed(&cache, &events, &no_hidden, now);

        // Выключили календарь 7: его строки сняты, чужие целы.
        let hit = cache.purge_calendar_reminders(7).expect("purge");
        assert_eq!(hit, vec![70], "гасим только события выключенного календаря");
        let due = scan(&cache, start + 60_000, &nothing_hidden);
        assert_eq!(due.len(), 1, "звонит лишь оставшийся календарь");
        assert_eq!(due[0].row.event_id, 80);

        // Включили обратно — пересев из того же снимка возвращает напоминание.
        seed(&cache, &events, &no_hidden, now);
        let due = scan(&cache, start + 60_000, &nothing_hidden);
        assert_eq!(due.len(), 1);
        assert_eq!(due[0].row.event_id, 70, "включённый календарь снова звонит");
    }

    #[test]
    fn purge_by_calendar_reaches_rows_seeded_before_the_column() {
        let cache = temp_cache();
        let now = 6_100_000_000_000;
        let start = now + 20 * 60_000;
        let ev = event_in(90, 9, "Ретро", start, &[10]);
        seed(&cache, std::slice::from_ref(&ev), &no_hidden, now);
        // Иммитируем строку из старой схемы: календарь не проставлен.
        cache.debug_clear_reminder_calendar(90).expect("clear");
        assert!(
            cache.purge_calendar_reminders(9).expect("purge").is_empty(),
            "строка без календаря на выключение не реагирует"
        );
        // Пересев проставляет привязку даже без изменений в событии...
        seed(&cache, std::slice::from_ref(&ev), &no_hidden, now);
        assert_eq!(cache.purge_calendar_reminders(9).expect("purge"), vec![90]);
        assert_eq!(scan(&cache, start, &nothing_hidden).len(), 0);
    }

    /// Строка взведена, календарь выключили — стрелять нельзя даже если
    /// строку никто не снял (щели: событие вне загруженного окна, выключение
    /// до первого фетча, переезд события между календарями).
    #[test]
    fn hidden_calendar_never_fires_even_if_its_row_survived() {
        let cache = temp_cache();
        let now = 6_200_000_000_000;
        let start = now + 20 * 60_000;
        let events = vec![
            event_in(100, 11, "Планёрка", start, &[10]),
            event_in(110, 12, "Созвон", start + 60_000, &[10]),
        ];
        seed(&cache, &events, &no_hidden, now);

        // Календарь 11 скрыт на клиенте, purge по нему НЕ звали.
        let hide_11 = |r: &ReminderRow| r.calendar_id == 11;
        let due = scan(&cache, start + 60_000, &hide_11);
        assert_eq!(due.len(), 1, "звонит только видимый календарь");
        assert_eq!(due[0].row.event_id, 110);

        // Строка скрытого осталась взведённой: включили — зазвонила, без пересева.
        let due = scan(&cache, start + 60_000, &nothing_hidden);
        assert_eq!(due.len(), 1);
        assert_eq!(due[0].row.event_id, 100, "включённый календарь звонит сам");
    }

    /// Та же встреча в скрытом и в видимом календаре: отбраковка скрытой
    /// строки идёт ДО дедупа логической оккурренции — иначе скрытая «съедала»
    /// бы встречу и видимый календарь молчал.
    #[test]
    fn hidden_twin_does_not_swallow_the_visible_one() {
        let cache = temp_cache();
        let now = 6_300_000_000_000;
        let start = now + 20 * 60_000;
        // Меньший event_id (скрытый) придёт из due_reminders первым: одинаковый
        // fire_at, порядок добивается сортировкой вставки.
        let events = vec![
            event_in(120, 11, "Синк", start, &[10]),
            event_in(130, 12, "Синк", start, &[10]),
        ];
        seed(&cache, &events, &no_hidden, now);

        let hide_11 = |r: &ReminderRow| r.calendar_id == 11;
        let due = scan(&cache, start - 5 * 60_000, &hide_11);
        assert_eq!(due.len(), 1, "видимый близнец звонит");
        assert_eq!(due[0].row.event_id, 130);
    }
}
