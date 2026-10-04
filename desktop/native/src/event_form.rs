//! Event card and event form: date/time parts, the create and edit forms,
//! saving them (contract §5б), and the human-readable labels the card shows
//! (attendees, recurrence, reminder lead).

use super::*;

/// Parse a form date/time string ("YYYY-MM-DD HH:MM", or "YYYY-MM-DD" when
/// all-day) as LOCAL time → ms since epoch. None on parse failure.
/// ms → (year, month, day, hour, minute) в локальной зоне — для заполнения
/// числовых полей пикера DateTimeField при открытии формы события.
pub(crate) fn dt_parts(ms: i64) -> (i32, i32, i32, i32, i32) {
    use chrono::{Datelike, Local, TimeZone, Timelike};
    let d = Local
        .timestamp_millis_opt(ms)
        .single()
        .unwrap_or_else(|| Local.timestamp_millis_opt(0).unwrap());
    (d.year(), d.month() as i32, d.day() as i32, d.hour() as i32, d.minute() as i32)
}

/// (year, month, day, hour, minute) из пикера → ms в локальной зоне. Для
/// «весь день» время обнуляется. None, если компоненты не образуют реальную
/// дату (например, 31 апреля) — вызывающий показывает ошибку вместо отправки.
pub(crate) fn parts_to_ms(y: i32, mo: i32, d: i32, h: i32, mi: i32, all_day: bool) -> Option<i64> {
    use chrono::{Local, NaiveDate, TimeZone};
    let (h, mi) = if all_day { (0, 0) } else { (h, mi) };
    let naive =
        NaiveDate::from_ymd_opt(y, mo as u32, d as u32)?.and_hms_opt(h as u32, mi as u32, 0)?;
    Local.from_local_datetime(&naive).single().map(|x| x.timestamp_millis())
}

/// Разложить ms в числовые поля start-пикера на UI.
pub(crate) fn set_form_start(ui: &MainWindow, ms: i64) {
    let (y, mo, d, h, mi) = dt_parts(ms);
    ui.set_edit_s_year(y);
    ui.set_edit_s_month(mo);
    ui.set_edit_s_day(d);
    ui.set_edit_s_hour(h);
    ui.set_edit_s_min(mi);
}

/// Разложить ms в числовые поля end-пикера на UI.
pub(crate) fn set_form_end(ui: &MainWindow, ms: i64) {
    let (y, mo, d, h, mi) = dt_parts(ms);
    ui.set_edit_e_year(y);
    ui.set_edit_e_month(mo);
    ui.set_edit_e_day(d);
    ui.set_edit_e_hour(h);
    ui.set_edit_e_min(mi);
}

/// Populate the edit-form's writable-calendar ComboBox; returns the ids
/// parallel to the model so the save step can map index → calendar_id.
///
/// `only_visible` restricts the list to calendars currently ticked active
/// in the sidebar — used when creating (you can only file a new event into
/// a calendar you're actually looking at). Editing passes `false` so an
/// event already living in a hidden calendar stays selectable.
pub(crate) fn fill_writable_calendars(
    ui: &MainWindow,
    sh: &Shared,
    only_visible: bool,
) -> Vec<i64> {
    let cals = sh.calendars.borrow();
    let visibility = sh.calendar_visible.borrow();
    let mut ids = Vec::new();
    let mut accounts = Vec::new();
    let mut names: Vec<slint::SharedString> = Vec::new();
    for c in cals.iter().filter(|c| c.can_write) {
        if only_visible && !*visibility.get(&c.id).unwrap_or(&true) {
            continue;
        }
        ids.push(c.id);
        accounts.push(c.account_key.clone());
        names.push(c.name.clone().into());
    }
    ui.set_edit_calendars(slint::ModelRc::new(slint::VecModel::from(names)));
    *sh.edit_cal_accounts.borrow_mut() = accounts;
    ids
}

/// Open the create form (blank, default now → +1h, first writable calendar).
pub(crate) fn open_create_form(ui: &MainWindow, sh: &Shared) {
    let now = chrono::Local::now().timestamp_millis();
    open_create_form_at(ui, sh, now);
}

/// Open the create form anchored at a specific start time (e.g. the slot a
/// double-click landed on), running one hour by default.
pub(crate) fn open_create_form_at(ui: &MainWindow, sh: &Shared, start_ms: i64) {
    // Active (visible) writable calendars only. If every writable calendar
    // is hidden, fall back to the full set so creation isn't a dead end.
    let mut ids = fill_writable_calendars(ui, sh, true);
    if ids.is_empty() {
        ids = fill_writable_calendars(ui, sh, false);
    }
    // Список календарей живёт только в памяти и заполняется единственным
    // ответом FetchCalendars. Открывать форму, из которой нечем сохранить,
    // бессмысленно — сразу говорим об этом и просим список заново; ответ
    // дозаполнит уже открытую карточку (см. EngineResult::Calendars).
    if ids.is_empty() {
        ui.set_edit_error("Список календарей не загружен — обновляю…".into());
        if let Some(etx) = sh.engine_tx.borrow().as_ref() {
            let _ = etx.send(engine::EngineCmd::FetchCalendars);
        }
    } else {
        ui.set_edit_error("".into());
    }
    *sh.edit_cal_ids.borrow_mut() = ids;
    sh.editing_event_id.set(0);
    ui.set_edit_is_create(true);
    ui.set_edit_title("".into());
    ui.set_edit_all_day(false);
    set_form_start(ui, start_ms);
    set_form_end(ui, start_ms + 3_600_000);
    ui.set_edit_location("".into());
    ui.set_edit_description("".into());
    ui.set_edit_calendar_idx(0);
    ui.set_edit_organizer("".into());
    ui.set_edit_attendees(ModelRc::new(VecModel::from(Vec::<AttendeeItem>::new())));
    ui.set_edit_meta("".into());
    ui.set_edit_extras(ModelRc::new(VecModel::from(Vec::<EventExtraItem>::new())));
    ui.set_edit_busy(false);
    ui.set_edit_visible(true);
}

/// Russian display label for an extra VEVENT property; unknown names pass
/// through as-is (they're already uppercased server-side).
pub(crate) fn extra_label(name: &str, value: &str) -> (String, String) {
    match name {
        "CONFERENCE" | "X-TELEMOST-CONFERENCE" | "X-GOOGLE-CONFERENCE" => {
            ("Видеовстреча".into(), value.into())
        }
        "URL" => ("Ссылка".into(), value.into()),
        "CATEGORIES" => ("Категории".into(), value.into()),
        "CLASS" => (
            "Доступ".into(),
            match value.to_uppercase().as_str() {
                "PRIVATE" => "Приватное".into(),
                "CONFIDENTIAL" => "Конфиденциальное".into(),
                _ => value.into(),
            },
        ),
        "TRANSP" => (
            "Занятость".into(),
            if value.eq_ignore_ascii_case("TRANSPARENT") {
                "Свободен".into()
            } else {
                value.into()
            },
        ),
        "PRIORITY" => ("Приоритет".into(), value.into()),
        "COMMENT" => ("Комментарий".into(), value.into()),
        "CONTACT" => ("Контакт".into(), value.into()),
        "ATTACH" => ("Вложение".into(), value.into()),
        _ => (name.into(), value.into()),
    }
}

/// PARTSTAT → status dot colour (green accepted / red declined / orange
/// tentative / grey no answer yet).
pub(crate) fn partstat_dot(partstat: &str) -> slint::Color {
    match partstat.to_uppercase().as_str() {
        "ACCEPTED" => slint::Color::from_rgb_u8(0x34, 0xa8, 0x53),
        "DECLINED" => slint::Color::from_rgb_u8(0xe2, 0x3b, 0x3b),
        "TENTATIVE" => slint::Color::from_rgb_u8(0xf5, 0xa6, 0x23),
        _ => slint::Color::from_rgb_u8(0xb5, 0xbc, 0xc6),
    }
}

/// "Имя <email>" when a display name exists, plain email otherwise.
pub(crate) fn person_label(name: &str, email: &str) -> String {
    if name.trim().is_empty() { email.to_string() } else { format!("{} <{}>", name.trim(), email) }
}

/// RRULE → короткая русская метка. Only FREQ/INTERVAL are surfaced — the
/// point is "это повторяющееся событие", not a full RFC 5545 rendering.
pub(crate) fn humanize_rrule(rrule: &str) -> String {
    let up = rrule.to_uppercase();
    let get =
        |k: &str| up.split(&[';', ':'][..]).find_map(|p| p.strip_prefix(k).map(|v| v.to_string()));
    let interval: u32 = get("INTERVAL=").and_then(|v| v.parse().ok()).unwrap_or(1);
    let (each, unit) = match get("FREQ=").as_deref() {
        Some("DAILY") => ("Ежедневно", "дн."),
        Some("WEEKLY") => ("Еженедельно", "нед."),
        Some("MONTHLY") => ("Ежемесячно", "мес."),
        Some("YEARLY") => ("Ежегодно", "г."),
        _ => return "Повторяется".to_string(),
    };
    if interval > 1 { format!("Каждые {interval} {unit}") } else { each.to_string() }
}

/// Minutes-before-start → "N мин" / "N ч" / "N дн".
pub(crate) fn humanize_lead(min: i32) -> String {
    if min % 1440 == 0 && min >= 1440 {
        format!("{} дн", min / 1440)
    } else if min % 60 == 0 && min >= 60 {
        format!("{} ч", min / 60)
    } else {
        format!("{min} мин")
    }
}

/// Open the edit form populated from an existing event.
pub(crate) fn open_edit_form(
    ui: &MainWindow,
    sh: &Shared,
    ev: &ddmail_core::types::DesktopCalendarEvent,
) {
    let ids = fill_writable_calendars(ui, sh, false);
    let idx = ids.iter().position(|&id| id == ev.calendar_id).unwrap_or(0) as i32;
    *sh.edit_cal_ids.borrow_mut() = ids;
    sh.editing_event_id.set(ev.id);
    ui.set_edit_error("".into());
    ui.set_edit_is_create(false);
    ui.set_edit_title(ev.summary.clone().into());
    ui.set_edit_all_day(ev.all_day);
    set_form_start(ui, ev.dtstart);
    set_form_end(ui, ev.dtend.unwrap_or(ev.dtstart));
    ui.set_edit_location(ev.location.clone().into());
    ui.set_edit_description(ev.description.clone().into());
    ui.set_edit_calendar_idx(idx);

    // Read-only meeting details (see the card's edit-organizer block).
    ui.set_edit_organizer(
        if ev.organizer_email.is_empty() {
            String::new()
        } else {
            person_label(&ev.organizer_name, &ev.organizer_email)
        }
        .into(),
    );
    let attendees: Vec<AttendeeItem> = ev
        .attendees
        .iter()
        .map(|a| AttendeeItem {
            label: person_label(&a.name, &a.email).into(),
            dot: partstat_dot(&a.partstat),
        })
        .collect();
    ui.set_edit_attendees(ModelRc::new(VecModel::from(attendees)));
    let extras: Vec<EventExtraItem> = ev
        .extras
        .iter()
        .map(|x| {
            let (label, value) = extra_label(&x.name, &x.value);
            // Голый хост тоже ссылка (CONFERENCE/X-…-CONFERENCE часто без
            // схемы) — схему достраивает handle_link на клике. Правило то же,
            // что в карточке просмотра: строки здесь так же кликабельны.
            // Признак кликабельности и сам клик считает одна функция, иначе
            // строка подсвечивалась бы ссылкой, а клик по ней ничего не делал.
            let is_link = click_target(&value, LinkOrigin::Text).is_some();
            EventExtraItem { label: label.into(), value: value.into(), is_link }
        })
        .collect();
    ui.set_edit_extras(ModelRc::new(VecModel::from(extras)));
    let mut meta: Vec<String> = Vec::new();
    match ev.status.to_uppercase().as_str() {
        "CANCELLED" => meta.push("Отменено".to_string()),
        "TENTATIVE" => meta.push("Предварительно".to_string()),
        _ => {}
    }
    if !ev.rrule.is_empty() {
        meta.push(humanize_rrule(&ev.rrule));
    }
    if ev.alarm_lead_min > 0 {
        meta.push(format!("Напоминание за {}", humanize_lead(ev.alarm_lead_min)));
    }
    ui.set_edit_meta(meta.join(" · ").into());

    ui.set_edit_busy(false);
    ui.set_edit_visible(true);
}

/// Validate the form and dispatch Create or Patch to the engine.
///
/// Каждый отказ обязан попасть в `edit-error`: раньше проверки писали причину
/// в stderr и выходили, а пользователь видел кнопку, которая «не нажимается».
pub(crate) fn save_edit_form(ui: &MainWindow, sh: &Shared) {
    ui.set_edit_error("".into());
    let all_day = ui.get_edit_all_day();
    let Some(start) = parts_to_ms(
        ui.get_edit_s_year(),
        ui.get_edit_s_month(),
        ui.get_edit_s_day(),
        ui.get_edit_s_hour(),
        ui.get_edit_s_min(),
        all_day,
    ) else {
        eprintln!("edit: bad start time");
        ui.set_edit_error("Не удалось разобрать дату начала — проверьте поля «Начало».".into());
        return;
    };
    let end = parts_to_ms(
        ui.get_edit_e_year(),
        ui.get_edit_e_month(),
        ui.get_edit_e_day(),
        ui.get_edit_e_hour(),
        ui.get_edit_e_min(),
        all_day,
    );
    let title = ui.get_edit_title().to_string();
    let location = ui.get_edit_location().to_string();
    let description = ui.get_edit_description().to_string();
    let Some(etx) = sh.engine_tx.borrow().clone() else {
        ui.set_edit_error("Нет соединения с сервером — событие не отправлено.".into());
        return;
    };

    let editing = sh.editing_event_id.get();
    if editing == 0 {
        // Отрицательный индекс (ничего не выбрано) как usize превращается в
        // огромное число, поэтому `get` здесь ловит и «список пуст», и «выбор
        // не сделан» — но различить их для человека надо.
        let idx = ui.get_edit_calendar_idx() as usize;
        let Some(&cal_id) = sh.edit_cal_ids.borrow().get(idx) else {
            eprintln!("edit: no writable calendar selected (idx={idx})");
            let empty = sh.edit_cal_ids.borrow().is_empty();
            if empty {
                // Список приезжает единственным ответом FetchCalendars и
                // нигде не кэшируется: если тот ответ не пришёл (или пришёл
                // пустым из-за ошибки запроса), сохранять действительно
                // некуда. Просим список заново прямо отсюда — карточка
                // дозаполнится, когда ответ придёт.
                ui.set_edit_error(
                    "Список календарей не загружен — обновляю. Повторите сохранение через пару секунд."
                        .into(),
                );
                let _ = etx.send(engine::EngineCmd::FetchCalendars);
            } else {
                ui.set_edit_error("Выберите календарь для события.".into());
            }
            return;
        };
        let mut body = serde_json::json!({
            "calendar_id": cal_id,
            "summary": title,
            "description": description,
            "location": location,
            "all_day": all_day,
            "dtstart": start,
        });
        if let Some(e) = end {
            body["dtend"] = e.into();
        }
        let cal_account = sh.edit_cal_accounts.borrow().get(idx).cloned().unwrap_or_default();
        let _ = etx.send(engine::EngineCmd::CreateEvent { body, account_key: cal_account });
    } else {
        let mut body = serde_json::json!({
            "scope": "all",
            "summary": title,
            "description": description,
            "location": location,
            "all_day": all_day,
            "dtstart": start,
        });
        body["dtend"] = end.unwrap_or(0).into(); // explicit 0 ⇒ clear on server
        // Edit drops all prior reminders for this event; the refetch's seed()
        // recreates them from the new settings (incl. wiping a manual snooze).
        if let Some(c) = sh.cache.as_ref() {
            let _ = c.purge_event_reminders(editing);
        }
        let ak = sh.event_accounts.borrow().get(&editing).cloned().unwrap_or_default();
        let _ =
            etx.send(engine::EngineCmd::PatchEvent { event_id: editing, body, account_key: ak });
    }
    // Карточка остаётся до ответа движка: закроет её подтверждение (Done), а
    // отказ покажет причину прямо здесь. Кнопки на это время гаснут, чтобы
    // повторное нажатие не отправило второе событие.
    sh.pending_event_save.set(true);
    ui.set_edit_busy(true);
}
