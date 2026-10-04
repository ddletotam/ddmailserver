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
                .cal
                .calendar_events
                .borrow()
                .iter()
                .find(|e| e.id == event_id)
                .and_then(|e| e.dtend)
                .unwrap_or(0);
            sh.cal.snooze_ctx.replace((event_id, occ_ms, occ_end, toast_id, summary.to_string()));
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
            sh.cal.calendar_week_start_days.set(jump_week);
            sh.cal.week_follows_today.set(jump_week == week_start_days_today());
            sh.cal.pending_open_event.set(event_id);
            sh.cal.pending_open_occ.set(occ_ms);
            *sh.cal.pending_open_summary.borrow_mut() = summary.to_string();
            // Land the viewport on the event itself (an hour of context
            // above), not on the working day — the toast points at it.
            {
                use chrono::{TimeZone as _, Timelike as _};
                let hour = chrono::Local
                    .timestamp_millis_opt(occ_ms)
                    .single()
                    .map(|t| t.hour() as f32 + t.minute() as f32 / 60.0)
                    .unwrap_or(sh.cal.work_start.get() as f32);
                sh.cal.pending_cal_scroll.set(Some((hour - 1.0).max(0.0)));
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
    let events = sh.cal.calendar_events.borrow();
    if visible {
        let now_ms = chrono::Utc::now().timestamp_millis();
        let vis = sh.cal.calendar_visible.borrow();
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

/// Start the two reminder timers: the due-reminder scan that raises toasts,
/// and the refresh of the upcoming-events window. Returned, because a
/// dropped `Timer` stops — the caller keeps them for the loop's lifetime.
/// Both reach `Shared` through `SHARED` on each tick.
pub(crate) fn start_reminder_timers(ui: &MainWindow) -> (slint::Timer, slint::Timer) {
    // Calendar reminders: a UI-thread timer scans the persisted reminder
    // table every interval and toasts whatever just came due. Runs on the
    // Slint event loop, which keeps ticking while hidden to tray — so we
    // don't need the background Tokio task the old build relied on. Bound
    // to a name (not bare `_`) so it lives for the loop's lifetime.
    let _reminder_timer = slint::Timer::default();
    _reminder_timer.start(
        slint::TimerMode::Repeated,
        std::time::Duration::from_secs(reminders::SCAN_INTERVAL_SECS),
        || {
            let now_ms = chrono::Utc::now().timestamp_millis();
            let due = SHARED.with(|s| {
                let borrow = s.borrow();
                let Some(sh) = borrow.as_ref() else { return Vec::new() };
                let Some(c) = sh.cache.as_ref() else { return Vec::new() };
                // Видимость календаря проверяется здесь, в момент выстрела, а
                // не только при посеве и на переключателе: строка могла
                // взвестись до выключения и не попасть под purge (событие вне
                // загруженного окна, переезд между календарями). Строка
                // остаётся взведённой — включат календарь, зазвонит сама.
                let vis = sh.cal.calendar_visible.borrow();
                let events = sh.cal.calendar_events.borrow();
                let hidden = |row: &ddmail_core::cache::ReminderRow| -> bool {
                    let cal = if row.calendar_id != 0 {
                        row.calendar_id
                    } else {
                        // Строка старой схемы: календарь берём из снимка.
                        events
                            .iter()
                            .find(|e| e.id == row.event_id)
                            .map(|e| e.calendar_id)
                            .unwrap_or(0)
                    };
                    cal != 0 && !*vis.get(&cal).unwrap_or(&true)
                };
                reminders::scan(c, now_ms, &hidden)
            });
            for t in due {
                // One toast per event on screen at a time (dedup a burst).
                if toast_window::has_for_event(t.row.event_id) {
                    continue;
                }
                let title = reminders::title_for(&t);
                let body = reminders::body_for(&t, now_ms);
                let eid = t.row.event_id;
                let occ = t.row.occurrence_start_ms;
                let seq = t.row.seq;
                let summary = t.row.summary.clone();

                match t.mode {
                    reminders::ToastMode::AtStart | reminders::ToastMode::AlreadyRunning => {
                        // ✕ = close only; body = open card + close. No snooze,
                        // no cascade advance (this is the terminal alarm).
                        let s_body = summary.clone();
                        let id = toast_window::show(
                            toast_window::KIND_STARTED,
                            eid,
                            &title,
                            &body,
                            false,
                            reminders::AT_START_TIMEOUT_SECS,
                            move || reminder_dispatch("cancel-occ", eid, occ, seq, String::new()),
                            move || reminder_dispatch("open-close", eid, occ, seq, s_body.clone()),
                            || {},
                        );
                        // Timeout = silent expiry; still retire the row so the
                        // cascade can't resurrect it.
                        toast_window::set_on_timeout(id, move || {
                            reminder_dispatch("timeout", eid, occ, seq, String::new())
                        });
                    }
                    reminders::ToastMode::Soon => {
                        // ✕ = kill the whole cascade of this occurrence.
                        // Body = open card, STOP the timer (toast stays).
                        // «Напомнить позже» = snooze dialog (pauses timer).
                        // Timeout = advance the cascade to the next alarm.
                        let s_body = summary.clone();
                        let s_act = summary.clone();
                        let id = toast_window::show(
                            toast_window::KIND_SOON,
                            eid,
                            &title,
                            &body,
                            true,
                            reminders::SOON_TIMEOUT_SECS,
                            move || reminder_dispatch("cancel-occ", eid, occ, seq, String::new()),
                            move || reminder_dispatch("open-stay", eid, occ, seq, s_body.clone()),
                            move || {
                                reminder_dispatch("snooze-window", eid, occ, seq, s_act.clone())
                            },
                        );
                        toast_window::set_on_timeout(id, move || {
                            reminder_dispatch("timeout", eid, occ, seq, String::new())
                        });
                    }
                }
            }
        },
    );

    // Окно посева напоминаний — отдельно от сетки и привязано к `now`.
    // Пересчитывается на каждом тике, поэтому перекат суток (и недели) лечится
    // сам: раньше посев жил только на фетчах отображаемой недели, и клиент,
    // проживший выходные, всю следующую неделю тянул прошлую и молчал.
    // Тем же тиком сетка подтягивается за сегодняшним днём, если пользователь
    // её сам не увёл на другую неделю.
    let ui_weak_rw = ui.as_weak();
    let _reminder_window_timer = slint::Timer::default();
    _reminder_window_timer.start(
        slint::TimerMode::Repeated,
        std::time::Duration::from_secs(REMINDER_WINDOW_REFRESH_SECS),
        move || {
            let Some(ui) = ui_weak_rw.upgrade() else { return };
            SHARED.with(|s| {
                let borrow = s.borrow();
                let Some(sh) = borrow.as_ref() else { return };
                fetch_reminder_window(sh);
                let today = week_start_days_today();
                if sh.cal.week_follows_today.get() && sh.cal.calendar_week_start_days.get() != today
                {
                    println!("[cal] перекат недели: сетка идёт за сегодня → {today}");
                    sh.cal.calendar_week_start_days.set(today);
                    apply_calendar_view(&ui, sh);
                    refetch_calendar_events(&ui, sh);
                }
            });
        },
    );
    (_reminder_timer, _reminder_window_timer)
}

/// The snooze dialog of a reminder toast: a choice, or dismissing it.
pub(crate) fn wire_snooze(ui: &MainWindow, shared: &Rc<Shared>) {
    // Snooze modal choice → commit through the same action machine the
    // toast buttons use ("snz:5" … "snz:atstart").
    let sh_snz = shared.clone();
    ui.on_snooze_choice(move |choice| {
        let (eid, occ, occ_end, toast_id, summary) = sh_snz.cal.snooze_ctx.borrow().clone();
        if eid != 0 {
            let now_ms = chrono::Utc::now().timestamp_millis();
            let at_start = choice == "atstart";
            let fire_at =
                if at_start { occ } else { now_ms + choice.parse::<i64>().unwrap_or(5) * 60_000 };
            // User made a choice: cascade → one reminder; toast closes
            // immediately and silently (no cascade-advancing timeout).
            if let Some(c) = sh_snz.cache.as_ref() {
                if let Err(e) =
                    c.user_choice_reminder(eid, occ, occ_end, fire_at, at_start, &summary)
                {
                    eprintln!("reminders: user choice failed for {eid}: {e}");
                }
            }
            toast_window::stop_timer(toast_id); // disarm the timeout hook
            toast_window::close(toast_id);
        }
        sh_snz.cal.snooze_ctx.replace((0, 0, 0, 0, String::new()));
    });
    // Snooze dialog dismissed WITHOUT a choice: the toast behaves as if the
    // button was never pressed — resume its paused countdown.
    let sh_snc = shared.clone();
    ui.on_snooze_cancel(move || {
        let (_, _, _, toast_id, _) = sh_snc.cal.snooze_ctx.borrow().clone();
        if toast_id != 0 {
            toast_window::resume_timer(toast_id);
        }
        sh_snc.cal.snooze_ctx.replace((0, 0, 0, 0, String::new()));
    });
}
