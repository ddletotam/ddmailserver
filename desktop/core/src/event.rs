//! UI-agnostic engine events: the engine pushes `EngineEvent`s into a
//! `Notifier` closure that the UI layer provides (the native client forwards
//! them onto the Slint event loop). No UI framework is referenced here.

use std::sync::Arc;

#[derive(Debug, Clone)]
pub enum EngineEvent {
    /// Connection lifecycle: state is "connecting" | "connected" | "error".
    ConnectionState { state: String, message: Option<String> },
    /// New mail arrived in `folder`. `count` is the folder total (IMAP
    /// EXISTS semantics); `new_count`/`from`/`subject`/`message_id`
    /// describe the LAST new message of the batch for toast content
    /// (empty/zero when the source can't provide them).
    NewMail {
        folder: String,
        count: u32,
        new_count: u32,
        from: String,
        subject: String,
        message_id: i64,
    },
    /// A calendar changed server-side.
    CalendarUpdated { calendar_id: i64 },
    /// The session token was refreshed (native provider); persist it.
    TokenRefreshed { account_id: String, token: String },
}

/// Sink the UI provides to receive engine events. Cheap to clone.
pub type Notifier = Arc<dyn Fn(EngineEvent) + Send + Sync>;

/// A no-op notifier for headless/testing use.
pub fn noop_notifier() -> Notifier {
    Arc::new(|_| {})
}
