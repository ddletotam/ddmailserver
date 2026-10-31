//! Айдентики в живом клиенте: сбор адресов со всех учёток, синхронизация
//! документа групп с сервером, цвета строк, выпадашка «От кого», учётка
//! для отправки и вкладка настроек «Айдентики» (контракт §1в).

use super::*;
use crate::folders::Store;
use crate::identities::{self, IdentityDoc, Source};

const SAVE_DEBOUNCE: Duration = Duration::from_millis(800);
const SAVE_RETRY: Duration = Duration::from_secs(30);

/// Состояние айдентик на UI-потоке.
pub(crate) struct IdentState {
    pub(crate) stored: RefCell<identities::Stored>,
    /// email → ключи учёток, которые этим адресом владеют (порядок
    /// accounts.json). Считается заново при каждом сборе, не хранится:
    /// адрес удалённой учётки остаётся в группах, но владельца у него нет.
    pub(crate) owners: RefCell<HashMap<String, Vec<String>>>,
    /// Правка не ушла на сервер.
    pub(crate) dirty: Cell<bool>,
    /// Среди неотправленного есть ручная правка (не только новые адреса из
    /// учёток). Только её нельзя молча заменить серверной версией.
    pub(crate) manual: Cell<bool>,
    /// В записи, что сейчас в пути, была ручная правка.
    pub(crate) inflight_manual: Cell<bool>,
    /// Запись на сервер в пути.
    pub(crate) inflight: Cell<bool>,
    pub(crate) error: RefCell<String>,
    pub(crate) save_timer: slint::Timer,
    /// Группа, чья подпись сейчас редактируется в композере.
    pub(crate) sig_edit: RefCell<Option<String>>,
}

impl IdentState {
    pub(crate) fn new(stored: identities::Stored, owners: HashMap<String, Vec<String>>) -> Self {
        IdentState {
            stored: RefCell::new(stored),
            owners: RefCell::new(owners),
            dirty: Cell::new(false),
            manual: Cell::new(false),
            inflight_manual: Cell::new(false),
            inflight: Cell::new(false),
            error: RefCell::new(String::new()),
            save_timer: slint::Timer::default(),
            sig_edit: RefCell::new(None),
        }
    }
}

/// Адреса из всех учёток: айдентики, которые учётка отдала (кэш), её
/// email и логин, если он похож на email. Порядок — accounts.json, внутри
/// учётки — как отдал сервер (основная айдентика первой).
pub(crate) fn collect_sources(
    cache: Option<&Cache>,
) -> (Vec<Source>, HashMap<String, Vec<String>>) {
    let mut sources = Vec::new();
    let mut owners: HashMap<String, Vec<String>> = HashMap::new();
    let own = |email: &str, key: &str, owners: &mut HashMap<String, Vec<String>>| {
        let e = identities::normalize(email);
        let list = owners.entry(e).or_default();
        if !list.iter().any(|k| k == key) {
            list.push(key.to_string());
        }
    };
    let accounts = crate::account_store::config_dir()
        .map(|d| crate::account_store::addresses_in(&d))
        .unwrap_or_default();
    for (key, email, username) in accounts {
        if let Some(cache) = cache {
            for id in cache.load_identities(&key).unwrap_or_default() {
                own(&id.email, &key, &mut owners);
                sources.push(Source { email: id.email, name: id.name, color: id.color });
            }
        }
        for e in [&email, &username] {
            if identities::looks_like_email(e) {
                own(e, &key, &mut owners);
                sources.push(Source { email: e.clone(), ..Default::default() });
            }
        }
    }
    (sources, owners)
}

/// Учётка, через которую уходит письмо с адреса `from`: учётка открытой
/// беседы, если адрес её; иначе первая владеющая. `None` — адрес ничей
/// (учётку удалили), отправлять не через что.
pub(crate) fn owner_for(sh: &Shared, from: &str, prefer: &str) -> Option<String> {
    let owners = sh.idents.owners.borrow();
    let lc = identities::normalize(from);
    let list =
        owners.get(&lc).or_else(|| identities::base_of_tagged(&lc).and_then(|b| owners.get(&b)))?;
    if list.iter().any(|k| k == prefer) { Some(prefer.to_string()) } else { list.first().cloned() }
}

/// Собрать адреса со всех учёток, добавить новые в документ и обновить всё,
/// что от него зависит. Зовётся на каждый список бесед: айдентики учёток
/// приезжают вместе с ним.
pub(crate) fn rebuild(ui: &MainWindow, sh: &Shared) {
    let (sources, owners) = collect_sources(sh.cache.as_deref());
    *sh.idents.owners.borrow_mut() = owners;
    let changed = {
        let mut st = sh.idents.stored.borrow_mut();
        let changed = st.doc.merge_sources(&sources);
        if changed {
            identities::save(&st);
        }
        changed
    };
    if changed {
        sh.idents.dirty.set(true);
        schedule_save(sh, SAVE_DEBOUNCE);
        refresh_settings(ui, sh);
    }
    apply(ui, sh);
}

/// Документ поменялся — цвета, выпадашка.
fn apply(ui: &MainWindow, sh: &Shared) {
    *sh.identity_colors.borrow_mut() = sh.idents.stored.borrow().doc.color_map();
    refresh_composer_identities(ui, sh);
}

/// Цвета поменялись — перекрасить список и шапку.
fn repaint(ui: &MainWindow, sh: &Shared) {
    let displays = displays_from(&sh.convs.borrow(), &sh.identity_colors.borrow());
    *sh.displays.borrow_mut() = displays;
    refresh_sidebar(sh, ui);
    let cur = sh.current.get();
    if cur < sh.convs.borrow().len() && sh.compose.pending_compose.borrow().is_none() {
        apply_active_header(ui, sh, cur);
    }
}

/// Ручная правка: применить, записать, отправить на сервер с дебаунсом.
pub(crate) fn commit(
    ui: &MainWindow,
    sh: &Shared,
    refresh_ui: bool,
    mutate: impl FnOnce(&mut IdentityDoc) -> bool,
) {
    let changed = {
        let mut st = sh.idents.stored.borrow_mut();
        let changed = mutate(&mut st.doc);
        if changed {
            identities::save(&st);
        }
        changed
    };
    if !changed {
        return;
    }
    sh.idents.dirty.set(true);
    sh.idents.manual.set(true);
    apply(ui, sh);
    repaint(ui, sh);
    if refresh_ui {
        refresh_settings(ui, sh);
    }
    schedule_save(sh, SAVE_DEBOUNCE);
}

fn schedule_save(sh: &Shared, after: Duration) {
    sh.idents.save_timer.start(slint::TimerMode::SingleShot, after, || {
        SHARED.with(|s| {
            if let Some(sh) = s.borrow().as_ref() {
                flush_save(sh);
            }
        });
    });
}

fn flush_save(sh: &Shared) {
    if !sh.idents.dirty.get() || sh.idents.inflight.get() {
        return;
    }
    let st = sh.idents.stored.borrow();
    let Store::Server { account } = &st.store else {
        sh.idents.dirty.set(false);
        sh.idents.manual.set(false);
        return;
    };
    let Some(etx) = sh.engine_tx.borrow().clone() else { return };
    sh.idents.dirty.set(false);
    sh.idents.inflight.set(true);
    sh.idents.inflight_manual.set(sh.idents.manual.replace(false));
    let _ = etx.send(engine::EngineCmd::SaveIdentityGroups {
        account_key: account.clone(),
        doc: st.doc.to_value(),
        base_version: st.version,
    });
}

/// Ответ на `FetchIdentityGroups`. Серверный документ берётся целиком, а
/// адреса, которых на сервере нет (учётки этой станции), добавляются к нему
/// и уходят на сервер.
pub(crate) fn on_fetched(
    ui: &MainWindow,
    sh: &Shared,
    store: Option<String>,
    version: i64,
    doc: Option<serde_json::Value>,
) {
    // Своя неотправленная ручная правка важнее: запись либо пройдёт, либо
    // получит конфликт и тогда сольётся с серверной версией. Новые адреса из
    // учёток — не правка: они сливаются с серверным документом прямо здесь.
    if sh.idents.manual.get() || sh.idents.inflight.get() {
        return;
    }
    let push = {
        let mut st = sh.idents.stored.borrow_mut();
        match store {
            Some(account) => {
                let target = Store::Server { account };
                if st.store == target
                    && st.version == version
                    && doc.is_some()
                    && !sh.idents.dirty.get()
                {
                    return; // ничего нового, в том числе эхо своей записи
                }
                let push = match doc {
                    None => !st.doc.groups.is_empty(),
                    Some(v) => {
                        let mut theirs = IdentityDoc::from_value(Some(&v));
                        let added = IdentityDoc::union_into(&mut theirs, &st.doc);
                        st.doc = theirs;
                        added
                    }
                };
                st.store = target;
                st.version = version;
                identities::save(&st);
                push
            }
            None => {
                if st.store != Store::Local {
                    st.store = Store::Local;
                    st.version = 0;
                    identities::save(&st);
                }
                false
            }
        }
    };
    sh.idents.error.borrow_mut().clear();
    if push {
        sh.idents.dirty.set(true);
        schedule_save(sh, Duration::from_millis(0));
    } else {
        sh.idents.dirty.set(false);
    }
    apply(ui, sh);
    repaint(ui, sh);
    refresh_settings(ui, sh);
}

/// Хранящее подключение не отдало группы. Сервер старой версии (нет
/// эндпоинта — 404) группы не поддерживает: они живут на станции. Прочие
/// ошибки — сеть: хранилище по-прежнему сервер, правки уйдут позже.
pub(crate) fn on_unavailable(ui: &MainWindow, sh: &Shared, account: String, error: String) {
    let unsupported = error.contains("404");
    {
        let mut st = sh.idents.stored.borrow_mut();
        let target = if unsupported { Store::Local } else { Store::Server { account } };
        if st.store != target {
            st.store = target;
            if unsupported {
                st.version = 0;
            }
            identities::save(&st);
        }
    }
    *sh.idents.error.borrow_mut() = if unsupported { String::new() } else { error };
    refresh_settings(ui, sh);
}

pub(crate) fn on_saved(
    ui: &MainWindow,
    sh: &Shared,
    version: i64,
    conflict: Option<Option<serde_json::Value>>,
) {
    sh.idents.inflight.set(false);
    sh.idents.error.borrow_mut().clear();
    match conflict {
        None => {
            let mut st = sh.idents.stored.borrow_mut();
            st.version = version;
            identities::save(&st);
        }
        Some(doc) => {
            // Группы поменяли на другой станции. Её правку не затираем:
            // берём серверный документ и добавляем к нему свои новые адреса.
            let push = {
                let mut st = sh.idents.stored.borrow_mut();
                let mut theirs = IdentityDoc::from_value(doc.as_ref());
                let added = IdentityDoc::union_into(&mut theirs, &st.doc);
                st.doc = theirs;
                st.version = version;
                identities::save(&st);
                added
            };
            sh.idents.dirty.set(push);
            apply(ui, sh);
            repaint(ui, sh);
            refresh_settings(ui, sh);
            if sh.idents.inflight_manual.get() {
                toast_window::show(
                    2,
                    0,
                    "Айдентики изменены на другой станции",
                    "Последняя правка не сохранена — повторите её.",
                    false,
                    600,
                    || {},
                    || {},
                    || {},
                );
            }
            if push {
                schedule_save(sh, Duration::from_millis(0));
            }
        }
    }
}

pub(crate) fn on_save_failed(ui: &MainWindow, sh: &Shared, error: String) {
    sh.idents.inflight.set(false);
    sh.idents.dirty.set(true);
    if sh.idents.inflight_manual.get() {
        sh.idents.manual.set(true);
    }
    *sh.idents.error.borrow_mut() = error;
    refresh_settings(ui, sh);
    schedule_save(sh, SAVE_RETRY);
}

// ---- Выпадашка «От кого» ----

/// Выставить выбор в выпадашке и пересчитать, сколько строк в ней видно:
/// скрытый адрес показывается, только пока он выбран.
pub(crate) fn set_picker_index(ui: &MainWindow, sh: &Shared, index: i32) {
    ui.set_composer_identity_index(index);
    sync_picker_rows(ui, sh);
}

pub(crate) fn sync_picker_rows(ui: &MainWindow, sh: &Shared) {
    let index = ui.get_composer_identity_index();
    let context = send_context(sh);
    let st = sh.idents.stored.borrow();
    let emails = sh.compose.composer_identities.borrow();
    let rows = emails
        .iter()
        .enumerate()
        .filter(|(i, e)| *i as i32 == index || st.doc.offered(e, context.as_deref()))
        .count();
    ui.set_identity_menu_rows(rows as i32);
}

/// Контекст отправки для строгих групп (§1в): адрес, на который пришло
/// письмо открытой беседы (ответ и пересылка — оба отсюда); `None` — новое
/// письмо. Пустой `received_by` — «не мой адрес», строгим группам нельзя.
pub(crate) fn send_context(sh: &Shared) -> Option<String> {
    if sh.compose.pending_compose.borrow().is_some() {
        return None;
    }
    sh.convs.borrow().get(sh.current.get()).map(|c| c.received_by.to_lowercase())
}

/// Сменился контекст (открыли беседу, начали новое письмо): пересобрать
/// выпадашку под него и выбрать отправителя по правилу §1в. Подходящего
/// нет (все группы строгие и письмо не их) — ничего не выбрано, отправка
/// ждёт ручного выбора.
pub(crate) fn aim_for_context(ui: &MainWindow, sh: &Shared) {
    refresh_composer_identities(ui, sh);
    let context = send_context(sh);
    let target = {
        let doc = &sh.idents.stored.borrow().doc;
        match &context {
            None => doc.new_mail_sender(),
            Some(r) => doc.reply_sender(r),
        }
    };
    match target {
        Some(t) => aim_composer_identity(ui, sh, &t),
        None => set_picker_index(ui, sh, -1),
    }
}

// ---- Вкладка настроек ----

/// Перечитать вкладку «Айдентики». Не трогает поля, в которых сейчас печатают:
/// модель меняется, только если строки другие.
pub(crate) fn refresh_settings(ui: &MainWindow, sh: &Shared) {
    use slint::Model as _;
    let st = sh.idents.stored.borrow();
    let doc = &st.doc;
    let owners = sh.idents.owners.borrow();
    let mut rows: Vec<IdentRow> = Vec::new();
    let default_id = doc.default_group().map(|g| g.id.clone()).unwrap_or_default();
    for (gi, g) in doc.groups.iter().enumerate() {
        let color = identities::group_color(g, gi);
        rows.push(IdentRow {
            kind: 0,
            group: g.id.clone().into(),
            name: g.name.clone().into(),
            email: "".into(),
            color: parse_hex_color(&color),
            dot: parse_hex_color(&identities::vivid(&color)),
            is_default: g.id == default_id,
            is_sender: false,
            hidden: false,
            has_signature: g.has_signature(),
            strict: g.strict,
            orphan: false,
            first: gi == 0,
            last: gi + 1 == doc.groups.len(),
            count: g.addresses.len() as i32,
        });
        let sender = g.sender();
        for (ai, a) in g.addresses.iter().enumerate() {
            rows.push(IdentRow {
                kind: 1,
                group: g.id.clone().into(),
                name: g.name.clone().into(),
                email: a.email.clone().into(),
                color: parse_hex_color(&color),
                dot: parse_hex_color(&identities::vivid(&color)),
                is_default: false,
                is_sender: a.email == sender,
                hidden: a.hidden,
                has_signature: false,
                strict: g.strict,
                orphan: !owners.contains_key(&a.email),
                first: ai == 0,
                last: ai + 1 == g.addresses.len(),
                count: g.addresses.len() as i32,
            });
        }
    }
    let current = ui.get_ident_rows();
    let same = current.row_count() == rows.len()
        && rows.iter().enumerate().all(|(i, r)| current.row_data(i).as_ref() == Some(r));
    if !same {
        ui.set_ident_rows(ModelRc::new(VecModel::from(rows)));
    }
    let groups: Vec<slint::SharedString> =
        doc.groups.iter().map(|g| group_title(g).into()).collect();
    let ids: Vec<slint::SharedString> = doc.groups.iter().map(|g| g.id.clone().into()).collect();
    let cur_names = ui.get_ident_group_names();
    if cur_names.row_count() != groups.len()
        || groups.iter().enumerate().any(|(i, g)| cur_names.row_data(i).as_ref() != Some(g))
    {
        ui.set_ident_group_names(ModelRc::new(VecModel::from(groups)));
    }
    let cur_ids = ui.get_ident_group_ids();
    if cur_ids.row_count() != ids.len()
        || ids.iter().enumerate().any(|(i, g)| cur_ids.row_data(i).as_ref() != Some(g))
    {
        ui.set_ident_group_ids(ModelRc::new(VecModel::from(ids)));
    }
    ui.set_ident_store_note(store_note(&st, &sh.idents.error.borrow()).into());
}

fn group_title(g: &identities::Group) -> String {
    if g.name.trim().is_empty() {
        g.sender().to_string()
    } else {
        format!("{} · {}", g.name.trim(), g.sender())
    }
}

fn store_note(st: &identities::Stored, error: &str) -> String {
    match &st.store {
        Store::Local => "Сервер ddmail не подключён или не хранит айдентики — настройка живёт только на этой станции.".into(),
        Store::Server { .. } if !error.is_empty() => {
            let error: String = error.chars().take(120).collect();
            format!("Сервер не принял правку ({error}). Она сохранена здесь и уйдёт, когда он ответит.")
        }
        Store::Server { .. } => String::new(),
    }
}

pub(crate) fn wire(ui: &MainWindow, shared: &Rc<Shared>) {
    let w = ui.as_weak();
    let sh = shared.clone();
    ui.on_ident_rename(move |group, name| {
        let Some(ui) = w.upgrade() else { return };
        // Из текстового поля — без пересборки вкладки, иначе фокус слетит.
        commit(&ui, &sh, false, |d| {
            let Some(g) = d.group_mut(&group) else { return false };
            let name = name.to_string();
            if g.name == name {
                return false;
            }
            g.name = name;
            true
        });
    });
    let w = ui.as_weak();
    let sh = shared.clone();
    ui.on_ident_color(move |group, index| {
        let Some(ui) = w.upgrade() else { return };
        let Some(color) = IDENT_PASTEL.get(index.max(0) as usize) else { return };
        commit(&ui, &sh, true, |d| {
            let Some(g) = d.group_mut(&group) else { return false };
            if g.color.eq_ignore_ascii_case(color) {
                return false;
            }
            g.color = color.to_string();
            true
        });
    });
    let w = ui.as_weak();
    let sh = shared.clone();
    ui.on_ident_set_default(move |group| {
        let Some(ui) = w.upgrade() else { return };
        commit(&ui, &sh, true, |d| {
            if d.default_group == group.as_str() {
                return false;
            }
            d.default_group = group.to_string();
            true
        });
    });
    let w = ui.as_weak();
    let sh = shared.clone();
    ui.on_ident_toggle_strict(move |group| {
        let Some(ui) = w.upgrade() else { return };
        commit(&ui, &sh, true, |d| {
            let Some(g) = d.group_mut(&group) else { return false };
            g.strict = !g.strict;
            true
        });
    });
    let w = ui.as_weak();
    let sh = shared.clone();
    ui.on_ident_set_sender(move |email| {
        let Some(ui) = w.upgrade() else { return };
        commit(&ui, &sh, true, |d| {
            let Some((gi, _)) = d.locate(&email) else { return false };
            d.groups[gi].sender = email.to_string();
            true
        });
    });
    let w = ui.as_weak();
    let sh = shared.clone();
    ui.on_ident_toggle_hidden(move |email| {
        let Some(ui) = w.upgrade() else { return };
        commit(&ui, &sh, true, |d| {
            let Some(a) = d.address_mut(&email) else { return false };
            a.hidden = !a.hidden;
            true
        });
    });
    let w = ui.as_weak();
    let sh = shared.clone();
    ui.on_ident_shift(move |email, delta| {
        let Some(ui) = w.upgrade() else { return };
        commit(&ui, &sh, true, |d| d.shift_address(&email, delta));
    });
    let w = ui.as_weak();
    let sh = shared.clone();
    ui.on_ident_shift_group(move |group, delta| {
        let Some(ui) = w.upgrade() else { return };
        commit(&ui, &sh, true, |d| d.shift_group(&group, delta));
    });
    let w = ui.as_weak();
    let sh = shared.clone();
    ui.on_ident_move(move |email, target| {
        let Some(ui) = w.upgrade() else { return };
        let target = (!target.is_empty()).then(|| target.to_string());
        commit(&ui, &sh, true, |d| d.move_address(&email, target.as_deref()));
    });
    let w = ui.as_weak();
    let sh = shared.clone();
    ui.on_ident_edit_signature(move |group| {
        let Some(ui) = w.upgrade() else { return };
        begin_signature_edit(&ui, &sh, &group);
    });
    let w = ui.as_weak();
    let sh = shared.clone();
    ui.on_signature_save(move || {
        let Some(ui) = w.upgrade() else { return };
        finish_signature_edit(&ui, &sh, true);
    });
    let w = ui.as_weak();
    let sh = shared.clone();
    ui.on_signature_cancel(move || {
        let Some(ui) = w.upgrade() else { return };
        finish_signature_edit(&ui, &sh, false);
    });
}

// ---- Подпись: правка в композере ----

/// Открыть подпись группы в композере. Набранное письмо откладывается и
/// возвращается после «Сохранить»/«Отмена».
fn begin_signature_edit(ui: &MainWindow, sh: &Shared, group: &str) {
    let (sig, title) = {
        let st = sh.idents.stored.borrow();
        let Some(g) = st.doc.group_by_id(group) else { return };
        (g.signature.clone(), group_title(g))
    };
    if sh.idents.sig_edit.borrow().is_none() {
        let draft = std::mem::take(&mut *sh.compose.rich.borrow_mut());
        *sh.compose.sig_parked.borrow_mut() = Some(draft);
    }
    *sh.idents.sig_edit.borrow_mut() = Some(group.to_string());
    *sh.compose.rich.borrow_mut() =
        richtext::Editor::from_doc(identities::signature_to_doc(&sig, &format!("sig-{group}-")));
    ui.set_signature_edit_title(title.into());
    ui.set_signature_editing(true);
    ui.set_settings_visible(false);
    ui.set_view_mode(0);
    rich_refresh(ui, sh);
    ui.invoke_focus_composer();
}

fn finish_signature_edit(ui: &MainWindow, sh: &Shared, save: bool) {
    let Some(group) = sh.idents.sig_edit.borrow_mut().take() else { return };
    let parked = sh.compose.sig_parked.borrow_mut().take().unwrap_or_default();
    let edited = std::mem::replace(&mut *sh.compose.rich.borrow_mut(), parked);
    ui.set_signature_editing(false);
    rich_refresh(ui, sh);
    if save {
        let sig = identities::signature_from_doc(edited.doc());
        commit(ui, sh, true, |d| {
            let Some(g) = d.group_mut(&group) else { return false };
            if g.signature == sig {
                return false;
            }
            g.signature = sig;
            true
        });
    }
    // Обратно в настройки, на ту же вкладку.
    ui.set_settings_visible(true);
}

/// Подпись группы адреса `from` для письма: (html, text, картинки).
/// `None` — у группы подписи нет.
pub(crate) fn signature_for(
    sh: &Shared,
    from: &str,
) -> Option<(String, String, Vec<ddmail_core::types::OutgoingAttachment>)> {
    let st = sh.idents.stored.borrow();
    let g = st.doc.group_of(from)?;
    if !g.has_signature() {
        return None;
    }
    let seq = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_millis())
        .unwrap_or(0);
    let ed = richtext::Editor::from_doc(identities::signature_to_doc(
        &g.signature,
        &format!("sig{seq}-"),
    ));
    let atts = ed
        .images()
        .iter()
        .map(|img| ddmail_core::types::OutgoingAttachment {
            filename: format!("{}.png", img.cid.split('@').next().unwrap_or("signature")),
            mime_type: img.mime.clone(),
            content: img.bytes.as_ref().clone(),
            content_id: Some(img.cid.clone()),
        })
        .collect();
    // Своя обёртка: стили письма подписи не наследуют разметку тела, а
    // разделитель «-- » — общепринятый маркер подписи в text/plain.
    let html = format!("<div><br></div><div class=\"ddmail-signature\">{}</div>", ed.html());
    let text = format!("\n\n-- \n{}", ed.plain_text());
    Some((html, text, atts))
}
