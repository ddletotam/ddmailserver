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
    let prev_email = sh.compose.picked_identity.borrow().clone().or_else(|| {
        let list = sh.compose.composer_identities.borrow();
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
    *sh.compose.composer_identities.borrow_mut() = emails;
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
    let identities = sh.compose.composer_identities.borrow();
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
    let Some(target) = sh.compose.pending_switch.borrow().clone() else { return false };
    let idx = {
        let convs = sh.convs.borrow();
        convs.iter().position(|c| c.id == target).or_else(|| {
            let key = merges::MergeKey {
                account: sh.accounts.cur_account_key.borrow().clone(),
                id: target.clone(),
            };
            let primary = sh.merges.borrow().members_of(&key).first().cloned()?;
            convs.iter().position(|c| c.id == primary.id)
        })
    };
    let Some(idx) = idx else { return false };
    *sh.compose.pending_switch.borrow_mut() = None;
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
        .compose
        .picked_identity
        .borrow()
        .clone()
        .or_else(|| {
            let idx = ui.get_composer_identity_index();
            sh.compose.composer_identities.borrow().get(idx.max(0) as usize).cloned()
        })?
        .to_lowercase();
    let expected = sh.convs.borrow().get(sh.current.get())?.received_by.to_lowercase();
    if expected.is_empty() || expected == current {
        return None;
    }
    if !sh.compose.composer_identities.borrow().iter().any(|e| *e == expected) {
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
    if let Some(i) = sh.compose.composer_identities.borrow().iter().position(|e| *e == lc) {
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
    if sh.compose.pending_forward.borrow().is_some() {
        ui.set_composer_to_auto("".into());
        ui.set_composer_subject_auto("".into());
        return;
    }
    // Transient compose to a fresh address.
    if let Some(email) = sh.compose.pending_compose.borrow().clone() {
        ui.set_composer_to_auto(email.into());
        ui.set_composer_subject_auto("".into());
        return;
    }
    // Explicit reply via the quote ribbon: sender + (in groups) the
    // source's To/Cc, minus our own identity — same as the send branch.
    if let Some(body) = sh.compose.pending_reply.borrow().as_ref() {
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
    *sh.compose.pending_reply.borrow_mut() = Some(body);
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
    *sh.compose.pending_reply.borrow_mut() = None;
    *sh.compose.pending_forward.borrow_mut() = None;
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
    *sh.compose.pending_forward.borrow_mut() = Some(body);
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
/// target email on `Shared.compose.pending_compose` for `on_send` to pick up.
pub(crate) fn enter_compose_mode(sh: &Shared, ui: &MainWindow, email: &str) {
    let email = email.trim().to_lowercase();
    *sh.compose.pending_compose.borrow_mut() = Some(email.clone());
    // Свежий контекст — снимаем закреплённый ручной выбор отправителя от
    // предыдущей беседы/письма; для нового письма действует дефолтная
    // identity, пока пользователь не выберет другую в дропдауне.
    sh.compose.picked_identity.borrow_mut().take();
    // Any staged explicit-reply target is invalidated by jumping into
    // a fresh compose: the new conversation has no bubble to quote.
    exit_reply_mode(sh, ui);
    sh.current_msgs.borrow_mut().clear();
    // The pane is empty in compose mode; stale bodies of the previously
    // open conversation must not resurface when a send stub re-renders it.
    sh.current_bodies.borrow_mut().clear();
    sh.compose.pending_sends.borrow_mut().clear();
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
    let has_body = !sh.compose.rich.borrow().is_empty();
    let has_attachments = !sh.compose.compose_attachments.borrow().is_empty();
    ui.set_rt_can_send(has_body || has_attachments);
}

pub(crate) fn refresh_attachment_chips(ui: &MainWindow, sh: &Shared) {
    let chips: Vec<AttachChip> = sh
        .compose
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
    let width = sh.compose.rich_width.get();
    if width <= 1.0 {
        // Ширина ещё не приехала из Slint (первый кадр) — рисовать не по чему.
        return;
    }
    let scale = ui.window().scale_factor();
    let ed = sh.compose.rich.borrow();
    let sel = ed.has_selection().then(|| ed.selection());
    let mut slot = sh.compose.rich_renderer.borrow_mut();
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
    *sh.compose.rich.borrow_mut() = richtext::Editor::from_text(text);
    rich_refresh(ui, sh);
}

pub(crate) fn rich_clear(ui: &MainWindow, sh: &Shared) {
    sh.compose.rich.borrow_mut().clear();
    rich_refresh(ui, sh);
}

/// Вставка из буфера: сначала пробуем картинку (скриншот из Ножниц —
/// самый частый сценарий), затем текст.
pub(crate) fn rich_paste(ui: &MainWindow, sh: &Shared) {
    if let Some((png, w, h)) = clipboard_image() {
        let seq = sh.compose.rich_cid_seq.get() + 1;
        sh.compose.rich_cid_seq.set(seq);
        // cid должен быть уникален в пределах письма; время старта разводит
        // ещё и разные письма одной сессии.
        let stamp = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map(|d| d.as_millis())
            .unwrap_or(0);
        sh.compose.rich.borrow_mut().insert_image(richtext::InlineImage {
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
            sh.compose.rich.borrow_mut().insert_str(&text);
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
            let sel = sh.compose.rich.borrow().selection_text();
            if !sel.is_empty() {
                clipboard_set(&sel);
            }
            return true;
        }
        if hit('x') {
            let sel = sh.compose.rich.borrow().selection_text();
            if !sel.is_empty() {
                clipboard_set(&sel);
                sh.compose.rich.borrow_mut().delete_selection();
                rich_refresh(ui, sh);
            }
            return true;
        }
        if hit('v') {
            rich_paste(ui, sh);
            return true;
        }
        if hit('a') {
            sh.compose.rich.borrow_mut().select_all();
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
            sh.compose.rich.borrow_mut().toggle_style(bit);
            rich_refresh(ui, sh);
            return true;
        }
        if hit('k') {
            // Ctrl+K — ссылка из буфера обмена на выделенный текст. Диалога
            // ввода URL нет намеренно: адрес почти всегда уже скопирован.
            if let Some(url) = clipboard_text() {
                let url = url.trim().to_string();
                if url.starts_with("http://") || url.starts_with("https://") {
                    sh.compose.rich.borrow_mut().insert_link(&url);
                    rich_refresh(ui, sh);
                }
            }
            return true;
        }
        if hit('z') {
            if shift {
                sh.compose.rich.borrow_mut().redo();
            } else {
                sh.compose.rich.borrow_mut().undo();
            }
            rich_refresh(ui, sh);
            return true;
        }
        if hit('y') {
            sh.compose.rich.borrow_mut().redo();
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
    if (is(Key::Home) || is(Key::End)) && !shift && sh.compose.rich.borrow().is_empty() {
        chat_page(ui, if is(Key::Home) { 3 } else { 4 });
        return true;
    }

    if is(Key::Return) {
        if shift {
            sh.compose.rich.borrow_mut().split_block();
            rich_refresh(ui, sh);
        } else {
            // Enter отправляет (контракт композера, §3). Текст снимаем ДО
            // вызова — on_send читает документ и не должен встретить
            // одолженный RefCell.
            let (empty, plain) = {
                let ed = sh.compose.rich.borrow();
                (ed.is_empty(), ed.plain_text())
            };
            if !empty {
                ui.invoke_send(plain.into());
            }
        }
        return true;
    }
    if is(Key::Backspace) {
        sh.compose.rich.borrow_mut().backspace();
        rich_refresh(ui, sh);
        return true;
    }
    if is(Key::Delete) {
        sh.compose.rich.borrow_mut().delete_forward();
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
        sh.compose.rich.borrow_mut().move_caret(m, shift);
        rich_refresh(ui, sh);
        return true;
    }
    if is(Key::UpArrow) || is(Key::DownArrow) {
        // Вертикаль знает только слой раскладки: шаг идёт по ВИЗУАЛЬНЫМ
        // строкам, а переносы живут там.
        let up = is(Key::UpArrow);
        let pos = {
            let ed = sh.compose.rich.borrow();
            let slot = sh.compose.rich_renderer.borrow();
            slot.as_ref().map(|r| r.move_vertical(ed.caret(), up))
        };
        if let Some(pos) = pos {
            sh.compose.rich.borrow_mut().set_caret(pos, shift);
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
        sh.compose.rich.borrow_mut().move_caret(m, shift);
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
        sh.compose.rich.borrow_mut().insert_str(text);
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
    let uid = sh.compose.pending_send_seq.get() + 1;
    sh.compose.pending_send_seq.set(uid);
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
    sh.compose.pending_sends.borrow_mut().push(PendingSend {
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
            let waiting = sh.compose.pending_switch.borrow().is_some()
                || sh.compose.compose_sent_target.borrow().is_some();
            if attempt > 0 && !waiting {
                return false;
            }

            if let Some(cache) = &sh.cache {
                for k in sh.accounts.account_keys.borrow().iter() {
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

/// Composer attachments (file picker, remove) and the rich-text field
/// (resize, keys, pointer).
pub(crate) fn wire_composer_input(ui: &MainWindow, shared: &Rc<Shared>) {
    // ── Composer attachments ──
    //
    // The attach button opens the native file picker (blocking — the OS
    // dialog is modal, so the event loop has nothing to do meanwhile) and
    // appends the chosen paths to the staged set. `on_send` snapshots that
    // set into each outgoing message and clears it afterwards.
    let ui_weak_att = ui.as_weak();
    let sh_att = shared.clone();
    ui.on_attach_files(move || {
        let Some(u) = ui_weak_att.upgrade() else { return };
        let paths = pick_attachment_files(&u);
        if paths.is_empty() {
            return;
        }
        sh_att.compose.compose_attachments.borrow_mut().extend(paths);
        refresh_attachment_chips(&u, &sh_att);
    });
    let ui_weak_rm = ui.as_weak();
    let sh_rm = shared.clone();
    ui.on_remove_attachment(move |idx| {
        {
            let mut atts = sh_rm.compose.compose_attachments.borrow_mut();
            let i = idx as usize;
            if i < atts.len() {
                atts.remove(i);
            }
        }
        if let Some(u) = ui_weak_rm.upgrade() {
            refresh_attachment_chips(&u, &sh_rm);
        }
    });

    // ── Rich-text композер ──
    //
    // Slint отдаёт сюда ширину колонки, клавиши и мышь; обратно уезжают
    // битмап и геометрия каретки (rich_refresh). Модель — sh.compose.rich.
    let ui_weak_rtw = ui.as_weak();
    let sh_rtw = shared.clone();
    ui.on_rt_resize(move |w| {
        let Some(u) = ui_weak_rtw.upgrade() else { return };
        // Ширина скачет на каждом кадре ресайза — перевёрстываем только на
        // реальном изменении (сравнение в логических px с допуском ½ px).
        if (sh_rtw.compose.rich_width.get() - w).abs() < 0.5 {
            return;
        }
        sh_rtw.compose.rich_width.set(w);
        rich_refresh(&u, &sh_rtw);
    });
    let ui_weak_rtk = ui.as_weak();
    let sh_rtk = shared.clone();
    ui.on_rt_key(move |text, ctrl, shift, alt| {
        let Some(u) = ui_weak_rtk.upgrade() else { return false };
        rich_key(&u, &sh_rtk, text.as_str(), ctrl, shift, alt)
    });
    let ui_weak_rtp = ui.as_weak();
    let sh_rtp = shared.clone();
    ui.on_rt_pointer(move |x, y, kind| {
        let Some(u) = ui_weak_rtp.upgrade() else { return };
        let pos = {
            let slot = sh_rtp.compose.rich_renderer.borrow();
            let Some(r) = slot.as_ref() else { return };
            r.pos_at(x, y)
        };
        match kind {
            // Нажатие ставит каретку и начинает протяжку; Shift+клик тянет
            // выделение от прежнего якоря (как в любом текстовом поле).
            0 => {
                sh_rtp.compose.rich_dragging.set(true);
                sh_rtp.compose.rich.borrow_mut().set_caret(pos, false);
            }
            1 => {
                if !sh_rtp.compose.rich_dragging.get() {
                    return;
                }
                sh_rtp.compose.rich.borrow_mut().set_caret(pos, true);
            }
            2 => sh_rtp.compose.rich_dragging.set(false),
            _ => sh_rtp.compose.rich.borrow_mut().select_word_at(pos),
        }
        rich_refresh(&u, &sh_rtp);
    });
}

/// Sending: the «wrong sender address» dialog and `on_send` itself
/// (contract §1, §2).
pub(crate) fn wire_send(ui: &MainWindow, shared: &Rc<Shared>) {
    // Composer → three branches depending on staged intent:
    //   1. Transient compose target (search dropdown).
    //   2. Explicit reply to a specific bubble (quote ribbon).
    //   3. Implicit reply to the currently open conversation.
    // ---- «Отвечаешь не с того адреса»: решения по задержанной отправке ----
    {
        let weak = ui.as_weak();
        let sh = shared.clone();
        ui.on_from_mismatch_pick(move |index| {
            let Some(ui) = weak.upgrade() else { return };
            let email = sh.compose.composer_identities.borrow().get(index.max(0) as usize).cloned();
            if let Some(email) = email {
                // Закрепляем как ручной выбор — иначе дельта-refetch собьёт
                // индекс пикера обратно, и уйдёт снова не то.
                *sh.compose.picked_identity.borrow_mut() = Some(email.clone());
                aim_composer_identity(&ui, &sh, &email);
                // Письмо уедет в диалог своего набора адресов. Переходим туда
                // только если попросили галочкой и адрес действительно другой.
                let switching = ui.get_from_mismatch_switch()
                    && ui.get_from_mismatch_index() != ui.get_from_mismatch_expected_index();
                *sh.compose.pending_switch.borrow_mut() =
                    if switching { target_conversation_id(&ui, &sh, &email) } else { None };
            }
            let text = sh.compose.held_send.borrow().clone().unwrap_or_default();
            ui.invoke_send(text.into());
        });
    }
    {
        let sh = shared.clone();
        ui.on_from_mismatch_cancel(move || {
            // Возвращать текст не нужно: композер чистится только на
            // успешной ветке отправки (clear_overrides), документ на месте.
            // Снимаем и задержанную отправку, и ожидание перехода.
            sh.compose.held_send.borrow_mut().take();
            sh.compose.pending_switch.borrow_mut().take();
        });
    }

    let ui_weak_send = ui.as_weak();
    let sh_send = shared.clone();
    ui.on_send(move |text| {
        let text = text.to_string();
        // Тело письма — rich-документ композера; `text` (plain-зеркало) идёт
        // в text/plain-часть и в заглушку optimistic send. Пустой текст ещё
        // не значит «нечего отправлять»: письмо из одной картинки — валидное.
        let (rich_html, rich_images) = {
            let ed = sh_send.compose.rich.borrow();
            // Вложения спрашиваем отдельно: `ed.is_empty()` знает только
            // документ редактора — абзацы и inline-картинки, — а прикреплённые
            // файлы лежат в `compose_attachments`. Без этой проверки письмо из
            // одного вложения без единого слова не отправлялось, и кнопка при
            // этом молчала: обработчик выходил здесь же, до всякой обратной
            // связи.
            let has_attachments = !sh_send.compose.compose_attachments.borrow().is_empty();
            if ed.is_empty() && text.trim().is_empty() && !has_attachments {
                eprintln!("send: нечего отправлять — ни текста, ни картинок, ни вложений");
                return;
            }
            (ed.html(), ed.images())
        };
        let inline_atts: Vec<ddmail_core::types::OutgoingAttachment> = rich_images
            .iter()
            .map(|img| ddmail_core::types::OutgoingAttachment {
                filename: format!("{}.png", img.cid.split('@').next().unwrap_or("image")),
                mime_type: img.mime.clone(),
                content: img.bytes.as_ref().clone(),
                content_id: Some(img.cid.clone()),
            })
            .collect();
        // Отправка, возвращённая диалогом «не тот адрес», проверку уже прошла.
        let resumed = sh_send.compose.held_send.borrow_mut().take().is_some();
        if !resumed {
            if let Some(ui) = ui_weak_send.upgrade() {
                if let Some((expected, current)) = from_mismatch(&ui, &sh_send) {
                    *sh_send.compose.held_send.borrow_mut() = Some(text.clone());
                    // Список для дропдауна + предвыбор на адресе диалога:
                    // правильный вариант уже выбран, подтвердить — один клик.
                    let addresses = sh_send.compose.composer_identities.borrow().clone();
                    let picked = addresses.iter().position(|e| *e == expected).unwrap_or(0);
                    ui.set_from_mismatch_options(ModelRc::new(VecModel::from(
                        addresses.iter().map(|e| slint::SharedString::from(e.as_str())).collect::<Vec<_>>(),
                    )));
                    ui.set_from_mismatch_index(picked as i32);
                    ui.set_from_mismatch_expected_index(picked as i32);
                    // Галочка каждый раз с нуля: переход — осознанный выбор,
                    // а не залипшая настройка.
                    ui.set_from_mismatch_switch(false);
                    ui.set_from_mismatch_expected(expected.into());
                    ui.set_from_mismatch_current(current.into());
                    ui.set_from_mismatch_visible(true);
                    return;
                }
            }
        }
        // Graceful guard: if this thread belongs to a connection that was
        // since removed, the send has nowhere to go — say so plainly instead
        // of misrouting to another account.
        {
            let ak = sh_send.accounts.cur_account_key.borrow().clone();
            let alive = ak.is_empty() || sh_send.accounts.account_keys.borrow().iter().any(|k| *k == ak);
            if !alive {
                toast_window::show(
                    2,
                    0,
                    "Отправка недоступна",
                    "Это письмо из подключения, которое было удалено. Добавьте подключение заново, чтобы отправлять с этого адреса.",
                    false,
                    600,
                    || {},
                    || {},
                    || {},
                );
                return;
            }
        }
        // Read chevron-panel overrides up front. Non-empty subject
        // override wins over the per-branch auto-derivation; cc is
        // parsed once and passed through to the engine in every
        // branch.
        let ui_now = ui_weak_send.upgrade();
        let subject_override = ui_now
            .as_ref()
            .map(|u| u.get_composer_subject().to_string().trim().to_string())
            .unwrap_or_default();
        let cc: Vec<String> = ui_now
            .as_ref()
            .map(|u| u.get_composer_cc().to_string())
            .unwrap_or_default()
            .split(|c: char| c == ',' || c == ';')
            .map(|s| s.trim().to_string())
            .filter(|s| !s.is_empty())
            .collect();
        // Explicit «Кому» override — same contract as the subject override:
        // a filled field is the user's explicit order and beats EVERY
        // auto-derivation, reply-all included. Ignoring it while showing an
        // editable field once sent a reply to 18 people instead of one.
        let to_override: Vec<String> = ui_now
            .as_ref()
            .map(|u| u.get_composer_to().to_string())
            .unwrap_or_default()
            .split(|c: char| c == ',' || c == ';')
            .map(|s| s.trim().to_string())
            .filter(|s| !s.is_empty())
            .collect();
        // Sending identity: закреплённый ручной выбор (picked_identity) имеет
        // приоритет над индексом пикера — индекс могла сбить дельта-refetch
        // между выбором и отправкой (баг «выбрал dd, ушло info»). Без явного
        // выбора — резолвим по индексу через Shared-список (Slint-модель
        // только для отрисовки). None до синка identities → движок подставит
        // адрес аккаунта.
        let from_identity: Option<String> = sh_send
            .compose.picked_identity
            .borrow()
            .clone()
            .or_else(|| {
                ui_now.as_ref().and_then(|u| {
                    let idx = u.get_composer_identity_index();
                    sh_send
                        .compose.composer_identities
                        .borrow()
                        .get(idx.max(0) as usize)
                        .cloned()
                })
            });
        // Staged attachment paths for this send, snapshotted up front so the
        // per-branch Send commands all carry the same list.
        let attachments: Vec<String> = sh_send
            .compose.compose_attachments
            .borrow()
            .iter()
            .map(|p| p.to_string_lossy().into_owned())
            .collect();
        // After a successful staging the override fields + attachments reset
        // so the next message starts blank again. Keeps the chevron panel
        // from silently inheriting last message's headers.
        let clear_overrides = || {
            sh_send.compose.compose_attachments.borrow_mut().clear();
            if let Some(u) = ui_weak_send.upgrade() {
                u.set_composer_subject("".into());
                u.set_composer_cc("".into());
                u.set_composer_to("".into());
                // Тело чистим здесь, а не в Slint при клике: до этой точки
                // ветка могла отказаться отправлять (нет адресата, мёртвое
                // подключение) — набранное тогда обязано остаться на месте.
                rich_clear(&u, &sh_send);
                refresh_attachment_chips(&u, &sh_send);
            }
        };

        // Branch 0: forward — explicit recipients from the «Кому» field;
        // the typed text is the covering note, the original's text goes
        // below it after a separator, attachments re-attach engine-side.
        // Клон берётся ОТДЕЛЬНЫМ стейтментом, а не в скрутинии `if let`:
        // временное значение из скрутинии живёт до конца блока, поэтому `Ref`
        // пережил бы `exit_reply_mode` в конце ветки, а тот пишет в эту же
        // ячейку. Именно так клиент и умирал сразу после отправки —
        // «RefCell already borrowed», main.rs:1063. То же и в двух ветках ниже.
        let forwarded = sh_send.compose.pending_forward.borrow().clone();
        if let Some(orig) = forwarded {
            let to = to_override.clone();
            if to.is_empty() {
                eprintln!("forward: адресат не указан — заполните «Кому»");
                if let Some(u) = ui_now.as_ref() {
                    u.set_composer_expanded(true);
                    u.set_focus_to_seq(u.get_focus_to_seq() + 1);
                }
                return;
            }
            // enter_forward_mode pre-filled composer-subject with «Fwd: …»,
            // so the override carries it; fall back defensively anyway.
            let subject = if !subject_override.is_empty() {
                subject_override.clone()
            } else {
                format!("Fwd: {}", orig.subject)
            };
            let from_line = if orig.from.is_empty() {
                orig.from_addr.clone()
            } else {
                orig.from.clone()
            };
            let orig_text = orig.text.clone().unwrap_or_default();
            let body_text = format!(
                "{text}\n\n---------- Пересланное сообщение ----------\n\
                 От: {from_line}\nДата: {}\nТема: {}\n\n{orig_text}",
                orig.date, orig.subject
            );
            // HTML-версия: набранная сопроводиловка (со стилями) + тот же
            // блок пересылки, экранированный как обычный текст.
            let body_html = format!(
                "{rich_html}<br><div>---------- Пересланное сообщение ----------</div>\
                 <div>От: {}</div><div>Дата: {}</div><div>Тема: {}</div><br><div>{}</div>",
                html_escape_plain(&from_line),
                html_escape_plain(&orig.date),
                html_escape_plain(&orig.subject),
                html_escape_plain(&orig_text).replace('\n', "<br>")
            );
            if let Some(etx) = sh_send.engine_tx.borrow().as_ref() {
                println!("forwarding {}/{} to {to:?}", orig.folder, orig.uid);
                let _ = etx.send(engine::EngineCmd::Send {
                    to,
                    cc: cc.clone(),
                    subject,
                    body: body_text,
                    html: body_html,
                    inline: inline_atts.clone(),
                    in_reply_to: None,
                    references: None,
                    from: from_identity.clone(),
                    attachments: attachments.clone(),
                    forward_attachments: Some(MessageRef {
                        folder: orig.folder.clone(),
                        uid: orig.uid,
                        message_id: orig.message_id.clone(),
                        seen: true,
                    }),
                    account_key: sh_send.accounts.cur_account_key.borrow().clone(),
                });
                clear_overrides();
                if let Some(u) = ui_now.as_ref() {
                    exit_reply_mode(&sh_send, u);
                    u.set_composer_expanded(false);
                }
            } else {
                eprintln!("send: no live engine (set DDMAIL_* env)");
            }
            return;
        }
        // Branch 1: transient compose target set via the search dropdown.
        let compose_target = sh_send.compose.pending_compose.borrow().clone();
        if let Some(target) = compose_target {
            let subject = if !subject_override.is_empty() {
                subject_override.clone()
            } else {
                "Новое сообщение".to_string()
            };
            if let Some(etx) = sh_send.engine_tx.borrow().as_ref() {
                println!("sending new message to {target}");
                let to = if to_override.is_empty() { vec![target] } else { to_override.clone() };
                let hdr = StubHeaders { subject: subject.clone(), to: to.clone(), cc: cc.clone() };
                let _ = etx.send(engine::EngineCmd::Send {
                    to,
                    cc: cc.clone(),
                    subject,
                    body: text.clone(),
                    html: rich_html.clone(),
                    inline: inline_atts.clone(),
                    in_reply_to: None,
                    references: None,
                    from: from_identity.clone(),
                    attachments: attachments.clone(),
                    forward_attachments: None,
                    account_key: sh_send.accounts.cur_account_key.borrow().clone(),
                });
                clear_overrides();
                // Optimistic bubble in the (empty) compose pane; no
                // conversation exists yet, so the stub carries no conv id.
                append_send_stub(
                    &sh_send,
                    &text,
                    Some(stub_html(&rich_html, &rich_images)),
                    stub_attachments(&attachments),
                    &from_identity.clone().unwrap_or_else(|| sh_send.key.clone()),
                    hdr,
                    "",
                );
            } else {
                eprintln!("send: no live engine (set DDMAIL_* env)");
            }
            return;
        }
        // Branch 2: explicit reply via quote ribbon.
        let quoted_reply = sh_send.compose.pending_reply.borrow().clone();
        if let Some(reply_body) = quoted_reply {
            // Reply-all in groups: the current convs entry tells us
            // group-ness; in 1:1 conversations the counterpart is the
            // sender anyway. The recipients are the source's from + to
            // + cc minus our identities (mirrored from svelte's
            // ChatView.svelte:478-501).
            let our_lc: std::collections::HashSet<String> =
                std::iter::once(sh_send.key.to_lowercase()).collect();
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
            let mut seen: std::collections::HashSet<String> = std::collections::HashSet::new();
            let mut push = |a: String| {
                if a.is_empty() || our_lc.contains(&a) || !seen.insert(a.clone()) {
                    return;
                }
                to.push(a);
            };
            push(reply_body.from_addr.to_lowercase());
            let is_group = sh_send
                .convs
                .borrow()
                .get(sh_send.current.get())
                .map(|c| c.is_group)
                .unwrap_or(false);
            if is_group {
                for a in reply_body.to.iter().chain(reply_body.cc.iter()) {
                    push(extract_addr(a));
                }
            }
            // Explicit «Кому» beats the reply-all derivation entirely.
            let to = if to_override.is_empty() { to } else { to_override.clone() };
            if to.is_empty() {
                eprintln!("reply: no recipient resolved");
                return;
            }
            let subject = if !subject_override.is_empty() {
                subject_override.clone()
            } else if reply_body.subject.to_lowercase().starts_with("re:") {
                reply_body.subject.clone()
            } else {
                format!("Re: {}", reply_body.subject)
            };
            let in_reply_to = (!reply_body.message_id.is_empty())
                .then(|| reply_body.message_id.clone());
            let mut refs = reply_body.references.clone();
            if !reply_body.message_id.is_empty() {
                refs.push(reply_body.message_id.clone());
            }
            let references = (!refs.is_empty()).then(|| refs.join(" "));
            if let Some(etx) = sh_send.engine_tx.borrow().as_ref() {
                println!("sending explicit reply to {to:?}");
                let hdr = StubHeaders { subject: subject.clone(), to: to.clone(), cc: cc.clone() };
                let _ = etx.send(engine::EngineCmd::Send {
                    to, cc: cc.clone(), subject, body: text.clone(),
                    html: rich_html.clone(), inline: inline_atts.clone(),
                    in_reply_to, references,
                    from: from_identity.clone(),
                    attachments: attachments.clone(),
                    forward_attachments: None,
                    account_key: sh_send.accounts.cur_account_key.borrow().clone(),
                });
                clear_overrides();
                let conv_id = sh_send
                    .convs
                    .borrow()
                    .get(sh_send.current.get())
                    .map(|c| c.id.clone())
                    .unwrap_or_default();
                append_send_stub(
                    &sh_send,
                    &text,
                    Some(stub_html(&rich_html, &rich_images)),
                    stub_attachments(&attachments),
                    &from_identity.clone().unwrap_or_else(|| sh_send.key.clone()),
                    hdr,
                    &conv_id,
                );
            } else {
                eprintln!("send: no live engine (set DDMAIL_* env)");
            }
            // Quote ribbon goes away once the message is staged for send.
            if let Some(ui) = ui_weak_send.upgrade() {
                exit_reply_mode(&sh_send, &ui);
            }
            return;
        }
        // Branch 3: implicit reply within the currently selected conversation.
        let convs = sh_send.convs.borrow();
        let Some(c) = convs.get(sh_send.current.get()) else { return };
        let to: Vec<String> = if !to_override.is_empty() {
            // Explicit «Кому» beats the conversation's counterparts.
            to_override.clone()
        } else {
            c.counterparts
                .iter()
                .map(|cp| cp.addr.clone())
                .filter(|a| !a.is_empty())
                .collect()
        };
        if to.is_empty() {
            eprintln!("send: no recipient for this conversation");
            return;
        }
        // Subject mirrors the *last incoming* message per the spec — that's
        // the one the user is replying to, even if our own outgoing came
        // after it. Bodies of the open conversation are already in memory;
        // fall back to conversation last_subject when there are none.
        let cached = sh_send.current_bodies.borrow();
        let last_incoming = cached.iter().rev().find(|b| !b.is_outgoing);
        let base_subject = last_incoming
            .map(|b| b.subject.clone())
            .unwrap_or_else(|| c.last_subject.clone());
        let subject = if !subject_override.is_empty() {
            subject_override.clone()
        } else if base_subject.to_lowercase().starts_with("re:") {
            base_subject
        } else {
            format!("Re: {base_subject}")
        };
        // Threading headers from the same last-incoming we used for the subject.
        let (in_reply_to, references) = last_incoming
            .or_else(|| cached.last())
            .map(|b| {
                let irt = (!b.message_id.is_empty()).then(|| b.message_id.clone());
                let mut refs = b.references.clone();
                if !b.message_id.is_empty() {
                    refs.push(b.message_id.clone());
                }
                let refs = (!refs.is_empty()).then(|| refs.join(" "));
                (irt, refs)
            })
            .unwrap_or((None, None));
        // Release the convs/current_bodies borrows before the stub append —
        // it re-borrows current_bodies mutably.
        let conv_id = c.id.clone();
        drop(cached);
        drop(convs);
        if let Some(etx) = sh_send.engine_tx.borrow().as_ref() {
            println!("sending reply to {to:?}");
            // Described before the list is handed to the engine — the stub
            // shows the same chips as the message going out.
            let stub_atts = stub_attachments(&attachments);
            let hdr = StubHeaders { subject: subject.clone(), to: to.clone(), cc: cc.clone() };
            let _ = etx.send(engine::EngineCmd::Send {
                to, cc, subject, body: text.clone(),
                html: rich_html.clone(), inline: inline_atts.clone(),
                in_reply_to, references,
                from: from_identity.clone(),
                attachments,
                forward_attachments: None,
                account_key: sh_send.accounts.cur_account_key.borrow().clone(),
            });
            clear_overrides();
            append_send_stub(
                &sh_send,
                &text,
                Some(stub_html(&rich_html, &rich_images)),
                stub_atts,
                &from_identity.clone().unwrap_or_else(|| sh_send.key.clone()),
                hdr,
                &conv_id,
            );
        } else {
            eprintln!("send: no live engine (set DDMAIL_* env)");
        }
    });
}

/// Explicit sender choice in the composer's identity dropdown.
pub(crate) fn wire_identity_pick(ui: &MainWindow, shared: &Rc<Shared>) {
    // Явный выбор отправителя из дропдауна — закрепляем email, чтобы он
    // пережил дельта-refresh и авто-наведение (см. picked_identity).
    let sh_ip = shared.clone();
    ui.on_identity_picked(move |ii| {
        let email = sh_ip.compose.composer_identities.borrow().get(ii.max(0) as usize).cloned();
        if let Some(email) = email {
            println!("identity picked: {email}");
            *sh_ip.compose.picked_identity.borrow_mut() = Some(email);
        }
    });
}
