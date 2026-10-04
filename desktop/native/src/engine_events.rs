//! Results from the mail engine, applied on the UI thread: conversation
//! lists, message bodies, send outcomes, calendars, contacts, tasks — and
//! new mail with its toast, tray dot and sound (contract §5).

use super::*;

pub(crate) fn handle_engine_result(ui: &MainWindow, res: engine::EngineResult) {
    match res {
        engine::EngineResult::Conversations { mut list, partial } => {
            // First conversations after a (re)build arrived — drop the marker.
            ui.set_engine_reloading(false);
            SHARED.with(|s| {
                if let Some(sh) = s.borrow().as_ref() {
                    if partial && list.is_empty() {
                        println!("engine: conversations delta — no changes");
                        return;
                    }
                    // Remember the open conversation's identity: merging or a
                    // refetch can reorder the list and shift its index. Для
                    // склейки это id её первичного диалога — стабилен.
                    let current_id = sh.convs.borrow().get(sh.current.get()).map(|c| c.id.clone());
                    // Дельта-мерж идёт по СЫРОМУ списку (до склеек): дельта
                    // приносит исходные диалоги, а склеенный вид собирается
                    // заново из результата.
                    let raw: Vec<Conversation> = if partial {
                        let mut all = sh.raw_convs.borrow().clone();
                        for nc in list.drain(..) {
                            match all.iter_mut().find(|c| c.id == nc.id) {
                                Some(slot) => *slot = nc,
                                None => all.push(nc),
                            }
                        }
                        all.sort_by(|a, b| b.last_date_ts.cmp(&a.last_date_ts));
                        all
                    } else {
                        list
                    };
                    *sh.raw_convs.borrow_mut() = raw.clone();
                    let merged = apply_merges(&raw, &sh.merges.borrow(), &sh.key);
                    println!(
                        "engine: {} conversations, {} after merges (delta={})",
                        raw.len(),
                        merged.len(),
                        if partial { "merge" } else { "full" }
                    );
                    // Identities may have been (re)synced alongside the
                    // conversations — refresh the row-tint map and the
                    // composer from-picker from cache.
                    if let Some(cache) = &sh.cache {
                        *sh.identity_colors.borrow_mut() = identity_color_map(cache, &sh.key);
                        refresh_composer_identities(ui, sh);
                    }
                    let displays = displays_from(&merged, &sh.identity_colors.borrow());
                    let items = sidebar_items(&displays, &sh.avatars.borrow());
                    ui.set_conversations(ModelRc::new(VecModel::from(items)));
                    // Request avatars for unique counterpart emails not yet cached.
                    if let Some(etx) = sh.engine_tx.borrow().as_ref() {
                        let mut seen = std::collections::HashSet::new();
                        for d in &displays {
                            if d.email.is_empty() || sh.avatars.borrow().contains_key(&d.email) {
                                continue;
                            }
                            if seen.insert(d.email.clone()) {
                                let _ = etx.send(engine::EngineCmd::FetchAvatar {
                                    email: d.email.clone(),
                                });
                            }
                        }
                    }
                    *sh.convs.borrow_mut() = merged;
                    *sh.displays.borrow_mut() = displays;
                    // Диалог, созданный отправкой с другого адреса, мог
                    // приехать именно сейчас — тогда переходим в него, и
                    // восстанавливать выделение по старому id уже незачем.
                    let switched = try_pending_switch(ui, sh);
                    // Re-locate the selection by id (skip in transient-compose
                    // mode, where the sidebar has a synthetic first row).
                    if sh.compose.pending_compose.borrow().is_none() {
                        if let Some(id) = current_id.filter(|_| !switched) {
                            if let Some(idx) = sh.convs.borrow().iter().position(|c| c.id == id) {
                                if idx != sh.current.get() {
                                    sh.current.set(idx);
                                    ui.set_selected(idx as i32);
                                }
                            }
                        }
                        // The dialog on screen can never show an unread pill —
                        // the user is reading it. Push \Seen for whatever the
                        // server still reports unseen (covers letters whose
                        // uid never reached us, e.g. two in one sync batch).
                        if ui.window().is_visible() && ui.get_view_mode() == 0 {
                            let cur = sh.current.get();
                            let mut to_mark: Vec<MessageRef> = Vec::new();
                            let cleared_key = {
                                let mut convs = sh.convs.borrow_mut();
                                match convs.get_mut(cur) {
                                    Some(c) if c.unread_count > 0 => {
                                        for m in c.messages.iter_mut().filter(|m| !m.seen) {
                                            to_mark.push(m.clone());
                                            m.seen = true;
                                        }
                                        c.unread_count = 0;
                                        Some((conv_merge_key(&sh.key, c), c.merged))
                                    }
                                    _ => None,
                                }
                            };
                            let had_unread = cleared_key.is_some();
                            if let Some((k, mflag)) = cleared_key {
                                mark_raw_seen(sh, &k, mflag);
                            }
                            if had_unread {
                                let displays =
                                    displays_from(&sh.convs.borrow(), &sh.identity_colors.borrow());
                                *sh.displays.borrow_mut() = displays;
                                refresh_sidebar(sh, ui);
                            }
                            if !to_mark.is_empty() {
                                if let Some(etx) = sh.engine_tx.borrow().as_ref() {
                                    let _ = etx.send(engine::EngineCmd::SetFlags {
                                        messages: to_mark,
                                        flags: "\\Seen".into(),
                                        add: true,
                                        account_key: sh.accounts.cur_account_key.borrow().clone(),
                                    });
                                }
                            }
                        }
                    }
                    // Точка трея = факт наличия непрочитанного: дельта несёт
                    // свежие seen (прочитано здесь, из меню «прочитано» или
                    // на другом устройстве) и непрочитанное с других устройств
                    // / стартовой синхронизации.
                    tray_sync_dot(sh);
                    // Optimistic-send follow-ups.
                    // A transient-compose send landed: jump to its conversation
                    // as soon as the delta brings the row — per the agreed spec,
                    // instead of leaving the user on the stub pane.
                    let redirect = sh.compose.compose_sent_target.borrow().clone();
                    if let Some(addr) = redirect {
                        let idx = sh.convs.borrow().iter().position(|c| {
                            c.counterparts.iter().any(|cp| cp.addr.eq_ignore_ascii_case(&addr))
                        });
                        if let Some(idx) = idx {
                            *sh.compose.compose_sent_target.borrow_mut() = None;
                            ui.set_selected(idx as i32);
                            apply_active_header(ui, sh, idx);
                            open_conversation(ui, sh, idx);
                            ui.set_sidebar_row_y(idx as f32 * 64.0);
                            ui.set_sidebar_scroll_seq(ui.get_sidebar_scroll_seq() + 1);
                        }
                    } else if !sh.compose.pending_sends.borrow().is_empty() {
                        // Stubs wait in the open dialog: refetch its bodies so
                        // the real sent message (now in the refreshed refs)
                        // replaces the stub.
                        let cur = sh.current.get();
                        let refetch = sh.convs.borrow().get(cur).and_then(|c| {
                            let has_here = sh
                                .compose
                                .pending_sends
                                .borrow()
                                .iter()
                                .any(|p| !p.conv_id.is_empty() && p.conv_id == c.id);
                            has_here.then(|| c.messages.clone())
                        });
                        if let Some(messages) = refetch {
                            if let Some(etx) = sh.engine_tx.borrow().as_ref() {
                                let _ = etx.send(engine::EngineCmd::FetchMessages {
                                    messages,
                                    generation: sh.open_gen.get(),
                                    account_key: sh.accounts.cur_account_key.borrow().clone(),
                                });
                            }
                        }
                    }
                }
            });
        }
        engine::EngineResult::Avatar { email, rgba, w, h } => {
            let buf = SharedPixelBuffer::<Rgba8Pixel>::clone_from_slice(&rgba, w, h);
            let img = Image::from_rgba8(buf);
            SHARED.with(|s| {
                if let Some(sh) = s.borrow().as_ref() {
                    sh.avatars.borrow_mut().insert(email, img);
                    let items = sidebar_items(&sh.displays.borrow(), &sh.avatars.borrow());
                    ui.set_conversations(ModelRc::new(VecModel::from(items)));
                }
            });
        }
        engine::EngineResult::Messages { bodies, generation } => {
            SHARED.with(|s| {
                if let Some(sh) = s.borrow().as_ref() {
                    // Stale-fetch guard: the user may have switched
                    // conversations while this answer was in flight — an
                    // old answer must not overwrite the new screen (same
                    // pattern as the search dropdown's query echo).
                    if generation != sh.open_gen.get() {
                        println!(
                            "engine: dropping stale Messages (gen {generation} != {})",
                            sh.open_gen.get()
                        );
                        return;
                    }
                    // Optimistic-send reconcile: a fetched outgoing body with
                    // the same text = the stub's real message landed — drop
                    // the stub. Still-unconfirmed stubs are re-appended so an
                    // in-flight send doesn't vanish from screen; ones without
                    // an echo after 2 min die instead of ghosting forever.
                    let mut bodies = bodies;
                    {
                        let cur_id = sh
                            .convs
                            .borrow()
                            .get(sh.current.get())
                            .map(|c| c.id.clone())
                            .unwrap_or_default();
                        let mut stubs = sh.compose.pending_sends.borrow_mut();
                        if !stubs.is_empty() {
                            stubs.retain(|p| {
                                p.created.elapsed().as_secs() < 120
                                    && !bodies.iter().any(|b| {
                                        b.is_outgoing
                                            && b.folder != PENDING_FOLDER
                                            && norm_send_text(b.text.as_deref().unwrap_or(""))
                                                == norm_send_text(
                                                    p.body.text.as_deref().unwrap_or(""),
                                                )
                                    })
                            });
                            for p in
                                stubs.iter().filter(|p| !cur_id.is_empty() && p.conv_id == cur_id)
                            {
                                bodies.push(p.body.clone());
                            }
                        }
                    }
                    // Stubs never leave the client: they carry no server refs.
                    *sh.current_msgs.borrow_mut() = bodies
                        .iter()
                        .filter(|b| b.folder != PENDING_FOLDER)
                        .map(|b| MessageRef {
                            folder: b.folder.clone(),
                            uid: b.uid,
                            message_id: b.message_id.clone(),
                            seen: true,
                        })
                        .collect();
                    *sh.current_bodies.borrow_mut() = bodies.clone();
                    // Freshly fetched bodies can change the Re:-subject hint.
                    refresh_composer_hints(ui, sh);
                    // Last render after open is THIS one (it aborts the
                    // optimistic cached render) — consume the pending scroll.
                    let scroll = take_scroll_target(sh, &bodies, true);
                    send_render_job(sh, bodies, scroll);
                }
            });
        }
        engine::EngineResult::Done(what) => {
            println!("engine: {what} done — refreshing");
            if what == "rsvp" {
                // Calendar mutation → refresh the visible week's events.
                SHARED.with(|s| {
                    if let Some(sh) = s.borrow().as_ref() {
                        // Подтверждение сохранения формы — только теперь
                        // карточку можно закрывать.
                        if sh.cal.pending_event_save.replace(false) {
                            ui.set_edit_busy(false);
                            ui.set_edit_error("".into());
                            ui.set_edit_visible(false);
                        }
                        refetch_calendar_events(ui, sh);
                    }
                });
                return;
            }
            if what == "contact" {
                // Address-book mutation → refresh the current view/search.
                let q = ui.get_contacts_query().to_string();
                SHARED.with(|s| {
                    if let Some(sh) = s.borrow().as_ref() {
                        fetch_contacts(sh, &q);
                    }
                });
                return;
            }
            SHARED.with(|s| {
                if let Some(sh) = s.borrow().as_ref() {
                    // A flag change (seen/unseen) doesn't alter the conversation
                    // list — the optimistic local update already cleared the
                    // pill. Refetching here would re-detect a still-"unread"
                    // server state (e.g. an unfetchable/stale message whose
                    // \Seen never lands) and re-mark it forever. Only structural
                    // changes (delete / spam purge) need a refetch.
                    if what == "flags" {
                        return;
                    }
                    if let Some(etx) = sh.engine_tx.borrow().as_ref() {
                        let _ = etx.send(engine::EngineCmd::FetchConversations {
                            limit: CONV_FETCH_LIMIT,
                        });
                        // Reopen current conversation to refresh its bodies.
                        let cur = sh.current.get();
                        if let Some(c) = sh.convs.borrow().get(cur) {
                            let _ = etx.send(engine::EngineCmd::FetchMessages {
                                messages: c.messages.clone(),
                                generation: sh.open_gen.get(),
                                account_key: sh.accounts.cur_account_key.borrow().clone(),
                            });
                        }
                    }
                }
            });
        }
        engine::EngineResult::AccountState { account_key, state } => {
            println!("account {account_key}: {state}");
            SHARED.with(|s| {
                if let Some(sh) = s.borrow().as_ref() {
                    // Catch-up refetch on every (re)connect: the client is
                    // push-driven, and any event published while the socket
                    // was dead (silent TCP death, server restart, watchdog
                    // gap) is gone for good — the hub doesn't replay. One
                    // conversations fetch per reconnect closes that window.
                    if state == "connected" {
                        if let Some(etx) = sh.engine_tx.borrow().as_ref() {
                            let _ = etx.send(engine::EngineCmd::FetchConversations {
                                limit: CONV_FETCH_LIMIT,
                            });
                        }
                        // Тем же движением сеем напоминания: до коннекта
                        // спрашивать было некого, а ждать серверного push
                        // (единственного, кто раньше запускал посев) значит
                        // молчать неопределённое время после старта.
                        fetch_reminder_window(sh);
                    }
                    note_account_state(&mut sh.accounts.reauth.borrow_mut(), &account_key, &state);
                    sh.accounts.account_states.borrow_mut().insert(account_key, state);
                    apply_conn_status(ui, sh);
                }
            });
        }
        engine::EngineResult::Event(ev) => {
            use ddmail_core::event::EngineEvent;
            match ev {
                EngineEvent::NewMail { folder, count: _, new_count, from, subject, message_id } => {
                    println!("engine event: new mail in {folder} (+{new_count}) from {from:?}");
                    SHARED.with(|s| {
                        let Some(sh) = s.borrow().as_ref().cloned() else { return };
                        handle_new_mail(ui, &sh, folder, new_count, from, subject, message_id);
                    });
                }
                EngineEvent::ConnectionState { state, message } => {
                    println!("engine connection: {state} {}", message.unwrap_or_default());
                }
                EngineEvent::Expunged { folder } => {
                    println!("engine event: expunge in {folder} — forcing full resync");
                    SHARED.with(|s| {
                        if let Some(sh) = s.borrow().as_ref() {
                            // Force a FULL conversation resync so deleted threads
                            // drop off: the delta only reports CHANGED convs, not
                            // removals. Clearing each account's full-sync stamp
                            // makes the next FetchConversations run with since=0
                            // (full), which replaces the cache and prunes the
                            // gone conversations.
                            if let Some(cache) = &sh.cache {
                                for k in sh.accounts.account_keys.borrow().iter() {
                                    cache.set_meta(&format!("conv_full_ts:{k}"), "0").ok();
                                }
                            }
                            if let Some(etx) = sh.engine_tx.borrow().as_ref() {
                                let _ = etx.send(engine::EngineCmd::FetchConversations {
                                    limit: CONV_FETCH_LIMIT,
                                });
                            }
                        }
                    });
                }
                EngineEvent::MessageSent => {
                    // Our own message reached Sent. This is the moment the
                    // conversation it belongs to starts existing server-side —
                    // and the whole reason the post-send jump used to fail: the
                    // client refetched on a 2.5s timer while sending is
                    // asynchronous, so it looked for a conversation that was
                    // still in the outbox.
                    //
                    // Full, not delta: for a reply sent from a different address
                    // the conversation is brand new, and a full sync is what
                    // replaces the cache rather than merging into it.
                    println!("engine event: message landed in Sent — full refetch");
                    SHARED.with(|s| {
                        if let Some(sh) = s.borrow().as_ref() {
                            if let Some(cache) = &sh.cache {
                                for k in sh.accounts.account_keys.borrow().iter() {
                                    cache.set_meta(&format!("conv_full_ts:{k}"), "0").ok();
                                }
                            }
                            if let Some(etx) = sh.engine_tx.borrow().as_ref() {
                                let _ = etx.send(engine::EngineCmd::FetchConversations {
                                    limit: CONV_FETCH_LIMIT,
                                });
                            }
                        }
                    });
                }
                EngineEvent::FlagsChanged { folder } => {
                    // Read/starred in another client (or pulled from the
                    // source account by the server sync). No toast — just a
                    // delta conversations fetch: flag changes bump
                    // updated_at server-side, so the changed threads come
                    // back with fresh seen states and the sidebar unread
                    // badges correct themselves.
                    println!("engine event: flags changed in {folder} — delta refetch");
                    SHARED.with(|s| {
                        if let Some(sh) = s.borrow().as_ref() {
                            if let Some(etx) = sh.engine_tx.borrow().as_ref() {
                                let _ = etx.send(engine::EngineCmd::FetchConversations {
                                    limit: CONV_FETCH_LIMIT,
                                });
                            }
                        }
                    });
                }
                EngineEvent::CalendarUpdated { calendar_id } => {
                    println!("engine event: calendar {calendar_id} updated");
                    // A server-side re-sync may have re-created events under
                    // new ids and shifted times — refresh the displayed week
                    // (which also re-seeds reminders) at most once per 2 min:
                    // the push arrives in bursts, one per calendar per cycle.
                    SHARED.with(|s| {
                        if let Some(sh) = s.borrow().as_ref() {
                            let stale = sh
                                .cal
                                .last_cal_refetch
                                .get()
                                .map(|t| t.elapsed().as_secs() >= 120)
                                .unwrap_or(true);
                            if stale {
                                sh.cal.last_cal_refetch.set(Some(std::time::Instant::now()));
                                refetch_calendar_events(ui, sh);
                            }
                        }
                    });
                }
                EngineEvent::IdentitiesChanged => {
                    println!(
                        "engine event: identities changed — refreshing accounts, calendars, contacts"
                    );
                    // A profile import can add a mailbox, a calendar source and
                    // an address book in one go. Refresh all three: the
                    // identity list (sidebar tints and the from-picker), the
                    // calendar list, and the contacts. Not debounced like
                    // CalendarUpdated — this arrives once per import, not in
                    // per-calendar bursts.
                    SHARED.with(|s| {
                        if let Some(sh) = s.borrow().as_ref() {
                            if let Some(etx) = sh.engine_tx.borrow().as_ref() {
                                let _ = etx.send(engine::EngineCmd::RefreshIdentities);
                                let _ = etx.send(engine::EngineCmd::FetchCalendars);
                            }
                            fetch_contacts(sh, "");
                            // The new calendar has no events on screen yet.
                            // Bypass the CalendarUpdated debounce: this is a
                            // one-off, and waiting out its 2-minute window
                            // would leave the imported calendar blank.
                            sh.cal.last_cal_refetch.set(Some(std::time::Instant::now()));
                            refetch_calendar_events(ui, sh);
                        }
                    });
                }
                EngineEvent::TokenRefreshed { account_id, token } => {
                    // Persist the rotated JWT — otherwise the next launch
                    // starts from the stale token and, past the 30-day
                    // refresh window, would silently fall back to cache-only.
                    engine::AccountConfig::persist_native_token(&account_id, &token);
                    println!("engine event: token refreshed for {account_id} (persisted)");
                    // Ротация — доказательство, что сессия жива: снимаем
                    // плашку, не дожидаясь коннекта watcher'а (тот в этот
                    // момент может стоять на бэкоффе). Событие адресовано
                    // почтой (`account_id` провайдера — это email), а список
                    // ждущих пароля ведётся по account_key = хост+логин.
                    let key = engine::AccountConfig::load_all()
                        .into_iter()
                        .find(|a| a.email == account_id)
                        .map(|a| a.account_key());
                    if let Some(key) = key {
                        SHARED.with(|s| {
                            if let Some(sh) = s.borrow().as_ref() {
                                sh.accounts.reauth.borrow_mut().retain(|k| *k != key);
                                apply_conn_status(ui, sh);
                            }
                        });
                    }
                }
            }
        }
        engine::EngineResult::Sent(id) => {
            println!("engine: message sent ({id}) — refetching conversations");
            // Inline «✓ Отправлено» above the composer — the agreed subtle
            // confirmation (no toasts); dissolves after ~2s. Текст ставим
            // явно: плашку делит с собой «✓ Сохранено» вложений.
            ui.set_send_confirm_text("✓ Отправлено".into());
            ui.set_send_confirm_visible(true);
            let uiw = ui.as_weak();
            slint::Timer::single_shot(std::time::Duration::from_millis(2000), move || {
                if let Some(u) = uiw.upgrade() {
                    u.set_send_confirm_visible(false);
                }
            });
            SHARED.with(|s| {
                if let Some(sh) = s.borrow().as_ref() {
                    // Leaving transient-compose mode now that the message
                    // landed; remember the recipient so the Conversations
                    // delta can redirect to the (possibly brand-new)
                    // conversation as soon as its row exists.
                    let target = sh.compose.pending_compose.borrow_mut().take();
                    if let Some(t) = target {
                        *sh.compose.compose_sent_target.borrow_mut() = Some(t);
                        refresh_sidebar(sh, ui);
                    }
                    // Письмо принято сервером, но в «Отправленных» оно
                    // оказывается не в тот же миг: копию туда кладёт сервер
                    // (для внешнего ящика — удалённый провайдер) уже после
                    // того, как SMTP ответил «принято».
                    //
                    // Отсюда два следствия для синка. Он должен быть полным:
                    // дельта отдаёт только изменившиеся беседы, а беседы
                    // отправленного письма может ещё не существовать вовсе —
                    // особенно теперь, когда отправка с другого адреса заводит
                    // свой диалог (§4). И он должен повториться: первый запрос
                    // почти наверняка обгонит раскладку по папкам, и без
                    // второго отправленное всплыло бы только со следующим
                    // периодическим синком.
                    let force_full = |sh: &Shared| {
                        if let Some(cache) = &sh.cache {
                            let key = sh.accounts.cur_account_key.borrow().clone();
                            let key = if key.is_empty() { sh.key.clone() } else { key };
                            cache.set_meta(&format!("conv_full_ts:{key}"), "0").ok();
                        }
                    };
                    force_full(sh);
                    if let Some(etx) = sh.engine_tx.borrow().as_ref() {
                        let _ = etx.send(engine::EngineCmd::FetchConversations {
                            limit: CONV_FETCH_LIMIT,
                        });
                    }
                    schedule_post_send_refetch(0);
                }
            });
        }
        engine::EngineResult::SearchDropdown { query, contacts, messages } => {
            // Drop stale answers: the user has typed past this query, no
            // point updating the dropdown with results for a string they
            // no longer see.
            SHARED.with(|s| {
                if let Some(sh) = s.borrow().as_ref() {
                    if *sh.search.search_query_inflight.borrow() != query {
                        return;
                    }
                    // Contacts first: address book + sidebar counterparts
                    // (client-side), then fold in the engine's cache-contact
                    // hits (accounts whose book isn't mirrored locally),
                    // deduped by address. Messages stay their own section.
                    let q_lc = query.to_lowercase();
                    let skip = conv_hit_addrs(&sh.search.search_convs.borrow());
                    let mut merged =
                        local_search_contacts(&sh.contacts.address_book.borrow(), &skip, &q_lc);
                    // Собеседники найденных диалогов не повторяются и тут.
                    let mut seen: HashSet<String> =
                        merged.iter().map(|c| c.email.to_lowercase()).chain(skip).collect();
                    for c in contacts {
                        let key = c.email.to_lowercase();
                        if !key.is_empty() && seen.insert(key) {
                            merged.push(c);
                        }
                    }
                    merged.truncate(12);
                    let c_items = contact_items(&merged);
                    let m_items = message_hits(&messages);
                    *sh.search.search_contacts.borrow_mut() = merged;
                    *sh.search.search_messages.borrow_mut() = messages;
                    ui.set_search_contacts(ModelRc::new(VecModel::from(c_items)));
                    ui.set_search_messages(ModelRc::new(VecModel::from(m_items)));
                    ui.set_search_loading(false);
                }
            });
        }
        engine::EngineResult::AttachmentSaved(path) => {
            println!("attachment saved: {path} — opening");
            open_saved_file(&path);
        }
        engine::EngineResult::AttachmentFailed(e) => {
            eprintln!("engine: attachment failed: {e}");
            toast_window::show(
                2, // amber — как у SendFailed
                0,
                "Не удалось сохранить вложение",
                &format!("Причина: {}", e.chars().take(160).collect::<String>()),
                false,
                600,
                || {},
                || {},
                || {},
            );
        }
        engine::EngineResult::SpamPurged { rule_type, rule_value, deleted } => {
            println!("engine: spam purged {rule_type}={rule_value}, deleted={deleted}");
            // Structural change — reconcile the list with the server (same as
            // a delete). The engine already reset the full-sync stamp.
            SHARED.with(|s| {
                if let Some(sh) = s.borrow().as_ref() {
                    if let Some(etx) = sh.engine_tx.borrow().as_ref() {
                        let _ = etx.send(engine::EngineCmd::FetchConversations {
                            limit: CONV_FETCH_LIMIT,
                        });
                    }
                }
            });
            let what = match rule_type.as_str() {
                "domain" => format!("домен {rule_value}"),
                "address" => format!("адрес {rule_value}"),
                _ => "отправитель".to_string(),
            };
            ui.set_send_confirm_text(format!("✓ В спам: {what}").into());
            ui.set_send_confirm_visible(true);
            let uiw = ui.as_weak();
            slint::Timer::single_shot(std::time::Duration::from_millis(2200), move || {
                if let Some(u) = uiw.upgrade() {
                    u.set_send_confirm_visible(false);
                }
            });
        }
        engine::EngineResult::SpamFailed(e) => {
            eprintln!("engine: spam failed: {e}");
            toast_window::show(
                2, // amber
                0,
                "Не удалось отметить спам",
                &format!("Причина: {}", e.chars().take(160).collect::<String>()),
                false,
                600,
                || {},
                || {},
                || {},
            );
        }
        engine::EngineResult::AttachmentSavedTo(path) => {
            // Явное «Сохранить как…»: файл не открываем, подтверждаем той же
            // ненавязчивой плашкой, что и отправку (~2 с, без тостов).
            println!("attachment saved to: {path}");
            flash_confirm(ui, "✓ Сохранено");
        }
        engine::EngineResult::Source { uid, raw } => {
            SHARED.with(|s| {
                if let Some(sh) = s.borrow().as_ref() {
                    let what = sh.pending_source_view.get();
                    sh.pending_source_view.set(0);
                    if what == 1 {
                        show_headers(ui, uid, &raw);
                    } else {
                        // Source: the raw RFC-822 bytes, verbatim, no processing.
                        set_source_text(ui, sh, format!("Исходник сообщения (id {uid})"), raw);
                    }
                }
            });
        }
        engine::EngineResult::Calendars { list: cals, complete } => {
            println!(
                "engine: {} calendars{}",
                cals.len(),
                if complete { "" } else { " (partial)" }
            );
            SHARED.with(|s| {
                if let Some(sh) = s.borrow().as_ref() {
                    {
                        let mut vis = sh.cal.calendar_visible.borrow_mut();
                        for c in &cals {
                            vis.entry(c.id).or_insert(true);
                        }
                    }
                    *sh.cal.calendars.borrow_mut() = cals;
                    apply_calendar_view(ui, sh);

                    // Открытая форма создания, у которой не было куда сохранять,
                    // дозаполняется здесь же — иначе пришедший список пришлось
                    // бы «поймать» закрытием и повторным открытием карточки.
                    if ui.get_edit_visible()
                        && ui.get_edit_is_create()
                        && sh.cal.edit_cal_ids.borrow().is_empty()
                    {
                        let mut ids = fill_writable_calendars(ui, sh, true);
                        if ids.is_empty() {
                            ids = fill_writable_calendars(ui, sh, false);
                        }
                        let filled = !ids.is_empty();
                        *sh.cal.edit_cal_ids.borrow_mut() = ids;
                        ui.set_edit_calendar_idx(0);
                        ui.set_edit_error(
                            if filled {
                                ""
                            } else if complete {
                                "Нет календарей, доступных на запись."
                            } else {
                                "Не удалось получить список календарей — проверьте соединение."
                            }
                            .into(),
                        );
                    }
                }
            });
        }
        engine::EngineResult::Contacts { query, list } => {
            // Drop stale answers: the search box has moved on since this
            // request went out.
            if ui.get_contacts_query().as_str() != query {
                return;
            }
            let rows = address_book_rows(&list);
            ui.set_address_book(ModelRc::new(VecModel::from(rows)));
            SHARED.with(|s| {
                if let Some(sh) = s.borrow().as_ref() {
                    *sh.contacts.address_book.borrow_mut() = list;
                }
            });
        }
        engine::EngineResult::Tasks(list) => {
            println!("engine: {} tasks", list.len());
            ui.set_tasks_loading(false);
            ui.set_tasks(ModelRc::new(VecModel::from(task_rows(&list))));
        }
        engine::EngineResult::CalendarEvents {
            events,
            from_ms,
            to_ms,
            complete,
            for_reminders,
        } => {
            println!(
                "engine: {} calendar events{}",
                events.len(),
                if for_reminders { " (посев)" } else { "" }
            );
            SHARED.with(|s| {
                if let Some(sh) = s.borrow().as_ref() {
                    let now_ms = chrono::Utc::now().timestamp_millis();
                    if let Some(c) = sh.cache.as_ref() {
                        // Hidden calendars don't get reminders (spec #9).
                        let vis = sh.cal.calendar_visible.borrow();
                        let hidden = |cal_id: i64| !*vis.get(&cal_id).unwrap_or(&true);
                        reminders::seed(c, &events, &hidden, now_ms);
                        // Deleted-upstream events must stop toasting: cull
                        // reminders whose event vanished from this window and
                        // close any toast already on screen for them. Only a
                        // COMPLETE fetch is trusted — a failed account's
                        // missing events are not deletions.
                        if complete {
                            let keep: std::collections::HashSet<i64> =
                                events.iter().map(|e| e.id).collect();
                            match c.prune_orphan_reminders(from_ms, to_ms, &keep) {
                                Ok(orphans) => {
                                    for id in orphans {
                                        println!("[cal] reminder cull: event {id} vanished — closing its toasts");
                                        toast_window::close_for_event(id);
                                    }
                                }
                                Err(e) => eprintln!("reminder cull: {e}"),
                            }
                            // Событие осталось (тот же id), но ПЕРЕЕХАЛО:
                            // строки напоминаний со старым occurrence_start
                            // иначе остаются взведёнными и стреляют по
                            // старому времени. Валидны только вхождения,
                            // которые текущий фетч реально производит.
                            let valid = reminders::valid_starts(&events, from_ms, to_ms);
                            match c.prune_moved_reminders(from_ms, to_ms, &valid) {
                                Ok(moved) => {
                                    for id in moved {
                                        println!("[cal] reminder cull: event {id} moved — dropping stale occurrences");
                                        toast_window::close_for_event(id);
                                    }
                                }
                                Err(e) => eprintln!("reminder move-cull: {e}"),
                            }
                        }
                    }
                    // Фетч ради посева: его окно — не отображаемая неделя, и
                    // подставлять эти события в сетку нельзя (перетёрли бы
                    // чужим окном). Напоминания уже посеяны выше — выходим.
                    if for_reminders {
                        return;
                    }
                    // Remember each event's owning account for write routing.
                    {
                        let mut m = sh.cal.event_accounts.borrow_mut();
                        m.clear();
                        for e in &events {
                            if !e.account_key.is_empty() {
                                m.insert(e.id, e.account_key.clone());
                            }
                        }
                    }
                    *sh.cal.calendar_events.borrow_mut() = events;
                    apply_calendar_view(ui, sh);
                    // A reminder toast asked to open this event — its week
                    // is loaded now, so pop the detail card.
                    let pend = sh.cal.pending_open_event.get();
                    if pend != 0 {
                        sh.cal.pending_open_event.set(0);
                        let occ = sh.cal.pending_open_occ.get();
                        let summary = sh.cal.pending_open_summary.borrow().clone();
                        let open_id = {
                            let events = sh.cal.calendar_events.borrow();
                            if events.iter().any(|e| e.id == pend) {
                                Some(pend)
                            } else {
                                // Stale reminder id: the server re-creates
                                // events under new ids on calendar re-sync,
                                // so the id this reminder was seeded with may
                                // be dead. Recover the meeting by occurrence:
                                // same summary, and either an exact start
                                // match or a recurring master that can own
                                // this occurrence.
                                events
                                    .iter()
                                    .filter(|e| e.summary == summary && !summary.is_empty())
                                    .filter(|e| e.dtstart == occ || !e.rrule.is_empty())
                                    .min_by_key(|e| if e.dtstart == occ { 0 } else { 1 })
                                    .map(|e| e.id)
                            }
                        };
                        match open_id {
                            Some(id) => {
                                println!(
                                    "[cal] toast-open: event {pend} → card (resolved id {id}{})",
                                    if id == pend { "" } else { ", stale-id fallback" }
                                );
                                ui.invoke_event_clicked(id as i32);
                            }
                            None => println!(
                                "[cal] toast-open: event {pend} \"{summary}\" occ={occ} not in week payload — no card"
                            ),
                        }
                    }
                }
            });
            // Пилюля «Загрузка…» принадлежит фетчу сетки: посевной её не
            // зажигал и гасить не должен — иначе снял бы её с идущего рядом
            // фетча недели.
            if !for_reminders {
                ui.set_calendar_loading(false);
            }
        }
        engine::EngineResult::SendFailed(e) => {
            eprintln!("engine: send failed: {e}");
            // Roll back the optimistic bubble(s) — a stub that looks sent
            // would be a lie. The text returns to the composer so a retry is
            // one click away. (The error path carries no send id, so every
            // in-flight stub drops; overlapping sends are rare enough that
            // this stays simple.)
            SHARED.with(|s| {
                if let Some(sh) = s.borrow().as_ref() {
                    let stubs: Vec<PendingSend> =
                        sh.compose.pending_sends.borrow_mut().drain(..).collect();
                    if !stubs.is_empty() {
                        let bodies = {
                            let mut cur = sh.current_bodies.borrow_mut();
                            cur.retain(|b| b.folder != PENDING_FOLDER);
                            cur.clone()
                        };
                        send_render_job(sh, bodies, None);
                        // Don't clobber text the user already started typing.
                        // Возвращается plain-версия: разметку неудавшегося
                        // письма мы не храним, а терять текст нельзя.
                        if ui.get_composer_text().is_empty() {
                            if let Some(p) = stubs.last() {
                                rich_set_text(&ui, sh, &p.body.text.clone().unwrap_or_default());
                            }
                        }
                    }
                }
            });
            // Make the failure visible — the composer optimistically looked
            // like it sent, so without this the user only finds out by asking.
            // Translate the two common causes into plain Russian.
            let body = if e.contains("413") || e.to_lowercase().contains("too large") {
                "Вложения слишком большие. Уменьшите размер или пришлите ссылкой.".to_string()
            } else {
                format!("Причина: {}", e.chars().take(160).collect::<String>())
            };
            toast_window::show(
                2, // amber
                0, // not tied to a calendar event
                "Не удалось отправить письмо",
                &body,
                false,
                600,
                || {},
                || {},
                || {},
            );
        }
        engine::EngineResult::Error(e) => {
            eprintln!("engine error: {e}");
            // Отказ по сохранению события принадлежит открытой карточке. Без
            // этого «Сохранить» на событии, которое сервер отверг (403 на
            // календарь только для чтения, разорванное соединение), выглядел
            // ровно так же, как успех: карточка просто закрывалась.
            SHARED.with(|s| {
                if let Some(sh) = s.borrow().as_ref() {
                    if sh.cal.pending_event_save.replace(false) {
                        ui.set_edit_busy(false);
                        ui.set_edit_error(format!("Сервер не принял событие: {e}").into());
                    }
                }
            });
        }
    }
}

/// Display name from a "Name <addr>" header (falls back to the address).
pub(crate) fn display_from(raw: &str) -> String {
    let r = raw.trim();
    if r.is_empty() {
        return "Новое письмо".into();
    }
    if let Some(i) = r.rfind('<') {
        let name = r[..i].trim().trim_matches('"').trim();
        if !name.is_empty() {
            return name.to_string();
        }
    }
    header_addr(r)
}

/// New-mail behaviour per the notification spec:
///   * reading that very dialog → silent append (autoscroll only when the
///     user is already at the bottom; otherwise just a row flash);
///   * anywhere else → tray dot + sound (per settings), badge bump + row
///     flash when the window shows the mail view;
///   * window hidden → clickable toast (sender + subject) that raises the
///     window, opens the dialog and scrolls to the message.
pub(crate) fn handle_new_mail(
    ui: &MainWindow,
    sh: &Rc<Shared>,
    folder: String,
    new_count: u32,
    from: String,
    subject: String,
    message_id: i64,
) {
    let from_addr = header_addr(&from);
    // Spec #1: a toast only for a real letter to read. Spam and iTIP/ics are
    // already dropped server-side; «своё» (from one of our own identities) is
    // filtered here — our outgoing mail syncing back is not a notification.
    if !from_addr.is_empty() && sh.identity_colors.borrow().contains_key(&from_addr.to_lowercase())
    {
        return;
    }
    let conv_idx = if from_addr.is_empty() {
        None
    } else {
        sh.convs
            .borrow()
            .iter()
            .position(|c| c.counterparts.iter().any(|cp| cp.addr.eq_ignore_ascii_case(&from_addr)))
    };

    let visible = ui.window().is_visible();
    let in_mail_view = ui.get_view_mode() == 0;
    // «Я в этом диалоге?» — compare the sender against the OPEN conversation,
    // not the first list hit: pair-grouping can hold the same counterpart in
    // several rows, and list re-sorts make index equality a lottery.
    let belongs_to_open = !from_addr.is_empty()
        && sh.convs.borrow().get(sh.current.get()).is_some_and(|c| {
            c.counterparts.iter().any(|cp| cp.addr.eq_ignore_ascii_case(&from_addr))
        });
    let is_current = visible && in_mail_view && belongs_to_open;
    // Письмо в открытый диалог, но панели почты сейчас нет (календарь,
    // книга, задачи): дорисовать его некому и незачем — перерисуем при
    // возврате, тогда же анкор и встанет на него.
    if belongs_to_open && !in_mail_view {
        sh.missed_mail.set(true);
    }

    if is_current {
        let at_bottom = {
            let vp_y = ui.get_chat_vp_y(); // negative when scrolled down
            let vp_h = ui.get_chat_vp_h();
            let view_h = ui.get_chat_view_h();
            vp_h <= view_h || (-vp_y) + view_h >= vp_h - 60.0
        };
        if at_bottom {
            // Follow the conversation: append + scroll to the end.
            sh.open_unread.borrow_mut().clear();
            sh.scroll_pending.set(true);
        } else {
            // Reading history above: don't yank the viewport — just flash.
            flash_sidebar_row(ui, model_index(sh, sh.current.get()));
        }
        if let Some(etx) = sh.engine_tx.borrow().as_ref() {
            // In the dialog = read: push \Seen right away, so the delta
            // refetch can't resurrect an unread pill for this row.
            // The stable RFC Message-ID for this row, if the conversation refs
            // carry it — prefer it over the volatile uid (db id) so the flag
            // lands even if the server reinserted the row.
            let row_mid = sh
                .convs
                .borrow()
                .get(sh.current.get())
                .and_then(|c| {
                    c.messages
                        .iter()
                        .find(|m| m.uid == message_id as u32)
                        .map(|m| m.message_id.clone())
                })
                .unwrap_or_default();
            if message_id > 0 {
                let _ = etx.send(engine::EngineCmd::SetFlags {
                    messages: vec![MessageRef {
                        folder: folder.clone(),
                        uid: message_id as u32,
                        message_id: row_mid.clone(),
                        seen: false,
                    }],
                    flags: "\\Seen".into(),
                    add: true,
                    account_key: sh.accounts.cur_account_key.borrow().clone(),
                });
            }
            let mut refs = sh
                .convs
                .borrow()
                .get(sh.current.get())
                .map(|c| c.messages.clone())
                .unwrap_or_default();
            if message_id > 0 && !refs.iter().any(|m| m.uid == message_id as u32) {
                refs.push(MessageRef {
                    folder,
                    uid: message_id as u32,
                    message_id: row_mid,
                    seen: true,
                });
            }
            let _ = etx.send(engine::EngineCmd::FetchMessages {
                messages: refs,
                generation: sh.open_gen.get(),
                account_key: sh.accounts.cur_account_key.borrow().clone(),
            });
            let _ = etx.send(engine::EngineCmd::FetchConversations { limit: CONV_FETCH_LIMIT });
        }
        return;
    }

    // Not looking at that dialog: tray dot + sound.
    tray_set_dot(true);
    if ui.get_notify_sound_on() {
        toast::beep();
    }

    // Optimistic badge bump (data updates even when the calendar view
    // hides the sidebar); flash only when the row is actually on screen.
    if let Some(idx) = conv_idx {
        if let Some(c) = sh.convs.borrow_mut().get_mut(idx) {
            c.unread_count += new_count.max(1);
        }
        let displays = displays_from(&sh.convs.borrow(), &sh.identity_colors.borrow());
        *sh.displays.borrow_mut() = displays;
        refresh_sidebar(sh, ui);
        if visible && in_mail_view {
            flash_sidebar_row(ui, model_index(sh, idx));
        }
    }

    // Mail toast fires when the user can't see the new mail in-window: the
    // window is hidden (tray), OR the calendar view is up (mail sidebar not
    // visible). When the mail view is open and visible, the sidebar flash +
    // unread bump above is the notification — no toast.
    if !visible || !in_mail_view {
        // Spec #1: a batch collapses to a single «N новых» toast; a lone
        // message shows sender + subject.
        let (title, body) = if new_count > 1 {
            (format!("{new_count} новых"), String::new())
        } else {
            let b =
                if subject.is_empty() { "(без темы)".to_string() } else { subject.clone() };
            (display_from(&from), b)
        };
        let click_folder = folder.clone();
        let click_uid = message_id.max(0) as u32;
        let click_addr = from_addr.clone();
        toast::mail_toast(&title, &body, move || {
            let Some(weak) = UI_WEAK.get() else { return };
            let folder = click_folder.clone();
            let addr = click_addr.clone();
            let _ = weak.clone().upgrade_in_event_loop(move |ui| {
                raise_window(&ui);
                // Точку не гасим вручную: открытие диалога прочтёт его, и
                // tray_sync_dot погасит точку, если непрочитанного не осталось.
                SHARED.with(|s| {
                    if let Some(sh) = s.borrow().as_ref() {
                        open_message_from_toast(&ui, sh, &folder, click_uid, &addr);
                    }
                });
            });
        });
    }

    if let Some(etx) = sh.engine_tx.borrow().as_ref() {
        let _ = etx.send(engine::EngineCmd::FetchConversations { limit: CONV_FETCH_LIMIT });
    }
}

/// Toast click: jump to the conversation (by the message id when we know
/// it, else by sender) and scroll to the message once its body renders.
pub(crate) fn open_message_from_toast(
    ui: &MainWindow,
    sh: &Shared,
    folder: &str,
    uid: u32,
    addr: &str,
) {
    if uid > 0 {
        *sh.pending_open_ref.borrow_mut() = Some((folder.to_string(), uid));
    }
    let idx = {
        let convs = sh.convs.borrow();
        convs.iter().position(|c| uid > 0 && c.messages.iter().any(|m| m.uid == uid)).or_else(
            || {
                if addr.is_empty() {
                    None
                } else {
                    convs.iter().position(|c| {
                        c.counterparts.iter().any(|cp| cp.addr.eq_ignore_ascii_case(addr))
                    })
                }
            },
        )
    };
    // Переключение вида из Rust: колбэк `view-changed` при этом НЕ зовётся
    // (его дёргает только меню), так что ни снимка позиции, ни его
    // восстановления здесь нет — диалог открывается со своим анкором.
    ui.set_view_mode(0);
    if let Some(idx) = idx {
        ui.set_selected(idx as i32);
        apply_active_header(ui, sh, idx);
        open_conversation(ui, sh, idx);
        ui.set_sidebar_row_y(idx as f32 * 64.0);
        ui.set_sidebar_scroll_seq(ui.get_sidebar_scroll_seq() + 1);
        // Панель сайдбара создаётся этим же переключением — bump выше уходит
        // в никуда, строка остаётся за кадром.
        nudge_sidebar_scroll(ui.as_weak(), idx as f32 * 64.0, 200);
    }
}
