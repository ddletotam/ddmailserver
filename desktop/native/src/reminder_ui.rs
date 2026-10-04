//! Calendar reminders on the UI side: the action machine behind the reminder
//! toasts' buttons (open / ignore / snooze), the snooze choices, and the
//! window of upcoming events the scanner (`reminders.rs`) works from.

use super::*;

/// Route a reminder-toast action onto the UI loop. Toast callbacks fire on the
/// UI thread already, but hopping via the event loop keeps us clear of any
/// borrow that might be live while a toast window dispatches.
pub(crate) fn reminder_dispatch(
    action: &'static str,
    eid: i64,
    occ: i64,
    seq: i64,
    summary: String,
) {
    if let Some(weak) = UI_WEAK.get() {
        let _ = weak.upgrade_in_event_loop(move |ui| {
            SHARED.with(|s| {
                if let Some(sh) = s.borrow().as_ref() {
                    handle_reminder_action(&ui, sh, action, eid, occ, seq, &summary);
                }
            });
        });
    }
}

/// Smart-step «напомнить через …» options: only steps that land BEFORE the
/// event starts (`mins_until` = minutes from now to the occurrence). Returns
/// (value, label) pairs — value is minutes as a string for the snooze-choice
/// callback. «В момент начала» is a separate fixed button, so it's not here.
pub(crate) fn snooze_steps(mins_until: i64) -> Vec<(String, String)> {
    const STEPS: &[(i64, &str)] = &[
        (1, "Через 1 минуту"),
        (5, "Через 5 минут"),
        (10, "Через 10 минут"),
        (15, "Через 15 минут"),
        (30, "Через 30 минут"),
        (60, "Через 1 час"),
        (120, "Через 2 часа"),
        (180, "Через 3 часа"),
        (360, "Через 6 часов"),
        (720, "Через 12 часов"),
        (1440, "Через 1 день"),
    ];
    STEPS
        .iter()
        .filter(|(m, _)| *m < mins_until)
        .map(|(m, l)| (m.to_string(), l.to_string()))
        .collect()
}

/// Single dispatch point for what a calendar-reminder toast can ask for
/// (spec 2026-07-11):
///   cancel-occ    → «✕»: kill the whole cascade of this occurrence, forever.
///   timeout       → toast expired untouched: retire the row + arm the next
///                   cascade link (event-defined secondary alarm).
///   open-stay     → body of a «скоро» toast: navigate to the event, STOP the
///                   toast's timer, leave it open.
///   open-close    → body of an at-start / running toast: navigate + close.
///   snooze-window → «Напомнить позже»: pause the toast timer + open the
///                   in-app snooze dialog (choice committed in on_snooze_choice).
pub(crate) fn handle_reminder_action(
    ui: &MainWindow,
    sh: &Rc<Shared>,
    action: &str,
    event_id: i64,
    occ_ms: i64,
    seq: i64,
    summary: &str,
) {
    match action {
        "cancel-occ" => {
            if let Some(c) = sh.cache.as_ref() {
                let _ = c.cancel_occurrence_reminders(event_id, occ_ms);
            }
            toast_window::close_for_event(event_id);
        }
        "timeout" => {
            // The toast is already gone (tick removed it). Advance the
            // cascade so a secondary alarm can arm.
            if let Some(c) = sh.cache.as_ref() {
                let _ = c.reminder_timeout(event_id, occ_ms, seq);
            }
        }
        "snooze-window" => {
            let toast_id = toast_window::id_for_event(event_id);
            toast_window::pause_timer(toast_id);
            let now_ms = chrono::Utc::now().timestamp_millis();
            let mins_until = (occ_ms - now_ms) / 60_000;
            let opts: Vec<SnoozeOpt> = snooze_steps(mins_until)
                .into_iter()
                .map(|(value, label)| SnoozeOpt { value: value.into(), label: label.into() })
                .collect();
            // occ_end recovered from the current calendar view (0 if unknown;
            // user_choice_reminder tolerates it).
            let occ_end = sh
                .calendar_events
                .borrow()
                .iter()
                .find(|e| e.id == event_id)
                .and_then(|e| e.dtend)
                .unwrap_or(0);
            sh.snooze_ctx.replace((event_id, occ_ms, occ_end, toast_id, summary.to_string()));
            ui.set_snooze_summary(summary.into());
            ui.set_snooze_options(slint::ModelRc::new(slint::VecModel::from(opts)));
            ui.set_snooze_visible(true);
            raise_window(ui);
        }
        "open-stay" | "open-close" => {
            println!("[cal] toast-open: action={action} event={event_id} occ={occ_ms}");
            if action == "open-close" {
                toast_window::close_for_event(event_id);
            } else {
                // Body click on «скоро»: freeze the toast, it stays until the
                // user dismisses it (spec: таймер закрытия останавливается).
                toast_window::stop_timer(toast_window::id_for_event(event_id));
            }
            raise_window(ui);
            let jump_week = week_start_days_for_ms(occ_ms);
            sh.calendar_week_start_days.set(jump_week);
            sh.week_follows_today.set(jump_week == week_start_days_today());
            sh.pending_open_event.set(event_id);
            sh.pending_open_occ.set(occ_ms);
            *sh.pending_open_summary.borrow_mut() = summary.to_string();
            // Land the viewport on the event itself (an hour of context
            // above), not on the working day — the toast points at it.
            {
                use chrono::{TimeZone as _, Timelike as _};
                let hour = chrono::Local
                    .timestamp_millis_opt(occ_ms)
                    .single()
                    .map(|t| t.hour() as f32 + t.minute() as f32 / 60.0)
                    .unwrap_or(sh.work_start.get() as f32);
                sh.pending_cal_scroll.set(Some((hour - 1.0).max(0.0)));
            }
            ui.set_view_mode(1);
            apply_calendar_view(ui, sh);
            if let Some(etx) = sh.engine_tx.borrow().as_ref() {
                let _ = etx.send(engine::EngineCmd::FetchCalendars);
            }
            refetch_calendar_events(ui, sh);
        }
        other => eprintln!("reminders: unknown action {other:?}"),
    }
}

/// Переключение календаря — это и переключение его напоминаний.
///
/// Выключили: снимаем ВСЕ строки напоминаний этого календаря (не только по
/// событиям загруженного окна — за этим и заведён `calendar_id` в reminders2)
/// и гасим уже висящие тосты. Без этого выключенный календарь продолжал
/// звонить по всему, что успело взвестись до выключения: `seed` скрытые
/// пропускает, но взведённое раньше никто не отзывал.
///
/// Включили: пересеваем из текущего снимка событий, не дожидаясь следующего
/// фетча календаря — иначе до ближайшего обновления календарь был бы виден,
/// но нем.
pub(crate) fn apply_reminder_visibility(sh: &Shared, cal_id: i64, visible: bool) {
    let Some(cache) = sh.cache.as_ref() else { return };
    let events = sh.calendar_events.borrow();
    if visible {
        let now_ms = chrono::Utc::now().timestamp_millis();
        let vis = sh.calendar_visible.borrow();
        let hidden = |c: i64| !*vis.get(&c).unwrap_or(&true);
        reminders::seed(cache, &events, &hidden, now_ms);
        println!("[cal] календарь {cal_id} включён — напоминания пересеяны");
    } else {
        let mut hit: Vec<i64> = match cache.purge_calendar_reminders(cal_id) {
            Ok(ids) => ids,
            Err(e) => {
                eprintln!("reminders: purge календаря {cal_id}: {e}");
                Vec::new()
            }
        };
        // Добивка по событиям снимка: строки, посеянные до появления
        // `calendar_id`, лежат с нулём и под удаление по календарю не
        // подпадают. Пересев бы их проставил, но скрытый календарь как раз
        // не пересевается — без этой добивки они звонили бы вечно.
        for ev in events.iter().filter(|e| e.calendar_id == cal_id) {
            if hit.contains(&ev.id) {
                continue;
            }
            match cache.purge_event_reminders(ev.id) {
                Ok(()) => hit.push(ev.id),
                Err(e) => eprintln!("reminders: purge события {}: {e}", ev.id),
            }
        }
        for id in &hit {
            toast_window::close_for_event(*id);
        }
        println!("[cal] календарь {cal_id} выключен — снято напоминаний по {} событиям", hit.len());
    }
}

/// Как часто перепроверять окно посева (и перекат суток). Фетч дешёвый
/// (~100 мс на сервере), а цена промаха — молчащий на весь день клиент.
pub(crate) const REMINDER_WINDOW_REFRESH_SECS: u64 = 300;

/// Сколько суток вперёд от сегодняшней полуночи держать засеянными.
/// Больше горизонта посева (30 дней) смысла не имеет, меньше суток — опасно:
/// напоминание должно взвестись ЗАРАНЕЕ, а не в момент выстрела.
pub(crate) const REMINDER_WINDOW_DAYS: i64 = 8;

/// Фетч ради посева напоминаний, а не ради сетки.
///
/// Посев живёт исключительно на результатах фетча календаря, а окно фетча до
/// этого было ровно отображаемой неделей — со двумя дырами. Первая: на старте
/// фетча нет вовсе, и свежий клиент нем, пока не придёт серверный push (а он
/// не обязан). Вторая: `calendar_week_start_days` считается один раз при
/// запуске, поэтому клиент, проживший выходные, всю следующую неделю
/// перефетчивал ПРОШЛУЮ и не сеял ничего; симметрично — пролистанная вперёд
/// сетка глушила текущую неделю.
///
/// Это окно привязано к `now`, а не к сетке, и переоценивается на каждом тике,
/// так что перекат суток/недели лечится сам. Результат помечен
/// `for_reminders` и в сетку не попадает.
pub(crate) fn fetch_reminder_window(sh: &Shared) {
    use chrono::{Duration, Local};
    let today = Local::now().date_naive();
    let from_ms = match local_midnight_ms(today) {
        Some(ms) => ms,
        None => return, // DST-провал ровно в полночь — пропускаем тик
    };
    let to_ms = local_midnight_ms(today + Duration::days(REMINDER_WINDOW_DAYS))
        .unwrap_or(from_ms + REMINDER_WINDOW_DAYS * 24 * 3600 * 1000);
    if let Some(etx) = sh.engine_tx.borrow().as_ref() {
        let _ = etx.send(engine::EngineCmd::FetchCalendarEvents {
            from_ms,
            to_ms,
            calendar_ids: Vec::new(),
            for_reminders: true,
        });
    }
}
