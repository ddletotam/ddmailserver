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
    /// Messages were expunged server-side (deleted from another client or by a
    /// spam purge) — the client should resync conversations so deleted threads
    /// drop off instead of lingering with unloadable bodies.
    Expunged { folder: String },
    /// Flags changed server-side (read/starred in another client, or pulled
    /// from the source account by the sync). A cheap conversations delta is
    /// enough: flag changes bump updated_at, so the changed threads come back.
    FlagsChanged { folder: String },
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
