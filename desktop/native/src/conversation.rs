//! The open conversation: opening it, its header, the scroll anchor and
//! the render jobs sent to `render_worker` (contract §4, «Куда встаёт скролл
//! диалога»).

use super::*;

/// Open a conversation by index: show cached bodies immediately, and (if a live
/// engine is running) fire a background fetch to refresh them.
/// `#[track_caller]` — чтобы `[perf]`-строка называла, ОТКУДА диалог открыли.
/// Путей больше десятка (клик в списке, стрелки, поиск, тост, отправка,
/// склейка, возврат в почту), а по логу они выглядели одинаково: на поиск
/// виновника «кто это открыл диалог» уходил час на каждый заход.
#[track_caller]
pub(crate) fn open_conversation(ui: &MainWindow, sh: &Shared, idx: usize) {
    let caller = std::panic::Location::caller();
    let t0 = Instant::now();
    // Незаконченное переименование относится к прежнему диалогу — снять,
    // иначе Enter переименовал бы уже этот. Перечит того же диалога (дельта,
    // новое письмо) поле не трогает: пользователь может быть посреди ввода.
    if sh.current.get() != idx {
        ui.set_rename_open(false);
    }
    sh.current.set(idx);
    // New conversation generation: any in-flight FetchMessages answer for
    // the previously open conversation will be dropped on arrival.
    let generation = sh.open_gen.get() + 1;
    sh.open_gen.set(generation);
    let convs = sh.convs.borrow();
    let Some(c) = convs.get(idx) else { return };
    let conv_label = c.label.clone();
    let msg_count = c.messages.len();
    // Открытие диалога само по себе показывает всё, что в нём есть, — повод
    // перерисовывать его при возврате в почту снят.
    sh.missed_mail.set(false);
    // Запомнить открытый диалог — следующий запуск вернётся к нему, а не к
    // первому в списке. Пишем сразу, как и остальные настройки: «сохраним на
    // выходе» не выживает ни выхода через трей, ни kill. Сравнение — чтобы
    // ходьба стрелками по одному и тому же диалогу не переписывала файл.
    if *sh.last_conv_id.borrow() != c.id {
        *sh.last_conv_id.borrow_mut() = c.id.clone();
        save_calendar_settings(ui, sh);
    }
    // Which account this conversation belongs to (empty → primary). Drives the
    // cache namespace and every addressed command issued while it's open.
    let akey = if c.account_key.is_empty() { sh.key.clone() } else { c.account_key.clone() };
    sh.accounts.cur_account_key.replace(akey.clone());
    // Смена контекста — сбрасываем закреплённый ручной выбор отправителя:
    // новая беседа по умолчанию отвечает со своей received_by identity, и
    // aim ниже её проставляет. Пользователь снова может переопределить.
    sh.compose.picked_identity.borrow_mut().take();
    // Replies default to the identity that received this conversation; the
    // from-picker shows it and the user can still override before sending.
    aim_composer_identity(ui, sh, &c.received_by);
    ui.set_identity_menu_open(false);

    // The right pane clears IMMEDIATELY: stale bubbles from the previous
    // conversation must never linger while this one loads. The progress
    // bar appears right away (seeded with the ref count; the render job
    // re-seeds it with the real body count when it starts).
    ui.set_messages(ModelRc::new(VecModel::from(Vec::<RowItem>::new())));
    ui.set_render_total(msg_count.max(1) as i32);
    ui.set_render_progress(0);
    sh.current_msgs.borrow_mut().clear();
    sh.current_bodies.borrow_mut().clear();
    // Optimistic-send stubs live and die with the pane they were drawn in.
    sh.compose.pending_sends.borrow_mut().clear();

    // Unread snapshot BEFORE we mark anything read — it anchors the
    // scroll (first unread at top; none unread → scroll to the end).
    let unread: Vec<MessageRef> = c.messages.iter().filter(|m| !m.seen).cloned().collect();
    let had_unread = !unread.is_empty();
    *sh.open_unread.borrow_mut() = unread.iter().map(|m| (m.folder.clone(), m.uid)).collect();
    sh.scroll_pending.set(true);

    if let Some(cache) = &sh.cache {
        let key = &akey;
        let t_load_start = Instant::now();
        let bodies = cache.load_message_bodies(key, &c.messages).unwrap_or_default();
        let load_ms = t_load_start.elapsed().as_millis();
        if !bodies.is_empty() {
            *sh.current_msgs.borrow_mut() = bodies
                .iter()
                .map(|b| MessageRef {
                    folder: b.folder.clone(),
                    uid: b.uid,
                    message_id: b.message_id.clone(),
                    seen: true,
                })
                .collect();
            *sh.current_bodies.borrow_mut() = bodies.clone();
            println!(
                "[perf] open_conversation idx={idx} label={conv_label:?} \
                 messages={msg_count} bodies={} cache_load={load_ms}ms \
                 enqueue@{:?} caller={caller}",
                bodies.len(),
                t0.elapsed()
            );
            // Peek, don't consume: the FetchMessages answer will re-render
            // and must carry the same anchor (it aborts this render).
            let scroll = take_scroll_target(sh, &bodies, false);
            send_render_job(sh, bodies, scroll);
        } else {
            println!(
                "[perf] open_conversation idx={idx} label={conv_label:?} \
                 messages={msg_count} cache_miss (no cached bodies) \
                 cache_load={load_ms}ms caller={caller}"
            );
        }
    }
    // Ghost hints in the chevron panel follow the newly opened dialog.
    refresh_composer_hints(ui, sh);
    if let Some(etx) = sh.engine_tx.borrow().as_ref() {
        // A toast-click target may be newer than the cached conversation
        // refs — make sure the fetch includes it.
        let mut fetch_refs = c.messages.clone();
        if let Some((f, u)) = sh.pending_open_ref.borrow().clone() {
            if !fetch_refs.iter().any(|m| m.uid == u) {
                // Toast target identified only by (folder, uid); no RFC
                // Message-ID here — server falls back to uid for this one.
                fetch_refs.push(MessageRef {
                    folder: f,
                    uid: u,
                    message_id: String::new(),
                    seen: false,
                });
            }
        }
        let _ = etx.send(engine::EngineCmd::FetchMessages {
            messages: fetch_refs,
            generation,
            account_key: akey.clone(),
        });
        // Opening a conversation reads it: push \Seen for everything that
        // was unread. The scroll anchor above is already snapshotted.
        if !unread.is_empty() {
            let _ = etx.send(engine::EngineCmd::SetFlags {
                messages: unread,
                flags: "\\Seen".into(),
                add: true,
                account_key: akey.clone(),
            });
        }
    }
    drop(convs);
    // Optimistic badge clear: the server-side mark-read lands via the
    // delta refetch a second later, but the sidebar must not keep showing
    // an unread pill for the conversation the user is literally reading.
    if had_unread {
        let key_flag = sh.convs.borrow_mut().get_mut(idx).map(|c| {
            c.unread_count = 0;
            for m in c.messages.iter_mut() {
                m.seen = true;
            }
            (conv_merge_key(&sh.key, c), c.merged)
        });
        if let Some((k, merged)) = key_flag {
            mark_raw_seen(sh, &k, merged);
        }
        let displays = displays_from(&sh.convs.borrow(), &sh.identity_colors.borrow());
        *sh.displays.borrow_mut() = displays;
        refresh_sidebar(sh, ui);
        tray_sync_dot(sh);
    }
}

/// Tauri-era header meta line: counterpart address (1:1) or participants
/// (group), plus " → receiving identity". Кусками: у каждого адреса свой
/// `addr`, клик по нему копирует адрес (контракт §4, «Шапка диалога»).
pub(crate) fn conv_meta_parts(c: &Conversation) -> Vec<MetaPart> {
    let part = |text: &str, addr: &str| MetaPart { text: text.into(), addr: addr.into() };
    let mut out = Vec::new();
    if c.is_group {
        for (i, cp) in c.counterparts.iter().enumerate() {
            if i > 0 {
                out.push(part(", ", ""));
            }
            let text = if cp.name.is_empty() { &cp.addr } else { &cp.name };
            out.push(part(text, &cp.addr));
        }
    } else if let Some(cp) = c.counterparts.first() {
        out.push(part(&cp.addr, &cp.addr));
    }
    if !c.received_by.is_empty() {
        out.push(part(" → ", ""));
        out.push(part(&c.received_by, &c.received_by));
    }
    out
}

/// Ненавязчивое подтверждение мутации — плашка «✓ …» над композером ~2 с
/// (контракт §5: никогда не тостом).
pub(crate) fn flash_confirm(ui: &MainWindow, text: &str) {
    ui.set_send_confirm_text(text.into());
    ui.set_send_confirm_visible(true);
    let uiw = ui.as_weak();
    slint::Timer::single_shot(std::time::Duration::from_millis(2000), move || {
        if let Some(u) = uiw.upgrade() {
            u.set_send_confirm_visible(false);
        }
    });
}

/// Set every header property for conversation `idx` in one place: name,
/// initials, avatar colour, identity tint and the meta line. Replaces the
/// four hand-synced copies the review flagged.
pub(crate) fn apply_active_header(ui: &MainWindow, sh: &Shared, idx: usize) {
    if let Some(d) = sh.displays.borrow().get(idx) {
        ui.set_active_name(d.name.clone().into());
        ui.set_active_initials(d.initials.clone().into());
        ui.set_active_color(slint::Brush::SolidColor(hex(&d.color)));
        // Та же аватарка, что в строке списка — из того же кэша по тому же
        // ключу; инициалы остаются запасным вариантом.
        let avatar = sh.avatars.borrow().get(&d.email).cloned();
        ui.set_active_has_avatar(avatar.is_some());
        ui.set_active_avatar(avatar.unwrap_or_default());
        ui.set_active_ident_color(if d.ident_color.is_empty() {
            slint::Brush::SolidColor(hex("#ffffff"))
        } else {
            slint::Brush::SolidColor(hex(&d.ident_color))
        });
    }
    let meta = sh.convs.borrow().get(idx).map(conv_meta_parts).unwrap_or_default();
    ui.set_active_meta_parts(ModelRc::new(VecModel::from(meta)));
}

/// Mirror the policy's global «Медиа…» switches into root properties so
/// the menu's checkmarks and enabled-states stay live.
pub(crate) fn sync_media_globals(ui: &MainWindow, p: &policy::Policy) {
    ui.set_media_allow_all_on(p.allow_all);
    ui.set_media_all_images_on(p.allow_all_media);
    ui.set_media_all_scripts_on(p.allow_all_scripts);
}

/// Consume the pending post-open scroll: row index of the LAST unread
/// body (top-aligned), or -1 for "scroll to the end" when everything was
/// already read. None when this render isn't the first one after open.
/// Post-open scroll anchor: the FIRST unread row (the whole unread run then
/// reads top-to-bottom), or -1 (= scroll to end) when nothing is unread.
/// `consume` clears the pending flag; peek mode
/// (`consume=false`) is for the optimistic cached render — the anchor must
/// survive until the FetchMessages answer re-renders the conversation,
/// because that render ABORTS the cached one (seq bump) and would otherwise
/// arrive with no scroll target, losing the jump entirely.
pub(crate) fn take_scroll_target(
    sh: &Shared,
    bodies: &[MessageBody],
    consume: bool,
) -> Option<i32> {
    if !sh.scroll_pending.get() {
        return None;
    }
    if consume {
        sh.scroll_pending.set(false);
    }
    // Toast-click target wins: jump straight to the clicked message. Peek
    // mode must not take() the ref — open_conversation still needs it for
    // fetch_refs, and the consuming render needs it to re-anchor.
    let toast_uid = if consume {
        sh.pending_open_ref.borrow_mut().take().map(|(_, u)| u)
    } else {
        sh.pending_open_ref.borrow().as_ref().map(|(_, u)| *u)
    };
    if let Some(uid) = toast_uid {
        if let Some(r) = bodies.iter().position(|b| b.uid == uid) {
            return Some(r as i32);
        }
    }
    let unread = sh.open_unread.borrow();
    Some(
        bodies
            .iter()
            .position(|b| unread.contains(&(b.folder.clone(), b.uid)))
            .map(|r| r as i32)
            .unwrap_or(-1),
    )
}

/// Enqueue a (re)render of `bodies` at the current width/policy. Bumps the
/// shared render sequence so any older queued job becomes a no-op and a
/// mid-render older job aborts (latest wins).
pub(crate) fn send_render_job(sh: &Shared, bodies: Vec<MessageBody>, scroll_to: Option<i32>) {
    let seq = sh.render_seq.fetch_add(1, Ordering::SeqCst) + 1;
    let overrides = sh.body_view_text.borrow();
    let modes: Vec<u8> =
        bodies.iter().map(|b| u8::from(overrides.contains(&(b.folder.clone(), b.uid)))).collect();
    drop(overrides);
    // Склейка — свойство открытого диалога, а не писем: новое письмо
    // compose-режима ни к какой склейке не относится.
    let merged = sh.compose.pending_compose.borrow().is_none()
        && sh.convs.borrow().get(sh.current.get()).is_some_and(|c| c.merged);
    let _ = sh.tx.send(Job::SetConversation {
        bodies,
        width: sh.width.get(),
        policy: sh.policy.borrow().clone(),
        policy_gen: sh.policy_gen.get(),
        seq,
        scroll_to,
        modes,
        scale: sh.render_scale.get(),
        merged,
    });
}

/// Extract a bare lowercase address from a "Name <addr>" header value.
pub(crate) fn header_addr(raw: &str) -> String {
    if let (Some(i), Some(j)) = (raw.rfind('<'), raw.rfind('>')) {
        if i < j {
            return raw[i + 1..j].trim().to_lowercase();
        }
    }
    raw.trim().to_lowercase()
}

/// Через `delay_ms` повторить bump `chat-scroll-seq`, если переписка ещё не
/// доехала до `target`. Нужно всюду, где панель почты могла создаваться в
/// момент bump'а: мост `changed x` живёт ВНУТРИ панели, и bump, сделанный до
/// её появления, до моста не доходит — ни `changed`, ни первая раскладка его
/// не подберут (см. контракт §4).
///
/// Проверка по зеркалу `chat-vp-y` (свежая панель обнуляет его в `init`)
/// делает повтор безвредным: доехали или пользователь сам увёл вид — не
/// дёргаем. Цель клампится по содержимому здесь же, иначе «в конец» (`1e9`,
/// как его ставит рендер) никогда не совпало бы с реальной позицией и
/// каждый повтор считал бы, что не доехали.
pub(crate) fn nudge_chat_scroll(ui_weak: slint::Weak<MainWindow>, target: f32, delay_ms: u64) {
    slint::Timer::single_shot(std::time::Duration::from_millis(delay_ms), move || {
        let Some(ui) = ui_weak.upgrade() else { return };
        let content_h = ui.get_chat_content_h();
        let view_h = ui.get_chat_view_h();
        let want = if content_h > 0.0 && view_h > 0.0 {
            target.min((content_h - view_h).max(0.0))
        } else {
            target
        };
        if (-ui.get_chat_vp_y() - want).abs() <= 1.0 {
            return;
        }
        ui.set_chat_scroll_seq(ui.get_chat_scroll_seq() + 1);
    });
}

/// Switching between mail, calendar, address book and tasks — with the
/// mail panel's scroll position saved and restored around it (contract §4).
pub(crate) fn wire_view_switch(ui: &MainWindow, shared: &Rc<Shared>) {
    // ── Calendar callbacks ──
    //
    // Switching into the calendar view triggers the initial fetch of
    // both calendars + this-week events. Navigation buttons (prev /
    // today / next) and the workdays/non-work-hours toggles all push
    // the week-start forward/backward and re-fetch.
    let ui_weak_view = ui.as_weak();
    let sh_view = shared.clone();
    ui.on_view_changed(move |mode| {
        if let Some(ui) = ui_weak_view.upgrade() {
            if mode == 0 {
                // Возврат в почту. Панель почты условная, поэтому ListView
                // здесь СВЕЖИЙ и стоит в начале — вернуть скролл туда, где
                // его оставили, обязаны мы.
                //
                // Снимок ОДНОРАЗОВЫЙ: «Почта» при уже открытой почте зовёт
                // этот же обработчик, но панель не пересоздаётся, и
                // восстановление дёрнуло бы вид на старую позицию без
                // причины. Нечего восстанавливать → не трогаем вообще: на
                // первом входе анкор ставит сам open_conversation.
                //
                // Флаг ставим напрямую, а не бампом chat-scroll-seq: мост
                // (`changed x`) живёт внутри той же условной панели, и при её
                // пересоздании новое значение seq — начальное, а не
                // изменение, так что `changed` не сработает. Применит скролл
                // сам ListView на первом реальном layout (`viewport-height` /
                // `height`), как это уже сделано для сетки календаря.
                // Пока нас не было, в открытый диалог пришло письмо —
                // возвращать позицию, на которой ушли, значит спрятать его
                // под сгибом (а точнее — не показать вовсе: панели не
                // существовало, и в переписку письмо не дорисовывалось).
                // Открываем диалог заново: анкор встанет на первое
                // непрочитанное, то есть на него.
                let saved = sh_view.chat_vp_y.replace(-1.0);
                if sh_view.missed_mail.replace(false) {
                    let idx = sh_view.current.get();
                    apply_active_header(&ui, &sh_view, idx);
                    open_conversation(&ui, &sh_view, idx);
                } else if saved >= 0.0 {
                    ui.set_chat_scroll_y(saved);
                    ui.set_chat_scroll_pending(true);
                    // Одного флага мало: при создании панели с уже готовой
                    // геометрией пропадают ОБА триггера — bump seq был до
                    // появления моста, а первая раскладка не «изменение»
                    // свойства, так что ни один `changed` не срабатывает и
                    // переписка остаётся в начале. Тот же приём, что у сетки
                    // календаря (`scroll_calendar_to_hour`): доложить
                    // bump'ом после того, как панель уже существует.
                    nudge_chat_scroll(ui.as_weak(), saved, 120);
                    // Вторая попытка — на случай, когда к 120 мс раскладка
                    // ещё не настоящая (длинная переписка, рендер догоняет).
                    // Обе проверяют, доехал ли вид, и молчат, если да.
                    nudge_chat_scroll(ui.as_weak(), saved, 400);
                }
            } else {
                // Уходим из почты — снять позицию ДО того, как панель
                // уничтожится. chat-vp-y отрицательный (offset вверх).
                sh_view.chat_vp_y.set((-ui.get_chat_vp_y()).max(0.0));
            }
            if mode == 1 {
                // Land the viewport on the working day, not on 00:00 —
                // consumed by apply_calendar_view once the layout is real.
                sh_view.cal.pending_cal_scroll.set(Some(sh_view.cal.work_start.get() as f32));
                apply_calendar_view(&ui, &sh_view);
                if let Some(etx) = sh_view.engine_tx.borrow().as_ref() {
                    let _ = etx.send(engine::EngineCmd::FetchCalendars);
                }
                refetch_calendar_events(&ui, &sh_view);
            } else if mode == 2 {
                // Enter the address book: load the full book (empty query).
                ui.set_contacts_query("".into());
                fetch_contacts(&sh_view, "");
            } else if mode == 3 {
                fetch_tasks(&ui, &sh_view);
            }
        }
    });
}

/// The bubble context menu: reply / forward / source / headers / text-HTML
/// view / media permissions, and cancelling a staged reply.
pub(crate) fn wire_message_actions(ui: &MainWindow, shared: &Rc<Shared>) {
    // Context-menu actions on a message row.
    let ui_weak_act = ui.as_weak();
    let sh_act = shared.clone();
    ui.on_msg_action(move |row, action| {
        let row = row as usize;
        let action = action.to_string();
        let msg = sh_act.current_msgs.borrow().get(row).cloned();
        let Some(msg) = msg else { return };
        // Toggle per-sender media/scripts allowance. Cache-aware: bumps
        // policy_gen so the body_cache misses for entries rendered
        // under the old policy, and re-fires SetConversation so the
        // bubbles repaint immediately.
        // «Медиа…» menu: every item toggles one policy switch, persists it
        // immediately, and repaints (the policy generation is part of the
        // texture cache key, so the re-render is guaranteed to miss).
        if action.starts_with("media-") {
            let body_opt = sh_act.current_bodies.borrow().get(row).cloned();
            let Some(b) = body_opt else { return };
            let sender = b.from_addr.clone();
            let (media_host, script_host) =
                sanitize::first_external_hosts(b.html.as_deref().unwrap_or(""));
            {
                let mut p = sh_act.policy.borrow_mut();
                match action.as_str() {
                    "media-allow-all" => p.allow_all = !p.allow_all,
                    "media-scripts-all" => p.allow_all_scripts = !p.allow_all_scripts,
                    "media-scripts-sender" => {
                        p.toggle_scripts(&sender);
                    }
                    "media-scripts-host" => {
                        if script_host.is_empty() {
                            return;
                        }
                        p.toggle_script_host(&script_host);
                    }
                    "media-images-all" => p.allow_all_media = !p.allow_all_media,
                    "media-images-sender" => {
                        p.toggle_media(&sender);
                    }
                    "media-images-host" => {
                        if media_host.is_empty() {
                            return;
                        }
                        p.toggle_media_host(&media_host);
                    }
                    other => {
                        println!("media action {other} — not wired");
                        return;
                    }
                }
                println!("[policy] {action} (sender={sender}, img={media_host}, js={script_host})");
                // Bump the persisted generation BEFORE saving: the texture
                // cache key must change atomically with the policy.
                p.generation += 1;
                let gen_now = p.generation;
                policy::save(&p);
                sh_act.policy_gen.set(gen_now);
            }
            if let Some(ui) = ui_weak_act.upgrade() {
                sync_media_globals(&ui, &sh_act.policy.borrow());
            }
            // Repaint the in-memory bodies under the new policy — no SQLite
            // reload and no network refetch for a permission toggle.
            let bodies = sh_act.current_bodies.borrow().clone();
            send_render_job(&sh_act, bodies, None);
            return;
        }

        // Reply doesn't need the live engine — we just stage the bubble's
        // body into the quote ribbon and let the next Send pick up the
        // subject + threading headers.
        if action == "reply" {
            let body_opt = sh_act.current_bodies.borrow().get(row).cloned();
            let Some(body) = body_opt else {
                eprintln!("reply: body not in memory for {msg:?}");
                return;
            };
            if let Some(ui) = ui_weak_act.upgrade() {
                enter_reply_mode(&sh_act, &ui, body);
            }
            return;
        }
        // Forward: prefill the composer with the quoted original + a "Fwd:"
        // subject, then let the user pick a recipient via search (same path
        // as any new message). Attachments aren't carried yet — noted inline.
        if action == "forward" {
            let body_opt = sh_act.current_bodies.borrow().get(row).cloned();
            let Some(body) = body_opt else {
                eprintln!("forward: body not in memory for {msg:?}");
                return;
            };
            if let Some(ui) = ui_weak_act.upgrade() {
                enter_forward_mode(&sh_act, &ui, body);
            }
            return;
        }
        // «Копировать текст» — the whole message's plain-text part.
        if action == "copy" {
            let body_opt = sh_act.current_bodies.borrow().get(row).cloned();
            let Some(body) = body_opt else { return };
            let text = body
                .text
                .filter(|t| !t.trim().is_empty())
                .unwrap_or_else(|| "(письмо без текстовой версии)".to_string());
            println!("copy message text: {} chars", text.len());
            clipboard_set(&text);
            return;
        }
        // «Показать → Заголовки / Исходник сообщения» — fetch the raw
        // RFC-822 source; the result handler opens the viewer with the
        // requested slice.
        if action == "show-headers" || action == "show-source" {
            // Headers come out of the cache when they are there: the sync
            // already had the whole message in hand and kept the wire header
            // block, so asking the server again would be a login and a
            // multi-megabyte download for thirty lines.
            if action == "show-headers" {
                let cached = sh_act
                    .current_bodies
                    .borrow()
                    .get(row)
                    .map(|b| b.raw_headers.clone())
                    .filter(|h| !h.is_empty());
                if let Some(raw) = cached {
                    if let Some(ui) = ui_weak_act.upgrade() {
                        show_headers(&ui, msg.uid, &raw);
                    }
                    return;
                }
            }
            sh_act.pending_source_view.set(if action == "show-headers" { 1 } else { 2 });
            if let Some(etx) = sh_act.engine_tx.borrow().as_ref() {
                let _ = etx.send(engine::EngineCmd::FetchSource {
                    folder: msg.folder.clone(),
                    uid: msg.uid,
                    account_key: sh_act.accounts.cur_account_key.borrow().clone(),
                    headers_only: action == "show-headers",
                });
            }
            return;
        }
        // «Исходник тела» — the HTML part is already in memory.
        if action == "show-body-source" {
            let body_opt = sh_act.current_bodies.borrow().get(row).cloned();
            let Some(body) = body_opt else { return };
            if let Some(ui) = ui_weak_act.upgrade() {
                set_source_text(
                    &ui,
                    &sh_act,
                    format!("Исходник тела — {}", body.subject),
                    body.html.unwrap_or_default(),
                );
            }
            return;
        }
        // Per-message text/HTML view toggle. The render-mode is part of the
        // texture cache key, so this is a guaranteed re-render of that row.
        if action == "view-text" || action == "view-html" {
            let key = (msg.folder.clone(), msg.uid);
            {
                let mut ov = sh_act.body_view_text.borrow_mut();
                if action == "view-text" {
                    ov.insert(key);
                } else {
                    ov.remove(&key);
                }
            }
            let bodies = sh_act.current_bodies.borrow().clone();
            send_render_job(&sh_act, bodies, None);
            return;
        }
        // Everything else (delete / read / unread) goes through the engine.
        let Some(etx) = sh_act.engine_tx.borrow().clone() else {
            eprintln!("msg-action: no live engine");
            return;
        };
        match action.as_str() {
            "delete" => {
                let _ = etx.send(engine::EngineCmd::Delete {
                    messages: vec![msg],
                    account_key: sh_act.accounts.cur_account_key.borrow().clone(),
                });
            }
            "read" => {
                let _ = etx.send(engine::EngineCmd::SetFlags {
                    messages: vec![msg],
                    flags: "\\Seen".into(),
                    add: true,
                    account_key: sh_act.accounts.cur_account_key.borrow().clone(),
                });
            }
            "unread" => {
                let _ = etx.send(engine::EngineCmd::SetFlags {
                    messages: vec![msg],
                    flags: "\\Seen".into(),
                    add: false,
                    account_key: sh_act.accounts.cur_account_key.borrow().clone(),
                });
            }
            other => println!("msg-action {other} (not wired yet)"),
        }
    });

    // × on the reply ribbon — drop the staged reply target without sending.
    let ui_weak_rc = ui.as_weak();
    let sh_rc = shared.clone();
    ui.on_reply_ribbon_cancel(move || {
        if let Some(ui) = ui_weak_rc.upgrade() {
            exit_reply_mode(&sh_rc, &ui);
        }
    });
}

/// Chat column width and display scale → re-render at the new geometry.
/// Returns the width watcher, which must live as long as the window.
pub(crate) fn wire_viewport(ui: &MainWindow, shared: &Rc<Shared>) -> slint::Timer {
    // Resize = pure relayout: re-render the in-memory bodies at the new
    // width after the drag settles. No SQLite, no network — the contents
    // didn't change, only the pixels. Debounce coalesces the drag stream;
    // the render seq additionally kills any still-queued older job.
    let sh_rs = shared.clone();
    let resize_debounce = Rc::new(slint::Timer::default());
    ui.on_viewport_resized(move |w| {
        let neww = w as u32;
        // Sub-minimum widths are layout transients (chat pane hidden in
        // calendar mode, first frame) — rendering at them would be junk.
        if neww < 240 || neww == sh_rs.width.get() {
            return;
        }
        sh_rs.width.set(neww);
        let sh2 = sh_rs.clone();
        resize_debounce.start(
            slint::TimerMode::SingleShot,
            std::time::Duration::from_millis(150),
            move || {
                let bodies = sh2.current_bodies.borrow().clone();
                if !bodies.is_empty() {
                    send_render_job(&sh2, bodies, None);
                }
            },
        );
    });

    // Slint's `changed width` does NOT fire for the initial layout pass, so
    // after a restart the render width silently stayed at DEFAULT_WIDTH and
    // every bubble stretched to the real (wider) column. This watcher feeds
    // the actual chat-column width through the same resize path — covering
    // the first frame and any future missed events. It also tracks the
    // window scale factor: a DPI change re-renders so the bitmap comes out
    // at the display's real scale (crisp bubbles after a monitor move).
    let ui_weak_ww = ui.as_weak();
    let sh_ww = shared.clone();
    let width_watcher = slint::Timer::default();
    width_watcher.start(
        slint::TimerMode::Repeated,
        std::time::Duration::from_millis(500),
        move || {
            if let Some(ui) = ui_weak_ww.upgrade() {
                let sf = ui.window().scale_factor();
                if sf.is_finite() && sf > 0.0 && (sf - sh_ww.render_scale.get()).abs() > 0.01 {
                    sh_ww.render_scale.set(sf);
                    let bodies = sh_ww.current_bodies.borrow().clone();
                    if !bodies.is_empty() {
                        send_render_job(&sh_ww, bodies, None);
                    }
                }
                let w = ui.get_chat_width();
                if w > 0.0 {
                    ui.invoke_viewport_resized(w);
                }
            }
        },
    );
    width_watcher
}
