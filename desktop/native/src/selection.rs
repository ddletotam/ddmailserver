//! Text selection over rendered bitmaps (PDF-viewer style, word granular —
//! contract §4в) and the system clipboard it copies to.

use super::*;

/// Index of the text run nearest to (x, y): the containing run when there
/// is one, otherwise the run with the closest centre. None for empty layers.
pub(crate) fn nearest_run(runs: &[render_common::TextRun], x: f32, y: f32) -> Option<usize> {
    if let Some(i) = runs.iter().position(|r| r.contains(x, y)) {
        return Some(i);
    }
    runs.iter()
        .enumerate()
        .min_by(|(_, a), (_, b)| {
            let da = (a.x + a.w / 2.0 - x).powi(2) + (a.y + a.h / 2.0 - y).powi(2);
            let db = (b.x + b.w / 2.0 - x).powi(2) + (b.y + b.h / 2.0 - y).powi(2);
            da.partial_cmp(&db).unwrap_or(std::cmp::Ordering::Equal)
        })
        .map(|(i, _)| i)
}

/// Границы визуальной строки, на которой лежит прогон `i` — для выделения
/// тройным кликом. Прогоны идут в порядке документа, строка это непрерывный
/// отрезок с той же вертикалью; признак «та же строка» тот же, что в
/// `selection_rects_for` и `selection_text_for` — `|Δy| < h * 0.6`.
///
/// Пустая строка (`h == 0`) сама себе строка: без этого допуск обнулялся бы и
/// отрезок схлопывался в один прогон.
pub(crate) fn line_bounds(runs: &[render_common::TextRun], i: usize) -> (usize, usize) {
    if runs.is_empty() {
        return (0, 0);
    }
    let i = i.min(runs.len() - 1);
    let same_line = |a: &render_common::TextRun| {
        let tol = (runs[i].h.max(a.h)) * 0.6;
        (a.y - runs[i].y).abs() < tol.max(0.5)
    };
    let mut lo = i;
    while lo > 0 && same_line(&runs[lo - 1]) {
        lo -= 1;
    }
    let mut hi = i;
    while hi + 1 < runs.len() && same_line(&runs[hi + 1]) {
        hi += 1;
    }
    (lo, hi)
}

/// Merged highlight rects for a run range: consecutive selected words on the
/// same visual line merge into one rect. Pure — shared by the bubble rows and
/// the source-viewer modal.
pub(crate) fn selection_rects_for(
    runs: &[render_common::TextRun],
    anchor: usize,
    head: usize,
) -> Vec<SelRect> {
    let mut rects: Vec<SelRect> = Vec::new();
    if runs.is_empty() {
        return rects;
    }
    let (lo, hi) = (anchor.min(head), anchor.max(head).min(runs.len() - 1));
    for r in &runs[lo..=hi] {
        match rects.last_mut() {
            // Same visual line → extend the previous rect.
            Some(last) if (last.y - r.y).abs() < r.h * 0.6 => {
                let right = (r.x + r.w).max(last.x + last.w);
                last.x = last.x.min(r.x);
                last.w = right - last.x;
                last.h = last.h.max(r.h);
            }
            _ => rects.push(SelRect { x: r.x, y: r.y, w: r.w, h: r.h }),
        }
    }
    rects
}

/// Selected words joined back into text: spaces within a line, a newline when
/// the next word starts a new visual line. Pure — shared by rows and modal.
pub(crate) fn selection_text_for(
    runs: &[render_common::TextRun],
    anchor: usize,
    head: usize,
) -> Option<String> {
    if runs.is_empty() {
        return None;
    }
    let (lo, hi) = (anchor.min(head), anchor.max(head).min(runs.len() - 1));
    let mut out = String::new();
    let mut prev: Option<&render_common::TextRun> = None;
    for r in &runs[lo..=hi] {
        if let Some(p) = prev {
            // A continuation run is the next line fragment of the SAME word
            // (wrapped URL etc.) — join with nothing so it pastes intact.
            if r.cont {
                // no separator
            } else if (r.y - p.y).abs() > p.h * 0.6 {
                out.push('\n');
            } else {
                out.push(' ');
            }
        }
        out.push_str(&r.text);
        prev = Some(r);
    }
    (!out.is_empty()).then_some(out)
}

/// Rebuild the bubble-row highlight rects for the current selection.
pub(crate) fn refresh_selection_rects(ui: &MainWindow, sh: &Shared) {
    let row = sh.sel_row.get();
    if row < 0 || !sh.sel_moved.get() {
        ui.set_selection_row(-1);
        ui.set_selection_rects(ModelRc::new(VecModel::from(Vec::<SelRect>::new())));
        return;
    }
    let runs_all = sh.row_text_runs.borrow();
    let Some(runs) = runs_all.get(row as usize) else { return };
    let rects = selection_rects_for(runs, sh.sel_anchor.get(), sh.sel_head.get());
    ui.set_selection_rects(ModelRc::new(VecModel::from(rects)));
    ui.set_selection_row(row);
}

/// Selected bubble-row text (legacy entry point for the row selection).
pub(crate) fn selection_text(sh: &Shared) -> Option<String> {
    let row = sh.sel_row.get();
    if row < 0 || !sh.sel_moved.get() {
        return None;
    }
    let runs_all = sh.row_text_runs.borrow();
    let runs = runs_all.get(row as usize)?;
    selection_text_for(runs, sh.sel_anchor.get(), sh.sel_head.get())
}

thread_local! {
    // Held for the whole session. On X11 the clipboard is served live by the
    // owning process, so a Clipboard created per-call and dropped immediately
    // loses ownership the instant it returns — paste then comes back empty.
    // Keeping one instance alive keeps us the owner so paste actually works.
    pub(crate) static CLIPBOARD: RefCell<Option<arboard::Clipboard>> = RefCell::new(None);
}

pub(crate) fn clipboard_set(text: &str) {
    CLIPBOARD.with(|c| {
        let mut slot = c.borrow_mut();
        if slot.is_none() {
            match arboard::Clipboard::new() {
                Ok(cb) => *slot = Some(cb),
                Err(e) => {
                    eprintln!("clipboard init: {e}");
                    return;
                }
            }
        }
        if let Some(cb) = slot.as_mut() {
            if let Err(e) = cb.set_text(text.to_string()) {
                eprintln!("clipboard set: {e}");
            }
        }
    });
}

/// Экранировать plain-текст для вставки в HTML-тело (цитата пересылки).
pub(crate) fn html_escape_plain(s: &str) -> String {
    s.replace('&', "&amp;").replace('<', "&lt;").replace('>', "&gt;")
}

/// Текст из буфера обмена (None — там не текст либо буфер пуст).
pub(crate) fn clipboard_text() -> Option<String> {
    CLIPBOARD.with(|c| {
        let mut slot = c.borrow_mut();
        if slot.is_none() {
            slot.replace(arboard::Clipboard::new().ok()?);
        }
        slot.as_mut()?.get_text().ok()
    })
}

/// Картинка из буфера обмена, перекодированная в PNG (байты, ширина, высота).
/// Буфер отдаёт сырой RGBA — в письмо такое не положишь, нужен нормальный
/// формат с сжатием.
pub(crate) fn clipboard_image() -> Option<(Vec<u8>, u32, u32)> {
    let raw = CLIPBOARD.with(|c| {
        let mut slot = c.borrow_mut();
        if slot.is_none() {
            slot.replace(arboard::Clipboard::new().ok()?);
        }
        slot.as_mut()?.get_image().ok()
    })?;
    let (w, h) = (raw.width as u32, raw.height as u32);
    let buf = image::RgbaImage::from_raw(w, h, raw.bytes.into_owned())?;
    let mut png = Vec::new();
    image::DynamicImage::ImageRgba8(buf)
        .write_to(&mut std::io::Cursor::new(&mut png), image::ImageFormat::Png)
        .ok()?;
    Some((png, w, h))
}

// ── Rich-text композер ─────────────────────────────────────────────────────
// Модель и вёрстка живут в richtext.rs / richtext_render.rs; здесь — только
// мост в UI: перерисовка, клавиши, мышь, буфер обмена.

#[cfg(test)]
mod line_bounds_tests {
    use super::line_bounds;
    use crate::render_common::TextRun;

    fn run(x: f32, y: f32, text: &str) -> TextRun {
        TextRun { x, y, w: 40.0, h: 18.0, text: text.into(), cont: false }
    }

    /// Три строки по два слова. Тройной клик по любому слову даёт отрезок
    /// ровно своей строки — ни соседней сверху, ни снизу.
    #[test]
    fn line_bounds_covers_only_its_line() {
        let runs = vec![
            run(0.0, 0.0, "первая"),
            run(50.0, 0.0, "строка"),
            run(0.0, 20.0, "вторая"),
            run(50.0, 20.0, "строка"),
            run(0.0, 40.0, "третья"),
        ];
        assert_eq!(line_bounds(&runs, 0), (0, 1));
        assert_eq!(line_bounds(&runs, 1), (0, 1));
        assert_eq!(line_bounds(&runs, 2), (2, 3));
        assert_eq!(line_bounds(&runs, 3), (2, 3));
        assert_eq!(line_bounds(&runs, 4), (4, 4));
    }

    /// Строка из одного слова и пустой список не должны ломать индексы.
    #[test]
    fn line_bounds_degenerate() {
        assert_eq!(line_bounds(&[], 0), (0, 0));
        let one = vec![run(0.0, 0.0, "одно")];
        assert_eq!(line_bounds(&one, 0), (0, 0));
        // Индекс за пределами зажимается, а не паникует.
        assert_eq!(line_bounds(&one, 99), (0, 0));
    }

    /// Нулевая высота прогона не должна обнулять допуск и схлопывать строку.
    #[test]
    fn line_bounds_zero_height() {
        let runs = vec![
            TextRun { x: 0.0, y: 0.0, w: 10.0, h: 0.0, text: "a".into(), cont: false },
            TextRun { x: 20.0, y: 0.0, w: 10.0, h: 0.0, text: "b".into(), cont: false },
            TextRun { x: 0.0, y: 30.0, w: 10.0, h: 0.0, text: "c".into(), cont: false },
        ];
        assert_eq!(line_bounds(&runs, 0), (0, 1));
        assert_eq!(line_bounds(&runs, 2), (2, 2));
    }
}

/// Mouse text selection over bubbles and copying it.
pub(crate) fn wire_bubble_selection(ui: &MainWindow, shared: &Rc<Shared>) {
    // ── Mouse text selection over bubbles ──
    let ui_weak_ss = ui.as_weak();
    let sh_ss = shared.clone();
    ui.on_sel_start(move |row, x, y| {
        let Some(ui) = ui_weak_ss.upgrade() else { return };
        // Серия кликов: второй выделяет слово, третий — строку. Считаем сами,
        // у Slint нет ни двойного, ни тройного события. Порог 4px гасит дрожь
        // руки, но не даёт склеить клики по разным словам.
        let now = Instant::now();
        let (prow, px, py) = sh_ss.sel_click_pos.get();
        let same_spot = prow == row && (px - x).abs() < 4.0 && (py - y).abs() < 4.0;
        let quick = sh_ss
            .sel_click_at
            .get()
            .is_some_and(|t| now.duration_since(t) < Duration::from_millis(450));
        let streak = if same_spot && quick { sh_ss.sel_click_streak.get() + 1 } else { 1 };
        sh_ss.sel_click_streak.set(streak);
        sh_ss.sel_click_at.set(Some(now));
        sh_ss.sel_click_pos.set((row, x, y));

        sh_ss.sel_dragging.set(false);
        sh_ss.sel_moved.set(false);
        sh_ss.sel_row.set(-1);
        if let Some(runs) = sh_ss.row_text_runs.borrow().get(row as usize) {
            if let Some(i) = nearest_run(runs, x, y) {
                sh_ss.sel_row.set(row);
                let (anchor, head) = match streak {
                    1 => (i, i),
                    // Прогон и есть слово, так что двойной клик — это ровно он.
                    2 => (i, i),
                    _ => line_bounds(runs, i),
                };
                sh_ss.sel_anchor.set(anchor);
                sh_ss.sel_head.set(head);
                sh_ss.sel_dragging.set(true);
                if streak >= 2 {
                    // Выделение уже состоялось: пусть держится после отпускания
                    // (иначе `sel_end` сочтёт это кликом) и не открывает ссылку,
                    // если кликнули по ней.
                    sh_ss.sel_moved.set(true);
                    sh_ss.sel_suppress_click.set(true);
                }
            }
        }
        // Clear any previous highlight; Ctrl+C must reach the key sink.
        refresh_selection_rects(&ui, &sh_ss);
        ui.invoke_grab_key_focus();
    });
    let ui_weak_sm = ui.as_weak();
    let sh_sm = shared.clone();
    ui.on_sel_move(move |row, x, y| {
        if !sh_sm.sel_dragging.get() || sh_sm.sel_row.get() != row {
            return;
        }
        let Some(ui) = ui_weak_sm.upgrade() else { return };
        let head =
            sh_sm.row_text_runs.borrow().get(row as usize).and_then(|runs| nearest_run(runs, x, y));
        if let Some(i) = head {
            if !sh_sm.sel_moved.get() && i == sh_sm.sel_anchor.get() {
                return; // not an actual drag yet
            }
            sh_sm.sel_moved.set(true);
            sh_sm.sel_head.set(i);
            refresh_selection_rects(&ui, &sh_sm);
        }
    });
    let sh_se = shared.clone();
    ui.on_sel_end(move || {
        sh_se.sel_dragging.set(false);
        if sh_se.sel_moved.get() {
            // The release also fires `clicked` — it must not open a link.
            sh_se.sel_suppress_click.set(true);
        }
    });
    let sh_cs = shared.clone();
    ui.on_copy_selection(move || {
        if let Some(text) = selection_text(&sh_cs) {
            println!("copy selection: {} chars", text.len());
            clipboard_set(&text);
        }
    });
}
