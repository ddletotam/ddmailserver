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
