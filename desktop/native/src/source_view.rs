//! The message source / headers viewer modal: text rendered to a bitmap
//! through the same selection layer as bubbles.

use super::*;

/// Max raw text fed to the source-viewer widget. Slint lays out the entire
/// string (no text virtualization), so a multi-MB source — a big message is
/// mostly base64 — would freeze rendering and selection. We show the first
/// chunk; «Копировать всё» still copies the full text from `source_view_full`.
/// How much of the raw source the viewer renders.
///
/// The renderer stops laying out at 8000 CSS px, and this text is ~17 CSS px
/// per line, so past roughly 32 KB nothing more can appear on screen no matter
/// how much is fed in — the old 128 KB bought a bigger bitmap and no extra
/// content. «Копировать всё» still copies the whole message.
pub(crate) const SOURCE_VIEW_MAX: usize = 32 * 1024;

/// The source viewer renders at 1×, not at the display scale.
///
/// It is a monospace dump of a whole message, so the bitmap is thousands of px
/// tall: at 2× it came out 1520 × 16000 — 93 MB, and past the 8192 px texture
/// limit most GPUs have, which is where the ten seconds went. At 1× the same
/// content is 24 MB and well inside every limit.
pub(crate) const SOURCE_RENDER_SCALE: f32 = 1.0;

/// Stash the full text and push a capped, render-safe slice into the viewer.
pub(crate) fn set_source_text(ui: &MainWindow, sh: &Shared, title: String, full: String) {
    let display = if full.len() > SOURCE_VIEW_MAX {
        let mut cut = SOURCE_VIEW_MAX;
        while cut > 0 && !full.is_char_boundary(cut) {
            cut -= 1;
        }
        format!(
            "{}\n\n[… показаны первые {} КБ из {} КБ — «Копировать всё» даёт полный исходник …]",
            &full[..cut],
            SOURCE_VIEW_MAX / 1024,
            full.len() / 1024
        )
    } else {
        full.clone()
    };
    sh.source_view_full.replace(full);
    // Reset the modal selection; the render job repopulates src_runs.
    sh.src_sel_moved.set(false);
    sh.src_sel_dragging.set(false);
    sh.src_runs.borrow_mut().clear();
    ui.set_source_view_title(title.into());
    ui.set_source_view_is_headers(false);
    ui.set_source_img_h(0.0);
    ui.set_source_selection_rects(ModelRc::new(VecModel::from(Vec::<SelRect>::new())));
    ui.set_source_view_visible(true);
    // The right-click menu may have taken focus; pull it back to the main key
    // sink so the modal's Ctrl+C/Escape (handled in kb) reach us.
    ui.invoke_grab_key_focus();
    // Render the (capped) text to a bitmap + word rects on the worker thread;
    // the modal then selects via the fast text-run layer.
    let _ = sh.tx.send(Job::RenderSource {
        text: display,
        width: SOURCE_RENDER_W,
        scale: SOURCE_RENDER_SCALE,
    });
}

/// Open the headers viewer on a raw header block.
pub(crate) fn show_headers(ui: &MainWindow, uid: u32, raw: &str) {
    let rows: Vec<HeaderRow> = parse_headers(raw)
        .into_iter()
        .map(|(name, value)| HeaderRow { name: name.into(), value: value.into() })
        .collect();
    ui.set_source_view_title(format!("Заголовки (id {uid})").into());
    ui.set_source_view_headers(ModelRc::new(VecModel::from(rows)));
    ui.set_source_view_is_headers(true);
    ui.set_source_view_visible(true);
}

/// Parse the RFC-822 header block into ordered (name, value) pairs.
///
/// Stops at the first blank line (end of headers). Folded values — RFC 5322
/// continuation lines starting with space/tab — are unfolded into the
/// preceding header's value. Order is preserved exactly as it appears in the
/// message. Values are returned raw (not RFC 2047-decoded); the table shows
/// the message as it is on the wire.
pub(crate) fn parse_headers(raw: &str) -> Vec<(String, String)> {
    let end = raw.find("\r\n\r\n").or_else(|| raw.find("\n\n")).unwrap_or(raw.len());
    let mut out: Vec<(String, String)> = Vec::new();
    for line in raw[..end].split('\n') {
        let line = line.strip_suffix('\r').unwrap_or(line);
        if line.is_empty() {
            continue;
        }
        if line.starts_with(' ') || line.starts_with('\t') {
            // Folded continuation — append to the current header's value.
            if let Some(last) = out.last_mut() {
                last.1.push(' ');
                last.1.push_str(line.trim_start());
            }
            continue;
        }
        match line.split_once(':') {
            Some((name, value)) => out.push((name.trim().to_string(), value.trim().to_string())),
            None => out.push((String::new(), line.trim().to_string())),
        }
    }
    out
}

/// Source viewer modal: «Копировать всё» and mouse selection over its bitmap.
pub(crate) fn wire_source_view(ui: &MainWindow, shared: &Rc<Shared>) {
    {
        let ui_weak = ui.as_weak();
        ui.on_source_view_copy(move || {
            use slint::Model;
            let Some(ui) = ui_weak.upgrade() else { return };
            if ui.get_source_view_is_headers() {
                let mut out = String::new();
                for h in ui.get_source_view_headers().iter() {
                    out.push_str(h.name.as_str());
                    out.push_str(": ");
                    out.push_str(h.value.as_str());
                    out.push('\n');
                }
                clipboard_set(&out);
            } else {
                // Full, untruncated source — not the capped slice in the widget.
                SHARED.with(|s| {
                    if let Some(sh) = s.borrow().as_ref() {
                        clipboard_set(&sh.source_view_full.borrow());
                    }
                });
            }
        });
    }

    // ── Mouse text selection over the rendered source bitmap (modal) ──
    {
        let ui_weak = ui.as_weak();
        let sh1 = shared.clone();
        ui.on_src_sel_start(move |x, y| {
            let Some(ui) = ui_weak.upgrade() else { return };
            sh1.src_sel_dragging.set(false);
            sh1.src_sel_moved.set(false);
            {
                let runs = sh1.src_runs.borrow();
                if let Some(i) = nearest_run(&runs, x, y) {
                    sh1.src_sel_anchor.set(i);
                    sh1.src_sel_head.set(i);
                    sh1.src_sel_dragging.set(true);
                }
            }
            ui.set_source_selection_rects(ModelRc::new(VecModel::from(Vec::<SelRect>::new())));
            // Keep the key sink focused so Ctrl+C lands in kb's modal branch.
            ui.invoke_grab_key_focus();
        });
        let ui_weak2 = ui.as_weak();
        let sh2 = shared.clone();
        ui.on_src_sel_move(move |x, y| {
            if !sh2.src_sel_dragging.get() {
                return;
            }
            let Some(ui) = ui_weak2.upgrade() else { return };
            let runs = sh2.src_runs.borrow();
            if let Some(i) = nearest_run(&runs, x, y) {
                if !sh2.src_sel_moved.get() && i == sh2.src_sel_anchor.get() {
                    return; // not an actual drag yet
                }
                sh2.src_sel_moved.set(true);
                sh2.src_sel_head.set(i);
                let rects =
                    selection_rects_for(&runs, sh2.src_sel_anchor.get(), sh2.src_sel_head.get());
                ui.set_source_selection_rects(ModelRc::new(VecModel::from(rects)));
            }
        });
        let sh3 = shared.clone();
        ui.on_src_sel_end(move || {
            sh3.src_sel_dragging.set(false);
        });
        let sh4 = shared.clone();
        ui.on_src_copy_selection(move || {
            let runs = sh4.src_runs.borrow();
            if sh4.src_sel_moved.get() {
                if let Some(t) =
                    selection_text_for(&runs, sh4.src_sel_anchor.get(), sh4.src_sel_head.get())
                {
                    println!("copy source selection: {} chars", t.len());
                    clipboard_set(&t);
                }
            }
        });
    }
}
