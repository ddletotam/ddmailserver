//! Папки диалогов на стороне UI: табы над списком, раздел настроек «Папки»,
//! пункт «В папку» контекстного меню и синхронизация документа с сервером.
//! Модель и правила — `folders.rs`, контракт — §4 «Папки диалогов».

use super::*;
use crate::folders::{FoldersDoc, Kind, Store};

/// Правки в настройках копятся столько, прежде чем уйти на сервер: имя
/// печатают по букве, и запрос на каждую был бы шумом и гонкой версий.
const SAVE_DEBOUNCE: Duration = Duration::from_millis(800);
/// Сервер не ответил — повтор через столько.
const SAVE_RETRY: Duration = Duration::from_secs(30);

/// Пересобрать вид после смены папки/документа. Открытый диалог остаётся
/// открытым, если он виден в новом списке; иначе — `reselect_after_switch`.
pub(crate) fn reapply_view(ui: &MainWindow, sh: &Shared) {
    let cur = sh.convs.borrow().get(sh.current.get()).map(|c| c.id.clone());
    rebuild_merged_view(ui, sh, cur.clone(), false);
    let still = cur.is_some_and(|id| sh.convs.borrow().iter().any(|c| c.id == id));
    if !still {
        reselect_after_switch(ui, sh);
    }
}

/// Открытый диалог не попал в новый список. Открываем первый **прочитанный**:
/// открытие ставит `\Seen`, и простое переключение таба не должно гасить
/// чужое непрочитанное. Все непрочитанные — первый. Пустая папка — пустая
/// правая панель.
fn reselect_after_switch(ui: &MainWindow, sh: &Shared) {
    let pick = {
        let convs = sh.convs.borrow();
        if convs.is_empty() {
            None
        } else {
            Some(convs.iter().position(|c| c.unread_count == 0).unwrap_or(0))
        }
    };
    match pick {
        Some(idx) => {
            sh.current.set(idx);
            ui.set_selected(idx as i32);
            apply_active_header(ui, sh, idx);
            open_conversation(ui, sh, idx);
            ui.set_sidebar_row_y(idx as f32 * 64.0);
            ui.set_sidebar_scroll_seq(ui.get_sidebar_scroll_seq() + 1);
        }
        None => {
            sh.current.set(0);
            ui.set_selected(-1);
            ui.set_messages(ModelRc::new(VecModel::from(Vec::<RowItem>::new())));
            ui.set_render_total(0);
            sh.current_msgs.borrow_mut().clear();
            sh.current_bodies.borrow_mut().clear();
        }
    }
}

/// Выбрать таб (`""` — инбокс).
pub(crate) fn switch_folder(ui: &MainWindow, sh: &Shared, id: &str) {
    if *sh.active_folder.borrow() == id {
        return;
    }
    *sh.active_folder.borrow_mut() = id.to_string();
    save_calendar_settings(ui, sh);
    reapply_view(ui, sh);
    // Новый список начинается сверху: позиция прокрутки прошлой папки к
    // этой отношения не имеет.
    ui.set_sidebar_row_y(sh.current.get() as f32 * 64.0);
    ui.set_sidebar_scroll_seq(ui.get_sidebar_scroll_seq() + 1);
}

/// Найти диалог в видимом списке, а если он в другой папке — переключиться
/// на неё. Нужен всем путям «открыть конкретный диалог»: клик по тосту,
/// выбор в поиске, переход после отправки. Без этого они молча ничего не
/// делали бы, если диалог лежит не в выбранной папке.
pub(crate) fn reveal(
    ui: &MainWindow,
    sh: &Shared,
    pred: impl Fn(&Conversation) -> bool,
) -> Option<usize> {
    if let Some(i) = sh.convs.borrow().iter().position(&pred) {
        return Some(i);
    }
    let target = {
        let st = sh.folders.borrow();
        if !folders_active(&st.doc, sh.folders_on.get()) {
            return None;
        }
        let full = full_view(sh);
        let c = full.iter().find(|c| pred(c))?;
        st.doc.folder_of(c, &sh.key).map(|i| st.doc.folders[i].id.clone()).unwrap_or_default()
    };
    *sh.active_folder.borrow_mut() = target;
    save_calendar_settings(ui, sh);
    // Без выбора: вызывающий сразу откроет найденный диалог сам.
    rebuild_merged_view(ui, sh, None, false);
    sh.convs.borrow().iter().position(&pred)
}

/// Правка документа: применить, показать сразу, сохранить с дебаунсом.
/// `refresh_settings` — перечитать раздел настроек. Для правок из текстовых
/// полей — нет: пересоздание строки на каждую букву срывало бы фокус ввода.
pub(crate) fn commit(
    ui: &MainWindow,
    sh: &Shared,
    refresh_settings: bool,
    mutate: impl FnOnce(&mut FoldersDoc),
) {
    {
        let mut st = sh.folders.borrow_mut();
        mutate(&mut st.doc);
        folders::save(&st);
    }
    sh.folders_dirty.set(true);
    reapply_view(ui, sh);
    // Скрытие папки меняет, что считает метка в трее.
    tray_sync_dot(sh);
    if refresh_settings {
        refresh_folder_settings(ui, sh);
    }
    schedule_save(sh, SAVE_DEBOUNCE);
}

fn schedule_save(sh: &Shared, after: Duration) {
    sh.folders_save_timer.start(slint::TimerMode::SingleShot, after, || {
        SHARED.with(|s| {
            if let Some(sh) = s.borrow().as_ref() {
                flush_save(sh);
            }
        });
    });
}

/// Отправить документ на сервер. Пока запрос в пути, следующий не уходит:
/// он ушёл бы со старой базовой версией и сам с собой конфликтовал.
fn flush_save(sh: &Shared) {
    if !sh.folders_dirty.get() || sh.folders_inflight.get() {
        return;
    }
    let st = sh.folders.borrow();
    let Store::Server { account } = &st.store else {
        // Локальное хранение: файл уже записан в `commit`.
        sh.folders_dirty.set(false);
        return;
    };
    let Some(etx) = sh.engine_tx.borrow().clone() else { return };
    sh.folders_dirty.set(false);
    sh.folders_inflight.set(true);
    let _ = etx.send(engine::EngineCmd::SaveDialogFolders {
        account_key: account.clone(),
        doc: st.doc.to_value(),
        base_version: st.version,
    });
}

/// Ответ движка на `FetchDialogFolders`.
pub(crate) fn on_fetched(
    ui: &MainWindow,
    sh: &Shared,
    store: Option<String>,
    version: i64,
    doc: Option<serde_json::Value>,
) {
    // Своя несохранённая правка важнее: её запись либо пройдёт, либо
    // получит конфликт и тогда возьмёт серверную версию.
    if sh.folders_dirty.get() || sh.folders_inflight.get() {
        return;
    }
    let push_local = {
        let mut st = sh.folders.borrow_mut();
        match store {
            Some(account) => {
                let target = Store::Server { account };
                // Папки, ни разу не побывавшие на сервере (версия 0: заведены
                // без нативного подключения или пока сервер не отвечал), не
                // теряем: сервер пуст — отправляем их туда, а не берём пустоту.
                let push = doc.is_none() && st.version == 0 && !st.doc.folders.is_empty();
                if !push && st.store == target && st.version == version {
                    return; // ничего нового (в том числе эхо своей записи)
                }
                st.store = target;
                if push {
                    st.version = 0;
                } else {
                    st.version = version;
                    st.doc = FoldersDoc::from_value(doc.as_ref());
                }
                folders::save(&st);
                push
            }
            None => {
                // Хранящего подключения больше нет: последний документ
                // остаётся локальной копией и дальше правится здесь.
                if st.store != Store::Local {
                    st.store = Store::Local;
                    st.version = 0;
                    folders::save(&st);
                }
                return;
            }
        }
    };
    sh.folders_error.borrow_mut().clear();
    if push_local {
        sh.folders_dirty.set(true);
        schedule_save(sh, Duration::from_millis(0));
    }
    reapply_view(ui, sh);
    refresh_folder_settings(ui, sh);
}

/// Хранящее подключение есть, но папки с него не прочитались (сервер старой
/// версии отвечает 404, сеть, 5xx). Это не «подключения нет»: хранилище —
/// сервер, правки копятся локально и уходят туда, когда он ответит.
pub(crate) fn on_unavailable(ui: &MainWindow, sh: &Shared, account: String, error: String) {
    {
        let mut st = sh.folders.borrow_mut();
        let target = Store::Server { account };
        if st.store != target {
            st.store = target;
            folders::save(&st);
        }
    }
    *sh.folders_error.borrow_mut() = error;
    refresh_folder_note(ui, sh);
}

/// Ответ на запись.
pub(crate) fn on_saved(
    ui: &MainWindow,
    sh: &Shared,
    version: i64,
    conflict: Option<Option<serde_json::Value>>,
) {
    sh.folders_inflight.set(false);
    sh.folders_error.borrow_mut().clear();
    match conflict {
        None => {
            let mut st = sh.folders.borrow_mut();
            st.version = version;
            folders::save(&st);
        }
        Some(doc) => {
            // Между чтением и записью папки поменяли на другой станции. Её
            // правку не затираем: берём серверный документ, свою отбрасываем
            // и говорим об этом.
            {
                let mut st = sh.folders.borrow_mut();
                st.version = version;
                st.doc = FoldersDoc::from_value(doc.as_ref());
                folders::save(&st);
            }
            sh.folders_dirty.set(false);
            reapply_view(ui, sh);
            refresh_folder_settings(ui, sh);
            toast_window::show(
                2, // amber
                0,
                "Папки изменены на другой станции",
                "Последняя правка не сохранена — повторите её.",
                false,
                600,
                || {},
                || {},
                || {},
            );
            return;
        }
    }
    if sh.folders_dirty.get() {
        schedule_save(sh, Duration::from_millis(0));
    }
}

/// Запись не дошла. Документ уже лежит локально; повторим позже.
pub(crate) fn on_save_failed(ui: &MainWindow, sh: &Shared, error: String) {
    sh.folders_inflight.set(false);
    sh.folders_dirty.set(true);
    *sh.folders_error.borrow_mut() = error;
    refresh_folder_note(ui, sh);
    schedule_save(sh, SAVE_RETRY);
}

/// Подпись под выключателем: где сейчас живут папки, если не на сервере.
fn store_note(st: &folders::Stored, error: &str) -> String {
    match &st.store {
        Store::Local => {
            "Нет подключения к серверу ddmail — папки хранятся только на этой станции.".into()
        }
        Store::Server { .. } if !error.is_empty() => {
            let error: String = error.chars().take(120).collect();
            format!(
                "Сервер не принял папки ({error}). Правки сохранены на этой станции \
                 и уйдут на сервер, когда он ответит."
            )
        }
        Store::Server { .. } => String::new(),
    }
}

/// Иконка папки — до четырёх символов, но не разрезая эмодзи: склейка через
/// ZWJ (👨‍👩‍👧), модификаторы (вариации, оттенок кожи, keycap 1️⃣, теги
/// флагов регионов) идут вместе со своим символом, флаг страны — пара
/// региональных букв (🇷🇺). Иначе от семьи оставался один папа и висячий ZWJ.
fn clip_icon(text: &str) -> String {
    const ZWJ: char = '\u{200D}';
    let joins = |c: char| {
        c == ZWJ
            || c == '\u{20E3}'
            || ('\u{FE00}'..='\u{FE0F}').contains(&c)
            || ('\u{1F3FB}'..='\u{1F3FF}').contains(&c)
            || ('\u{E0020}'..='\u{E007F}').contains(&c)
    };
    let regional = |c: char| ('\u{1F1E6}'..='\u{1F1FF}').contains(&c);
    let mut out = String::new();
    let mut units = 0;
    let mut prev_zwj = false;
    // Нечётная региональная буква ждёт пару — вторая её не начинает.
    let mut open_flag = false;
    for c in text.chars() {
        let second_of_flag = regional(c) && open_flag;
        if !joins(c) && !prev_zwj && !second_of_flag {
            if units == 4 {
                break;
            }
            units += 1;
        }
        out.push(c);
        prev_zwj = c == ZWJ;
        open_flag = regional(c) && !second_of_flag;
    }
    out
}

fn split_list(text: &str) -> Vec<String> {
    text.split([',', ';', ' ', '\n'])
        .map(|s| s.trim().to_lowercase())
        .filter(|s| !s.is_empty())
        .collect()
}

/// Только подпись о хранилище. Ошибки сохранения идут сюда, а не в
/// `refresh_folder_settings`: новая модель пересоздаёт все строки раздела, и
/// поле, в котором как раз печатают, теряло фокус — при лежащем сервере
/// каждые 30 с, на каждом повторе.
fn refresh_folder_note(ui: &MainWindow, sh: &Shared) {
    let st = sh.folders.borrow();
    ui.set_folders_store_note(store_note(&st, &sh.folders_error.borrow()).into());
}

/// Раздел настроек «Папки»: строка на папку.
pub(crate) fn refresh_folder_settings(ui: &MainWindow, sh: &Shared) {
    let st = sh.folders.borrow();
    let rows: Vec<FolderEdit> = st
        .doc
        .folders
        .iter()
        .map(|f| FolderEdit {
            id: f.id.as_str().into(),
            name: f.name.as_str().into(),
            icon: f.icon.as_str().into(),
            identities: f.rules.identities.join(", ").into(),
            domains: f.rules.domains.join(", ").into(),
            kind: match f.rules.kind {
                Kind::Any => 0,
                Kind::Direct => 1,
                Kind::Group => 2,
            },
            manual: f.include.len() as i32,
            hidden: f.hidden,
            auto_read: f.auto_read,
        })
        .collect();
    ui.set_folder_edits(ModelRc::new(VecModel::from(rows)));
    ui.set_folders_on(sh.folders_on.get());
    ui.set_folders_store_note(store_note(&st, &sh.folders_error.borrow()).into());
}

pub(crate) fn wire_folders(ui: &MainWindow, shared: &Rc<Shared>) {
    refresh_folder_settings(ui, shared);
    refresh_folder_tabs(shared, ui);

    let uiw = ui.as_weak();
    let sh = shared.clone();
    ui.on_folder_tab_clicked(move |id| {
        let Some(ui) = uiw.upgrade() else { return };
        // Повторный клик по выбранному табу снимает выбор — это инбокс.
        let id = if *sh.active_folder.borrow() == id.as_str() { "" } else { id.as_str() };
        switch_folder(&ui, &sh, id);
    });

    let uiw = ui.as_weak();
    let sh = shared.clone();
    ui.on_folder_tab_moved(move |from, to| {
        let Some(ui) = uiw.upgrade() else { return };
        // Номера — в полосе табов, где скрытых папок может не быть; в
        // документе переставляем по id.
        let tabs = ui.get_folder_tabs();
        let id_at =
            |i: i32| slint::Model::row_data(&tabs, i.max(0) as usize).map(|t| t.id.to_string());
        let (Some(from_id), Some(to_id)) = (id_at(from), id_at(to)) else { return };
        let (from, to) = {
            let doc = &sh.folders.borrow().doc;
            match (doc.index_of(&from_id), doc.index_of(&to_id)) {
                (Some(a), Some(b)) => (a, b),
                _ => return,
            }
        };
        if from == to {
            return;
        }
        commit(&ui, &sh, true, |doc| {
            let f = doc.folders.remove(from);
            doc.folders.insert(to, f);
        });
    });

    let uiw = ui.as_weak();
    let sh = shared.clone();
    ui.on_folders_toggle(move || {
        let Some(ui) = uiw.upgrade() else { return };
        sh.folders_on.set(!sh.folders_on.get());
        save_calendar_settings(&ui, &sh);
        reapply_view(&ui, &sh);
        refresh_folder_settings(&ui, &sh);
    });

    let uiw = ui.as_weak();
    let sh = shared.clone();
    ui.on_folder_add(move || {
        let Some(ui) = uiw.upgrade() else { return };
        commit(&ui, &sh, true, |doc| {
            let id = doc.new_id();
            let n = doc.folders.len() + 1;
            doc.folders.push(folders::Folder {
                id,
                name: format!("Папка {n}"),
                icon: "📁".into(),
                ..Default::default()
            });
        });
    });

    // Меню полосы табов: новая папка (и сразу в настройки — назвать её),
    // показать скрытые, «все прочитаны».
    let uiw = ui.as_weak();
    let sh = shared.clone();
    ui.on_folder_add_open(move || {
        let Some(ui) = uiw.upgrade() else { return };
        ui.invoke_folder_add();
        ui.set_settings_tab(4);
        ui.set_settings_visible(true);
        let _ = &sh;
    });
    let uiw = ui.as_weak();
    let sh = shared.clone();
    ui.on_toggle_show_hidden_folders(move || {
        let Some(ui) = uiw.upgrade() else { return };
        sh.show_hidden_folders.set(!sh.show_hidden_folders.get());
        refresh_folder_tabs(&sh, &ui);
    });
    let uiw = ui.as_weak();
    let sh = shared.clone();
    ui.on_folder_mark_read(move |id| {
        let Some(ui) = uiw.upgrade() else { return };
        mark_folder_read(&ui, &sh, &id);
    });

    let uiw = ui.as_weak();
    let sh = shared.clone();
    ui.on_folder_delete(move |id| {
        let Some(ui) = uiw.upgrade() else { return };
        commit(&ui, &sh, true, |doc| doc.folders.retain(|f| f.id != id.as_str()));
    });

    let uiw = ui.as_weak();
    let sh = shared.clone();
    ui.on_folder_edit(move |id, field, value| {
        let Some(ui) = uiw.upgrade() else { return };
        let Some(i) = sh.folders.borrow().doc.index_of(&id) else { return };
        let value = value.to_string();
        let structural = matches!(field.as_str(), "kind" | "clear-manual" | "hidden" | "auto-read");
        commit(&ui, &sh, structural, |doc| {
            let f = &mut doc.folders[i];
            match field.as_str() {
                "name" => f.name = value.trim().to_string(),
                "icon" => f.icon = clip_icon(value.trim()),
                "identities" => f.rules.identities = split_list(&value),
                "domains" => f.rules.domains = split_list(&value),
                "kind" => {
                    f.rules.kind = match value.as_str() {
                        "1" => Kind::Direct,
                        "2" => Kind::Group,
                        _ => Kind::Any,
                    }
                }
                "clear-manual" => {
                    f.include.clear();
                    f.exclude.clear();
                }
                "hidden" => f.hidden = value == "1",
                "auto-read" => f.auto_read = value == "1",
                _ => {}
            }
        });
    });

    // Контекстное меню строки: «В папку → …» / «Убрать из папки».
    let uiw = ui.as_weak();
    let sh = shared.clone();
    ui.on_conv_to_folder(move |idx, id| {
        let Some(ui) = uiw.upgrade() else { return };
        let real = ctx_conv_index(&ui, &sh, idx);
        let Some(c) = real.and_then(|i| sh.convs.borrow().get(i).cloned()) else { return };
        let target = sh.folders.borrow().doc.index_of(&id);
        commit(&ui, &sh, true, |doc| doc.assign(&c, &sh.key, target));
    });
}

#[cfg(test)]
mod tests {
    use super::clip_icon;

    #[test]
    fn icon_keeps_four_symbols_without_splitting_emoji() {
        assert_eq!(clip_icon("abcdef"), "abcd");
        // Семья через ZWJ — один символ, не режется посередине.
        let family = "👨\u{200D}👩\u{200D}👧";
        assert_eq!(clip_icon(&format!("{family}xyzw")), format!("{family}xyz"));
        // Оттенок кожи и вариация держатся за свой символ.
        assert_eq!(clip_icon("👍🏽❤\u{FE0F}ab c"), "👍🏽❤\u{FE0F}ab");
        // Флаг — пара региональных букв, keycap держится за цифру.
        assert_eq!(clip_icon("abc🇷🇺🇰🇿"), "abc🇷🇺");
        assert_eq!(clip_icon("1\u{FE0F}\u{20E3}xyzw"), "1\u{FE0F}\u{20E3}xyz");
    }
}

/// Папка (её индекс в документе) диалога из сырого списка.
fn folder_index(sh: &Shared, c: &Conversation) -> Option<usize> {
    sh.folders.borrow().doc.folder_of(c, &sh.key)
}

/// Пометить прочитанными сообщения `refs` диалогов на сервере — по учётке
/// каждого диалога.
fn push_seen(sh: &Shared, by_account: HashMap<String, Vec<MessageRef>>) {
    let Some(etx) = sh.engine_tx.borrow().clone() else { return };
    for (account_key, messages) in by_account {
        if messages.is_empty() {
            continue;
        }
        let _ = etx.send(engine::EngineCmd::SetFlags {
            messages,
            flags: "\\Seen".into(),
            add: true,
            account_key,
        });
    }
}

/// «Все прочитаны» из меню таба: всё непрочитанное в диалогах папки —
/// прочитано, локально сразу и на сервере.
fn mark_folder_read(ui: &MainWindow, sh: &Shared, id: &str) {
    let Some(target) = sh.folders.borrow().doc.index_of(id) else { return };
    let mut by_account: HashMap<String, Vec<MessageRef>> = HashMap::new();
    {
        let doc = sh.folders.borrow();
        let mut raw = sh.raw_convs.borrow_mut();
        for c in raw.iter_mut().filter(|c| c.unread_count > 0) {
            if doc.doc.folder_of(c, &sh.key) != Some(target) {
                continue;
            }
            let account = eff_account(&sh.key, c);
            for m in c.messages.iter_mut().filter(|m| !m.seen) {
                m.seen = true;
                by_account
                    .entry(account.clone())
                    .or_default()
                    .push(MessageRef { seen: false, ..m.clone() });
            }
            c.unread_count = 0;
        }
    }
    let n: usize = by_account.values().map(Vec::len).sum();
    push_seen(sh, by_account);
    println!("folder {id}: marked {n} messages read");
    reapply_view(ui, sh);
    tray_sync_dot(sh);
}

/// «Письма сразу прочитаны»: новые непрочитанные сообщения диалогов таких
/// папок — прочитаны. «Новые» — те, которых не было в прошлом списке
/// (`known`); на самом первом списке (`known` пуст) не трогаем ничего, иначе
/// включение флага гасило бы и давно лежавшее непрочитанное.
pub(crate) fn auto_read_new(sh: &Shared, known: &HashSet<(String, u32)>, raw: &mut [Conversation]) {
    if known.is_empty() {
        return;
    }
    let doc = sh.folders.borrow();
    if !doc.doc.folders.iter().any(|f| f.auto_read) {
        return;
    }
    let mut by_account: HashMap<String, Vec<MessageRef>> = HashMap::new();
    for c in raw.iter_mut().filter(|c| c.unread_count > 0) {
        let Some(fi) = doc.doc.folder_of(c, &sh.key) else { continue };
        if !doc.doc.folders[fi].auto_read {
            continue;
        }
        let account = eff_account(&sh.key, c);
        let mut marked = 0u32;
        for m in c.messages.iter_mut().filter(|m| !m.seen) {
            if known.contains(&(m.folder.clone(), m.uid)) {
                continue;
            }
            m.seen = true;
            marked += 1;
            by_account
                .entry(account.clone())
                .or_default()
                .push(MessageRef { seen: false, ..m.clone() });
        }
        c.unread_count = c.unread_count.saturating_sub(marked);
    }
    drop(doc);
    if !by_account.is_empty() {
        println!("auto-read: {} new messages", by_account.values().map(Vec::len).sum::<usize>());
        push_seen(sh, by_account);
    }
}

/// Что делать с новым письмом в диалоге `c` (или в ещё не известном диалоге
/// с собеседником `from_addr`): (сразу прочитать, папка скрытая).
pub(crate) fn new_mail_policy(sh: &Shared, from_addr: &str) -> (bool, bool) {
    let probe = sh
        .raw_convs
        .borrow()
        .iter()
        .find(|c| c.counterparts.iter().any(|cp| cp.addr.eq_ignore_ascii_case(from_addr)))
        .cloned()
        .or_else(|| {
            // Новый собеседник: диалога ещё нет, но правила по домену его
            // уже ловят.
            (!from_addr.is_empty()).then(|| Conversation {
                id: String::new(),
                label: String::new(),
                avatar_hash: String::new(),
                received_by: String::new(),
                counterparts: vec![ddmail_core::types::ContactInfo {
                    name: String::new(),
                    addr: from_addr.to_lowercase(),
                }],
                is_group: false,
                last_date: String::new(),
                last_date_ts: 0,
                last_subject: String::new(),
                unread_count: 0,
                total_count: 0,
                messages: Vec::new(),
                draft: None,
                account_key: String::new(),
                merged: false,
            })
        });
    let Some(c) = probe else { return (false, false) };
    let Some(fi) = folder_index(sh, &c) else { return (false, false) };
    let doc = sh.folders.borrow();
    let f = &doc.doc.folders[fi];
    (f.auto_read, f.hidden)
}
