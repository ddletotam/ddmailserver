#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

mod cache;
mod commands;
mod imap;
mod imap_provider;
mod native_provider;
mod notify;
mod provider;
mod registry;
mod reminders;
mod session;
mod smtp;
mod tray;
mod types;

use std::sync::Arc;

use cache::Cache;
use registry::ProviderRegistry;
use reminders::ReminderScheduler;
use session::SessionPool;
use tauri::Manager;

/// Push the latest unread-conversation total from the JS store down to the
/// native tray, which composites a notification dot when count > 0.
#[tauri::command]
fn set_tray_unread(count: u32) {
    tray::set_unread(count);
}

/// Close a window by label from JS — bypasses the JS-side allow-close
/// capability check so secondary windows (calendar, snooze) can always
/// close themselves regardless of the capabilities JSON state.
#[tauri::command]
fn close_window(app: tauri::AppHandle, label: String) -> Result<(), String> {
    app.get_webview_window(&label)
        .ok_or_else(|| format!("no window: {label}"))?
        .close()
        .map_err(|e| e.to_string())
}

#[tauri::command]
fn open_url(app: tauri::AppHandle, url: String) -> Result<(), String> {
    use tauri_plugin_shell::ShellExt;
    // Shell plugin routes through the per-OS native opener:
    //   * Windows  → ShellExecuteW("open", url)   — default browser
    //   * macOS    → /usr/bin/open
    //   * Linux    → xdg-open (with WSL fallback handled inside the plugin)
    // The previous implementation tried `explorer.exe URL` first, which on
    // native Windows occasionally launches Explorer with the URL as a path
    // instead of handing it to the http-protocol handler.
    app.shell()
        .open(&url, None)
        .map_err(|e| e.to_string())
}

fn main() {
    env_logger::init();

    tauri::Builder::default()
        // Single-instance guard: a second `ddmail` launch hands its CLI
        // args to the running process and exits. We use that signal to
        // raise/focus the existing main window so the user gets the
        // familiar "click again to bring it forward" behaviour instead
        // of a duplicate WebView, duplicate IMAP sessions, and racing
        // reminder schedulers.
        .plugin(tauri_plugin_single_instance::init(|app, _argv, _cwd| {
            if let Some(w) = app.get_webview_window("main") {
                let _ = w.unminimize();
                let _ = w.show();
                let _ = w.set_focus();
            }
        }))
        .plugin(
            // Default builder has no state flags set, so the plugin loads
            // but doesn't persist anything. Enable the full set: position,
            // size, maximized/fullscreen/visible/decorations. Without this
            // every launch on Windows lands at the tauri.conf.json default
            // 1200×800 in the upper-left corner.
            tauri_plugin_window_state::Builder::default()
                .with_state_flags(tauri_plugin_window_state::StateFlags::all())
                .build(),
        )
        .plugin(tauri_plugin_shell::init())
        .plugin(tauri_plugin_notification::init())
        .plugin(tauri_plugin_dialog::init())
        .manage(SessionPool::new())
        .manage(ProviderRegistry::new())
        .setup(|app| {
            // Init cache in app data dir. Wrapped in an Arc so the
            // long-lived reminder scheduler can hold its own clone — the
            // tauri::State path needs a ref with the app's lifetime, which
            // a Tokio task can't satisfy.
            let app_dir = app.path().app_data_dir().map_err(|e| e.to_string())?;
            let cache = Arc::new(Cache::new(app_dir).map_err(|e| e.to_string())?);
            app.manage(cache.clone());

            tray::create_tray(app.handle())?;

            // Reminder scheduler — owns its own Tokio task and a clone of
            // the cache. The handle stays in app state so commands can
            // push fresh schedules into it.
            let scheduler = ReminderScheduler::spawn(cache.clone(), app.handle().clone());
            app.manage(scheduler);

            Ok(())
        })
        .invoke_handler(tauri::generate_handler![
            imap::connect,
            imap::list_folders,
            imap::load_cached_conversations,
            imap::fetch_conversations,
            imap::load_cached_messages,
            imap::fetch_conversation_messages,
            imap::search_messages,
            imap::search_contacts,
            imap::set_flags,
            imap::set_flags_batch,
            imap::fetch_message_source,
            imap::fetch_identities,
            imap::start_watching,
            smtp::send_message,
            close_window,
            open_url,
            set_tray_unread,
            reminders::schedule_reminders,
            reminders::reminder_action,
            reminders::get_reminder,
            // v2 commands (provider-based)
            commands::detect_server,
            commands::native_login,
            commands::activate_account,
            commands::v2_list_folders,
            commands::v2_fetch_conversations,
            commands::v2_fetch_conversation_messages,
            commands::v2_search_messages,
            commands::v2_set_flags,
            commands::v2_set_flags_batch,
            commands::v2_delete_messages,
            commands::v2_mark_spam_by_domain,
            commands::v2_blacklist_and_purge,
            commands::v2_fetch_message_source,
            commands::v2_download_attachment,
            commands::v2_open_attachment_with,
            commands::v2_save_attachment_to_path,
            commands::v2_fetch_inline_part,
            commands::v2_fetch_identities,
            commands::v2_send_message,
            commands::v2_start_watching,
            commands::v2_list_calendars,
            commands::v2_fetch_calendar_events,
            commands::v2_rsvp_event,
            commands::v2_patch_event,
            commands::v2_create_event,
            commands::v2_delete_event,
            commands::v2_fetch_avatar,
        ])
        .run(tauri::generate_context!())
        .expect("error while running DDMail");
}
