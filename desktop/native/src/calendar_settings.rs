//! Persistent calendar-view settings, saved IMMEDIATELY on every change
//! (not on exit): which calendars are toggled off, per-calendar colour
//! overrides, the left-panel collapsed state, and the 5/7-day + work-hours
//! view toggles. Stored as `calendar.json` next to `window.json`.

use std::collections::HashMap;
use std::fs;
use std::path::PathBuf;

use serde::{Deserialize, Serialize};

#[derive(Clone, Default, Serialize, Deserialize)]
#[serde(default)]
pub struct CalendarSettings {
    /// Calendars the user toggled OFF. Stored as a deny-list so newly
    /// appearing calendars default to visible.
    pub hidden: Vec<i64>,
    /// Per-calendar colour overrides picked by the user (id → "#rrggbb").
    /// Takes precedence over both the server colour and the palette default.
    pub colors: HashMap<i64, String>,
    pub panel_collapsed: bool,
    /// true = 5-day work week.
    pub workdays_only: bool,
    /// true = full 0–24 hour range.
    pub show_non_work_hours: bool,
}

fn settings_path() -> Option<PathBuf> {
    #[cfg(target_os = "windows")]
    {
        let appdata = std::env::var("APPDATA").ok()?;
        return Some(
            PathBuf::from(appdata)
                .join("ru.letotam.ddmail")
                .join("calendar.json"),
        );
    }
    #[cfg(target_os = "macos")]
    {
        let home = std::env::var("HOME").ok()?;
        return Some(
            PathBuf::from(home)
                .join("Library/Application Support/ru.letotam.ddmail/calendar.json"),
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
        return Some(base.join("ru.letotam.ddmail").join("calendar.json"));
    }
    #[allow(unreachable_code)]
    None
}

pub fn load() -> CalendarSettings {
    let Some(path) = settings_path() else {
        return CalendarSettings::default();
    };
    let Ok(bytes) = fs::read(&path) else {
        return CalendarSettings::default();
    };
    serde_json::from_slice(&bytes).unwrap_or_default()
}

pub fn save(settings: &CalendarSettings) {
    let Some(path) = settings_path() else { return };
    if let Some(dir) = path.parent() {
        let _ = fs::create_dir_all(dir);
    }
    if let Ok(json) = serde_json::to_vec_pretty(settings) {
        let _ = fs::write(&path, json);
    }
}
