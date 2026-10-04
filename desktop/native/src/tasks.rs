//! Tasks (`view-mode 3`, VTODO — contract §5е): fetching, rows, toggling
//! done.

use super::*;

/// Ask the engine for the task list, honouring the "show completed" toggle.
pub(crate) fn fetch_tasks(ui: &MainWindow, sh: &Shared) {
    ui.set_tasks_loading(true);
    if let Some(etx) = sh.engine_tx.borrow().as_ref() {
        let _ =
            etx.send(engine::EngineCmd::FetchTasks { include_completed: ui.get_tasks_show_done() });
    }
}

/// Tick a task off, moving the row immediately.
///
/// The checkbox flips before the server is told. A CalDAV round trip can take
/// a second or two — waiting for it makes the list feel unresponsive, and the
/// engine emits an error if the write is refused, at which point the next
/// refresh corrects the row.
pub(crate) fn toggle_task(ui: &MainWindow, sh: &Shared, task_id: i64) {
    use slint::Model;

    let model = ui.get_tasks();
    let mut completed = false;
    let mut found = false;

    for i in 0..model.row_count() {
        if let Some(mut row) = model.row_data(i) {
            if row.id as i64 == task_id {
                row.done = !row.done;
                completed = row.done;
                found = true;
                // A ticked task is no longer overdue, whatever its due date
                // says; leaving the red would be a lie the next fetch corrects
                // a beat too late.
                if row.done {
                    row.due_color = slint::Color::from_rgb_u8(0xb3, 0xbc, 0xc7).into();
                }
                model.set_row_data(i, row);
                break;
            }
        }
    }

    if !found {
        return;
    }

    if let Some(etx) = sh.engine_tx.borrow().as_ref() {
        let _ = etx.send(engine::EngineCmd::SetTaskCompletion {
            task_id,
            completed,
            account_key: String::new(),
        });
    }
}

/// Build the task-list rows.
///
/// Dates are formatted here rather than in Slint, which has neither date
/// formatting nor a locale: the row receives the exact string it will draw,
/// already coloured by how overdue it is.
pub(crate) fn task_rows(list: &[ddmail_core::types::DesktopTask]) -> Vec<TaskRow> {
    let now = chrono::Local::now();
    let today = now.date_naive();

    list.iter()
        .map(|t| {
            let mut due_text = String::new();
            // Grey by default — "no deadline" is the common case and must not
            // look like a warning.
            let mut due_color = slint::Color::from_rgb_u8(0x8a, 0x94, 0xa0);

            if let Some(due_ms) = t.due {
                if let Some(dt) = chrono::DateTime::from_timestamp_millis(due_ms) {
                    let local = dt.with_timezone(&chrono::Local);
                    let date = local.date_naive();

                    // All-day tasks have no meaningful clock time; showing
                    // 00:00 on them reads as a real deadline at midnight.
                    due_text = if t.all_day {
                        format!("до {}", local.format("%d.%m.%Y"))
                    } else {
                        format!("до {}", local.format("%d.%m.%Y %H:%M"))
                    };

                    if date < today {
                        due_text = format!("{due_text} · просрочено");
                        due_color = slint::Color::from_rgb_u8(0xdc, 0x26, 0x26);
                    } else if date == today {
                        due_text = format!("{due_text} · сегодня");
                        due_color = slint::Color::from_rgb_u8(0xd9, 0x77, 0x06);
                    }
                }
            }

            // A completed task is never overdue, whatever its due date says.
            if t.completed {
                due_color = slint::Color::from_rgb_u8(0xb3, 0xbc, 0xc7);
            }

            TaskRow {
                id: t.id as i32,
                summary: t.summary.clone().into(),
                due_text: due_text.into(),
                due_color: due_color.into(),
                done: t.completed,
                calendar: t.calendar_name.clone().into(),
                color: parse_hex_color(&t.color).into(),
                can_write: t.can_write,
            }
        })
        .collect()
}
