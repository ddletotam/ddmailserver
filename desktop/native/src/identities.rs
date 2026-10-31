//! Айдентики — адреса, с которых я пишу, сведённые в группы (контракт §1в).
//!
//! Модель:
//! - каждый адрес лежит ровно в одной группе; новый адрес — группа из одного;
//! - у группы одно имя (ФИО), один цвет, одна подпись и адрес-отправитель:
//!   с него уходит ответ на письмо, пришедшее на любой адрес группы;
//! - адреса собираются из всех учёток (айдентики сервера, email и логин
//!   учётки) и только добавляются: ни автоматического, ни ручного удаления
//!   нет. Учётку удалили — адрес остаётся и дальше красит строки;
//! - «не предлагать» (`hidden`) убирает адрес из выпадашки «От кого», но он
//!   остаётся «моим» и может быть отправителем группы.
//!
//! Документ хранится на сервере (`/api/desktop/v1/identity-groups`), если
//! нативное подключение это умеет; иначе — в локальном файле. Копия лежит в
//! `identities.json` в любом случае: цвета видны на старте.

use std::collections::HashMap;
use std::sync::Arc;

use base64::Engine as _;
use serde::{Deserialize, Serialize};

use crate::folders::Store;
use crate::richtext::{Block, Doc, InlineImage, Run, Style};
use crate::sidebar::{IDENT_PASTEL, IDENT_VIVID};

/// Адрес внутри группы.
#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct Address {
    /// Всегда в нижнем регистре.
    pub email: String,
    /// «Не предлагать»: нет в выпадашке, но адрес по-прежнему мой.
    pub hidden: bool,
    #[serde(flatten)]
    pub extra: serde_json::Map<String, serde_json::Value>,
}

/// Кусок текста подписи — сериализуемое зеркало `richtext::Run`.
#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct SigRun {
    pub text: String,
    pub b: bool,
    pub i: bool,
    pub u: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub link: Option<String>,
}

/// Блок подписи — сериализуемое зеркало `richtext::Block`. Картинка лежит
/// прямо в документе (base64): подпись должна приехать на другую станцию
/// целиком.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(tag = "t", rename_all = "lowercase")]
pub enum SigBlock {
    Para { runs: Vec<SigRun> },
    Image { mime: String, data: String, w: u32, h: u32 },
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct Group {
    pub id: String,
    /// ФИО для поля «От кого». Пусто — только адрес.
    pub name: String,
    /// Пастельный цвет строки списка (`#rrggbb`).
    pub color: String,
    /// Адрес-отправитель группы. Не из группы (или пусто) — первый адрес.
    pub sender: String,
    pub addresses: Vec<Address>,
    /// Подпись; пусто — без подписи.
    pub signature: Vec<SigBlock>,
    #[serde(flatten)]
    pub extra: serde_json::Map<String, serde_json::Value>,
}

impl Group {
    /// Адрес, с которого пишет группа.
    pub fn sender(&self) -> &str {
        if self.addresses.iter().any(|a| a.email == self.sender) {
            &self.sender
        } else {
            self.addresses.first().map(|a| a.email.as_str()).unwrap_or("")
        }
    }

    pub fn has_signature(&self) -> bool {
        self.signature.iter().any(|b| match b {
            SigBlock::Para { runs } => runs.iter().any(|r| !r.text.trim().is_empty()),
            SigBlock::Image { .. } => true,
        })
    }
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct IdentityDoc {
    pub groups: Vec<Group>,
    /// Группа нового письма. Пусто или нет такой — первая.
    pub default_group: String,
    #[serde(flatten)]
    pub extra: serde_json::Map<String, serde_json::Value>,
}

/// Адрес, найденный в учётке или пришедший с сервера.
#[derive(Debug, Clone, Default, PartialEq)]
pub struct Source {
    pub email: String,
    pub name: String,
    pub color: String,
}

pub fn normalize(email: &str) -> String {
    email.trim().to_lowercase()
}

/// Похоже на email: одна `@`, непустые части, точка в домене, без пробелов.
pub fn looks_like_email(s: &str) -> bool {
    let s = s.trim();
    let Some((local, domain)) = s.split_once('@') else { return false };
    !local.is_empty()
        && !domain.is_empty()
        && !domain.contains('@')
        && domain.contains('.')
        && !domain.starts_with('.')
        && !domain.ends_with('.')
        && !s.chars().any(char::is_whitespace)
}

/// `user+tag@host` → `user@host`; без `+` — `None`.
pub fn base_of_tagged(email: &str) -> Option<String> {
    let (local, domain) = email.split_once('@')?;
    let (base, _) = local.split_once('+')?;
    (!base.is_empty()).then(|| format!("{base}@{domain}"))
}

impl IdentityDoc {
    pub fn from_value(v: Option<&serde_json::Value>) -> Self {
        v.and_then(|v| serde_json::from_value(v.clone()).ok()).unwrap_or_default()
    }

    pub fn to_value(&self) -> serde_json::Value {
        serde_json::to_value(self).unwrap_or(serde_json::Value::Null)
    }

    /// (группа, адрес) ровно по адресу.
    fn locate_exact(&self, email: &str) -> Option<(usize, usize)> {
        self.groups.iter().enumerate().find_map(|(gi, g)| {
            g.addresses.iter().position(|a| a.email == email).map(|ai| (gi, ai))
        })
    }

    /// (группа, адрес); не добавленный явно `user+tag@` сводится к `user@`.
    pub fn locate(&self, email: &str) -> Option<(usize, usize)> {
        let lc = normalize(email);
        self.locate_exact(&lc).or_else(|| base_of_tagged(&lc).and_then(|b| self.locate_exact(&b)))
    }

    pub fn group_of(&self, email: &str) -> Option<&Group> {
        self.locate(email).map(|(gi, _)| &self.groups[gi])
    }

    pub fn group_by_id(&self, id: &str) -> Option<&Group> {
        self.groups.iter().find(|g| g.id == id)
    }

    pub fn is_mine(&self, email: &str) -> bool {
        self.locate(email).is_some()
    }

    pub fn default_group(&self) -> Option<&Group> {
        self.group_by_id(&self.default_group).or_else(|| self.groups.first())
    }

    /// С какого адреса отвечать на письмо, пришедшее на `received_by`:
    /// отправитель его группы. Адрес не мой — отправитель группы по
    /// умолчанию.
    pub fn reply_sender(&self, received_by: &str) -> Option<String> {
        self.group_of(received_by)
            .or_else(|| self.default_group())
            .map(|g| g.sender().to_string())
            .filter(|s| !s.is_empty())
    }

    /// Отправитель нового письма.
    pub fn new_mail_sender(&self) -> Option<String> {
        self.default_group().map(|g| g.sender().to_string()).filter(|s| !s.is_empty())
    }

    /// Все мои адреса → пастельный цвет их группы.
    pub fn color_map(&self) -> HashMap<String, String> {
        let mut m = HashMap::new();
        for (gi, g) in self.groups.iter().enumerate() {
            let color = group_color(g, gi);
            for a in &g.addresses {
                m.insert(a.email.clone(), color.clone());
            }
        }
        m
    }

    pub fn new_id(&self) -> String {
        let mut n = self.groups.len() + 1;
        loop {
            let id = format!("g{n}");
            if self.group_by_id(&id).is_none() {
                return id;
            }
            n += 1;
        }
    }

    /// Свободный пастельный цвет: первый из палитры, которого нет ни у одной
    /// группы; все заняты — по кругу.
    fn next_color(&self) -> String {
        let used: Vec<String> = self.groups.iter().map(|g| g.color.to_uppercase()).collect();
        IDENT_PASTEL
            .iter()
            .find(|c| !used.contains(&c.to_uppercase()))
            .unwrap_or(&IDENT_PASTEL[self.groups.len() % IDENT_PASTEL.len()])
            .to_string()
    }

    /// Добавить адреса, которых ещё нет: каждый — новой группой из одного
    /// (имя и цвет источника — только начальные значения). Ничего не удаляет
    /// и не трогает уже известные адреса — ручная настройка главнее.
    /// `true` — документ изменился.
    pub fn merge_sources(&mut self, sources: &[Source]) -> bool {
        let mut changed = false;
        for s in sources {
            let email = normalize(&s.email);
            if !looks_like_email(&email) || self.locate_exact(&email).is_some() {
                continue;
            }
            let color = if s.color.trim().is_empty() {
                self.next_color()
            } else {
                s.color.trim().to_string()
            };
            let id = self.new_id();
            self.groups.push(Group {
                id,
                name: s.name.trim().to_string(),
                color,
                sender: email.clone(),
                addresses: vec![Address { email, ..Default::default() }],
                ..Default::default()
            });
            changed = true;
        }
        if changed && self.group_by_id(&self.default_group).is_none() {
            if let Some(g) = self.groups.first() {
                self.default_group = g.id.clone();
            }
        }
        changed
    }

    /// Перенести адрес в группу `target` (`None` — в новую группу из него
    /// одного). Опустевшая группа исчезает: группа — это её адреса, а не
    /// отдельная сущность. Отправитель, ушедший из группы, сменяется первым
    /// оставшимся адресом.
    pub fn move_address(&mut self, email: &str, target: Option<&str>) -> bool {
        let Some((gi, ai)) = self.locate_exact(&normalize(email)) else { return false };
        if target == Some(self.groups[gi].id.as_str()) {
            return false;
        }
        if target.is_some_and(|t| self.group_by_id(t).is_none()) {
            return false;
        }
        if target.is_none() && self.groups[gi].addresses.len() == 1 {
            return false; // уже одна в своей группе
        }
        let addr = self.groups[gi].addresses.remove(ai);
        match target {
            Some(t) => {
                let g = self.groups.iter_mut().find(|g| g.id == t).expect("checked");
                g.addresses.push(addr);
            }
            None => {
                let color = self.next_color();
                let id = self.new_id();
                let name = self.groups[gi].name.clone();
                self.groups.insert(
                    gi + 1,
                    Group {
                        id,
                        name,
                        color,
                        sender: addr.email.clone(),
                        addresses: vec![addr],
                        ..Default::default()
                    },
                );
            }
        }
        if self.groups[gi].addresses.is_empty() {
            let gone = self.groups.remove(gi).id;
            if self.default_group == gone {
                self.default_group = target.map(str::to_string).unwrap_or_default();
            }
        }
        true
    }

    /// Сдвинуть адрес внутри группы на `delta` позиций.
    pub fn shift_address(&mut self, email: &str, delta: i32) -> bool {
        let Some((gi, ai)) = self.locate_exact(&normalize(email)) else { return false };
        let list = &mut self.groups[gi].addresses;
        let to = ai as i32 + delta;
        if to < 0 || to >= list.len() as i32 {
            return false;
        }
        list.swap(ai, to as usize);
        true
    }

    /// Сдвинуть группу на `delta` позиций.
    pub fn shift_group(&mut self, id: &str, delta: i32) -> bool {
        let Some(gi) = self.groups.iter().position(|g| g.id == id) else { return false };
        let to = gi as i32 + delta;
        if to < 0 || to >= self.groups.len() as i32 {
            return false;
        }
        self.groups.swap(gi, to as usize);
        true
    }

    pub fn group_mut(&mut self, id: &str) -> Option<&mut Group> {
        self.groups.iter_mut().find(|g| g.id == id)
    }

    pub fn address_mut(&mut self, email: &str) -> Option<&mut Address> {
        let (gi, ai) = self.locate_exact(&normalize(email))?;
        Some(&mut self.groups[gi].addresses[ai])
    }

    /// Объединение после конфликта или при первом знакомстве с сервером:
    /// берём `theirs` целиком и добавляем адреса, которых в нём нет, из
    /// `ours` — каждый своей группой со своими настройками. Ничего не
    /// теряется, а настройки сервера побеждают для общих адресов.
    pub fn union_into(theirs: &mut IdentityDoc, ours: &IdentityDoc) -> bool {
        let mut changed = false;
        for g in &ours.groups {
            let missing: Vec<Address> = g
                .addresses
                .iter()
                .filter(|a| theirs.locate_exact(&a.email).is_none())
                .cloned()
                .collect();
            if missing.is_empty() {
                continue;
            }
            let mut ng = g.clone();
            ng.addresses = missing;
            if theirs.group_by_id(&ng.id).is_some() {
                ng.id = theirs.new_id();
            }
            theirs.groups.push(ng);
            changed = true;
        }
        if theirs.group_by_id(&theirs.default_group).is_none() {
            if let Some(g) = theirs.groups.first() {
                theirs.default_group = g.id.clone();
                changed = true;
            }
        }
        changed
    }
}

/// Пастельный цвет группы: свой или из палитры по номеру.
pub fn group_color(g: &Group, index: usize) -> String {
    if g.color.trim().is_empty() {
        IDENT_PASTEL[index % IDENT_PASTEL.len()].to_string()
    } else {
        g.color.trim().to_string()
    }
}

/// Насыщенная пара пастельного цвета — для точки в выпадашке, где 12px
/// пастели не видно. Цвет из палитры — его пара по индексу; чужой —
/// тот же тон с поднятой насыщенностью.
pub fn vivid(pastel: &str) -> String {
    if let Some(i) = IDENT_PASTEL.iter().position(|c| c.eq_ignore_ascii_case(pastel.trim())) {
        return IDENT_VIVID[i].to_string();
    }
    let hex = pastel.trim().trim_start_matches('#');
    let Ok(v) = u32::from_str_radix(hex, 16) else { return "#6b7480".into() };
    if hex.len() != 6 {
        return "#6b7480".into();
    }
    let (r, g, b) = (
        ((v >> 16) & 0xff) as f32 / 255.0,
        ((v >> 8) & 0xff) as f32 / 255.0,
        (v & 0xff) as f32 / 255.0,
    );
    let max = r.max(g).max(b);
    let min = r.min(g).min(b);
    let d = max - min;
    let h = if d == 0.0 {
        0.0
    } else if max == r {
        60.0 * (((g - b) / d).rem_euclid(6.0))
    } else if max == g {
        60.0 * ((b - r) / d + 2.0)
    } else {
        60.0 * ((r - g) / d + 4.0)
    };
    let s = if d == 0.0 { 0.0 } else { 0.65 };
    let l = 0.45_f32;
    let c = (1.0 - (2.0 * l - 1.0).abs()) * s;
    let x = c * (1.0 - ((h / 60.0).rem_euclid(2.0) - 1.0).abs());
    let m = l - c / 2.0;
    let (r1, g1, b1) = match h as u32 {
        0..=59 => (c, x, 0.0),
        60..=119 => (x, c, 0.0),
        120..=179 => (0.0, c, x),
        180..=239 => (0.0, x, c),
        240..=299 => (x, 0.0, c),
        _ => (c, 0.0, x),
    };
    let to = |f: f32| ((f + m) * 255.0).round().clamp(0.0, 255.0) as u8;
    format!("#{:02x}{:02x}{:02x}", to(r1), to(g1), to(b1))
}

// ---- Подпись: richtext::Doc ⇄ сериализуемые блоки ----

pub fn signature_from_doc(doc: &Doc) -> Vec<SigBlock> {
    let mut out: Vec<SigBlock> = doc
        .blocks
        .iter()
        .map(|b| match b {
            Block::Para(runs) => SigBlock::Para {
                runs: runs
                    .iter()
                    .filter(|r| !r.text.is_empty())
                    .map(|r| SigRun {
                        text: r.text.clone(),
                        b: r.style.bold,
                        i: r.style.italic,
                        u: r.style.underline,
                        link: r.link.clone(),
                    })
                    .collect(),
            },
            Block::Image(img) => SigBlock::Image {
                mime: img.mime.clone(),
                data: base64::engine::general_purpose::STANDARD.encode(img.bytes.as_ref()),
                w: img.w,
                h: img.h,
            },
        })
        .collect();
    // Хвостовые пустые абзацы — не подпись, а Enter в конце.
    while matches!(out.last(), Some(SigBlock::Para { runs }) if runs.is_empty()) {
        out.pop();
    }
    out
}

/// `cid_prefix` разводит картинки подписи с картинками самого письма.
pub fn signature_to_doc(sig: &[SigBlock], cid_prefix: &str) -> Doc {
    let mut blocks = Vec::new();
    for (n, b) in sig.iter().enumerate() {
        match b {
            SigBlock::Para { runs } => blocks.push(Block::Para(
                runs.iter()
                    .map(|r| Run {
                        text: r.text.clone(),
                        style: Style { bold: r.b, italic: r.i, underline: r.u },
                        link: r.link.clone(),
                    })
                    .collect(),
            )),
            SigBlock::Image { mime, data, w, h } => {
                let Ok(bytes) = base64::engine::general_purpose::STANDARD.decode(data) else {
                    continue;
                };
                blocks.push(Block::Image(InlineImage {
                    cid: format!("{cid_prefix}{n}@ddmail"),
                    mime: mime.clone(),
                    bytes: Arc::new(bytes),
                    w: *w,
                    h: *h,
                }));
            }
        }
    }
    if blocks.is_empty() {
        return Doc::default();
    }
    Doc { blocks }
}

// ---- Хранение ----

/// Содержимое `identities.json`: последний известный документ и где он
/// хранится. Для `Server` это кэш — истина на сервере.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
#[serde(default)]
pub struct Stored {
    pub store: Store,
    pub version: i64,
    pub doc: IdentityDoc,
}

fn path() -> Option<std::path::PathBuf> {
    crate::account_store::config_dir().map(|d| d.join("identities.json"))
}

pub fn load() -> Stored {
    let Some(path) = path() else { return Stored::default() };
    let Ok(bytes) = std::fs::read(&path) else { return Stored::default() };
    serde_json::from_slice::<Stored>(&bytes).unwrap_or_default()
}

pub fn save(s: &Stored) {
    let Some(path) = path() else { return };
    if let Some(dir) = path.parent() {
        let _ = std::fs::create_dir_all(dir);
    }
    if let Ok(json) = serde_json::to_vec_pretty(s) {
        if let Err(e) = crate::account_store::write_atomic(&path, &json) {
            eprintln!("identities: не записан {}: {e}", path.display());
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn src(e: &str) -> Source {
        Source { email: e.into(), ..Default::default() }
    }

    fn doc_of(list: &[&str]) -> IdentityDoc {
        let mut d = IdentityDoc::default();
        d.merge_sources(&list.iter().map(|e| src(e)).collect::<Vec<_>>());
        d
    }

    #[test]
    fn merge_adds_only_new_valid_addresses_and_never_removes() {
        let mut d = doc_of(&["A@x.ru", "b@y.ru", "lucky", "not an@email.ru"]);
        assert_eq!(d.groups.len(), 2);
        assert_eq!(d.groups[0].addresses[0].email, "a@x.ru");
        assert_eq!(d.default_group, d.groups[0].id);
        d.groups[0].name = "Ручное".into();
        assert!(!d.merge_sources(&[Source {
            email: "a@x.ru".into(),
            name: "С сервера".into(),
            color: "#000000".into()
        }]));
        assert_eq!(d.groups[0].name, "Ручное");
        // Пустой набор источников ничего не убирает.
        assert!(!d.merge_sources(&[]));
        assert_eq!(d.groups.len(), 2);
    }

    #[test]
    fn new_groups_get_distinct_colors() {
        let d = doc_of(&["a@x.ru", "b@x.ru", "c@x.ru"]);
        let mut colors: Vec<_> = d.groups.iter().map(|g| g.color.clone()).collect();
        colors.dedup();
        assert_eq!(colors.len(), 3);
    }

    #[test]
    fn reply_goes_from_group_sender() {
        let mut d = doc_of(&["a@x.ru", "b@x.ru", "c@x.ru"]);
        let g = d.groups[1].id.clone();
        assert!(d.move_address("a@x.ru", Some(&g)));
        // a ушла из своей группы — та опустела и исчезла.
        assert_eq!(d.groups.len(), 2);
        assert_eq!(d.reply_sender("a@x.ru").as_deref(), Some("b@x.ru"));
        d.group_mut(&g).unwrap().sender = "a@x.ru".into();
        assert_eq!(d.reply_sender("b@x.ru").as_deref(), Some("a@x.ru"));
        // Не мой адрес — отправитель группы по умолчанию.
        assert!(d.reply_sender("stranger@z.ru").is_some());
    }

    #[test]
    fn tagged_address_resolves_to_base_unless_added() {
        let mut d = doc_of(&["user@x.ru"]);
        assert!(d.is_mine("User+shop@x.ru"));
        assert_eq!(d.reply_sender("user+shop@x.ru").as_deref(), Some("user@x.ru"));
        d.merge_sources(&[src("user+shop@x.ru")]);
        let (gi, _) = d.locate("user+shop@x.ru").unwrap();
        assert_eq!(d.groups[gi].addresses[0].email, "user+shop@x.ru");
    }

    #[test]
    fn hidden_address_still_mine_and_can_be_sender() {
        let mut d = doc_of(&["a@x.ru"]);
        d.address_mut("a@x.ru").unwrap().hidden = true;
        assert!(d.is_mine("a@x.ru"));
        assert_eq!(d.reply_sender("a@x.ru").as_deref(), Some("a@x.ru"));
    }

    #[test]
    fn sender_falls_back_to_first_when_moved_out() {
        let mut d = doc_of(&["a@x.ru", "b@x.ru"]);
        let g0 = d.groups[0].id.clone();
        d.move_address("b@x.ru", Some(&g0));
        d.group_mut(&g0).unwrap().sender = "b@x.ru".into();
        assert!(d.move_address("b@x.ru", None));
        assert_eq!(d.group_by_id(&g0).unwrap().sender(), "a@x.ru");
        assert_eq!(d.groups.len(), 2);
    }

    #[test]
    fn default_group_moves_with_its_last_address() {
        let mut d = doc_of(&["a@x.ru", "b@x.ru"]);
        let g1 = d.groups[1].id.clone();
        d.move_address("a@x.ru", Some(&g1));
        assert_eq!(d.default_group, g1);
        assert_eq!(d.new_mail_sender().as_deref(), Some("b@x.ru"));
    }

    #[test]
    fn union_keeps_their_settings_and_adds_our_addresses() {
        let mut theirs = doc_of(&["a@x.ru"]);
        theirs.groups[0].name = "Сервер".into();
        let mut ours = doc_of(&["a@x.ru", "m@mail.ru"]);
        ours.groups[0].name = "Локально".into();
        assert!(IdentityDoc::union_into(&mut theirs, &ours));
        assert_eq!(theirs.groups.len(), 2);
        assert_eq!(theirs.groups[0].name, "Сервер");
        assert!(theirs.is_mine("m@mail.ru"));
        let ids: Vec<_> = theirs.groups.iter().map(|g| g.id.clone()).collect();
        assert_ne!(ids[0], ids[1]);
    }

    #[test]
    fn signature_roundtrip() {
        let doc = Doc {
            blocks: vec![
                Block::Para(vec![Run {
                    text: "Денис".into(),
                    style: Style { bold: true, ..Default::default() },
                    link: None,
                }]),
                Block::Image(InlineImage {
                    cid: "x".into(),
                    mime: "image/png".into(),
                    bytes: Arc::new(vec![1, 2, 3]),
                    w: 4,
                    h: 5,
                }),
                Block::Para(vec![]),
            ],
        };
        let sig = signature_from_doc(&doc);
        assert_eq!(sig.len(), 2);
        let back = signature_to_doc(&sig, "sig-g1-");
        assert_eq!(back.blocks.len(), 2);
        match &back.blocks[1] {
            Block::Image(i) => {
                assert_eq!(i.bytes.as_ref(), &vec![1, 2, 3]);
                assert_eq!(i.cid, "sig-g1-1@ddmail");
            }
            _ => panic!(),
        }
        let json = serde_json::to_string(&sig).unwrap();
        let again: Vec<SigBlock> = serde_json::from_str(&json).unwrap();
        assert_eq!(again, sig);
    }

    #[test]
    fn vivid_pairs_palette_and_saturates_others() {
        assert_eq!(vivid("#ffe4e1"), "#E53935");
        let v = vivid("#e0e0f8");
        assert_ne!(v, "#6b7480");
        assert_eq!(vivid("bad"), "#6b7480");
    }

    #[test]
    fn unknown_fields_survive_roundtrip() {
        let v = serde_json::json!({"groups":[{"id":"g1","addresses":[{"email":"a@x.ru","future":1}],"later":true}],"default_group":"g1","v2":[1]});
        let d = IdentityDoc::from_value(Some(&v));
        let back = d.to_value();
        assert_eq!(back["v2"], serde_json::json!([1]));
        assert_eq!(back["groups"][0]["later"], serde_json::json!(true));
        assert_eq!(back["groups"][0]["addresses"][0]["future"], serde_json::json!(1));
    }
}
