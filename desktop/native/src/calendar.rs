//! The calendar view (`view-mode 1`): week grid geometry and zoom, event
//! blocks and overlap lanes, the calendar list with colours and visibility,
//! and the settings it persists.

use super::*;

/// Calendar palette, modelled on Google Calendar's event colours — the
/// reference design for "distinct, calm, and readable with white text on
/// event blocks". Their Banana yellow is swapped for a darker amber (white
/// text drowns on yellow).
pub(crate) const CAL_PALETTE: [&str; 12] = [
    "#D50000", // tomato
    "#E67C73", // flamingo
    "#F4511E", // tangerine
    "#F09300", // amber
    "#33B679", // sage
    "#0B8043", // basil
    "#039BE5", // peacock
    "#3F51B5", // blueberry
    "#7986CB", // lavender
    "#8E24AA", // grape
    "#616161", // graphite
    "#009688", // teal
];

/// Stable default colour for a calendar that the server gave no colour for.
/// Keyed on the calendar id via a multiplicative hash so the mapping is
/// deterministic across sessions and spreads ids across the palette.
pub(crate) fn default_cal_color(id: i64) -> &'static str {
    let idx = ((id.unsigned_abs().wrapping_mul(2_654_435_761)) >> 16) as usize % CAL_PALETTE.len();
    CAL_PALETTE[idx]
}

/// Colour to actually paint a calendar with. The CalDAV import stamps most
/// calendars with the generic placeholder `#3788d8`, so we treat that (and
/// an empty value) as "no real colour" and fall back to our distinct
/// per-calendar palette; a genuinely customised colour is kept.
pub(crate) fn cal_color(id: i64, server_color: &str) -> String {
    if server_color.is_empty() || server_color.eq_ignore_ascii_case("#3788d8") {
        default_cal_color(id).to_string()
    } else {
        server_color.to_string()
    }
}

/// Days-since-epoch for the Monday of the calendar week containing
/// "today" (local time).
pub(crate) fn week_start_days_today() -> i64 {
    use chrono::{Datelike, Duration, Local};
    let today = Local::now().date_naive();
    let from_mon = today.weekday().num_days_from_monday() as i64;
    let monday = today - Duration::days(from_mon);
    monday.signed_duration_since(chrono::NaiveDate::from_ymd_opt(1970, 1, 1).unwrap()).num_days()
}

/// Same, but for the week containing an arbitrary timestamp — used to
/// navigate the calendar to a reminder's occurrence.
pub(crate) fn week_start_days_for_ms(ms: i64) -> i64 {
    use chrono::{Datelike, Duration, Local, TimeZone};
    let date = Local
        .timestamp_millis_opt(ms)
        .single()
        .map(|t| t.date_naive())
        .unwrap_or_else(|| Local::now().date_naive());
    let monday = date - Duration::days(date.weekday().num_days_from_monday() as i64);
    monday.signed_duration_since(chrono::NaiveDate::from_ymd_opt(1970, 1, 1).unwrap()).num_days()
}

/// Side-by-side lanes for overlapping blocks within a day column: events
/// that share time split the column into equal-width lanes, like every
/// desktop calendar. Without this, same-time events from different
/// calendars paint over each other and only the topmost stays visible.
pub(crate) fn assign_overlap_lanes(blocks: &mut [EventBlock], day_count: i32) {
    // Assign greedy lanes within one cluster of transitively-overlapping
    // blocks, then split the column between the lanes used.
    fn flush(blocks: &mut [EventBlock], cluster: &mut Vec<usize>) {
        if cluster.is_empty() {
            return;
        }
        let mut lane_ends: Vec<f32> = Vec::new();
        let mut lane_of: Vec<usize> = Vec::with_capacity(cluster.len());
        for &i in cluster.iter() {
            let top = blocks[i].top;
            let lane = match lane_ends.iter().position(|&e| e <= top) {
                Some(l) => l,
                None => {
                    lane_ends.push(f32::MIN);
                    lane_ends.len() - 1
                }
            };
            lane_ends[lane] = top + blocks[i].h;
            lane_of.push(lane);
        }
        let n = lane_ends.len() as f32;
        for (k, &i) in cluster.iter().enumerate() {
            blocks[i].xf = lane_of[k] as f32 / n;
            blocks[i].wf = 1.0 / n;
        }
        cluster.clear();
    }

    for day in 0..day_count {
        let mut idx: Vec<usize> = (0..blocks.len()).filter(|&i| blocks[i].day == day).collect();
        idx.sort_by(|&a, &b| {
            blocks[a].top.partial_cmp(&blocks[b].top).unwrap_or(std::cmp::Ordering::Equal)
        });
        let mut cluster: Vec<usize> = Vec::new();
        let mut cluster_end = f32::MIN;
        for i in idx {
            if !cluster.is_empty() && blocks[i].top >= cluster_end {
                flush(blocks, &mut cluster);
                cluster_end = f32::MIN;
            }
            cluster_end = cluster_end.max(blocks[i].top + blocks[i].h);
            cluster.push(i);
        }
        flush(blocks, &mut cluster);
    }
}

/// Absolute ms of local midnight for the given calendar date. None only if
/// the local timezone genuinely has no midnight that day (DST gap).
pub(crate) fn local_midnight_ms(date: chrono::NaiveDate) -> Option<i64> {
    use chrono::{Local, TimeZone};
    Local.from_local_datetime(&date.and_hms_opt(0, 0, 0)?).single().map(|d| d.timestamp_millis())
}

/// Compute the [from_ms, to_ms) range covering the displayed week
/// (5 or 7 days, full 24h regardless of hour-toggle), anchored to LOCAL
/// Monday midnight — must match apply_calendar_view's window.
pub(crate) fn week_range_ms(week_start_days: i64, day_count: i32) -> (i64, i64) {
    use chrono::Duration;
    let day_ms: i64 = 24 * 60 * 60 * 1000;
    let monday =
        chrono::NaiveDate::from_ymd_opt(1970, 1, 1).unwrap() + Duration::days(week_start_days);
    let from = local_midnight_ms(monday).unwrap_or(week_start_days * day_ms);
    let to = from + day_count as i64 * day_ms;
    (from, to)
}

/// Snapshot the current calendar-view preferences into calendar.json.
/// Called immediately after every change (visibility / colour /
/// day- and hour-range toggles) — never deferred to exit.
pub(crate) fn save_calendar_settings(ui: &MainWindow, sh: &Shared) {
    let hidden: Vec<i64> = sh
        .cal
        .calendar_visible
        .borrow()
        .iter()
        .filter(|(_, visible)| !**visible)
        .map(|(id, _)| *id)
        .collect();
    calendar_settings::save(&calendar_settings::CalendarSettings {
        hidden,
        colors: sh.cal.calendar_colors.borrow().clone(),
        notify_sound: ui.get_notify_sound_on(),
        work_start_hour: sh.cal.work_start.get(),
        work_end_hour: sh.cal.work_end.get(),
        manual_hour_height: sh.cal.manual_hour_h.get(),
        manual_col_width: sh.cal.manual_col_w.get(),
        last_conversation: sh.last_conv_id.borrow().clone(),
    });
}

/// Scroll the calendar grid so `hour` (fractional local hours) sits at the
/// top of the viewport. Entering the calendar always lands on the working
/// day, not on 00:00 — with a manual hour-zoom the grid models the full 0–24
/// and would otherwise open on the night hours. In the no-scroll band layout
/// the clamp in the .slint bridge makes this a no-op.
pub(crate) fn scroll_calendar_to_hour(ui: &MainWindow, hour: f32) {
    let top = (hour - ui.get_hour_start() as f32).max(0.0) * ui.get_hour_height();
    ui.set_grid_scroll_y(top);
    // pending: the pane may be instantiating right now (view-mode just
    // switched) — the Flickable then applies this on its first layout.
    ui.set_grid_scroll_pending(true);
    ui.set_grid_scroll_seq(ui.get_grid_scroll_seq() + 1);
    // The conditional pane misses BOTH triggers when it instantiates with
    // its final geometry (second visit, data already cached): the seq bump
    // predates the bridge and the first layout isn't a property *change*.
    // One delayed re-bump lands after instantiation and covers that hole.
    let ui_weak = ui.as_weak();
    slint::Timer::single_shot(std::time::Duration::from_millis(120), move || {
        if let Some(ui) = ui_weak.upgrade() {
            if ui.get_grid_scroll_pending() {
                ui.set_grid_scroll_seq(ui.get_grid_scroll_seq() + 1);
            }
        }
    });
}

/// Hard floors from the spec: an hour cell is never shorter than 52px nor a
/// day column narrower than 300px. The 48px gutter holds the time labels.
pub(crate) const MIN_HOUR_H: f32 = 52.0;

pub(crate) const MIN_COL_W: f32 = 300.0;

pub(crate) const GUTTER_W: f32 = 48.0;

/// Choose day-count + column width for the available width.
///   - manual day-zoom → 7-day content at the chosen width (horizontal scroll)
///   - else 7 days if they fit at ≥300px (filling the width)
///   - else 5 days (Mon–Fri) if they fit
///   - else 5 days at the 300px floor (horizontal scroll)
pub(crate) fn compute_horizontal(canvas_w: f32, manual_col_w: f32) -> (i32, f32) {
    let avail = (canvas_w - GUTTER_W).max(MIN_COL_W);
    if manual_col_w > 0.0 {
        return (7, manual_col_w.clamp(MIN_COL_W, avail));
    }
    if avail >= 7.0 * MIN_COL_W {
        (7, avail / 7.0)
    } else if avail >= 5.0 * MIN_COL_W {
        (5, avail / 5.0)
    } else {
        (5, MIN_COL_W)
    }
}

/// Choose the visible hour band + hour height for the available height.
/// Returns (vis_start, vis_end, hour_height) where the band is [vis_start,
/// vis_end) local hours.
///   - manual hour-zoom → full 0–24 at the chosen height (vertical scroll)
///   - else if 24h fits at ≥52px → fill the canvas with all 24h
///   - else if there are events outside work hours → full 0–24 scroll at 52px
///   - else hide non-work symmetrically: the work window plus as many equal
///     padding hours above/below as fit, filling the canvas (no scroll)
pub(crate) fn compute_vertical(
    canvas_h: f32,
    work_start: i32,
    work_end: i32,
    has_out_of_work: bool,
    manual_hour_h: f32,
) -> (i32, i32, f32) {
    let ws = work_start.clamp(0, 23);
    let we = work_end.clamp(ws + 1, 24);
    let h = canvas_h.max(MIN_HOUR_H);

    if manual_hour_h > 0.0 {
        return (0, 24, manual_hour_h.clamp(MIN_HOUR_H, h));
    }
    if h >= 24.0 * MIN_HOUR_H {
        return (0, 24, h / 24.0); // all day fits — fill
    }
    if has_out_of_work {
        return (0, 24, MIN_HOUR_H); // must show everything — scroll
    }
    // Hide non-work, keep work window + symmetric padding that still fits.
    let work_hours = (we - ws).max(1);
    let fit_rows = (h / MIN_HOUR_H).floor() as i32;
    if fit_rows <= work_hours {
        return (ws, we, MIN_HOUR_H); // even the work window must scroll
    }
    let extra = fit_rows - work_hours;
    let pad = (extra / 2).min(ws).min(24 - we);
    let top = ws - pad;
    let bottom = we + pad;
    let rows = (bottom - top).max(1);
    (top, bottom, h / rows as f32)
}

#[cfg(test)]
mod grid_layout_tests {
    use super::{MIN_COL_W, MIN_HOUR_H, compute_horizontal, compute_vertical};

    #[test]
    fn horizontal_seven_then_five_then_scroll() {
        // Wide enough for 7 columns → 7, filling the width.
        let (d, w) = compute_horizontal(48.0 + 7.0 * 320.0, 0.0);
        assert_eq!(d, 7);
        assert!((w - 320.0).abs() < 0.1);
        // Fits 5 but not 7 → 5 days, filled.
        let (d, w) = compute_horizontal(48.0 + 5.0 * 320.0, 0.0);
        assert_eq!(d, 5);
        assert!(w >= MIN_COL_W);
        // Too narrow even for 5 at the floor → 5 days at the 300px floor.
        let (d, w) = compute_horizontal(48.0 + 3.0 * MIN_COL_W, 0.0);
        assert_eq!(d, 5);
        assert!((w - MIN_COL_W).abs() < 0.1);
    }

    #[test]
    fn horizontal_manual_zoom_is_seven_days_clamped() {
        let (d, w) = compute_horizontal(2000.0, 9999.0);
        assert_eq!(d, 7);
        assert!(w <= 2000.0 - 48.0 + 0.1); // clamped to available width
        let (_, w) = compute_horizontal(2000.0, 100.0);
        assert!((w - MIN_COL_W).abs() < 0.1); // clamped up to the floor
    }

    #[test]
    fn vertical_fills_when_all_day_fits() {
        // Plenty of height → all 24h, filled (hour height > floor).
        let (s, e, hh) = compute_vertical(24.0 * 80.0, 8, 19, false, 0.0);
        assert_eq!((s, e), (0, 24));
        assert!((hh - 80.0).abs() < 0.1);
    }

    #[test]
    fn vertical_scrolls_full_day_when_out_of_work_events() {
        // Can't fit 24h and there ARE out-of-work events → 0–24 at the floor.
        let (s, e, hh) = compute_vertical(10.0 * MIN_HOUR_H, 8, 19, true, 0.0);
        assert_eq!((s, e), (0, 24));
        assert!((hh - MIN_HOUR_H).abs() < 0.1);
    }

    #[test]
    fn vertical_hides_non_work_symmetrically() {
        // Work window 8–19 (11h). Room for ~15 rows → 4 extra → 2 above/below.
        let h = 15.0 * MIN_HOUR_H;
        let (s, e, hh) = compute_vertical(h, 8, 19, false, 0.0);
        assert_eq!(s, 6);
        assert_eq!(e, 21);
        assert!(hh >= MIN_HOUR_H); // fills, no scroll
        assert!((hh - h / 15.0).abs() < 0.1);
    }

    #[test]
    fn vertical_manual_zoom_full_day_scroll() {
        let (s, e, hh) = compute_vertical(600.0, 8, 19, false, 120.0);
        assert_eq!((s, e), (0, 24));
        assert!((hh - 120.0).abs() < 0.1);
    }
}

pub(crate) fn apply_calendar_view(ui: &MainWindow, sh: &Shared) {
    use chrono::{Datelike, Duration, NaiveDate};
    let (day_count, col_width) =
        compute_horizontal(sh.cal.grid_canvas_w.get(), sh.cal.manual_col_w.get());
    ui.set_col_width(col_width);
    // Dash segments per quarter-hour line (24px period), capped so a very
    // wide manual zoom can't spawn an absurd number of rects.
    let dash_count = ((day_count as f32 * col_width) / 24.0).floor().clamp(0.0, 160.0) as i32;
    ui.set_dash_count(dash_count);
    let week_days = sh.cal.calendar_week_start_days.get();
    let monday = NaiveDate::from_ymd_opt(1970, 1, 1).unwrap() + Duration::days(week_days);
    let headers: Vec<slint::SharedString> = (0..day_count as i64)
        .map(|i| {
            let d = monday + Duration::days(i);
            const NAMES: [&str; 7] = ["Пн", "Вт", "Ср", "Чт", "Пт", "Сб", "Вс"];
            let n = NAMES[d.weekday().num_days_from_monday() as usize];
            format!("{n}, {:02}.{:02}", d.day(), d.month()).into()
        })
        .collect();
    ui.set_day_headers(slint::ModelRc::new(slint::VecModel::from(headers)));
    ui.set_day_count(day_count);
    // Mark "today" when it falls inside the displayed week: its column
    // index drives the header highlight + column tint, and the current
    // local time drives the now-line.
    {
        let now = chrono::Local::now();
        let today_days =
            (now.date_naive() - NaiveDate::from_ymd_opt(1970, 1, 1).unwrap()).num_days();
        let col = today_days - week_days;
        let in_view = (0..day_count as i64).contains(&col);
        ui.set_today_col(if in_view { col as i32 } else { -1 });
        use chrono::Timelike;
        ui.set_now_hour(now.hour() as f32 + now.minute() as f32 / 60.0);
    }
    let title = {
        const MONTHS: [&str; 12] = [
            "Январь",
            "Февраль",
            "Март",
            "Апрель",
            "Май",
            "Июнь",
            "Июль",
            "Август",
            "Сентябрь",
            "Октябрь",
            "Ноябрь",
            "Декабрь",
        ];
        format!("{} {}", MONTHS[(monday.month() - 1) as usize], monday.year())
    };
    ui.set_week_title(title.into());

    // Sidebar — calendar list. Sorted by name for stability. User-picked
    // colour overrides win over server colour / palette default.
    let cal_items: Vec<CalendarItem> = {
        let cals = sh.cal.calendars.borrow();
        let visibility = sh.cal.calendar_visible.borrow();
        let overrides = sh.cal.calendar_colors.borrow();
        let mut v: Vec<&ddmail_core::types::DesktopCalendar> = cals.iter().collect();
        v.sort_by(|a, b| a.name.to_lowercase().cmp(&b.name.to_lowercase()));
        v.into_iter()
            .map(|c| CalendarItem {
                id: c.id as i32,
                name: c.name.clone().into(),
                color: hex(&overrides
                    .get(&c.id)
                    .cloned()
                    .unwrap_or_else(|| cal_color(c.id, &c.color)))
                .into(),
                visible: *visibility.get(&c.id).unwrap_or(&true),
            })
            .collect()
    };
    ui.set_calendars(slint::ModelRc::new(slint::VecModel::from(cal_items)));

    // Place event blocks in two passes. Pass 1 expands every event into
    // per-day timed segments + all-day chips, recording whether anything
    // falls outside the work window (drives the vertical layout choice).
    // Pass 2 — after the vertical band is known — turns segments into
    // positioned blocks.
    let day_ms: i64 = 24 * 60 * 60 * 1000;
    // Week window in LOCAL time: the grid's day columns are local days, so
    // the window must start at local Monday midnight, not UTC midnight.
    let week_start_ms = local_midnight_ms(monday).unwrap_or(week_days * day_ms);
    // Always expand a full 7 days so dropping to a 5-day view doesn't lose
    // data and the out-of-work scan stays stable; layout clamps to day_count.
    let week_end_ms = week_start_ms + 7 * day_ms;

    /// One timed segment confined to a single day column.
    struct Seg {
        id: i32,
        day: i32,
        start_in_day: i64, // ms from that day's local midnight
        end_in_day: i64,
        color: slint::Color,
        title: slint::SharedString,
        time: slint::SharedString,
        count: i32,
        tentative: bool,
        writable: bool, // calendar can_write → drag/resize allowed
    }

    let (segs, all_day_blocks, all_day_rows, has_out_of_work, occ_map) = {
        let events = sh.cal.calendar_events.borrow();
        let visibility = sh.cal.calendar_visible.borrow();
        let cals = sh.cal.calendars.borrow();
        let overrides = sh.cal.calendar_colors.borrow();
        let color_for = |cal_id: i64| -> slint::Color {
            let raw = overrides.get(&cal_id).cloned().unwrap_or_else(|| {
                cals.iter()
                    .find(|c| c.id == cal_id)
                    .map(|c| cal_color(c.id, &c.color))
                    .unwrap_or_else(|| "#3788d8".to_string())
            });
            hex(&raw)
        };
        let fmt_hm = |abs_ms: i64| -> String {
            use chrono::{Local, TimeZone, Timelike};
            match Local.timestamp_millis_opt(abs_ms).single() {
                Some(d) => format!("{:02}:{:02}", d.hour(), d.minute()),
                None => "??:??".into(),
            }
        };

        let mut segs: Vec<Seg> = Vec::new();
        let mut occ_map: HashMap<(i32, i32), (i64, i64, bool)> = HashMap::new();
        let mut all_day_blocks: Vec<AllDayBlock> = Vec::new();
        let mut all_day_fill = vec![0i32; day_count.max(0) as usize];
        let idents = sh.identity_colors.borrow();
        let me_key = sh.key.to_lowercase();
        let ws_ms = sh.cal.work_start.get() as i64 * 3_600_000;
        let we_ms = sh.cal.work_end.get() as i64 * 3_600_000;
        let mut has_out_of_work = false;

        for e in events.iter() {
            if !*visibility.get(&e.calendar_id).unwrap_or(&true) {
                continue;
            }
            let color = color_for(e.calendar_id);
            let writable =
                cals.iter().find(|c| c.id == e.calendar_id).map(|c| c.can_write).unwrap_or(false);
            let att_count = e.attendees.len() as i32;
            let tentative = att_count >= 2
                && e.attendees
                    .iter()
                    .find(|a| {
                        let lc = a.email.to_lowercase();
                        lc == me_key || idents.contains_key(&lc)
                    })
                    .map(|a| {
                        let ps = a.partstat.to_uppercase();
                        ps.is_empty() || ps == "NEEDS-ACTION"
                    })
                    .unwrap_or(false);
            let occ = recurrence::expand(
                e.dtstart,
                e.dtend,
                &e.rrule,
                &e.exdates,
                week_start_ms,
                week_end_ms,
            );
            if occ.is_empty() && !e.rrule.is_empty() {
                println!(
                    "[cal] no-occurrence: id={} dtstart={} rrule={:?} exdates={} {:?}",
                    e.id,
                    e.dtstart,
                    e.rrule,
                    e.exdates.len(),
                    e.summary.chars().take(30).collect::<String>()
                );
            }
            for o in occ {
                let first = ((o.start_ms - week_start_ms) / day_ms) as i32;
                let last = (((o.end_ms - 1) - week_start_ms) / day_ms) as i32;
                if e.all_day {
                    for day in first.max(0)..=last.min(day_count - 1) {
                        let idx = day as usize;
                        let row = all_day_fill[idx];
                        all_day_fill[idx] += 1;
                        all_day_blocks.push(AllDayBlock {
                            id: e.id as i32,
                            day,
                            row,
                            color,
                            title: e.summary.clone().into(),
                        });
                    }
                    continue;
                }
                let title: slint::SharedString = if e.summary.is_empty() {
                    "(без названия)".into()
                } else {
                    e.summary.clone().into()
                };
                let time: slint::SharedString =
                    format!("{} – {}", fmt_hm(o.start_ms), fmt_hm(o.end_ms)).into();
                for day in first.max(0)..=last.min(day_count - 1) {
                    let day_start_ms = week_start_ms + day as i64 * day_ms;
                    let start_in_day = (o.start_ms - day_start_ms).max(0);
                    let end_in_day = (o.end_ms - day_start_ms).min(day_ms);
                    if end_in_day <= start_in_day {
                        continue;
                    }
                    if start_in_day < ws_ms || end_in_day > we_ms {
                        has_out_of_work = true;
                    }
                    segs.push(Seg {
                        id: e.id as i32,
                        day,
                        start_in_day,
                        end_in_day,
                        color,
                        title: title.clone(),
                        time: time.clone(),
                        count: att_count,
                        tentative,
                        writable,
                    });
                    // Exact instance bounds for drag-move (recurrence_id +
                    // duration). Recurring → only a scope=single override moves
                    // the day (an "all" dtstart shift keeps BYDAY's weekday).
                    // An override row (non-empty recurrence_id, empty rrule)
                    // must ALSO stay scope=single: patching it as "all" would
                    // rewrite the master series' times with one occurrence's.
                    occ_map.insert(
                        (e.id as i32, day),
                        (o.start_ms, o.end_ms, !e.rrule.is_empty() || !e.recurrence_id.is_empty()),
                    );
                }
            }
        }
        let rows = *all_day_fill.iter().max().unwrap_or(&0);
        (segs, all_day_blocks, rows, has_out_of_work, occ_map)
    };
    *sh.cal.cal_occ.borrow_mut() = occ_map;

    // Vertical band now that we know whether anything sits outside work hours.
    let (vis_start, vis_end, hour_height) = compute_vertical(
        sh.cal.grid_canvas_h.get(),
        sh.cal.work_start.get(),
        sh.cal.work_end.get(),
        has_out_of_work,
        sh.cal.manual_hour_h.get(),
    );
    ui.set_hour_height(hour_height);
    ui.set_hour_start(vis_start);
    ui.set_hour_end(vis_end);
    ui.set_work_start(sh.cal.work_start.get());
    ui.set_work_end(sh.cal.work_end.get());

    let visible_top_ms = vis_start as i64 * 3_600_000;
    let visible_bottom_ms = vis_end as i64 * 3_600_000;
    let to_px = |ms: i64| -> f32 { (ms - visible_top_ms) as f32 / 3_600_000.0 * hour_height };
    let mut blocks: Vec<EventBlock> = segs
        .iter()
        .filter_map(|s| {
            let top_ms = s.start_in_day.max(visible_top_ms);
            let bot_ms = s.end_in_day.min(visible_bottom_ms);
            if bot_ms <= top_ms {
                return None;
            }
            let top = to_px(top_ms);
            let h = (to_px(bot_ms) - top).max(18.0);
            Some(EventBlock {
                id: s.id,
                day: s.day,
                top,
                h,
                color: s.color,
                title: s.title.clone(),
                time: s.time.clone(),
                all_day: false,
                xf: 0.0,
                wf: 1.0,
                count: s.count,
                tentative: s.tentative,
                writable: s.writable,
            })
        })
        .collect();
    println!(
        "[cal] layout: blocks={} all_day={} days={} col_w={:.0} vis=[{}..{}) hh={:.0} oow={}",
        blocks.len(),
        all_day_blocks.len(),
        day_count,
        col_width,
        vis_start,
        vis_end,
        hour_height,
        has_out_of_work
    );
    assign_overlap_lanes(&mut blocks, day_count);
    ui.set_events(slint::ModelRc::new(slint::VecModel::from(blocks)));
    ui.set_all_day_events(slint::ModelRc::new(slint::VecModel::from(all_day_blocks)));
    ui.set_all_day_rows(all_day_rows);

    // Requested grid scroll (entering the view / opening from a toast).
    // Issued only now — against the hour props THIS layout just set. Kept
    // pending until the week's EVENTS have arrived: they decide whether the
    // grid is the no-scroll work band or the full 0–24 scroll (out-of-work
    // events force the latter), and a pixel target computed before that
    // settles points at the wrong hour.
    if let Some(hour) = sh.cal.pending_cal_scroll.get() {
        if !sh.cal.calendar_events.borrow().is_empty() {
            sh.cal.pending_cal_scroll.set(None);
        }
        scroll_calendar_to_hour(ui, hour);
    }
}

/// Re-fire FetchCalendarEvents for the currently displayed week. Also
/// flips `calendar-loading` on so the topbar shows a "Загрузка…" pill
/// until the result lands.
pub(crate) fn refetch_calendar_events(ui: &MainWindow, sh: &Shared) {
    // Always fetch the full 7-day week so toggling to a 5-day view (or
    // horizontal scroll) never needs a refetch.
    let (from_ms, to_ms) = week_range_ms(sh.cal.calendar_week_start_days.get(), 7);
    if let Some(etx) = sh.engine_tx.borrow().as_ref() {
        ui.set_calendar_loading(true);
        let _ = etx.send(engine::EngineCmd::FetchCalendarEvents {
            from_ms,
            to_ms,
            calendar_ids: Vec::new(),
            for_reminders: false,
        });
    }
}

/// Seed the calendar view's read-only state before the engine produces
/// events: day labels for the current week + a sane initial vertical band
/// so the grid isn't blank. Real layout is computed in `apply_calendar_view`
/// once the grid's on-screen size is known.
pub(crate) fn apply_calendar_defaults(ui: &MainWindow) {
    use chrono::{Datelike, Duration, Local};
    let day_count = 7;
    let now = Local::now();
    // Week starts on Monday (ISO).
    let weekday_from_mon = now.weekday().num_days_from_monday() as i64;
    let monday = now.date_naive() - Duration::days(weekday_from_mon);
    let headers: Vec<slint::SharedString> = (0..day_count as i64)
        .map(|i| {
            let d = monday + Duration::days(i);
            const NAMES: [&str; 7] = ["Пн", "Вт", "Ср", "Чт", "Пт", "Сб", "Вс"];
            let n = NAMES[d.weekday().num_days_from_monday() as usize];
            format!("{n}, {:02}.{:02}", d.day(), d.month()).into()
        })
        .collect();
    ui.set_day_headers(slint::ModelRc::new(slint::VecModel::from(headers)));
    ui.set_day_count(day_count);
    ui.set_col_width(MIN_COL_W);
    let title = {
        use chrono::Datelike as _;
        const MONTHS: [&str; 12] = [
            "Январь",
            "Февраль",
            "Март",
            "Апрель",
            "Май",
            "Июнь",
            "Июль",
            "Август",
            "Сентябрь",
            "Октябрь",
            "Ноябрь",
            "Декабрь",
        ];
        format!("{} {}", MONTHS[(monday.month() - 1) as usize], monday.year())
    };
    ui.set_week_title(title.into());
    ui.set_hour_height(MIN_HOUR_H);
    if ui.get_hour_end() == 0 {
        ui.set_hour_start(8);
        ui.set_hour_end(19);
    }
    // Empty models so the for-loops don't trip on undefined.
    ui.set_calendars(slint::ModelRc::new(slint::VecModel::from(Vec::<CalendarItem>::new())));
    ui.set_events(slint::ModelRc::new(slint::VecModel::from(Vec::<EventBlock>::new())));
}

/// Direct manipulation on the week grid: double-click to create, drag to
/// move, drag an edge to resize.
pub(crate) fn wire_grid_editing(ui: &MainWindow, shared: &Rc<Shared>) {
    // Double-click on empty grid space → create form prefilled with that
    // day/time. x/y are viewport-content px; view_w is the viewport width.
    let ui_weak_gc = ui.as_weak();
    let sh_gc = shared.clone();
    // Manual double-click detection (the Flickable eats TouchArea::double-clicked):
    // (last_ms, last_x, last_y). A create fires only on the second click within
    // 450 ms and ~12 px of the first.
    let gc_last = std::cell::Cell::new((0i64, 0f32, 0f32));
    ui.on_grid_create_at(move |x, y, view_w| {
        let Some(ui) = ui_weak_gc.upgrade() else { return };
        let now = chrono::Local::now().timestamp_millis();
        let (last_ms, last_x, last_y) = gc_last.get();
        let is_double =
            now - last_ms < 450 && (x - last_x).abs() < 12.0 && (y - last_y).abs() < 12.0;
        if !is_double {
            // First click — arm and wait for the second.
            gc_last.set((now, x, y));
            return;
        }
        gc_last.set((0, 0.0, 0.0)); // consume, so a triple-click doesn't re-fire
        const GUTTER: f32 = 48.0;
        if x < GUTTER {
            return; // clicked in the time-label gutter
        }
        let day_count = ui.get_day_count();
        if day_count <= 0 {
            return;
        }
        let col_w = (view_w - GUTTER) / day_count as f32;
        if col_w <= 0.0 {
            return;
        }
        let day = ((x - GUTTER) / col_w).floor() as i64;
        if day < 0 || day >= day_count as i64 {
            return;
        }
        // y px → hour-of-day, then snap the start to the nearest 15 minutes.
        let hour_height = ui.get_hour_height();
        let hour_start = ui.get_hour_start();
        let minutes = hour_start as f32 * 60.0 + (y / hour_height) * 60.0;
        let snapped = ((minutes / 15.0).round() as i64) * 15;
        let day_ms: i64 = 24 * 60 * 60 * 1000;
        let (week_start_ms, _) = week_range_ms(sh_gc.cal.calendar_week_start_days.get(), day_count);
        let start_ms = week_start_ms + day * day_ms + snapped * 60_000;
        open_create_form_at(&ui, &sh_gc, start_ms);
    });

    // Drag-to-move a block to a new day/time (writable calendars only — the
    // block's TouchArea won't even start a drag otherwise). The ghost's final
    // top-left (grid px) → nearest day column + 15-min-snapped start; duration
    // and all other fields are preserved.
    let ui_weak_gm = ui.as_weak();
    let sh_gm = shared.clone();
    ui.on_grid_event_moved(move |id, orig_x, _orig_y, new_x, new_y| {
        let Some(ui) = ui_weak_gm.upgrade() else { return };
        const GUTTER: f32 = 48.0;
        let day_count = ui.get_day_count();
        let col_w = ui.get_col_width();
        if day_count <= 0 || col_w <= 0.0 {
            return;
        }
        let hour_height = ui.get_hour_height();
        let hour_start = ui.get_hour_start();
        let day_ms: i64 = 24 * 60 * 60 * 1000;
        let (week_start_ms, _) = week_range_ms(sh_gm.cal.calendar_week_start_days.get(), day_count);

        // Block x = GUTTER + (day + lane_xf)*col_w + 2px, lane_xf ∈ [0,1) for
        // overlap lanes — floor recovers the day column. round() broke every
        // block in lane xf >= 0.5: the lookup jumped to the NEXT day, the
        // cal_occ probe missed, and the drag silently did nothing.
        let px_to_day = |x: f32| -> i64 {
            (((x - GUTTER - 2.0) / col_w).floor() as i64).clamp(0, day_count as i64 - 1)
        };
        let px_to_min = |y: f32| -> i64 {
            let minutes = hour_start as f32 * 60.0 + (y / hour_height) * 60.0;
            ((minutes / 15.0).round().max(0.0) as i64) * 15
        };

        let orig_day = px_to_day(orig_x);
        let new_day = px_to_day(new_x);
        let new_start = week_start_ms + new_day * day_ms + px_to_min(new_y) * 60_000;

        // Exact instance grabbed (gives recurrence_id + duration + whether the
        // event recurs). Keyed (event_id, original day column).
        let (occ_start, occ_end, recurring) =
            match sh_gm.cal.cal_occ.borrow().get(&(id, orig_day as i32)).copied() {
                Some(v) => v,
                None => {
                    eprintln!(
                        "[cal] move: no occurrence for id={id} day={orig_day} — drop ignored"
                    );
                    return;
                }
            };
        if new_start == occ_start {
            return; // dropped back where it was
        }
        let new_end = new_start + (occ_end - occ_start).max(0);

        // Preserve the event's display fields.
        let (summary, description, location, all_day) = {
            let events = sh_gm.cal.calendar_events.borrow();
            match events.iter().find(|e| e.id as i32 == id) {
                Some(e) => {
                    (e.summary.clone(), e.description.clone(), e.location.clone(), e.all_day)
                }
                None => return,
            }
        };

        let mut body = serde_json::json!({
            "summary": summary,
            "description": description,
            "location": location,
            "all_day": all_day,
            "dtstart": new_start,
            "dtend": new_end,
        });
        if recurring {
            // Move just THIS occurrence — an "all" dtstart shift keeps BYDAY's
            // weekday, so only scope=single (an override) actually re-days it.
            body["scope"] = "single".into();
            body["recurrence_id"] = occ_start.into();
            // No optimistic redraw: the override can't be reflected by local
            // RRULE expansion; the refetch after PatchEvent shows it.
        } else {
            body["scope"] = "all".into();
            // Optimistic shift so the block lands immediately; refetch reconciles.
            {
                let mut events = sh_gm.cal.calendar_events.borrow_mut();
                if let Some(e) = events.iter_mut().find(|e| e.id as i32 == id) {
                    if e.dtend.is_some() {
                        e.dtend = Some(new_end);
                    }
                    e.dtstart = new_start;
                }
            }
            apply_calendar_view(&ui, &sh_gm);
        }

        if let Some(c) = sh_gm.cache.as_ref() {
            let _ = c.purge_event_reminders(id as i64);
        }
        let ak = sh_gm.cal.event_accounts.borrow().get(&(id as i64)).cloned().unwrap_or_default();
        if let Some(etx) = sh_gm.engine_tx.borrow().as_ref() {
            let _ = etx.send(engine::EngineCmd::PatchEvent {
                event_id: id as i64,
                body,
                account_key: ak,
            });
        }
    });

    // Resize a block by its top/bottom edge (writable only). Day is unchanged
    // (taken from the original x); new start = top edge, new end = bottom edge.
    let ui_weak_gr = ui.as_weak();
    let sh_gr = shared.clone();
    ui.on_grid_event_resized(move |id, orig_x, orig_y, new_top_y, new_bottom_y| {
        let _ = orig_y;
        let Some(ui) = ui_weak_gr.upgrade() else { return };
        const GUTTER: f32 = 48.0;
        let day_count = ui.get_day_count();
        let col_w = ui.get_col_width();
        if day_count <= 0 || col_w <= 0.0 {
            return;
        }
        let hour_height = ui.get_hour_height();
        let hour_start = ui.get_hour_start();
        let day_ms: i64 = 24 * 60 * 60 * 1000;
        let (week_start_ms, _) = week_range_ms(sh_gr.cal.calendar_week_start_days.get(), day_count);
        // floor, not round: orig_x carries the overlap-lane fraction (xf) —
        // see px_to_day in the move handler above.
        let day = (((orig_x - GUTTER - 2.0) / col_w).floor() as i64).clamp(0, day_count as i64 - 1);
        let to_min = |y: f32| -> i64 {
            let m = hour_start as f32 * 60.0 + (y / hour_height) * 60.0;
            ((m / 15.0).round().max(0.0) as i64) * 15
        };
        let new_start = week_start_ms + day * day_ms + to_min(new_top_y) * 60_000;
        let mut new_end = week_start_ms + day * day_ms + to_min(new_bottom_y) * 60_000;
        if new_end <= new_start {
            new_end = new_start + 15 * 60_000;
        }

        let (occ_start, occ_end, recurring) =
            match sh_gr.cal.cal_occ.borrow().get(&(id, day as i32)).copied() {
                Some(v) => v,
                None => {
                    eprintln!("[cal] resize: no occurrence for id={id} day={day} — ignored");
                    return;
                }
            };
        if new_start == occ_start && new_end == occ_end {
            return; // no change
        }
        let (summary, description, location, all_day) = {
            let events = sh_gr.cal.calendar_events.borrow();
            match events.iter().find(|e| e.id as i32 == id) {
                Some(e) => {
                    (e.summary.clone(), e.description.clone(), e.location.clone(), e.all_day)
                }
                None => return,
            }
        };
        let mut body = serde_json::json!({
            "summary": summary,
            "description": description,
            "location": location,
            "all_day": all_day,
            "dtstart": new_start,
            "dtend": new_end,
        });
        if recurring {
            body["scope"] = "single".into();
            body["recurrence_id"] = occ_start.into();
        } else {
            body["scope"] = "all".into();
            {
                let mut events = sh_gr.cal.calendar_events.borrow_mut();
                if let Some(e) = events.iter_mut().find(|e| e.id as i32 == id) {
                    e.dtstart = new_start;
                    e.dtend = Some(new_end);
                }
            }
            apply_calendar_view(&ui, &sh_gr);
        }
        if let Some(c) = sh_gr.cache.as_ref() {
            let _ = c.purge_event_reminders(id as i64);
        }
        let ak = sh_gr.cal.event_accounts.borrow().get(&(id as i64)).cloned().unwrap_or_default();
        if let Some(etx) = sh_gr.engine_tx.borrow().as_ref() {
            let _ = etx.send(engine::EngineCmd::PatchEvent {
                event_id: id as i64,
                body,
                account_key: ak,
            });
        }
    });
}

/// Colour picked in the per-calendar palette popup.
pub(crate) fn wire_calendar_color(ui: &MainWindow, shared: &Rc<Shared>) {
    // Colour picked in the per-calendar palette popup.
    let ui_weak_cc = ui.as_weak();
    let sh_cc = shared.clone();
    ui.on_calendar_set_color(move |cal_id, palette_idx| {
        let Some(ui) = ui_weak_cc.upgrade() else { return };
        if let Some(hex_color) = CAL_PALETTE.get(palette_idx as usize) {
            sh_cc.cal.calendar_colors.borrow_mut().insert(cal_id as i64, (*hex_color).to_string());
            apply_calendar_view(&ui, &sh_cc);
            save_calendar_settings(&ui, &sh_cc);
        }
    });
}

/// Calendar navigation and view options: week prev/next/today, grid
/// resize and zoom, work hours, calendar visibility, notification sound.
pub(crate) fn wire_calendar_nav(ui: &MainWindow, shared: &Rc<Shared>) {
    // If we start straight in calendar mode (e.g. saved state), kick
    // off the same fetch. (Not yet persisted, but trivial when it is.)

    let nav = |delta_days: i64| {
        let ui_weak = ui.as_weak();
        let sh = shared.clone();
        move || {
            let Some(ui) = ui_weak.upgrade() else { return };
            let new_start = if delta_days == 0 {
                week_start_days_today()
            } else {
                sh.cal.calendar_week_start_days.get() + delta_days
            };
            sh.cal.calendar_week_start_days.set(new_start);
            sh.cal.week_follows_today.set(new_start == week_start_days_today());
            apply_calendar_view(&ui, &sh);
            refetch_calendar_events(&ui, &sh);
        }
    };
    ui.on_calendar_prev(nav(-7));
    ui.on_calendar_next(nav(7));
    ui.on_calendar_today(nav(0));

    // Grid body size mirror: layout depends on the on-screen canvas, so
    // recompute whenever it changes (and once on init — `changed` doesn't
    // fire for the first layout pass).
    let ui_weak_gr = ui.as_weak();
    let sh_gr = shared.clone();
    ui.on_grid_area_resized(move |w, h| {
        let Some(ui) = ui_weak_gr.upgrade() else { return };
        if w <= 0.0 || h <= 0.0 {
            return;
        }
        let changed = (sh_gr.cal.grid_canvas_w.get() - w).abs() > 0.5
            || (sh_gr.cal.grid_canvas_h.get() - h).abs() > 0.5;
        sh_gr.cal.grid_canvas_w.set(w);
        sh_gr.cal.grid_canvas_h.set(h);
        if changed {
            apply_calendar_view(&ui, &sh_gr);
        }
    });
    // Ctrl-wheel = zoom hours; Ctrl-Alt-wheel = zoom day width. Manual zoom
    // wins over autofit (the layout then scrolls). delta>0 = zoom in.
    let ui_weak_zh = ui.as_weak();
    let sh_zh = shared.clone();
    ui.on_calendar_zoom_hours(move |delta| {
        let Some(ui) = ui_weak_zh.upgrade() else { return };
        // A manual zoom overrides any queued programmatic scroll.
        sh_zh.cal.pending_cal_scroll.set(None);
        let canvas_h = sh_zh.cal.grid_canvas_h.get().max(MIN_HOUR_H);
        let cur = if sh_zh.cal.manual_hour_h.get() > 0.0 {
            sh_zh.cal.manual_hour_h.get()
        } else {
            ui.get_hour_height()
        };
        let factor = if delta > 0.0 { 1.1 } else { 1.0 / 1.1 };
        // Zooming out при упоре в пол returns to autofit (manual = 0): the
        // grid collapses back to the work-hours band. Without this escape
        // hatch a single ctrl-wheel pinned the layout to the full 0–24
        // scroll forever — every launch then opened on the night hours.
        if delta < 0.0 && cur <= MIN_HOUR_H + 0.5 {
            sh_zh.cal.manual_hour_h.set(0.0);
            apply_calendar_view(&ui, &sh_zh);
            save_calendar_settings(&ui, &sh_zh);
            return;
        }
        let next = (cur * factor).clamp(MIN_HOUR_H, canvas_h);
        sh_zh.cal.manual_hour_h.set(next);
        apply_calendar_view(&ui, &sh_zh);
        save_calendar_settings(&ui, &sh_zh);
    });
    let ui_weak_zd = ui.as_weak();
    let sh_zd = shared.clone();
    ui.on_calendar_zoom_days(move |delta| {
        let Some(ui) = ui_weak_zd.upgrade() else { return };
        let avail = (sh_zd.cal.grid_canvas_w.get() - GUTTER_W).max(MIN_COL_W);
        let cur = if sh_zd.cal.manual_col_w.get() > 0.0 {
            sh_zd.cal.manual_col_w.get()
        } else {
            ui.get_col_width()
        };
        let factor = if delta > 0.0 { 1.1 } else { 1.0 / 1.1 };
        // Same escape hatch as the hour zoom: bottoming out returns to
        // autofit column widths.
        if delta < 0.0 && cur <= MIN_COL_W + 0.5 {
            sh_zd.cal.manual_col_w.set(0.0);
            apply_calendar_view(&ui, &sh_zd);
            save_calendar_settings(&ui, &sh_zd);
            return;
        }
        let next = (cur * factor).clamp(MIN_COL_W, avail);
        sh_zd.cal.manual_col_w.set(next);
        apply_calendar_view(&ui, &sh_zd);
        save_calendar_settings(&ui, &sh_zd);
    });
    // Working-day start/end from the settings «Календарь» tab.
    let ui_weak_ws = ui.as_weak();
    let sh_ws = shared.clone();
    ui.on_set_work_hours(move |start, end| {
        let Some(ui) = ui_weak_ws.upgrade() else { return };
        let s = start.clamp(0, 23);
        let e = end.clamp(s + 1, 24);
        sh_ws.cal.work_start.set(s);
        sh_ws.cal.work_end.set(e);
        ui.set_work_start(s);
        ui.set_work_end(e);
        apply_calendar_view(&ui, &sh_ws);
        save_calendar_settings(&ui, &sh_ws);
    });
    let ui_weak_vis = ui.as_weak();
    let sh_vis = shared.clone();
    ui.on_calendar_toggle_visibility(move |cal_id| {
        if let Some(ui) = ui_weak_vis.upgrade() {
            let id = cal_id as i64;
            let cur = *sh_vis.cal.calendar_visible.borrow().get(&id).unwrap_or(&true);
            sh_vis.cal.calendar_visible.borrow_mut().insert(id, !cur);
            apply_reminder_visibility(&sh_vis, id, !cur);
            apply_calendar_view(&ui, &sh_vis);
            save_calendar_settings(&ui, &sh_vis);
        }
    });
    // Notification-sound toggle (burger menu) — persisted immediately.
    let ui_weak_snd = ui.as_weak();
    let sh_snd = shared.clone();
    ui.on_toggle_notify_sound(move || {
        if let Some(ui) = ui_weak_snd.upgrade() {
            ui.set_notify_sound_on(!ui.get_notify_sound_on());
            save_calendar_settings(&ui, &sh_snd);
        }
    });
}
