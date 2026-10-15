//! Папки диалогов — табы над списком бесед, как папки чатов в Telegram.
//!
//! Модель (контракт §4, «Папки диалогов»):
//! - диалог лежит **строго в одной** папке или ни в одной;
//! - не выбранная ни одна папка — «инбокс»: всё, что не попало в папки;
//! - состав папки — правила плюс ручные `include`/`exclude`;
//! - ручное `include` побеждает правила; среди правил побеждает первая
//!   подходящая папка в порядке табов (перетаскивание меняет приоритет).
//!
//! Документ хранится на сервере (`/api/desktop/v1/dialog-folders`) и
//! толкуется только здесь: диалоги считает клиент, сервер видит JSON.
//! Без нативного подключения — тот же документ в локальном файле.
//! Копия последнего документа лежит в `dialog_folders.json` в любом случае:
//! табы видны на старте, до ответа движка.

use std::collections::HashMap;
use std::fs;
use std::path::PathBuf;

use ddmail_core::types::Conversation;
use serde::{Deserialize, Serialize};

use crate::merges::MergeKey;
use crate::sidebar::conv_merge_key;

/// 1:1 или группа.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Kind {
    #[default]
    Any,
    Direct,
    Group,
}

/// Правила папки. Между категориями — «И», внутри списка — «ИЛИ»; пустая
/// категория не ограничивает. Папка без единого правила набирается только
/// вручную.
///
/// «Только непрочитанные» нет намеренно: открытый диалог тут же получает
/// `\Seen` и выпрыгивал бы из папки прямо из-под курсора.
#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct Rules {
    /// Наш адрес, на который пришла переписка (`Conversation::received_by`).
    pub identities: Vec<String>,
    /// Домен собеседника; `acme.com` ловит и `mail.acme.com`.
    pub domains: Vec<String>,
    pub kind: Kind,
    /// Поля будущих версий — переживают чтение и запись этой.
    #[serde(flatten)]
    pub extra: serde_json::Map<String, serde_json::Value>,
}

impl Rules {
    pub fn is_empty(&self) -> bool {
        self.identities.is_empty() && self.domains.is_empty() && self.kind == Kind::Any
    }

    fn matches(&self, c: &Conversation) -> bool {
        if self.is_empty() {
            return false;
        }
        if !self.identities.is_empty()
            && !self.identities.iter().any(|i| i.eq_ignore_ascii_case(&c.received_by))
        {
            return false;
        }
        if !self.domains.is_empty()
            && !c.counterparts.iter().any(|cp| {
                let addr = cp.addr.to_lowercase();
                let Some((_, host)) = addr.rsplit_once('@') else { return false };
                self.domains.iter().any(|d| {
                    let d = d.trim().trim_start_matches('@').to_lowercase();
                    !d.is_empty() && (host == d || host.ends_with(&format!(".{d}")))
                })
            })
        {
            return false;
        }
        match self.kind {
            Kind::Any => true,
            Kind::Direct => !c.is_group,
            Kind::Group => c.is_group,
        }
    }
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct Folder {
    /// Стабильный id: переименование и перестановка его не меняют, по нему
    /// запоминается выбранный таб.
    pub id: String,
    pub name: String,
    /// Эмодзи перед названием; пусто — без иконки.
    pub icon: String,
    pub rules: Rules,
    /// Положены в папку руками. Ключ склейки — её первичный диалог.
    pub include: Vec<MergeKey>,
    /// Вынуты из папки руками, хотя подходят под её правила.
    pub exclude: Vec<MergeKey>,
    #[serde(flatten)]
    pub extra: serde_json::Map<String, serde_json::Value>,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct FoldersDoc {
    pub folders: Vec<Folder>,
    #[serde(flatten)]
    pub extra: serde_json::Map<String, serde_json::Value>,
}

impl FoldersDoc {
    pub fn from_value(v: Option<&serde_json::Value>) -> Self {
        v.and_then(|v| serde_json::from_value(v.clone()).ok()).unwrap_or_default()
    }

    pub fn to_value(&self) -> serde_json::Value {
        serde_json::to_value(self).unwrap_or_else(|_| serde_json::json!({ "folders": [] }))
    }

    pub fn index_of(&self, id: &str) -> Option<usize> {
        self.folders.iter().position(|f| f.id == id)
    }

    /// Папка диалога: индекс в `folders` или `None` — инбокс.
    pub fn folder_of(&self, c: &Conversation, fallback: &str) -> Option<usize> {
        let key = conv_merge_key(fallback, c);
        if let Some(i) = self.folders.iter().position(|f| f.include.contains(&key)) {
            return Some(i);
        }
        self.folders.iter().position(|f| !f.exclude.contains(&key) && f.rules.matches(c))
    }

    /// Положить диалог в папку `target` руками (`None` — в инбокс).
    ///
    /// Ручной выбор обязан победить всё: снимаем ключ из чужих `include`,
    /// а из каждой папки, чьи правила его ловят, — в её `exclude`. Иначе
    /// «Убрать из папки» у диалога, подходящего под правила, ничего бы не
    /// меняло, а перенос из папки-по-правилам в ручную зависел бы от порядка.
    pub fn assign(&mut self, c: &Conversation, fallback: &str, target: Option<usize>) {
        let key = conv_merge_key(fallback, c);
        for (i, f) in self.folders.iter_mut().enumerate() {
            f.include.retain(|k| k != &key);
            f.exclude.retain(|k| k != &key);
            if Some(i) == target {
                f.include.push(key.clone());
            } else if f.rules.matches(c) {
                f.exclude.push(key.clone());
            }
        }
    }

    /// Новый id, которого ещё нет в документе.
    pub fn new_id(&self) -> String {
        let base = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map(|d| d.as_millis())
            .unwrap_or(0);
        let mut n = 0u32;
        loop {
            let id = format!("f{base:x}{n}");
            if self.index_of(&id).is_none() {
                return id;
            }
            n += 1;
        }
    }
}

/// Счётчики табов: сколько диалогов с непрочитанным в каждой папке, и
/// отдельно — в инбоксе (для чипа «Входящие»). Как у Telegram — диалоги, а
/// не письма: десяток писем в одной беседе остаётся одной беседой.
pub fn unread_counts(doc: &FoldersDoc, convs: &[Conversation], fallback: &str) -> (Vec<u32>, u32) {
    let mut out = vec![0u32; doc.folders.len()];
    let mut inbox = 0u32;
    for c in convs.iter().filter(|c| c.unread_count > 0) {
        match doc.folder_of(c, fallback) {
            Some(i) => out[i] += 1,
            None => inbox += 1,
        }
    }
    (out, inbox)
}

/// Видимая часть списка для выбранной папки (`None` — инбокс). Порядок
/// исходный.
pub fn filter(
    doc: &FoldersDoc,
    convs: &[Conversation],
    fallback: &str,
    active: Option<usize>,
) -> Vec<Conversation> {
    // Кэш на один проход: диалоги одной склейки/учётки считаются один раз.
    let mut memo: HashMap<(String, String), Option<usize>> = HashMap::new();
    convs
        .iter()
        .filter(|c| {
            let key = conv_merge_key(fallback, c);
            let f = *memo
                .entry((key.account.clone(), key.id.clone()))
                .or_insert_with(|| doc.folder_of(c, fallback));
            f == active
        })
        .cloned()
        .collect()
}

/// Где живёт документ.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Store {
    /// Локальный файл — нативного подключения нет.
    #[default]
    Local,
    /// Сервер нативного подключения `account`.
    Server { account: String },
}

/// Содержимое `dialog_folders.json`: последний известный документ и где он
/// хранится. Для `Server` это кэш — истина на сервере.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
#[serde(default)]
pub struct Stored {
    pub store: Store,
    pub version: i64,
    pub doc: FoldersDoc,
}

fn path() -> Option<PathBuf> {
    #[cfg(target_os = "windows")]
    {
        let appdata = std::env::var("APPDATA").ok()?;
        return Some(PathBuf::from(appdata).join("ru.letotam.ddmail").join("dialog_folders.json"));
    }
    #[cfg(target_os = "macos")]
    {
        let home = std::env::var("HOME").ok()?;
        return Some(
            PathBuf::from(home)
                .join("Library/Application Support/ru.letotam.ddmail/dialog_folders.json"),
        );
    }
    #[cfg(target_os = "linux")]
    {
        let base = std::env::var("XDG_CONFIG_HOME")
            .ok()
            .map(PathBuf::from)
            .or_else(|| std::env::var("HOME").ok().map(|h| PathBuf::from(h).join(".config")))?;
        return Some(base.join("ru.letotam.ddmail").join("dialog_folders.json"));
    }
    #[allow(unreachable_code)]
    None
}

pub fn load() -> Stored {
    let Some(path) = path() else { return Stored::default() };
    let Ok(bytes) = fs::read(&path) else { return Stored::default() };
    serde_json::from_slice::<Stored>(&bytes).unwrap_or_default()
}

pub fn save(s: &Stored) {
    let Some(path) = path() else { return };
    if let Some(dir) = path.parent() {
        let _ = fs::create_dir_all(dir);
    }
    if let Ok(json) = serde_json::to_vec_pretty(s) {
        let _ = fs::write(&path, json);
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use ddmail_core::types::ContactInfo;

    fn conv(id: &str, received_by: &str, cps: &[&str], group: bool) -> Conversation {
        Conversation {
            id: id.into(),
            label: id.into(),
            avatar_hash: String::new(),
            received_by: received_by.into(),
            counterparts: cps
                .iter()
                .map(|a| ContactInfo { name: String::new(), addr: (*a).into() })
                .collect(),
            is_group: group,
            last_date: String::new(),
            last_date_ts: 0,
            last_subject: String::new(),
            unread_count: 0,
            total_count: 1,
            messages: Vec::new(),
            draft: None,
            account_key: "me@host".into(),
            merged: false,
        }
    }

    fn folder(id: &str, rules: Rules) -> Folder {
        Folder { id: id.into(), name: id.into(), rules, ..Default::default() }
    }

    fn key(id: &str) -> MergeKey {
        MergeKey { account: "me@host".into(), id: id.into() }
    }

    #[test]
    fn no_rules_no_include_matches_nothing() {
        let doc = FoldersDoc { folders: vec![folder("a", Rules::default())], ..Default::default() };
        assert_eq!(doc.folder_of(&conv("x", "me@a", &["bob@b"], false), ""), None);
    }

    #[test]
    fn rules_and_between_categories_or_within() {
        let r = Rules {
            identities: vec!["ME@a".into(), "me@c".into()],
            domains: vec!["acme.com".into()],
            ..Default::default()
        };
        let doc = FoldersDoc { folders: vec![folder("work", r)], ..Default::default() };
        assert_eq!(doc.folder_of(&conv("1", "me@a", &["bob@mail.acme.com"], false), ""), Some(0));
        assert_eq!(doc.folder_of(&conv("2", "me@c", &["x@acme.com"], false), ""), Some(0));
        // Идентичность подходит, домен — нет.
        assert_eq!(doc.folder_of(&conv("3", "me@a", &["bob@notacme.com"], false), ""), None);
        // Домен подходит, идентичность — нет.
        assert_eq!(doc.folder_of(&conv("4", "me@z", &["bob@acme.com"], false), ""), None);
    }

    #[test]
    fn first_matching_folder_wins_include_beats_rules() {
        let groups = Rules { kind: Kind::Group, ..Default::default() };
        let any_a = Rules { identities: vec!["me@a".into()], ..Default::default() };
        let mut doc = FoldersDoc {
            folders: vec![
                folder("g", groups),
                folder("a", any_a),
                folder("manual", Rules::default()),
            ],
            ..Default::default()
        };
        let c = conv("grp", "me@a", &["b@x", "c@y"], true);
        assert_eq!(doc.folder_of(&c, ""), Some(0));
        // Перестановка табов меняет приоритет.
        doc.folders.swap(0, 1);
        assert_eq!(doc.folder_of(&c, ""), Some(0));
        assert_eq!(doc.folders[0].id, "a");
        // Ручное include побеждает правила, даже в папке правее.
        doc.folders[2].include.push(key("grp"));
        assert_eq!(doc.folder_of(&c, ""), Some(2));
    }

    #[test]
    fn exclude_drops_to_next_matching_or_inbox() {
        let any_a = Rules { identities: vec!["me@a".into()], ..Default::default() };
        let direct = Rules { kind: Kind::Direct, ..Default::default() };
        let mut doc = FoldersDoc {
            folders: vec![folder("a", any_a), folder("d", direct)],
            ..Default::default()
        };
        let c = conv("1", "me@a", &["bob@b"], false);
        doc.folders[0].exclude.push(key("1"));
        assert_eq!(doc.folder_of(&c, ""), Some(1));
        doc.folders[1].exclude.push(key("1"));
        assert_eq!(doc.folder_of(&c, ""), None);
    }

    #[test]
    fn assign_moves_strictly_and_to_inbox() {
        let any_a = Rules { identities: vec!["me@a".into()], ..Default::default() };
        let mut doc = FoldersDoc {
            folders: vec![folder("a", any_a), folder("m", Rules::default())],
            ..Default::default()
        };
        let c = conv("1", "me@a", &["bob@b"], false);
        assert_eq!(doc.folder_of(&c, ""), Some(0));
        doc.assign(&c, "", Some(1));
        assert_eq!(doc.folder_of(&c, ""), Some(1));
        doc.assign(&c, "", None);
        assert_eq!(doc.folder_of(&c, ""), None, "в инбокс, хотя правила папки a его ловят");
        doc.assign(&c, "", Some(0));
        assert_eq!(doc.folder_of(&c, ""), Some(0));
        // Повторные переносы не копят дублей.
        doc.assign(&c, "", Some(0));
        assert_eq!(doc.folders[0].include.len(), 1);
        assert!(doc.folders[0].exclude.is_empty());
    }

    #[test]
    fn filter_and_counts() {
        let direct = Rules { kind: Kind::Direct, ..Default::default() };
        let doc = FoldersDoc { folders: vec![folder("d", direct)], ..Default::default() };
        let mut a = conv("a", "me@a", &["x@x"], false);
        a.unread_count = 3;
        let mut g = conv("g", "me@a", &["x@x", "y@y"], true);
        g.unread_count = 1;
        let b = conv("b", "me@a", &["z@z"], false);
        let all = vec![a, g, b];
        let ids = |v: Vec<Conversation>| v.into_iter().map(|c| c.id).collect::<Vec<_>>();
        assert_eq!(ids(filter(&doc, &all, "", Some(0))), vec!["a", "b"]);
        assert_eq!(ids(filter(&doc, &all, "", None)), vec!["g"]);
        assert_eq!(unread_counts(&doc, &all, ""), (vec![1], 1));
    }

    #[test]
    fn unknown_fields_survive_roundtrip() {
        let v = serde_json::json!({
            "folders": [{ "id": "a", "name": "A", "color": "#fff",
                          "rules": { "kind": "group", "future": 1 } }],
            "schema": 2
        });
        let doc = FoldersDoc::from_value(Some(&v));
        assert_eq!(doc.folders[0].rules.kind, Kind::Group);
        let back = doc.to_value();
        assert_eq!(back["schema"], 2);
        assert_eq!(back["folders"][0]["color"], "#fff");
        assert_eq!(back["folders"][0]["rules"]["future"], 1);
    }
}
