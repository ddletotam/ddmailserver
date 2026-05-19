//! Tray-icon entry point. Each OS has its own implementation file because
//! the icon format, theming expectations and click semantics differ enough
//! that a single shared codepath ends up full of `cfg!(target_os = ...)`
//! branches. The dispatcher below picks the right one at compile time —
//! anything else is dead code on a given platform.

use tauri::{AppHandle, Runtime};

#[cfg(target_os = "linux")]
mod linux;
#[cfg(target_os = "macos")]
mod macos;
#[cfg(target_os = "windows")]
mod windows;

mod common;

pub fn create_tray<R: Runtime>(app: &AppHandle<R>) -> Result<(), Box<dyn std::error::Error>> {
    #[cfg(target_os = "linux")]
    return linux::create(app);
    #[cfg(target_os = "macos")]
    return macos::create(app);
    #[cfg(target_os = "windows")]
    return windows::create(app);
    #[cfg(not(any(target_os = "linux", target_os = "macos", target_os = "windows")))]
    {
        let _ = app;
        Ok(())
    }
}

/// Push the current unread-message total to the tray. Linux and Windows
/// re-composite the icon with a blue dot when count > 0; macOS still
/// no-ops because the menu-bar template-icon convention rules out
/// per-app badges (Apple expects the count to live in the Dock).
pub fn set_unread(count: u32) {
    #[cfg(target_os = "linux")]
    linux::set_unread(count);
    #[cfg(target_os = "windows")]
    windows::set_unread(count);
    #[cfg(not(any(target_os = "linux", target_os = "windows")))]
    let _ = count;
}
