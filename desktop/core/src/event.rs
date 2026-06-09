//! UI-agnostic engine events: the engine pushes `EngineEvent`s into a
//! `Notifier` closure that the UI layer provides (the native client forwards
//! them onto the Slint event loop). No UI framework is referenced here.

use std::sync::Arc;

#[derive(Debug, Clone)]
pub enum EngineEvent {
    /// Connection lifecycle: state is "connecting" | "connected" | "error".
    ConnectionState { state: String, message: Option<String> },
    /// New mail arrived in `folder` (`count` messages).
    NewMail { folder: String, count: u32 },
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
