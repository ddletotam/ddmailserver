#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

mod cache;
mod commands;
mod imap;
mod imap_provider;
mod native_provider;
mod provider;
mod registry;
mod session;
mod smtp;
mod tray;
mod types;

use cache::Cache;
use registry::ProviderRegistry;
use session::SessionPool;
use tauri::Manager;

#[tauri::command]
fn open_url(url: String) -> Result<(), String> {
    // WSL2: explorer.exe opens URLs in Windows default browser
    if std::process::Command::new("explorer.exe").arg(&url).spawn().is_ok() {
        return Ok(());
    }
    // Linux native
    if std::process::Command::new("xdg-open").arg(&url).spawn().is_ok() {
        return Ok(());
    }
    // macOS
    if std::process::Command::new("open").arg(&url).spawn().is_ok() {
        return Ok(());
    }
    Err("Failed to open URL".into())
}

fn main() {
    env_logger::init();

    tauri::Builder::default()
        .plugin(tauri_plugin_window_state::Builder::default().build())
        .plugin(tauri_plugin_shell::init())
        .plugin(tauri_plugin_notification::init())
        .manage(SessionPool::new())
        .manage(ProviderRegistry::new())
        .setup(|app| {
            // Init cache in app data dir
            let app_dir = app.path().app_data_dir().map_err(|e| e.to_string())?;
            let cache = Cache::new(app_dir).map_err(|e| e.to_string())?;
            app.manage(cache);

            tray::create_tray(app.handle())?;
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
            imap::download_attachment,
            imap::fetch_identities,
            imap::fetch_avatar,
            imap::start_watching,
            smtp::send_message,
            open_url,
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
            commands::v2_fetch_message_source,
            commands::v2_fetch_identities,
            commands::v2_send_message,
            commands::v2_start_watching,
        ])
        .run(tauri::generate_context!())
        .expect("error while running DDMail");
}
