//! Windows reminder toast — stub.
//!
//! Real implementation will build an XML toast template with an
//! `<input type="selection">` dropdown for snooze offsets plus an OK
//! action. Requires an AppUserModelID registered with the COM activator
//! class (Tauri's installer is the natural place to wire this up).

use std::sync::Arc;
use tauri::{AppHandle, Runtime};

use crate::cache::{Cache, ReminderRow};

pub(super) fn show_reminder<R: Runtime>(
    _app: &AppHandle<R>,
    _cache: &Arc<Cache>,
    row: &ReminderRow,
) -> Result<(), String> {
    log::info!(
        "[reminders] Windows notifier not yet implemented; would have shown event {} at {}",
        row.event_id, row.occurrence_start_ms
    );
    Ok(())
}
