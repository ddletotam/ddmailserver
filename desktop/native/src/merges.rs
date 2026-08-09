//! Пользовательские объединения диалогов — «склейка» нескольких бесед
//! сайдбара в одну с возможностью обратного разъединения.
//!
//! Модель: каждая группа — список ключей исходных (сырых) диалогов
//! `(account, id)`; первый элемент — «первичный» диалог, чьи имя/аватар/
//! идентичность представляют склейку. Ключи отсутствующих сейчас диалогов
//! (например, удалённых) в группе не мешают — склейка собирается из тех,
//! что реально пришли в списке.
//!
//! Хранение — JSON рядом с permissions.json:
//! `%APPDATA%/ru.letotam.ddmail/merges.json` (per-OS путь как в policy.rs).

use std::fs;
use std::path::PathBuf;

use serde::{Deserialize, Serialize};

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct MergeKey {
    pub account: String,
    pub id: String,
}

#[derive(Default, Clone, Serialize, Deserialize)]
#[serde(default)]
pub struct Merges {
    pub groups: Vec<Vec<MergeKey>>,
}

impl Merges {
    /// Индекс группы, содержащей этот ключ.
    pub fn group_of(&self, key: &MergeKey) -> Option<usize> {
        self.groups.iter().position(|g| g.contains(key))
    }

    /// Члены группы этого диалога; одиночный диалог — группа из него самого.
    pub fn members_of(&self, key: &MergeKey) -> Vec<MergeKey> {
        match self.group_of(key) {
            Some(g) => self.groups[g].clone(),
            None => vec![key.clone()],
        }
    }

    /// Склеить два набора: убрать старые группы, содержащие любой из ключей,
    /// и записать одну общую. `target` идёт первым — его голова остаётся
    /// первичным диалогом объединения.
    pub fn merge(&mut self, target: Vec<MergeKey>, source: Vec<MergeKey>) {
        self.groups
            .retain(|g| !g.iter().any(|k| target.contains(k) || source.contains(k)));
        let mut group = target;
        for k in source {
            if !group.contains(&k) {
                group.push(k);
            }
        }
        if group.len() > 1 {
            self.groups.push(group);
        }
    }


    /// Перевести сохранённые id на схему «ключ диалога — набор адресов».
    ///
    /// Меняется ровно одна форма: диалог, все адреса которого мои (письмо с
    /// одной своей айдентики на другую). Раньше он опознавался парой
    /// `{айдентика}|{отправитель}`, теперь — `{все мои через +}|self`. Всё
    /// остальное сохранило прежний вид байт в байт, поэтому и не трогается.
    ///
    /// Возвращает `true`, если что-то переписано — тогда файл надо сохранить.
    pub fn migrate_self_chat_ids(&mut self, identities: &[String]) -> bool {
        let mine: Vec<String> = identities.iter().map(|i| i.to_lowercase()).collect();
        let is_mine = |a: &str| mine.iter().any(|m| m == a);
        let mut changed = false;
        for group in &mut self.groups {
            for key in group.iter_mut() {
                let Some((left, right)) = key.id.split_once('|') else { continue };
                let (left, right) = (left.to_lowercase(), right.to_lowercase());
                if right.contains(':') || right.contains(',') {
                    continue; // групповая форма — состав менялся не так
                }
                if !is_mine(&left) || !is_mine(&right) {
                    continue;
                }
                let mut parts = vec![left, right];
                parts.sort();
                parts.dedup();
                let next = format!("{}|self", parts.join("+"));
                if next != key.id {
                    key.id = next;
                    changed = true;
                }
            }
        }
        changed
    }

    /// Разъединить: убрать группу, содержащую ключ.
    pub fn unmerge(&mut self, key: &MergeKey) {
        if let Some(g) = self.group_of(key) {
            self.groups.remove(g);
        }
    }
}

fn merges_path() -> Option<PathBuf> {
    #[cfg(target_os = "windows")]
    {
        let appdata = std::env::var("APPDATA").ok()?;
        return Some(PathBuf::from(appdata).join("ru.letotam.ddmail").join("merges.json"));
    }
    #[cfg(target_os = "macos")]
    {
        let home = std::env::var("HOME").ok()?;
        return Some(
            PathBuf::from(home).join("Library/Application Support/ru.letotam.ddmail/merges.json"),
        );
    }
    #[cfg(target_os = "linux")]
    {
        let base = std::env::var("XDG_CONFIG_HOME")
            .ok()
            .map(PathBuf::from)
            .or_else(|| {
                std::env::var("HOME").ok().map(|h| PathBuf::from(h).join(".config"))
            })?;
        return Some(base.join("ru.letotam.ddmail").join("merges.json"));
    }
    #[allow(unreachable_code)]
    None
}

pub fn load() -> Merges {
    let Some(path) = merges_path() else {
        return Merges::default();
    };
    let Ok(bytes) = fs::read(&path) else {
        return Merges::default();
    };
    serde_json::from_slice::<Merges>(&bytes).unwrap_or_default()
}

pub fn save(merges: &Merges) {
    let Some(path) = merges_path() else { return };
    if let Some(dir) = path.parent() {
        let _ = fs::create_dir_all(dir);
    }
    if let Ok(json) = serde_json::to_vec_pretty(merges) {
        let _ = fs::write(&path, json);
    }
}

#[cfg(test)]
mod tests {
    use super::{MergeKey, Merges};

    fn k(id: &str) -> MergeKey {
        MergeKey { account: "acc".into(), id: id.into() }
    }

    #[test]
    fn migrates_a_chat_where_both_addresses_are_mine() {
        let mut m = Merges { groups: vec![vec![k("me@b.ru|me@a.ru"), k("me@a.ru|bob@x.ru")]] };
        assert!(m.migrate_self_chat_ids(&["me@a.ru".into(), "me@b.ru".into()]));
        assert_eq!(m.groups[0][0].id, "me@a.ru+me@b.ru|self");
        // Обычный диалог не тронут — его id и не менялся.
        assert_eq!(m.groups[0][1].id, "me@a.ru|bob@x.ru");
    }

    #[test]
    fn self_to_the_same_address_collapses_to_one() {
        let mut m = Merges { groups: vec![vec![k("me@a.ru|me@a.ru")]] };
        assert!(m.migrate_self_chat_ids(&["me@a.ru".into()]));
        assert_eq!(m.groups[0][0].id, "me@a.ru|self");
    }

    #[test]
    fn leaves_groups_and_foreign_counterparts_alone() {
        let before = Merges {
            groups: vec![vec![k("me@a.ru|group:bob@x.ru,carol@x.ru"), k("me@a.ru|bob@x.ru")]],
        };
        let mut m = before.clone();
        assert!(!m.migrate_self_chat_ids(&["me@a.ru".into(), "me@b.ru".into()]));
        assert_eq!(m.groups, before.groups);
    }

    #[test]
    fn is_idempotent() {
        let mut m = Merges { groups: vec![vec![k("me@b.ru|me@a.ru")]] };
        let ids = ["me@a.ru".to_string(), "me@b.ru".to_string()];
        assert!(m.migrate_self_chat_ids(&ids));
        assert!(!m.migrate_self_chat_ids(&ids));
    }
}
