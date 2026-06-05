//! Content-permission policy for email rendering — port of the
//! svelte-era `permissionStore`.
//!
//! Three toggles per sender / globally:
//!   * `allow_media[addr]`  — load external images & background-images
//!     from messages by that sender.
//!   * `allow_scripts[addr]` — let `<script>`/event handlers survive
//!     the sanitizer for that sender (off by default; scripts almost
//!     never produce useful rendering and add risk).
//!   * `allow_domains`      — trusted hosts whose resources load no
//!     matter who sent the mail (e.g. `mc.yandex.ru`, MIME-server
//!     CDNs). Per-domain instead of per-sender so widgets work
//!     across senders.
//!
//! State lives on disk as JSON at
//! `$XDG_CONFIG_HOME/ru.letotam.ddmail/permissions.json` (or
//! `~/.config/...`); load on startup, save on every toggle.

use std::collections::HashSet;
use std::fs;
use std::path::PathBuf;

use serde::{Deserialize, Serialize};

#[derive(Default, Clone, Serialize, Deserialize)]
#[serde(default)]
pub struct Policy {
    pub allow_media: HashSet<String>,
    pub allow_scripts: HashSet<String>,
    pub allow_domains: HashSet<String>,
}

impl Policy {
    pub fn media_allowed(&self, sender: &str) -> bool {
        self.allow_media.contains(&sender.to_lowercase())
    }
    pub fn scripts_allowed(&self, sender: &str) -> bool {
        self.allow_scripts.contains(&sender.to_lowercase())
    }
    pub fn domain_allowed(&self, host: &str) -> bool {
        self.allow_domains.contains(&host.to_lowercase())
    }

    pub fn toggle_media(&mut self, sender: &str) -> bool {
        let k = sender.to_lowercase();
        if self.allow_media.contains(&k) {
            self.allow_media.remove(&k);
            false
        } else {
            self.allow_media.insert(k);
            true
        }
    }

    pub fn toggle_scripts(&mut self, sender: &str) -> bool {
        let k = sender.to_lowercase();
        if self.allow_scripts.contains(&k) {
            self.allow_scripts.remove(&k);
            false
        } else {
            self.allow_scripts.insert(k);
            true
        }
    }

    pub fn toggle_domain(&mut self, host: &str) -> bool {
        let k = host.to_lowercase();
        if self.allow_domains.contains(&k) {
            self.allow_domains.remove(&k);
            false
        } else {
            self.allow_domains.insert(k);
            true
        }
    }
}

fn policy_path() -> Option<PathBuf> {
    #[cfg(target_os = "windows")]
    {
        let appdata = std::env::var("APPDATA").ok()?;
        return Some(
            PathBuf::from(appdata)
                .join("ru.letotam.ddmail")
                .join("permissions.json"),
        );
    }
    #[cfg(target_os = "macos")]
    {
        let home = std::env::var("HOME").ok()?;
        return Some(
            PathBuf::from(home)
                .join("Library/Application Support/ru.letotam.ddmail/permissions.json"),
        );
    }
    #[cfg(target_os = "linux")]
    {
        let base = std::env::var("XDG_CONFIG_HOME")
            .ok()
            .map(PathBuf::from)
            .or_else(|| {
                std::env::var("HOME")
                    .ok()
                    .map(|h| PathBuf::from(h).join(".config"))
            })?;
        return Some(base.join("ru.letotam.ddmail").join("permissions.json"));
    }
    #[allow(unreachable_code)]
    None
}

pub fn load() -> Policy {
    let Some(path) = policy_path() else {
        return Policy::default();
    };
    let Ok(bytes) = fs::read(&path) else {
        return Policy::default();
    };
    serde_json::from_slice::<Policy>(&bytes).unwrap_or_default()
}

pub fn save(policy: &Policy) {
    let Some(path) = policy_path() else { return };
    if let Some(dir) = path.parent() {
        let _ = fs::create_dir_all(dir);
    }
    if let Ok(json) = serde_json::to_vec_pretty(policy) {
        let _ = fs::write(&path, json);
    }
}
