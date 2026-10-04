//! The composer: sender identity and «Кому», reply / forward / new-mail
//! modes, attachment chips, the rich-text field and its keys, and the
//! optimistic-send stubs (contract §1–§3б).

use super::*;

/// (Re)fill the composer from-picker: the Slint model for drawing and the
/// parallel email list in Shared for send-time resolution. Keeps the current
/// selection if its email survives the refresh; otherwise re-aims at the
/// default identity (falling back to the primary account email).
pub(crate) fn refresh_composer_identities(ui: &MainWindow, sh: &Shared) {
    let Some(cache) = &sh.cache else { return };
    let idents = cache.load_identities(&sh.key).unwrap_or_default();
    // Явно выбранный отправитель закреплён — он выигрывает у восстановления
    // по индексу (иначе дельта-refetch, прилетающая каждые пару секунд, сбивала
    // выбор обратно на дефолтную identity: ровно этот баг «выбрал dd, ушло
    // info»). Без явного выбора — прежнее поведение (сохранить по email).
    let prev_email = sh.picked_identity.borrow().clone().or_else(|| {
        let list = sh.composer_identities.borrow();
        let idx = ui.get_composer_identity_index();
        list.get(idx.max(0) as usize).cloned()
    });
    let mut items: Vec<IdentityItem> = Vec::with_capacity(idents.len());
    let mut emails: Vec<String> = Vec::with_capacity(idents.len());
    let mut selected: i32 = -1;
    let mut default_idx: i32 = 0;
    for (i, id) in idents.iter().enumerate() {
        // From-picker dot: use the VIVID palette for a palette-fallback
        // identity (a 12px pastel dot reads as white). A server-provided
        // colour is the user's own choice — keep it verbatim.
        let color = if id.color.trim().is_empty() {
            IDENT_VIVID[i % IDENT_VIVID.len()]
        } else {
            id.color.as_str()
        };
        let label = if id.name.trim().is_empty() {
            id.email.clone()
        } else {
            format!("{} <{}>", id.name.trim(), id.email)
        };
        items.push(IdentityItem {
            label: label.into(),
            email: id.email.clone().into(),
            tint: parse_hex_color(color),
        });
        emails.push(id.email.to_lowercase());
        if id.is_default {
            default_idx = i as i32;
        }
        if Some(&id.email.to_lowercase()) == prev_email.as_ref() {
            selected = i as i32;
        }
    }
    // No identities synced yet — the picker hides (length < 2), sends fall
    // back to the account email engine-side.
    ui.set_composer_identities(ModelRc::new(VecModel::from(items)));
    ui.set_composer_identity_index(if selected >= 0 { selected } else { default_idx });
    *sh.composer_identities.borrow_mut() = emails;
}

/// id диалога, которому будет принадлежать письмо, отправленное с адреса
/// `chosen`.
///
/// Диалог опознаётся набором адресов, поэтому смена отправителя переносит
/// письмо в другую беседу — эта функция считает, в какую именно. Получатели
/// берутся по тому же правилу, что и в самой отправке (контракт §1): непустое
/// «Кому» побеждает собеседников открытого диалога.
pub(crate) fn target_conversation_id(ui: &MainWindow, sh: &Shared, chosen: &str) -> Option<String> {
    let split = |raw: &str| -> Vec<String> {
        raw.split([',', ';']).map(|p| p.trim().to_lowercase()).filter(|p| !p.is_empty()).collect()
    };

    let to = split(ui.get_composer_to().as_str());
    let cc = split(ui.get_composer_cc().as_str());
    let mut recipients = if to.is_empty() {
        let convs = sh.convs.borrow();
        let c = convs.get(sh.current.get())?;
        c.counterparts.iter().map(|cp| cp.addr.to_lowercase()).collect::<Vec<_>>()
    } else {
        to
    };
    recipients.extend(cc);
    if recipients.is_empty() {
        return None;
    }

    let participants = ddmail_core::imap::conversation_participants(chosen, &recipients, &[]);
    let identities = sh.composer_identities.borrow();
    let is_mine = |a: &String| identities.iter().any(|i| i.eq_ignore_ascii_case(a));
    let mine: Vec<String> = participants.iter().filter(|a| is_mine(a)).cloned().collect();
    let others: Vec<String> = participants.iter().filter(|a| !is_mine(a)).cloned().collect();
    // `chosen` — айдентика, с которой пишем: она же и владелец диалога, если
    // нашего адреса в наборе не окажется.
    Some(ddmail_core::imap::conversation_id(&mine, &others, chosen))
}

/// Перейти в диалог, которого ждали после отправки с другого адреса, — если он
/// уже приехал. Склеенный диалог показывается под id своего первичного, поэтому
/// цель ищется и через merges.
///
/// `true` — переход состоялся; тогда восстанавливать выделение по прежнему
/// диалогу уже нельзя, иначе оно тут же вернёт нас обратно.
pub(crate) fn try_pending_switch(ui: &MainWindow, sh: &Shared) -> bool {
    let Some(target) = sh.pending_switch.borrow().clone() else { return false };
    let idx = {
        let convs = sh.convs.borrow();
        convs.iter().position(|c| c.id == target).or_else(|| {
            let key = merges::MergeKey {
                account: sh.cur_account_key.borrow().clone(),
                id: target.clone(),
            };
            let primary = sh.merges.borrow().members_of(&key).first().cloned()?;
            convs.iter().position(|c| c.id == primary.id)
        })
    };
    let Some(idx) = idx else { return false };
    *sh.pending_switch.borrow_mut() = None;
    sh.current.set(idx);
    ui.set_selected(idx as i32);
    apply_active_header(ui, sh, idx);
    open_conversation(ui, sh, idx);
    ui.set_sidebar_row_y(idx as f32 * 64.0);
    ui.set_sidebar_scroll_seq(ui.get_sidebar_scroll_seq() + 1);
    true
}

/// Отправитель не тот, на кого пишут в этом диалоге?
///
/// Возвращает (адрес диалога, выбранный адрес), когда они разошлись. Оба —
/// нормализованные. `None`, если сравнивать нечего: диалог не открыт, адрес
/// получателя неизвестен, или он не среди наших identity — предупреждать про
/// алиас, который всё равно не выбрать, значит учить пользователя жать «всё
/// равно отправить» не глядя.
pub(crate) fn from_mismatch(ui: &MainWindow, sh: &Shared) -> Option<(String, String)> {
    // Та же логика, что и в on_send: закреплённый выбор важнее индекса пикера.
    let current = sh
        .picked_identity
        .borrow()
        .clone()
        .or_else(|| {
            let idx = ui.get_composer_identity_index();
            sh.composer_identities.borrow().get(idx.max(0) as usize).cloned()
        })?
        .to_lowercase();
    let expected = sh.convs.borrow().get(sh.current.get())?.received_by.to_lowercase();
    if expected.is_empty() || expected == current {
        return None;
    }
    if !sh.composer_identities.borrow().iter().any(|e| *e == expected) {
        return None;
    }
    Some((expected, current))
}

/// Aim the from-picker at a specific identity email (case-insensitive).
/// No-op when the email isn't one of ours — the previous selection stays.
pub(crate) fn aim_composer_identity(ui: &MainWindow, sh: &Shared, email: &str) {
    if email.is_empty() {
        return;
    }
    let lc = email.to_lowercase();
    if let Some(i) = sh.composer_identities.borrow().iter().position(|e| *e == lc) {
        ui.set_composer_identity_index(i as i32);
    }
}

/// Recompute the chevron panel's ghost hints: the EFFECTIVE «Кому»/«Тема»
/// an implicit send would use right now. The panel's fields are overrides
/// (empty = follow the dialog), so without hints an empty «Кому» reads as
/// «уйдёт в никуда» — this shows exactly where a plain reply goes. Mirrors
/// the on_send branch logic; must be re-run whenever the reply context
/// changes (conversation switch, bodies arrival, reply/forward/compose
/// mode transitions).
pub(crate) fn refresh_composer_hints(ui: &MainWindow, sh: &Shared) {
    let re_subject = |s: &str| -> String {
        if s.trim().is_empty() {
            String::new()
        } else if s.to_lowercase().starts_with("re:") {
            s.to_string()
        } else {
            format!("Re: {s}")
        }
    };
    // Forward: «Кому» is a REQUIRED real field (the send refuses without
    // it) — a hint would wrongly suggest it can stay empty.
    if sh.pending_forward.borrow().is_some() {
        ui.set_composer_to_auto("".into());
        ui.set_composer_subject_auto("".into());
        return;
    }
    // Transient compose to a fresh address.
    if let Some(email) = sh.pending_compose.borrow().clone() {
        ui.set_composer_to_auto(email.into());
        ui.set_composer_subject_auto("".into());
        return;
    }
    // Explicit reply via the quote ribbon: sender + (in groups) the
    // source's To/Cc, minus our own identity — same as the send branch.
    if let Some(body) = sh.pending_reply.borrow().as_ref() {
        let our = sh.key.to_lowercase();
        let extract_addr = |raw: &str| -> String {
            let lt = raw.find('<');
            let gt = lt.and_then(|i| raw[i..].find('>').map(|j| i + j));
            if let (Some(i), Some(j)) = (lt, gt) {
                raw[i + 1..j].trim().to_lowercase()
            } else {
                raw.trim().to_lowercase()
            }
        };
        let mut to: Vec<String> = Vec::new();
        let mut push = |a: String| {
            if !a.is_empty() && a != our && !to.contains(&a) {
                to.push(a);
            }
        };
        push(body.from_addr.to_lowercase());
        let is_group = sh.convs.borrow().get(sh.current.get()).map(|c| c.is_group).unwrap_or(false);
        if is_group {
            for a in body.to.iter().chain(body.cc.iter()) {
                push(extract_addr(a));
            }
        }
        ui.set_composer_to_auto(to.join(", ").into());
        ui.set_composer_subject_auto(re_subject(&body.subject).into());
        return;
    }
    // Implicit reply within the open conversation: all counterparts
    // (reply-all in groups), subject from the last incoming message.
    let convs = sh.convs.borrow();
    let Some(c) = convs.get(sh.current.get()) else {
        ui.set_composer_to_auto("".into());
        ui.set_composer_subject_auto("".into());
        return;
    };
    let to: Vec<String> =
        c.counterparts.iter().map(|cp| cp.addr.clone()).filter(|a| !a.is_empty()).collect();
    let cached = sh.current_bodies.borrow();
    let base_subject = cached
        .iter()
        .rev()
        .find(|b| !b.is_outgoing)
        .map(|b| b.subject.clone())
        .unwrap_or_else(|| c.last_subject.clone());
    ui.set_composer_to_auto(to.join(", ").into());
    ui.set_composer_subject_auto(re_subject(&base_subject).into());
}

pub(crate) fn enter_reply_mode(sh: &Shared, ui: &MainWindow, body: MessageBody) {
    let display_from =
        if body.from.is_empty() { body.from_addr.clone() } else { body.from.clone() };
    let preview = body_preview(&body);
    *sh.pending_reply.borrow_mut() = Some(body);
    ui.set_reply_ribbon_from(display_from.into());
    ui.set_reply_ribbon_preview(preview.into());
    ui.set_reply_ribbon_visible(true);
    ui.invoke_focus_composer();
    refresh_composer_hints(ui, sh);
    // Явный ответ показывает поля заполненными ТЕКСТОМ, а не подсказками
    // (контракт §3): видно, кому и с какой темой уйдёт, и правится как
    // обычный текст. Значения — ровно то, что вывела бы сама ветка отправки
    // (`refresh_composer_hints` повторяет её правило), так что адресаты не
    // меняются; непустое поле просто становится override'ом (§1).
    ui.set_composer_to(ui.get_composer_to_auto());
    ui.set_composer_subject(ui.get_composer_subject_auto());
    ui.set_composer_cc("".into());
    ui.set_composer_expanded(true);
}

pub(crate) fn exit_reply_mode(sh: &Shared, ui: &MainWindow) {
    *sh.pending_reply.borrow_mut() = None;
    *sh.pending_forward.borrow_mut() = None;
    ui.set_reply_ribbon_visible(false);
    ui.set_reply_ribbon_from("".into());
    ui.set_reply_ribbon_preview("".into());
    ui.set_composer_to("".into());
    // Тему ставят явно и ответ, и пересылка — снятая лента забирает её с
    // собой, иначе «Re:»/«Fwd:» чужого письма уехал бы в неявный ответ.
    ui.set_composer_subject("".into());
    refresh_composer_hints(ui, sh);
}

/// «Переслать» — Telegram-style: the original is pinned above the input as
/// a non-editable ribbon, the recipients panel unfolds with EMPTY Кому/Cc
/// and focus lands in «Кому». The composer text stays free for the user's
/// covering note; at send time the original's text goes below it after a
/// separator and its attachments are re-attached as-is (engine-side).
pub(crate) fn enter_forward_mode(sh: &Shared, ui: &MainWindow, body: MessageBody) {
    exit_reply_mode(sh, ui); // a forward replaces any staged reply
    let display_from =
        if body.from.is_empty() { body.from_addr.clone() } else { body.from.clone() };
    let subj_lc = body.subject.to_lowercase();
    let subject = if subj_lc.starts_with("fwd:") || subj_lc.starts_with("fw:") {
        body.subject.clone()
    } else {
        format!("Fwd: {}", body.subject)
    };
    let preview = body_preview(&body);
    *sh.pending_forward.borrow_mut() = Some(body);
    ui.set_reply_ribbon_from(format!("Переслать: {display_from}").into());
    ui.set_reply_ribbon_preview(preview.into());
    ui.set_reply_ribbon_visible(true);
    ui.set_composer_subject(subject.into());
    ui.set_composer_to("".into());
    ui.set_composer_cc("".into());
    ui.set_composer_expanded(true);
    ui.set_focus_to_seq(ui.get_focus_to_seq() + 1);
    refresh_composer_hints(ui, sh);
}

/// Shared logic for "enter transient compose mode". Pins the chat header
/// to the new recipient, blanks the bubble list, deselects the sidebar
/// row (none of the existing conversations match), and stashes the
/// target email on `Shared.pending_compose` for `on_send` to pick up.
pub(crate) fn enter_compose_mode(sh: &Shared, ui: &MainWindow, email: &str) {
    let email = email.trim().to_lowercase();
    *sh.pending_compose.borrow_mut() = Some(email.clone());
    // Свежий контекст — снимаем закреплённый ручной выбор отправителя от
    // предыдущей беседы/письма; для нового письма действует дефолтная
    // identity, пока пользователь не выберет другую в дропдауне.
    sh.picked_identity.borrow_mut().take();
    // Any staged explicit-reply target is invalidated by jumping into
    // a fresh compose: the new conversation has no bubble to quote.
    exit_reply_mode(sh, ui);
    sh.current_msgs.borrow_mut().clear();
    // The pane is empty in compose mode; stale bodies of the previously
    // open conversation must not resurface when a send stub re-renders it.
    sh.current_bodies.borrow_mut().clear();
    sh.pending_sends.borrow_mut().clear();
    let initial = email.chars().next().map(|c| c.to_uppercase().to_string()).unwrap_or_default();
    ui.set_active_name(email.clone().into());
    ui.set_active_initials(initial.into());
    ui.set_active_color(slint::Brush::SolidColor(hex("#10b981")));
    ui.set_active_meta_parts(ModelRc::new(VecModel::from(Vec::<MetaPart>::new())));
    ui.set_rename_open(false);
    ui.set_active_ident_color(slint::Brush::SolidColor(hex("#ffffff")));
    ui.set_messages(ModelRc::new(VecModel::from(Vec::<RowItem>::new())));
    ui.set_search_open(false);
    ui.set_search_query("".into());
    ui.set_search_selected_row(-1);
    ui.set_search_compose_email("".into());
    ui.set_search_contacts(ModelRc::new(VecModel::from(Vec::<ContactItem>::new())));
    ui.set_search_messages(ModelRc::new(VecModel::from(Vec::<MessageHit>::new())));
    // Prepend the synthetic "new chat" row and pull focus to the input.
    refresh_sidebar(sh, ui);
    ui.invoke_focus_composer();
}

/// Mirror the staged attachment basenames into the composer's chip model.
/// Активность кнопки «Отправить»: есть что отправлять, если непуст редактор
/// ИЛИ приложен файл.
///
/// Гейт смотрел только редактор, а письмо из одного вложения без текста —
/// валидное. Кнопка на него не реагировала совсем: `send()` в Slint вызывается
/// под `if (root.rt-can-send)`, так что обработчик отправки даже не начинался и
/// в логе не оставалось ничего. Пересчитывать обязательно в двух местах — при
/// правке текста и при смене набора вложений, иначе прикрепление файла не
/// оживит кнопку до следующего нажатия клавиши.
pub(crate) fn refresh_can_send(ui: &MainWindow, sh: &Shared) {
    let has_body = !sh.rich.borrow().is_empty();
    let has_attachments = !sh.compose_attachments.borrow().is_empty();
    ui.set_rt_can_send(has_body || has_attachments);
}

pub(crate) fn refresh_attachment_chips(ui: &MainWindow, sh: &Shared) {
    let chips: Vec<AttachChip> = sh
        .compose_attachments
        .borrow()
        .iter()
        .map(|p| AttachChip {
            name: p
                .file_name()
                .map(|n| n.to_string_lossy().into_owned())
                .unwrap_or_else(|| p.to_string_lossy().into_owned())
                .into(),
        })
        .collect();
    ui.set_composer_attachments(slint::ModelRc::new(slint::VecModel::from(chips)));
    // Набор вложений изменился — вместе с ним и ответ на вопрос «есть что
    // отправлять?».
    refresh_can_send(ui, sh);
}

/// Переверстать документ, отдать в UI битмап + геометрию каретки и обновить
/// производные свойства (plain-зеркало, состояние кнопок форматирования).
/// Вызывается после КАЖДОЙ правки — другого пути обновить картинку нет.
pub(crate) fn rich_refresh(ui: &MainWindow, sh: &Shared) {
    let width = sh.rich_width.get();
    if width <= 1.0 {
        // Ширина ещё не приехала из Slint (первый кадр) — рисовать не по чему.
        return;
    }
    let scale = ui.window().scale_factor();
    let ed = sh.rich.borrow();
    let sel = ed.has_selection().then(|| ed.selection());
    let mut slot = sh.rich_renderer.borrow_mut();
    let renderer = slot.get_or_insert_with(richtext_render::Renderer::new);
    let out = renderer.render(ed.doc(), ed.caret(), sel, width, scale);
    renderer.forget_unused(ed.doc());

    ui.set_rt_image(out.image);
    ui.set_rt_height(out.height);
    ui.set_rt_caret_x(out.caret_x);
    ui.set_rt_caret_y(out.caret_y);
    ui.set_rt_caret_h(out.caret_h);
    ui.set_rt_has_selection(ed.has_selection());
    ui.set_rt_empty(ed.is_empty());
    ui.set_composer_text(ed.plain_text().into());
    // Текст мог появиться или исчезнуть — пересчитать активность «Отправить».
    // Вложения тоже участвуют, поэтому через общий помощник, а не по `ed`.
    refresh_can_send(ui, sh);
}

/// Заменить содержимое композера plain-текстом (восстановление черновика
/// после неудачной отправки) и перерисовать.
pub(crate) fn rich_set_text(ui: &MainWindow, sh: &Shared, text: &str) {
    *sh.rich.borrow_mut() = richtext::Editor::from_text(text);
    rich_refresh(ui, sh);
}

pub(crate) fn rich_clear(ui: &MainWindow, sh: &Shared) {
    sh.rich.borrow_mut().clear();
    rich_refresh(ui, sh);
}

/// Вставка из буфера: сначала пробуем картинку (скриншот из Ножниц —
/// самый частый сценарий), затем текст.
pub(crate) fn rich_paste(ui: &MainWindow, sh: &Shared) {
    if let Some((png, w, h)) = clipboard_image() {
        let seq = sh.rich_cid_seq.get() + 1;
        sh.rich_cid_seq.set(seq);
        // cid должен быть уникален в пределах письма; время старта разводит
        // ещё и разные письма одной сессии.
        let stamp = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map(|d| d.as_millis())
            .unwrap_or(0);
        sh.rich.borrow_mut().insert_image(richtext::InlineImage {
            cid: format!("img{seq}.{stamp}@ddmail"),
            mime: "image/png".into(),
            bytes: Arc::new(png),
            w,
            h,
        });
        rich_refresh(ui, sh);
        return;
    }
    if let Some(text) = clipboard_text() {
        if !text.is_empty() {
            sh.rich.borrow_mut().insert_str(&text);
            rich_refresh(ui, sh);
        }
    }
}

/// Клавиатурная прокрутка открытой переписки: 1 = страница вверх, 2 = вниз,
/// 3 = в начало, 4 = в конец. Применяет мост `chat-page-seq` в app.slint.
pub(crate) fn chat_page(ui: &MainWindow, cmd: i32) {
    ui.set_chat_page_cmd(cmd);
    ui.set_chat_page_seq(ui.get_chat_page_seq() + 1);
}

/// Клавиша в композере. `true` — обработали (событие не всплывает дальше).
///
/// Раскладка: шорткаты сверяются и с латиницей, и с кириллицей тех же клавиш —
/// Slint матчит их по символу, а под ЙЦУКЕН приходит «с»/«м»/«и»…
/// (см. `global Kb` в app.slint — здесь та же болезнь, своё лечение).
pub(crate) fn rich_key(
    ui: &MainWindow,
    sh: &Rc<Shared>,
    text: &str,
    ctrl: bool,
    shift: bool,
    alt: bool,
) -> bool {
    use richtext::{Motion, StyleBit};
    use slint::platform::Key;

    let ch = text.chars().next();
    let is = |k: Key| ch == Some(char::from(k));

    if ctrl && !alt {
        // Та же клавиша, что и в остальных полях ввода: спрашиваем раскладку,
        // а не сравниваем букву (см. `shortcut_key`).
        let key = shortcut_key(text);
        let hit = |latin: char| key == Some(latin.to_ascii_uppercase() as u8);
        if hit('c') {
            let sel = sh.rich.borrow().selection_text();
            if !sel.is_empty() {
                clipboard_set(&sel);
            }
            return true;
        }
        if hit('x') {
            let sel = sh.rich.borrow().selection_text();
            if !sel.is_empty() {
                clipboard_set(&sel);
                sh.rich.borrow_mut().delete_selection();
                rich_refresh(ui, sh);
            }
            return true;
        }
        if hit('v') {
            rich_paste(ui, sh);
            return true;
        }
        if hit('a') {
            sh.rich.borrow_mut().select_all();
            rich_refresh(ui, sh);
            return true;
        }
        if hit('b') || hit('i') || hit('u') {
            let bit = if hit('b') {
                StyleBit::Bold
            } else if hit('i') {
                StyleBit::Italic
            } else {
                StyleBit::Underline
            };
            sh.rich.borrow_mut().toggle_style(bit);
            rich_refresh(ui, sh);
            return true;
        }
        if hit('k') {
            // Ctrl+K — ссылка из буфера обмена на выделенный текст. Диалога
            // ввода URL нет намеренно: адрес почти всегда уже скопирован.
            if let Some(url) = clipboard_text() {
                let url = url.trim().to_string();
                if url.starts_with("http://") || url.starts_with("https://") {
                    sh.rich.borrow_mut().insert_link(&url);
                    rich_refresh(ui, sh);
                }
            }
            return true;
        }
        if hit('z') {
            if shift {
                sh.rich.borrow_mut().redo();
            } else {
                sh.rich.borrow_mut().undo();
            }
            rich_refresh(ui, sh);
            return true;
        }
        if hit('y') {
            sh.rich.borrow_mut().redo();
            rich_refresh(ui, sh);
            return true;
        }
        // Прочие Ctrl-сочетания — не наши: пусть всплывают к общим хоткеям.
        if !is(Key::LeftArrow)
            && !is(Key::RightArrow)
            && !is(Key::Home)
            && !is(Key::End)
            && !is(Key::Backspace)
        {
            return false;
        }
    }

    // Escape и Tab отдаём наружу: закрытие панелей и обход фокуса — не дело
    // текстового поля.
    if is(Key::Escape) || is(Key::Tab) || is(Key::Backtab) {
        return false;
    }

    // Прокрутка переписки из композера (контракт §4). PgUp/PgDn — всегда:
    // поле ввода капнуто десятью строками, листать его страницами незачем.
    // Home/End — только в пустом поле: в набранном тексте это ход каретки.
    if is(Key::PageUp) || is(Key::PageDown) {
        chat_page(ui, if is(Key::PageUp) { 1 } else { 2 });
        return true;
    }
    if (is(Key::Home) || is(Key::End)) && !shift && sh.rich.borrow().is_empty() {
        chat_page(ui, if is(Key::Home) { 3 } else { 4 });
        return true;
    }

    if is(Key::Return) {
        if shift {
            sh.rich.borrow_mut().split_block();
            rich_refresh(ui, sh);
        } else {
            // Enter отправляет (контракт композера, §3). Текст снимаем ДО
            // вызова — on_send читает документ и не должен встретить
            // одолженный RefCell.
            let (empty, plain) = {
                let ed = sh.rich.borrow();
                (ed.is_empty(), ed.plain_text())
            };
            if !empty {
                ui.invoke_send(plain.into());
            }
        }
        return true;
    }
    if is(Key::Backspace) {
        sh.rich.borrow_mut().backspace();
        rich_refresh(ui, sh);
        return true;
    }
    if is(Key::Delete) {
        sh.rich.borrow_mut().delete_forward();
        rich_refresh(ui, sh);
        return true;
    }
    if is(Key::LeftArrow) || is(Key::RightArrow) {
        let right = is(Key::RightArrow);
        let m = match (right, ctrl) {
            (true, true) => Motion::WordRight,
            (true, false) => Motion::Right,
            (false, true) => Motion::WordLeft,
            (false, false) => Motion::Left,
        };
        sh.rich.borrow_mut().move_caret(m, shift);
        rich_refresh(ui, sh);
        return true;
    }
    if is(Key::UpArrow) || is(Key::DownArrow) {
        // Вертикаль знает только слой раскладки: шаг идёт по ВИЗУАЛЬНЫМ
        // строкам, а переносы живут там.
        let up = is(Key::UpArrow);
        let pos = {
            let ed = sh.rich.borrow();
            let slot = sh.rich_renderer.borrow();
            slot.as_ref().map(|r| r.move_vertical(ed.caret(), up))
        };
        if let Some(pos) = pos {
            sh.rich.borrow_mut().set_caret(pos, shift);
            rich_refresh(ui, sh);
        }
        return true;
    }
    if is(Key::Home) || is(Key::End) {
        let m = match (is(Key::Home), ctrl) {
            (true, true) => Motion::DocStart,
            (true, false) => Motion::LineStart,
            (false, true) => Motion::DocEnd,
            (false, false) => Motion::LineEnd,
        };
        sh.rich.borrow_mut().move_caret(m, shift);
        rich_refresh(ui, sh);
        return true;
    }

    // Печатаемый ввод. Именованные клавиши Slint кодирует управляющими
    // символами и приватной областью U+F700…U+F8FF — их в текст пускать нельзя.
    if !ctrl
        && !alt
        && !text.is_empty()
        && text.chars().all(|c| !c.is_control() && !matches!(c, '\u{F700}'..='\u{F8FF}'))
    {
        sh.rich.borrow_mut().insert_str(text);
        rich_refresh(ui, sh);
        return true;
    }
    false
}

/// Folder name of optimistic-send stub bodies. Never a real IMAP folder —
/// doubles as the marker that keeps stubs out of `current_msgs`, out of
/// the texture disk cache and out of server-bound refs.
pub(crate) const PENDING_FOLDER: &str = "__pending__";

/// One in-flight optimistic send: the stub body appended to the open pane
/// plus the conversation it belongs to ("" for a transient compose — no
/// conversation exists yet). `created` bounds the stub's lifetime: if the
/// server echo never text-matches (edge case), the ghost dies in 2 min
/// instead of living forever.
pub(crate) struct PendingSend {
    pub(crate) conv_id: String,
    pub(crate) body: MessageBody,
    pub(crate) created: Instant,
}

/// Body text normalised for stub↔real matching: the server may CRLF-fold
/// or trim what we sent, so compare modulo '\r' and outer whitespace.
pub(crate) fn norm_send_text(s: &str) -> String {
    s.replace('\r', "").trim().to_string()
}

/// The composer's HTML with its `cid:` images inlined as `data:` URIs.
///
/// A real message resolves `cid:` against the parts the server stored
/// (`engine::resolve_inline_parts`). A stub has no message on the server yet,
/// so its images have to travel inside the HTML — otherwise the preview shows
/// an empty box where the screenshot the user just pasted should be.
pub(crate) fn stub_html(html: &str, images: &[richtext::InlineImage]) -> String {
    use base64::Engine as _;
    let mut out = html.to_string();
    for img in images {
        let data = format!(
            "data:{};base64,{}",
            img.mime,
            base64::engine::general_purpose::STANDARD.encode(img.bytes.as_ref())
        );
        out = out.replace(&format!("cid:{}", img.cid), &data);
    }
    out
}

/// Attachment chips for the stub, described from the staged files themselves.
///
/// Only name and size reach the bubble (`attachment_chips`), and both are on
/// disk; mime is left empty because nothing reads it here. A file that cannot
/// be stat'd still gets a chip — its presence is the point, and a zero size
/// beats a chip that appears only after the server echo.
pub(crate) fn stub_attachments(paths: &[String]) -> Vec<Attachment> {
    paths
        .iter()
        .enumerate()
        .map(|(index, path)| {
            let p = std::path::Path::new(path);
            Attachment {
                filename: p
                    .file_name()
                    .map(|n| n.to_string_lossy().into_owned())
                    .unwrap_or_else(|| path.clone()),
                mime_type: String::new(),
                size: std::fs::metadata(p).map(|m| m.len() as usize).unwrap_or(0),
                index,
            }
        })
        .collect()
}

/// Optimistic send: append an outgoing stub bubble to the open pane the
/// moment «Отправить» is clicked, so the message is visible before the
/// server confirms. The stub is reconciled in the Messages handler and
/// rolled back in SendFailed.
///
/// It carries the same HTML and the same attachment list as the message on
/// its way out, because the stub is on screen for as long as a round trip
/// takes and the swap should not be visible. Sending it as bare text made
/// every reply flash: formatting, inline images and attachment chips all
/// appeared a second later, when the real message arrived from the server.
pub(crate) fn append_send_stub(
    sh: &Shared,
    text: &str,
    html: Option<String>,
    attachments: Vec<Attachment>,
    from: &str,
    hdr: StubHeaders,
    conv_id: &str,
) {
    let uid = sh.pending_send_seq.get() + 1;
    sh.pending_send_seq.set(uid);
    let body = MessageBody {
        uid,
        // A stub the user just typed: it has never been on the wire.
        raw_headers: String::new(),
        folder: PENDING_FOLDER.into(),
        subject: hdr.subject,
        from: from.to_string(),
        from_addr: from.to_string(),
        to: hdr.to,
        cc: hdr.cc,
        date: String::new(),
        date_ts: chrono::Local::now().timestamp(),
        html,
        text: Some(text.to_string()),
        attachments,
        is_outgoing: true,
        message_id: String::new(),
        in_reply_to: String::new(),
        references: Vec::new(),
    };
    sh.pending_sends.borrow_mut().push(PendingSend {
        conv_id: conv_id.to_string(),
        body: body.clone(),
        created: Instant::now(),
    });
    let bodies = {
        let mut cur = sh.current_bodies.borrow_mut();
        cur.push(body);
        cur.clone()
    };
    // Scroll to the end — the user's own message always lands at the bottom.
    send_render_job(sh, bodies, Some(-1));
}

/// Тема и адресаты заглушки — те же, что уходят на сервер. В склеенном
/// диалоге пузырь подписан темой, а у своего есть подсказка с адресатами
/// (контракт §4, «Склеенный диалог»): без них заглушка «прыгала» бы, когда
/// её сменит настоящее письмо.
pub(crate) struct StubHeaders {
    pub(crate) subject: String,
    pub(crate) to: Vec<String>,
    pub(crate) cc: Vec<String>,
}

/// Keep refetching conversations after a send until what we are waiting for has
/// arrived.
///
/// Sending is asynchronous: the server queues the message and writes the copy to
/// Sent only once delivery succeeds. A single timer could therefore never be
/// right — the old «через 2.5 с» shot looked for a conversation that was still
/// sitting in the outbox, which is exactly why a reply sent from a different
/// address had nothing to jump to.
///
/// The `message_sent` WS event is the primary signal now; this is the fallback
/// for a socket that is down. Attempt 0 always fires (it is the ordinary
/// «отправленное всплыло в открытом диалоге» case); later attempts only while a
/// post-send target is still outstanding, so a plain send costs one extra fetch
/// and nothing more.
pub(crate) fn schedule_post_send_refetch(attempt: usize) {
    // Roughly two minutes in total, front-loaded: delivery usually takes about a
    // second now that queueing kicks the scheduler, but a refusing MX with
    // retries can push it much further out.
    const DELAYS_MS: [u64; 5] = [2_500, 7_000, 15_000, 30_000, 60_000];
    let Some(&delay) = DELAYS_MS.get(attempt) else { return };

    slint::Timer::single_shot(std::time::Duration::from_millis(delay), move || {
        let keep_going = SHARED.with(|s| {
            let b = s.borrow();
            let Some(sh) = b.as_ref() else { return false };

            // Nothing left to wait for — stop, so a quiet client is not woken
            // every half minute for no reason.
            let waiting =
                sh.pending_switch.borrow().is_some() || sh.compose_sent_target.borrow().is_some();
            if attempt > 0 && !waiting {
                return false;
            }

            if let Some(cache) = &sh.cache {
                for k in sh.account_keys.borrow().iter() {
                    cache.set_meta(&format!("conv_full_ts:{k}"), "0").ok();
                }
            }
            if let Some(etx) = sh.engine_tx.borrow().as_ref() {
                let _ = etx.send(engine::EngineCmd::FetchConversations { limit: CONV_FETCH_LIMIT });
            }
            waiting
        });

        if keep_going {
            schedule_post_send_refetch(attempt + 1);
        }
    });
}
