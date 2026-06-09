//! Persistent window-state: window size + sidebar width, restored on launch.
//!
//! Stored as JSON at `$XDG_CONFIG_HOME/ru.letotam.ddmail/window.json`
//! (Linux), `%APPDATA%\ru.letotam.ddmail\window.json` (Windows), or
//! `~/Library/Application Support/ru.letotam.ddmail/window.json`
//! (macOS).
//!
//! We save on close-requested; the JSON is cheap, so adding more save
//! points is easy if a hard-kill loses the last state.

use std::fs;
use std::path::PathBuf;

use serde::{Deserialize, Serialize};

#[derive(Clone, Serialize, Deserialize)]
#[serde(default)]
pub struct WindowState {
    pub width: f32,
    pub height: f32,
    pub sidebar_width: f32,
}

impl Default for WindowState {
    fn default() -> Self {
        Self {
            width: 1100.0,
            height: 760.0,
            sidebar_width: 320.0,
        }
    }
}

impl WindowState {
    pub fn is_sane(&self) -> bool {
        self.width >= 600.0
            && self.height >= 400.0
            && self.width <= 10_000.0
            && self.height <= 10_000.0
            && self.sidebar_width >= 180.0
            && self.sidebar_width <= 640.0
    }
}

fn state_path() -> Option<PathBuf> {
    #[cfg(target_os = "windows")]
    {
        let appdata = std::env::var("APPDATA").ok()?;
        return Some(
            PathBuf::from(appdata)
                .join("ru.letotam.ddmail")
                .join("window.json"),
        );
    }
    #[cfg(target_os = "macos")]
    {
        let home = std::env::var("HOME").ok()?;
        return Some(
            PathBuf::from(home)
                .join("Library/Application Support/ru.letotam.ddmail/window.json"),
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
        return Some(base.join("ru.letotam.ddmail").join("window.json"));
    }
    #[allow(unreachable_code)]
    None
}

pub fn load() -> WindowState {
    let Some(path) = state_path() else {
        return WindowState::default();
    };
    let Ok(bytes) = fs::read(&path) else {
        return WindowState::default();
    };
    let parsed: WindowState =
        serde_json::from_slice(&bytes).unwrap_or_default();
    if parsed.is_sane() {
        parsed
    } else {
        WindowState::default()
    }
}

pub fn save(state: &WindowState) {
    let Some(path) = state_path() else { return };
    if let Some(dir) = path.parent() {
        let _ = fs::create_dir_all(dir);
    }
    if let Ok(json) = serde_json::to_vec_pretty(state) {
        let _ = fs::write(&path, json);
    }
}
