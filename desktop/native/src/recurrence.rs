//! Minimal RRULE expander for the calendar week-grid.
//!
//! The server hands the desktop client the *master* of a recurring event
//! (its original DTSTART, the raw `RRULE`, and the list of cancelled
//! `EXDATE` starts) and expects the client to expand occurrences into the
//! window it is displaying. We deliberately avoid pulling in a full RRULE
//! crate (no network dep, and our calendars only ever produce a small slice
//! of RFC-5545): this covers `FREQ=DAILY|WEEKLY|MONTHLY|YEARLY` with
//! `INTERVAL`, `COUNT`, `UNTIL`, and `BYDAY` (the weekly multi-day case),
//! which is everything Yandex / Google / Apple emit for ordinary events.
//!
//! Time handling mirrors the grid: occurrences are stepped in UTC so they
//! land in the same day-buckets `apply_calendar_view` computes from
//! `dtstart` ms. DST-correct wall-clock stepping needs a per-event TZID the
//! payload doesn't carry yet — tracked as a separate refinement.

use chrono::{DateTime, Datelike, Duration, TimeZone, Timelike, Utc, Weekday};

/// One concrete occurrence: absolute start/end in ms-since-epoch.
#[derive(Debug, Clone, Copy, PartialEq)]
pub struct Occurrence {
    pub start_ms: i64,
    pub end_ms: i64,
}

/// Hard cap on how many candidates we step through, so a malformed or
/// open-ended rule can never spin forever. A daily event running for ten
/// years is ~3650 candidates; 20k leaves generous headroom.
const MAX_ITER: usize = 20_000;

#[derive(Debug, Clone, Copy, PartialEq)]
enum Freq {
    Daily,
    Weekly,
    Monthly,
    Yearly,
}

struct Rule {
    freq: Freq,
    interval: i64,
    count: Option<usize>,
    until_ms: Option<i64>,
    /// Weekly BYDAY set (empty = recur on the master's own weekday).
    byday: Vec<Weekday>,
}

/// Expand `rrule` (possibly empty) into the occurrences that overlap
/// `[win_start_ms, win_end_ms)`, with `exdates` removed.
///
/// A blank rule yields the single master occurrence when it overlaps the
/// window. The returned occurrences carry the master's duration.
pub fn expand(
    dtstart_ms: i64,
    dtend_ms: Option<i64>,
    rrule: &str,
    exdates: &[i64],
    win_start_ms: i64,
    win_end_ms: i64,
) -> Vec<Occurrence> {
    let dur_ms = dtend_ms
        .map(|e| (e - dtstart_ms).max(0))
        .unwrap_or(30 * 60 * 1000);

    let overlaps = |start: i64| -> bool {
        let end = start + dur_ms;
        end > win_start_ms && start < win_end_ms
    };
    let excluded = |start: i64| -> bool { exdates.iter().any(|&x| x == start) };
    let emit = |start: i64| -> Occurrence {
        Occurrence {
            start_ms: start,
            end_ms: start + dur_ms,
        }
    };

    let Some(rule) = parse_rule(rrule) else {
        // Non-recurring (or unparseable): just the master.
        return if overlaps(dtstart_ms) && !excluded(dtstart_ms) {
            vec![emit(dtstart_ms)]
        } else {
            vec![]
        };
    };

    let mut out = Vec::new();
    // `produced` counts every occurrence the series has yielded from its
    // start — that, not the in-window count, is what COUNT bounds.
    let mut produced = 0usize;
    let mut push = |start: i64, out: &mut Vec<Occurrence>| {
        if overlaps(start) && !excluded(start) {
            out.push(emit(start));
        }
    };

    if rule.freq == Freq::Weekly && !rule.byday.is_empty() {
        // Anchor on the Monday of the master's week, then walk week blocks
        // `interval` apart, emitting the selected weekdays in order.
        let start_dt = Utc.timestamp_millis_opt(dtstart_ms).single();
        let Some(start_dt) = start_dt else { return out };
        let monday = start_dt.date_naive()
            - Duration::days(start_dt.weekday().num_days_from_monday() as i64);
        let tod_ms = (start_dt.hour() as i64 * 3600
            + start_dt.minute() as i64 * 60
            + start_dt.second() as i64)
            * 1000;
        let mut byday = rule.byday.clone();
        byday.sort_by_key(|d| d.num_days_from_monday());

        let mut block = 0i64;
        for _ in 0..MAX_ITER {
            let week_start = monday + Duration::weeks(block * rule.interval);
            let mut any_emitted_this_block = false;
            for wd in &byday {
                let day = week_start + Duration::days(wd.num_days_from_monday() as i64);
                let start = Utc
                    .from_utc_datetime(&day.and_hms_opt(0, 0, 0).unwrap())
                    .timestamp_millis()
                    + tod_ms;
                if start < dtstart_ms {
                    continue; // before the series actually begins
                }
                if let Some(until) = rule.until_ms {
                    if start > until {
                        return out;
                    }
                }
                if let Some(c) = rule.count {
                    if produced >= c {
                        return out;
                    }
                }
                if start > win_end_ms && produced > 0 {
                    return out;
                }
                produced += 1;
                any_emitted_this_block = true;
                push(start, &mut out);
            }
            // Once we're emitting past the window, the next block is too.
            if !any_emitted_this_block && week_start > start_dt.date_naive() {
                if Utc
                    .from_utc_datetime(&week_start.and_hms_opt(0, 0, 0).unwrap())
                    .timestamp_millis()
                    > win_end_ms
                {
                    break;
                }
            }
            block += 1;
        }
        return out;
    }

    // Simple stepping: the n-th occurrence is the master advanced by
    // n * interval of the frequency unit.
    for i in 0..MAX_ITER {
        let Some(start) = nth_simple(dtstart_ms, &rule, i as i64) else {
            // Invalid civil date (e.g. the 31st in a 30-day month) — RFC
            // says skip it, but it still counts toward neither COUNT nor
            // the iteration budget meaningfully; just move on.
            continue;
        };
        if let Some(until) = rule.until_ms {
            if start > until {
                break;
            }
        }
        if let Some(c) = rule.count {
            if i >= c {
                break;
            }
        }
        if start > win_end_ms {
            break;
        }
        push(start, &mut out);
    }
    out
}

/// The n-th occurrence start for the non-BYDAY frequencies. Returns None for
/// calendar dates that don't exist (skipped per RFC-5545).
fn nth_simple(dtstart_ms: i64, rule: &Rule, n: i64) -> Option<i64> {
    match rule.freq {
        Freq::Daily => Some(dtstart_ms + n * rule.interval * 86_400_000),
        Freq::Weekly => Some(dtstart_ms + n * rule.interval * 7 * 86_400_000),
        Freq::Monthly => add_months(dtstart_ms, n * rule.interval),
        Freq::Yearly => add_months(dtstart_ms, n * rule.interval * 12),
    }
}

/// Add `months` calendar months to a ms timestamp, preserving the time of
/// day. Returns None when the target month has no such day-of-month.
fn add_months(ms: i64, months: i64) -> Option<i64> {
    let dt: DateTime<Utc> = Utc.timestamp_millis_opt(ms).single()?;
    let total = (dt.year() as i64) * 12 + (dt.month0() as i64) + months;
    let year = total.div_euclid(12) as i32;
    let month0 = total.rem_euclid(12) as u32;
    let date = chrono::NaiveDate::from_ymd_opt(year, month0 + 1, dt.day())?;
    let naive = date.and_hms_opt(dt.hour(), dt.minute(), dt.second())?;
    Some(Utc.from_utc_datetime(&naive).timestamp_millis())
}

fn parse_rule(rrule: &str) -> Option<Rule> {
    let s = rrule.trim();
    if s.is_empty() {
        return None;
    }
    // Tolerate an "RRULE:" prefix even though the payload usually omits it.
    let s = s.strip_prefix("RRULE:").unwrap_or(s);

    let mut freq = None;
    let mut interval = 1i64;
    let mut count = None;
    let mut until_ms = None;
    let mut byday = Vec::new();

    for part in s.split(';') {
        let (k, v) = part.split_once('=')?;
        match k.trim().to_ascii_uppercase().as_str() {
            "FREQ" => {
                freq = Some(match v.trim().to_ascii_uppercase().as_str() {
                    "DAILY" => Freq::Daily,
                    "WEEKLY" => Freq::Weekly,
                    "MONTHLY" => Freq::Monthly,
                    "YEARLY" => Freq::Yearly,
                    _ => return None, // unsupported (HOURLY/MINUTELY/…)
                });
            }
            "INTERVAL" => interval = v.trim().parse().ok().filter(|&n| n > 0).unwrap_or(1),
            "COUNT" => count = v.trim().parse::<usize>().ok(),
            "UNTIL" => until_ms = parse_until(v.trim()),
            "BYDAY" => {
                for tok in v.split(',') {
                    // Strip any ordinal prefix (e.g. "2MO", "-1FR") — we
                    // don't honour the ordinal, but the weekday still scopes
                    // the rule better than dropping it.
                    let wd = tok.trim_matches(|c: char| c == '-' || c.is_ascii_digit());
                    if let Some(d) = weekday_from_str(wd) {
                        byday.push(d);
                    }
                }
            }
            _ => {} // BYMONTHDAY/BYSETPOS/WKST/… ignored for now
        }
    }

    Some(Rule {
        freq: freq?,
        interval,
        count,
        until_ms,
        byday,
    })
}

/// Parse an RFC-5545 UNTIL value to ms. Accepts `YYYYMMDD`,
/// `YYYYMMDDTHHMMSS`, and `YYYYMMDDTHHMMSSZ` (all treated as UTC).
fn parse_until(v: &str) -> Option<i64> {
    let v = v.trim_end_matches('Z');
    let (date, time) = match v.split_once('T') {
        Some((d, t)) => (d, Some(t)),
        None => (v, None),
    };
    if date.len() != 8 {
        return None;
    }
    let year: i32 = date[0..4].parse().ok()?;
    let month: u32 = date[4..6].parse().ok()?;
    let day: u32 = date[6..8].parse().ok()?;
    let (h, m, s) = match time {
        Some(t) if t.len() >= 6 => (
            t[0..2].parse().ok()?,
            t[2..4].parse().ok()?,
            t[4..6].parse().ok()?,
        ),
        // Date-only UNTIL is inclusive of the whole day.
        _ => (23, 59, 59),
    };
    let naive = chrono::NaiveDate::from_ymd_opt(year, month, day)?.and_hms_opt(h, m, s)?;
    Some(Utc.from_utc_datetime(&naive).timestamp_millis())
}

fn weekday_from_str(s: &str) -> Option<Weekday> {
    match s.trim().to_ascii_uppercase().as_str() {
        "MO" => Some(Weekday::Mon),
        "TU" => Some(Weekday::Tue),
        "WE" => Some(Weekday::Wed),
        "TH" => Some(Weekday::Thu),
        "FR" => Some(Weekday::Fri),
        "SA" => Some(Weekday::Sat),
        "SU" => Some(Weekday::Sun),
        _ => None,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    // 2026-06-08 09:00:00 UTC (a Monday) as ms.
    fn mon_0900() -> i64 {
        Utc.with_ymd_and_hms(2026, 6, 8, 9, 0, 0)
            .unwrap()
            .timestamp_millis()
    }
    fn day(n: i64) -> i64 {
        n * 86_400_000
    }

    #[test]
    fn non_recurring_in_window() {
        let s = mon_0900();
        let occ = expand(s, Some(s + 3_600_000), "", &[], s - day(1), s + day(1));
        assert_eq!(occ.len(), 1);
        assert_eq!(occ[0].start_ms, s);
        assert_eq!(occ[0].end_ms, s + 3_600_000);
    }

    #[test]
    fn non_recurring_out_of_window() {
        let s = mon_0900();
        let occ = expand(s, None, "", &[], s + day(5), s + day(7));
        assert!(occ.is_empty());
    }

    #[test]
    fn daily_master_in_past_still_fills_window() {
        // Master a month ago; window is one day. Must still produce that day.
        let s = mon_0900();
        let win_start = s + day(30);
        let win_end = s + day(31);
        let occ = expand(s, Some(s + 3_600_000), "FREQ=DAILY", &[], win_start, win_end);
        assert_eq!(occ.len(), 1, "exactly one daily occurrence in a 1-day window");
        assert_eq!(occ[0].start_ms, s + day(30));
    }

    #[test]
    fn daily_interval_and_count() {
        let s = mon_0900();
        // Every 2 days, 3 times total: s, s+2d, s+4d.
        let occ = expand(s, None, "FREQ=DAILY;INTERVAL=2;COUNT=3", &[], s - day(1), s + day(30));
        let starts: Vec<i64> = occ.iter().map(|o| o.start_ms).collect();
        assert_eq!(starts, vec![s, s + day(2), s + day(4)]);
    }

    #[test]
    fn weekly_until_bound() {
        let s = mon_0900();
        // Weekly for ~2.5 weeks via UNTIL: occurrences at s, +7d, +14d.
        let until = s + day(16); // between the 3rd and 4th
        let rule = format!(
            "FREQ=WEEKLY;UNTIL={}",
            Utc.timestamp_millis_opt(until)
                .unwrap()
                .format("%Y%m%dT%H%M%SZ")
        );
        let occ = expand(s, None, &rule, &[], s - day(1), s + day(60));
        assert_eq!(occ.len(), 3);
        assert_eq!(occ[2].start_ms, s + day(14));
    }

    #[test]
    fn weekly_byday_multi() {
        let s = mon_0900(); // Monday
        // Mon/Wed/Fri for the master week; window = that week.
        let occ = expand(
            s,
            None,
            "FREQ=WEEKLY;BYDAY=MO,WE,FR",
            &[],
            s,
            s + day(7),
        );
        let starts: Vec<i64> = occ.iter().map(|o| o.start_ms).collect();
        assert_eq!(starts, vec![s, s + day(2), s + day(4)]);
    }

    #[test]
    fn exdate_removes_instance() {
        let s = mon_0900();
        let occ = expand(
            s,
            None,
            "FREQ=DAILY;COUNT=3",
            &[s + day(1)], // cancel the middle instance
            s - day(1),
            s + day(10),
        );
        let starts: Vec<i64> = occ.iter().map(|o| o.start_ms).collect();
        assert_eq!(starts, vec![s, s + day(2)]);
    }

    #[test]
    fn monthly_keeps_day_of_month() {
        // 2026-01-31; monthly. Feb has no 31st → skipped; Mar 31 present.
        let jan31 = Utc.with_ymd_and_hms(2026, 1, 31, 9, 0, 0).unwrap().timestamp_millis();
        let win_end = Utc.with_ymd_and_hms(2026, 4, 1, 0, 0, 0).unwrap().timestamp_millis();
        let occ = expand(jan31, None, "FREQ=MONTHLY", &[], jan31, win_end);
        let months: Vec<u32> = occ
            .iter()
            .map(|o| Utc.timestamp_millis_opt(o.start_ms).unwrap().month())
            .collect();
        assert_eq!(months, vec![1, 3], "Feb 31 skipped, Jan and Mar kept");
    }

    #[test]
    fn yearly_steps_a_year() {
        let s = Utc.with_ymd_and_hms(2024, 2, 29, 12, 0, 0).unwrap().timestamp_millis();
        let win_end = Utc.with_ymd_and_hms(2029, 1, 1, 0, 0, 0).unwrap().timestamp_millis();
        // Leap-day yearly: only leap years have Feb 29 → 2024, 2028.
        let occ = expand(s, None, "FREQ=YEARLY", &[], s, win_end);
        let years: Vec<i32> = occ
            .iter()
            .map(|o| Utc.timestamp_millis_opt(o.start_ms).unwrap().year())
            .collect();
        assert_eq!(years, vec![2024, 2028]);
    }
}
