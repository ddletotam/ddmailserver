#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

mod cache;
mod imap;
mod session;
mod smtp;
mod tray;

use cache::Cache;
use session::SessionPool;
use tauri::Manager;

fn main() {
    env_logger::init();

    tauri::Builder::default()
        .plugin(tauri_plugin_shell::init())
        .plugin(tauri_plugin_notification::init())
        .manage(SessionPool::new())
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
            imap::set_flags,
            imap::fetch_identities,
            imap::fetch_avatar,
            imap::start_watching,
            smtp::send_message,
        ])
        .run(tauri::generate_context!())
        .expect("error while running DDMail");
}
