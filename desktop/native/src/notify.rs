//! Thin wrapper over `notify-rust` for desktop notifications. Cross-platform
//! (Linux zbus / Windows WinRT toast / macOS) behind one call; failures are
//! swallowed — a missing notification daemon must never crash the client.

/// Show a desktop notification. Best-effort.
pub fn notify(title: &str, body: &str) {
    if let Err(e) = notify_rust::Notification::new()
        .summary(title)
        .body(body)
        .show()
    {
        eprintln!("notify: {e}");
    }
}
