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
    /// Legacy shared per-host allow-list (pre-«Медиа…» menu). Read as a
    /// synonym of media_hosts; new toggles write media_hosts/script_hosts.
    pub allow_domains: HashSet<String>,
    /// Monotonic generation, bumped on every mutation and persisted with
    /// the policy. Part of the rendered-texture cache key (RAM and disk),
    /// so it must stay monotonic ACROSS restarts — a session-local counter
    /// would resurrect textures rendered under a pre-restart policy.
    pub generation: u64,
    /// «Разрешить всё» — master switch, overrides everything below.
    pub allow_all: bool,
    /// «Изображения → разрешить все» / «Скрипты → разрешить все».
    pub allow_all_media: bool,
    pub allow_all_scripts: bool,
    /// Per-host allow-lists, split by resource class.
    pub media_hosts: HashSet<String>,
    pub script_hosts: HashSet<String>,
}

impl Policy {
    pub fn media_allowed(&self, sender: &str) -> bool {
        self.allow_all || self.allow_all_media || self.allow_media.contains(&sender.to_lowercase())
    }
    pub fn scripts_allowed(&self, sender: &str) -> bool {
        self.allow_all
            || self.allow_all_scripts
            || self.allow_scripts.contains(&sender.to_lowercase())
    }
    /// Image/CSS resources from this host may load.
    pub fn domain_allowed(&self, host: &str) -> bool {
        if self.allow_all || self.allow_all_media {
            return true;
        }
        let k = host.to_lowercase();
        self.media_hosts.contains(&k) || self.allow_domains.contains(&k)
    }
    /// External <script src> from this host may survive sanitization.
    pub fn script_host_allowed(&self, host: &str) -> bool {
        self.allow_all || self.allow_all_scripts || self.script_hosts.contains(&host.to_lowercase())
    }

    pub fn toggle_media_host(&mut self, host: &str) -> bool {
        let k = host.to_lowercase();
        // Migrate a legacy allow_domains entry into media_hosts on touch.
        if self.allow_domains.remove(&k) || self.media_hosts.remove(&k) {
            false
        } else {
            self.media_hosts.insert(k);
            true
        }
    }

    pub fn toggle_script_host(&mut self, host: &str) -> bool {
        let k = host.to_lowercase();
        if self.script_hosts.remove(&k) {
            false
        } else {
            self.script_hosts.insert(k);
            true
        }
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
