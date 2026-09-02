//! Persistent calendar-view settings, saved IMMEDIATELY on every change
//! (not on exit): which calendars are toggled off, per-calendar colour
//! overrides, and the 5/7-day + work-hours view toggles. Stored as
//! `calendar.json` next to `window.json`.

use std::collections::HashMap;
use std::fs;
use std::path::PathBuf;

use serde::{Deserialize, Serialize};

#[derive(Clone, Serialize, Deserialize)]
#[serde(default)]
pub struct CalendarSettings {
    /// Calendars the user toggled OFF. Stored as a deny-list so newly
    /// appearing calendars default to visible.
    pub hidden: Vec<i64>,
    /// Per-calendar colour overrides picked by the user (id → "#rrggbb").
    /// Takes precedence over both the server colour and the palette default.
    pub colors: HashMap<i64, String>,
    /// Sound on new-mail notifications (the file is de-facto the client
    /// settings store, calendar name notwithstanding).
    #[serde(default = "default_true")]
    pub notify_sound: bool,
    /// Working-day window (local hours). The grid always models 0–24, but
    /// when it can't all fit this is the band kept visible; outside it is
    /// shaded as non-work.
    #[serde(default = "default_work_start")]
    pub work_start_hour: i32,
    #[serde(default = "default_work_end")]
    pub work_end_hour: i32,
    /// Manual zoom levels (px). 0 = automatic fit. Set once the user
    /// ctrl-/ctrl-alt-scrolls; manual zoom then wins over autofit.
    #[serde(default)]
    pub manual_hour_height: f32,
    #[serde(default)]
    pub manual_col_width: f32,
    /// Диалог, открытый последним (`Conversation::id` — набор адресов, см.
    /// контракт §4). Стартовый выбор возвращается к нему, а не к первому в
    /// списке. Пусто / диалог исчез → первый с закэшированными телами.
    #[serde(default)]
    pub last_conversation: String,
}

/// `Default` — руками, а не `derive`: у derive'а это нули и `false`, тогда
/// как `#[serde(default = ...)]` у полей даёт 8..19 и звук включённым.
/// Расходились они не безобидно: `load()` при ОТСУТСТВУЮЩЕМ файле идёт через
/// `default()`, так что первый запуск получал рабочий день 0..1 и выключенный
/// звук — и первое же сохранение настроек цементировало это в файле.
impl Default for CalendarSettings {
    fn default() -> Self {
        Self {
            hidden: Vec::new(),
            colors: HashMap::new(),
            notify_sound: default_true(),
            work_start_hour: default_work_start(),
            work_end_hour: default_work_end(),
            manual_hour_height: 0.0,
            manual_col_width: 0.0,
            last_conversation: String::new(),
        }
    }
}

fn default_true() -> bool {
    true
}

fn default_work_start() -> i32 {
    8
}

fn default_work_end() -> i32 {
    19
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
