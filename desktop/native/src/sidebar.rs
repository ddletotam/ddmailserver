//! The conversation list: display rows and identity colours, merged
//! conversations (merges.json), the sidebar model with its flash and scroll
//! helpers (contract §4).

use super::*;

pub(crate) const NAMES: [&str; 25] = [
    "Анна Соколова",
    "Команда AppSec",
    "Дмитрий П.",
    "Поддержка letotam",
    "Ольга Кузнецова",
    "DevSecOps канал",
    "Игорь Лебедев",
    "Мария В.",
    "Никита Орлов",
    "Рассылки",
    "Светлана Г.",
    "Павел Морозов",
    "QA дайджест",
    "Елена Фомина",
    "Артём Зайцев",
    "Релизы 4.x",
    "Юлия Беляева",
    "Сергей Котов",
    "HR отдел",
    "Григорий Н.",
    "Вера Полякова",
    "Алексей Тимофеев",
    "Финансы",
    "Дарья Жукова",
    "Roadmap",
];

pub(crate) const PALETTE: [&str; 6] =
    ["#2f80ed", "#27ae60", "#eb5757", "#9b51e0", "#f2994a", "#11998e"];

pub(crate) fn initials(name: &str) -> String {
    name.split_whitespace()
        .filter_map(|w| w.chars().next())
        .take(2)
        .collect::<String>()
        .to_uppercase()
}

#[derive(Clone)]
pub(crate) struct Disp {
    pub(crate) name: String,
    pub(crate) initials: String,
    pub(crate) color: String,
    pub(crate) preview: String,
    pub(crate) email: String,
    /// Sidebar row tint — colour of the identity that received the
    /// conversation (см. identity_color_map). Empty = no tint.
    pub(crate) ident_color: String,
    /// Unread badge value (0 = no badge).
    pub(crate) unread: u32,
    /// Строка — пользовательская склейка нескольких диалогов (merges.json).
    pub(crate) merged: bool,
}

/// Pastel palette for identities lacking a server-side colour. Used as the
/// SIDEBAR ROW TINT — a soft wash behind the conversation row, so it must
/// stay light. The first 15 mirror the old Tauri identityStore +
/// imap.rs::fetch_identities_impl (so existing rows don't change colour);
/// the rest extend the wheel for users with many aliases.
/// `IDENT_VIVID` is the hue-aligned saturated counterpart — same index, same
/// hue, used only for the from-picker dot (see `refresh_composer_identities`).
pub(crate) const IDENT_PASTEL: [&str; 24] = [
    "#FFE4E1", "#E8F5E9", "#E3F2FD", "#FFF9C4", "#F3E5F5", "#E0F7FA", "#FBE9E7", "#F1F8E9",
    "#EDE7F6", "#E8EAF6", "#FCE4EC", "#E0F2F1", "#FFF3E0", "#F9FBE7", "#EFEBE9", "#ECEFF1",
    "#FFF8E1", "#E1F5FE", "#DCEDC8", "#FFCDD2", "#F8BBD0", "#D1C4E9", "#B2DFDB", "#B3E5FC",
];

/// Saturated counterpart to `IDENT_PASTEL`, index-aligned by hue. Used ONLY
/// for the from-picker dot: at 12px a pastel dot is invisible, so the sender
/// selector shows the intense version while the sidebar keeps the wash.
pub(crate) const IDENT_VIVID: [&str; 24] = [
    "#E53935", "#43A047", "#1E88E5", "#FDD835", "#8E24AA", "#00ACC1", "#F4511E", "#7CB342",
    "#5E35B1", "#3949AB", "#D81B60", "#00897B", "#FB8C00", "#C0CA33", "#6D4C41", "#546E7A",
    "#FFB300", "#039BE5", "#558B2F", "#C62828", "#AD1457", "#6A1B9A", "#00695C", "#0277BD",
];

/// «Ugly gray» for conversations received by an unknown alias.
pub(crate) const IDENT_UNKNOWN: &str = "#d5d5d0";

/// email(lowercase) → row tint for every known identity.
pub(crate) fn identity_color_map(cache: &Cache, key: &str) -> HashMap<String, String> {
    cache
        .load_identities(key)
        .unwrap_or_default()
        .into_iter()
        .enumerate()
        .map(|(i, id)| {
            let color = if id.color.trim().is_empty() {
                IDENT_PASTEL[i % IDENT_PASTEL.len()].to_string()
            } else {
                id.color
            };
            (id.email.to_lowercase(), color)
        })
        .collect()
}

pub(crate) fn conv_name(c: &Conversation) -> String {
    if !c.label.is_empty() {
        c.label.clone()
    } else {
        c.counterparts
            .first()
            .map(|cp| if cp.name.is_empty() { cp.addr.clone() } else { cp.name.clone() })
            .unwrap_or_default()
    }
}

pub(crate) fn displays_from(
    convs: &[Conversation],
    ident_colors: &HashMap<String, String>,
) -> Vec<Disp> {
    convs
        .iter()
        .enumerate()
        .map(|(i, c)| {
            let name = conv_name(c);
            let ident_color = ident_colors
                .get(&c.received_by.to_lowercase())
                .cloned()
                .unwrap_or_else(|| IDENT_UNKNOWN.to_string());
            Disp {
                initials: initials(&name),
                name,
                color: PALETTE[i % PALETTE.len()].to_string(),
                preview: if c.last_subject.is_empty() {
                    "(без темы)".to_string()
                } else {
                    c.last_subject.clone()
                },
                email: c.counterparts.first().map(|cp| cp.addr.clone()).unwrap_or_default(),
                ident_color,
                unread: c.unread_count,
                merged: c.merged,
            }
        })
        .collect()
}

/// Effective account key диалога: загрузка из кэша на старте оставляет
/// account_key пустым — это первичный аккаунт.
pub(crate) fn eff_account(fallback: &str, c: &Conversation) -> String {
    if c.account_key.is_empty() { fallback.to_string() } else { c.account_key.clone() }
}

pub(crate) fn conv_merge_key(fallback: &str, c: &Conversation) -> merges::MergeKey {
    merges::MergeKey { account: eff_account(fallback, c), id: c.id.clone() }
}

/// Свернуть сырой список бесед по merges.json: члены группы склеиваются в
/// один синтетический диалог на позиции самого свежего из них. Имя, аватар
/// и identity — от первичного (головы группы); дата/тема-превью — от самого
/// свежего; счётчики суммируются, refs сообщений конкатенируются (тела
/// потом сортируются по date_ts в load_message_bodies). Группа, от которой
/// в списке остался один диалог, рендерится как есть.
///
/// Поверх склеек ложатся пользовательские имена (`Merges::names`): они
/// меняют только `label` в этом виде — сырой список, кэш и письма их не
/// видят (контракт §4, «Переименование диалога»).
pub(crate) fn apply_merges(
    raw: &[Conversation],
    m: &merges::Merges,
    fallback: &str,
) -> Vec<Conversation> {
    let mut out = merge_groups(raw, m, fallback);
    if !m.names.is_empty() {
        for c in &mut out {
            if let Some(name) = m.name_of(&conv_merge_key(fallback, c)) {
                c.label = name.to_string();
            }
        }
    }
    out
}

pub(crate) fn merge_groups(
    raw: &[Conversation],
    m: &merges::Merges,
    fallback: &str,
) -> Vec<Conversation> {
    if m.groups.is_empty() {
        return raw.to_vec();
    }
    let mut group_of: HashMap<(String, String), usize> = HashMap::new();
    for (gi, g) in m.groups.iter().enumerate() {
        for k in g {
            group_of.insert((k.account.clone(), k.id.clone()), gi);
        }
    }
    enum Slot {
        Single(usize), // index into raw
        Group(usize),  // index into m.groups, placed at first member seen
    }
    let mut slots: Vec<Slot> = Vec::new();
    let mut members: HashMap<usize, Vec<usize>> = HashMap::new();
    for (i, c) in raw.iter().enumerate() {
        match group_of.get(&(eff_account(fallback, c), c.id.clone())) {
            Some(&gi) => {
                let v = members.entry(gi).or_default();
                if v.is_empty() {
                    slots.push(Slot::Group(gi));
                }
                v.push(i);
            }
            None => slots.push(Slot::Single(i)),
        }
    }
    let mut out = Vec::with_capacity(slots.len());
    for s in slots {
        match s {
            Slot::Single(i) => out.push(raw[i].clone()),
            Slot::Group(gi) => {
                let idxs = &members[&gi];
                if idxs.len() == 1 {
                    out.push(raw[idxs[0]].clone());
                    continue;
                }
                let head = m.groups[gi].first();
                let primary = idxs
                    .iter()
                    .find(|&&i| {
                        head.is_some_and(|h| {
                            h.id == raw[i].id && h.account == eff_account(fallback, &raw[i])
                        })
                    })
                    .copied()
                    .unwrap_or(idxs[0]);
                let newest =
                    idxs.iter().max_by_key(|&&i| raw[i].last_date_ts).copied().unwrap_or(idxs[0]);
                let mut combined = raw[primary].clone();
                combined.merged = true;
                combined.last_date = raw[newest].last_date.clone();
                combined.last_date_ts = raw[newest].last_date_ts;
                combined.last_subject = raw[newest].last_subject.clone();
                combined.unread_count = idxs.iter().map(|&i| raw[i].unread_count).sum();
                combined.total_count = idxs.iter().map(|&i| raw[i].total_count).sum();
                combined.is_group = idxs.iter().any(|&i| raw[i].is_group);
                combined.messages = Vec::new();
                combined.counterparts = Vec::new();
                combined.draft = None;
                let mut seen_addr: HashSet<String> = HashSet::new();
                for &i in idxs {
                    combined.messages.extend(raw[i].messages.iter().cloned());
                    for cp in &raw[i].counterparts {
                        if seen_addr.insert(cp.addr.to_lowercase()) {
                            combined.counterparts.push(cp.clone());
                        }
                    }
                    if combined.draft.is_none() {
                        combined.draft = raw[i].draft.clone();
                    }
                }
                out.push(combined);
            }
        }
    }
    out
}

pub(crate) fn synthetic_displays() -> Vec<Disp> {
    (0..25)
        .map(|i| Disp {
            name: NAMES[i].to_string(),
            initials: initials(NAMES[i]),
            color: PALETTE[i % PALETTE.len()].to_string(),
            preview: "Последнее сообщение в диалоге…".to_string(),
            email: String::new(),
            ident_color: String::new(),
            unread: 0,
            merged: false,
        })
        .collect()
}

/// Optimistic local removal of the conversation at `cur` (delete + spam-purge
/// share it): drop the cache row, rebuild the sidebar, and select a neighbour.
/// The engine resets the full-sync stamp on success, so the follow-up refetch
/// reconciles with the server.
pub(crate) fn optimistic_remove_conversation(
    ui: &MainWindow,
    sh: &Shared,
    cur: usize,
    conv_id: &str,
) {
    if let Some(cache) = &sh.cache {
        cache.delete_conversation(&sh.key, conv_id).ok();
    }
    {
        let mut convs = sh.convs.borrow_mut();
        if cur < convs.len() {
            convs.remove(cur);
        }
        let displays = displays_from(&convs, &sh.identity_colors.borrow());
        let items = sidebar_items(&displays, &sh.avatars.borrow());
        ui.set_conversations(ModelRc::new(VecModel::from(items)));
        *sh.displays.borrow_mut() = displays;
    }
    let len = sh.convs.borrow().len();
    if len == 0 {
        ui.set_messages(ModelRc::new(VecModel::from(Vec::<RowItem>::new())));
        ui.set_render_total(0);
        sh.current_msgs.borrow_mut().clear();
        sh.current_bodies.borrow_mut().clear();
        return;
    }
    let next = cur.min(len - 1);
    ui.set_selected(next as i32);
    apply_active_header(ui, sh, next);
    open_conversation(ui, sh, next);
    ui.set_sidebar_row_y(next as f32 * 64.0);
    ui.set_sidebar_scroll_seq(ui.get_sidebar_scroll_seq() + 1);
}

/// Зеркалит optimistic-mark-seen в сырой список (raw_convs): без этого
/// пересборка склеенного вида (merge/unmerge) воскресила бы уже погашенный
/// unread-бейдж до прихода серверной дельты.
pub(crate) fn mark_raw_seen(sh: &Shared, conv_key: &merges::MergeKey, conv_merged: bool) {
    let keys: Vec<merges::MergeKey> =
        if conv_merged { sh.merges.borrow().members_of(conv_key) } else { vec![conv_key.clone()] };
    let mut raw = sh.raw_convs.borrow_mut();
    for c in raw.iter_mut() {
        if keys.contains(&conv_merge_key(&sh.key, c)) {
            c.unread_count = 0;
            for m in c.messages.iter_mut() {
                m.seen = true;
            }
        }
    }
}

/// Пересобрать склеенный вид из raw_convs (после merge/unmerge), обновить
/// сайдбар и заново найти диалог `select_id`. `reopen` — перечитать его
/// содержимое (набор сообщений изменился); false — только поправить индекс
/// выделения, не трогая открытую панель.
pub(crate) fn rebuild_merged_view(
    ui: &MainWindow,
    sh: &Shared,
    select_id: Option<String>,
    reopen: bool,
) {
    let merged = apply_merges(&sh.raw_convs.borrow(), &sh.merges.borrow(), &sh.key);
    let displays = displays_from(&merged, &sh.identity_colors.borrow());
    *sh.convs.borrow_mut() = merged;
    *sh.displays.borrow_mut() = displays;
    refresh_sidebar(sh, ui);
    if let Some(id) = select_id {
        let idx = sh.convs.borrow().iter().position(|c| c.id == id);
        if let Some(idx) = idx {
            sh.current.set(idx);
            ui.set_selected(idx as i32);
            apply_active_header(ui, sh, idx);
            if reopen {
                open_conversation(ui, sh, idx);
            }
            ui.set_sidebar_row_y(idx as f32 * 64.0);
            ui.set_sidebar_scroll_seq(ui.get_sidebar_scroll_seq() + 1);
        }
    }
}

/// Rebuild the sidebar ConvItem list from displays + the avatar map.
pub(crate) fn sidebar_items(displays: &[Disp], avatars: &HashMap<String, Image>) -> Vec<ConvItem> {
    displays
        .iter()
        .map(|d| {
            let avatar = avatars.get(&d.email).cloned();
            ConvItem {
                name: d.name.clone().into(),
                preview: d.preview.clone().into(),
                initials: d.initials.clone().into(),
                color: slint::Brush::SolidColor(hex(&d.color)),
                time: "".into(),
                has_avatar: avatar.is_some(),
                avatar: avatar.unwrap_or_default(),
                ident_color: if d.ident_color.is_empty() {
                    slint::Brush::SolidColor(slint::Color::from_argb_u8(0, 0, 0, 0))
                } else {
                    slint::Brush::SolidColor(hex(&d.ident_color))
                },
                unread: d.unread as i32,
                highlight: false,
                is_merged: d.merged,
            }
        })
        .collect()
}

/// One row representing the transient-compose target — rendered at the
/// very top of the sidebar so the user sees "a chat" with the new
/// recipient before any message has been sent. Telegram does the same.
pub(crate) fn pending_compose_item(target: &str) -> ConvItem {
    let initials = target.chars().next().map(|c| c.to_uppercase().to_string()).unwrap_or_default();
    ConvItem {
        name: target.to_string().into(),
        preview: "Новое сообщение".into(),
        initials: initials.into(),
        color: slint::Brush::SolidColor(hex("#10b981")),
        time: "".into(),
        has_avatar: false,
        avatar: Image::default(),
        ident_color: slint::Brush::SolidColor(slint::Color::from_argb_u8(0, 0, 0, 0)),
        unread: 0,
        highlight: false,
        is_merged: false,
    }
}

/// One-second attention flash on a sidebar row (model index): flips the
/// row's highlight on, then off after 150 ms — the Slint side fades the
/// overlay out over ~a second.
pub(crate) fn flash_sidebar_row(ui: &MainWindow, model_idx: usize) {
    use slint::Model;
    let model = ui.get_conversations();
    let Some(mut item) = model.row_data(model_idx) else { return };
    item.highlight = true;
    model.set_row_data(model_idx, item.clone());
    let ui_weak = ui.as_weak();
    slint::Timer::single_shot(std::time::Duration::from_millis(150), move || {
        if let Some(ui) = ui_weak.upgrade() {
            let model = ui.get_conversations();
            if let Some(mut item) = model.row_data(model_idx) {
                item.highlight = false;
                model.set_row_data(model_idx, item);
            }
        }
    });
}

/// Push the latest displays + pending-compose state into the Slint
/// sidebar model. When `pending_compose` is Some, prepend a synthetic
/// "new chat" row at index 0 and select it.
pub(crate) fn refresh_sidebar(sh: &Shared, ui: &MainWindow) {
    let displays = sh.displays.borrow();
    let avatars = sh.avatars.borrow();
    let pending = sh.pending_compose.borrow().clone();

    let mut items = Vec::with_capacity(displays.len() + 1);
    if let Some(target) = pending.as_ref() {
        items.push(pending_compose_item(target));
    }
    items.extend(sidebar_items(&displays, &avatars));
    ui.set_conversations(ModelRc::new(VecModel::from(items)));
    if pending.is_some() {
        ui.set_selected(0);
    }
}

/// То же для сайдбара: строка выбранного диалога должна быть видна, а её
/// bump теряется по той же причине. Своего зеркала позиции у списка нет,
/// поэтому просто повторяем — мост доводит строку до видимости и ничего не
/// делает, если она уже видна.
pub(crate) fn nudge_sidebar_scroll(ui_weak: slint::Weak<MainWindow>, row_y: f32, delay_ms: u64) {
    slint::Timer::single_shot(std::time::Duration::from_millis(delay_ms), move || {
        let Some(ui) = ui_weak.upgrade() else { return };
        ui.set_sidebar_row_y(row_y);
        ui.set_sidebar_scroll_seq(ui.get_sidebar_scroll_seq() + 1);
    });
}

/// displays-index → sidebar model index (the transient compose row shifts
/// everything by one).
pub(crate) fn model_index(sh: &Shared, idx: usize) -> usize {
    if sh.pending_compose.borrow().is_some() { idx + 1 } else { idx }
}

/// Conversation list callbacks: select, merge / unmerge, rename, keyboard
/// navigation, delete and spam (with their confirmations).
pub(crate) fn wire_sidebar(ui: &MainWindow, shared: &Rc<Shared>) {
    let ui_weak2 = ui.as_weak();
    let sh_sel = shared.clone();
    ui.on_select(move |idx| {
        let Some(ui) = ui_weak2.upgrade() else { return };
        let model_idx = idx as usize;
        // While in transient-compose mode the first row is the synthetic
        // "new chat" — clicking it is a no-op (we're already there).
        let pending = sh_sel.pending_compose.borrow().is_some();
        if pending && model_idx == 0 {
            return;
        }
        // Real-conversation rows: when a transient row is present we
        // need to subtract one to map model index → displays index.
        let real_idx = if pending { model_idx - 1 } else { model_idx };
        // Picking any real conversation leaves transient-compose mode AND
        // drops any staged explicit-reply target — both are tied to the
        // previous context.
        let was_pending = sh_sel.pending_compose.borrow_mut().take().is_some();
        exit_reply_mode(&sh_sel, &ui);
        apply_active_header(&ui, &sh_sel, real_idx);
        if was_pending {
            refresh_sidebar(&sh_sel, &ui);
        }
        // Highlight the real row at its post-refresh model index.
        ui.set_selected(real_idx as i32);
        // Re-grab the window-level key sink so Delete works right after a
        // click (typing in the composer moves focus there as usual).
        ui.invoke_grab_key_focus();
        open_conversation(&ui, &sh_sel, real_idx);
    });

    // ── Объединение диалогов (правый клик по строке сайдбара) ──
    // «Объединить с открытым диалогом»: кликнутая строка вливается в группу
    // открытого; открытый остаётся первичным (его имя/аватар/identity).
    let ui_weak_cm = ui.as_weak();
    let sh_cm = shared.clone();
    ui.on_conv_merge(move |model_idx| {
        let Some(ui) = ui_weak_cm.upgrade() else { return };
        if sh_cm.pending_compose.borrow().is_some() {
            return; // transient compose: индексы сдвинуты, открытого диалога нет
        }
        let src_idx = model_idx as usize;
        let dst_idx = sh_cm.current.get();
        if src_idx == dst_idx {
            return;
        }
        let resolved = {
            let convs = sh_cm.convs.borrow();
            match (convs.get(dst_idx), convs.get(src_idx)) {
                (Some(dst), Some(src)) => Some((
                    conv_merge_key(&sh_cm.key, dst),
                    conv_merge_key(&sh_cm.key, src),
                    dst.id.clone(),
                )),
                _ => None,
            }
        };
        let Some((dst_key, src_key, dst_id)) = resolved else { return };
        // Refs сообщений склейки ходят в движок под ОДНИМ account_key —
        // диалоги разных подключений склеить нельзя.
        if dst_key.account != src_key.account {
            toast_window::show(
                2, // amber
                0,
                "Объединение недоступно",
                "Диалоги из разных подключений объединить нельзя.",
                false,
                600,
                || {},
                || {},
                || {},
            );
            return;
        }
        {
            let mut m = sh_cm.merges.borrow_mut();
            let target = m.members_of(&dst_key);
            let source = m.members_of(&src_key);
            m.merge(target, source);
            merges::save(&m);
        }
        println!("merge: {} + {} (primary {})", dst_key.id, src_key.id, dst_id);
        // Открытый диалог получил новые сообщения — перечитываем его.
        rebuild_merged_view(&ui, &sh_cm, Some(dst_id), true);
    });

    // «Разъединить диалоги»: группа удаляется, члены возвращаются в сайдбар
    // отдельными строками. Выделение остаётся на первичном (тот же id).
    let ui_weak_um = ui.as_weak();
    let sh_um = shared.clone();
    ui.on_conv_unmerge(move |model_idx| {
        let Some(ui) = ui_weak_um.upgrade() else { return };
        if sh_um.pending_compose.borrow().is_some() {
            return;
        }
        let idx = model_idx as usize;
        let resolved = {
            let convs = sh_um.convs.borrow();
            match convs.get(idx) {
                Some(c) if c.merged => Some((conv_merge_key(&sh_um.key, c), c.id.clone())),
                _ => None,
            }
        };
        let Some((key, id)) = resolved else { return };
        {
            let mut m = sh_um.merges.borrow_mut();
            m.unmerge(&key);
            merges::save(&m);
        }
        println!("unmerge: {}", key.id);
        // Если распустили ОТКРЫТУЮ склейку — перечитать панель (в ней
        // останется только первичный); чужую — не дёргать открытый диалог.
        let was_open = idx == sh_um.current.get();
        let keep_id = if was_open {
            Some(id)
        } else {
            sh_um.convs.borrow().get(sh_um.current.get()).map(|c| c.id.clone())
        };
        rebuild_merged_view(&ui, &sh_um, keep_id, was_open);
    });

    // Переименование открытого диалога (двойной клик по имени в шапке).
    // Пишется в merges.json рядом со склейками и меняет только имя в
    // клиенте: письма, кэш и сервер его не видят. Пустое — снять имя.
    let ui_weak_rn = ui.as_weak();
    let sh_rn = shared.clone();
    ui.on_rename_conversation(move |name| {
        let Some(ui) = ui_weak_rn.upgrade() else { return };
        if sh_rn.pending_compose.borrow().is_some() {
            return; // нового письма ещё нет в списке — переименовывать нечего
        }
        let resolved = {
            let convs = sh_rn.convs.borrow();
            convs.get(sh_rn.current.get()).map(|c| (conv_merge_key(&sh_rn.key, c), c.id.clone()))
        };
        let Some((key, id)) = resolved else { return };
        {
            let mut m = sh_rn.merges.borrow_mut();
            if m.name_of(&key).unwrap_or("") == name.trim() {
                return; // ничего не поменялось — ни записи, ни плашки
            }
            m.rename(key.clone(), &name);
            merges::save(&m);
        }
        println!("rename: {} -> {:?}", key.id, name.trim());
        // Тела не меняются — панель не перечитываем, только сайдбар и шапку.
        rebuild_merged_view(&ui, &sh_rn, Some(id), false);
        flash_confirm(&ui, "✓ Переименовано");
    });

    // ↑/↓ over the conversation list: select + open the neighbour and keep
    // its row visible. Pairs with Delete for sweeping unwanted dialogs.
    let ui_weak_nav = ui.as_weak();
    let sh_nav = shared.clone();
    ui.on_nav_conversation(move |delta| {
        let Some(ui) = ui_weak_nav.upgrade() else { return };
        if sh_nav.pending_compose.borrow().is_some() {
            return;
        }
        let len = sh_nav.convs.borrow().len() as i32;
        if len == 0 {
            return;
        }
        let cur = sh_nav.current.get() as i32;
        let new = (cur + delta).clamp(0, len - 1);
        if new == cur {
            return;
        }
        exit_reply_mode(&sh_nav, &ui);
        ui.set_selected(new);
        apply_active_header(&ui, &sh_nav, new as usize);
        open_conversation(&ui, &sh_nav, new as usize);
        ui.set_sidebar_row_y(new as f32 * 64.0);
        ui.set_sidebar_scroll_seq(ui.get_sidebar_scroll_seq() + 1);
    });

    // Delete key → confirm modal → delete the whole conversation (every
    // message incl. the user's own replies from Sent). The server handler
    // soft-deletes locally AND queues flag-sync deleted=true, so the worker
    // pushes STORE \Deleted + UID EXPUNGE to the source IMAP server.
    let ui_weak_delc = ui.as_weak();
    let sh_delc = shared.clone();
    ui.on_delete_conversation(move || {
        let Some(ui) = ui_weak_delc.upgrade() else { return };
        if sh_delc.pending_compose.borrow().is_some() {
            return; // transient compose has no conversation to delete
        }
        let convs = sh_delc.convs.borrow();
        let Some(c) = convs.get(sh_delc.current.get()) else { return };
        sh_delc.confirm_mode.set(1);
        ui.set_confirm_is_spam(false);
        ui.set_confirm_delete_title("Удалить диалог?".into());
        ui.set_confirm_delete_text(
            format!(
                "«{}» — сообщений: {}. Все письма диалога, включая ваши ответы, \
                 будут удалены и на сервере.",
                c.label,
                c.messages.len()
            )
            .into(),
        );
        ui.set_confirm_delete_visible(true);
    });

    // «Спам» in the chat header: blacklist the counterpart's domain and
    // purge every message from them (Tauri-era behaviour), confirmed
    // through the same modal as conversation deletion.
    let ui_weak_spam = ui.as_weak();
    let sh_spam = shared.clone();
    ui.on_spam_conversation(move || {
        let Some(ui) = ui_weak_spam.upgrade() else { return };
        if sh_spam.pending_compose.borrow().is_some() {
            return;
        }
        let convs = sh_spam.convs.borrow();
        let Some(c) = convs.get(sh_spam.current.get()) else { return };
        // A conversation must have at least one counterpart to be spam-worthy,
        // but we DON'T trust which one is the sender — for BCC-blasts the
        // participant set mixes From and To. The server resolves the real
        // sender from the message ids; here we only gate on non-emptiness.
        if c.counterparts.iter().all(|cp| cp.addr.is_empty()) {
            return;
        }
        sh_spam.confirm_mode.set(2);
        ui.set_confirm_is_spam(true);
        ui.set_confirm_delete_title("В спам?".into());
        ui.set_confirm_delete_text(
            "Удалить письма этого диалога и заблокировать отправителя. \
             «Домен» останавливает спам с меняющихся адресов одного домена."
                .into(),
        );
        ui.set_confirm_delete_visible(true);
    });
    let ui_weak_delk = ui.as_weak();
    let sh_delk = shared.clone();
    ui.on_delete_conversation_confirmed(move || {
        let Some(ui) = ui_weak_delk.upgrade() else { return };
        let cur = sh_delk.current.get();
        sh_delk.confirm_mode.set(0);
        let (conv_id, refs) = {
            let convs = sh_delk.convs.borrow();
            let Some(c) = convs.get(cur) else { return };
            (c.id.clone(), c.messages.clone())
        };
        if let Some(etx) = sh_delk.engine_tx.borrow().as_ref() {
            println!("delete conversation {conv_id} ({} messages)", refs.len());
            let _ = etx.send(engine::EngineCmd::Delete {
                messages: refs,
                account_key: sh_delk.cur_account_key.borrow().clone(),
            });
        }
        optimistic_remove_conversation(&ui, &sh_delk, cur, &conv_id);
    });

    // Spam: blacklist + purge. `scope` ("address"|"domain") comes from which
    // button the user pressed. We send the conversation's message ids so the
    // SERVER resolves the real sender (the client can't tell From from To in a
    // grouped/BCC conversation); `fallback_addr` is only a hint for sources
    // that resolve no ids. On success a «✓ …» plashka names what was blocked.
    let ui_weak_spamc = ui.as_weak();
    let sh_spamc = shared.clone();
    ui.on_spam_confirmed(move |scope| {
        let Some(ui) = ui_weak_spamc.upgrade() else { return };
        let cur = sh_spamc.current.get();
        sh_spamc.confirm_mode.set(0);
        let (conv_id, refs, fallback_addr) = {
            let convs = sh_spamc.convs.borrow();
            let Some(c) = convs.get(cur) else { return };
            (
                c.id.clone(),
                c.messages.clone(),
                c.counterparts.first().map(|cp| cp.addr.to_lowercase()).unwrap_or_default(),
            )
        };
        let ids: Vec<i64> = refs.iter().map(|m| m.uid as i64).collect();
        if let Some(etx) = sh_spamc.engine_tx.borrow().as_ref() {
            println!("spam purge scope={scope} ({} rows)", ids.len());
            let _ = etx.send(engine::EngineCmd::BlacklistAndPurge {
                scope: scope.to_string(),
                fallback_addr,
                message_ids: ids,
                account_key: sh_spamc.cur_account_key.borrow().clone(),
            });
        }
        optimistic_remove_conversation(&ui, &sh_spamc, cur, &conv_id);
    });
}
