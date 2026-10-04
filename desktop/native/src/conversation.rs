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
    sh.cur_account_key.replace(akey.clone());
    // Смена контекста — сбрасываем закреплённый ручной выбор отправителя:
    // новая беседа по умолчанию отвечает со своей received_by identity, и
    // aim ниже её проставляет. Пользователь снова может переопределить.
    sh.picked_identity.borrow_mut().take();
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
    sh.pending_sends.borrow_mut().clear();

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
    let merged = sh.pending_compose.borrow().is_none()
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
