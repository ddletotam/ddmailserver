//! UI-agnostic engine events: the engine pushes `EngineEvent`s into a
//! `Notifier` closure that the UI layer provides (the native client forwards
//! them onto the Slint event loop). No UI framework is referenced here.

use std::sync::Arc;

#[derive(Debug, Clone)]
pub enum EngineEvent {
    /// Connection lifecycle: state is "connecting" | "connected" | "error"
    /// | "auth".
    ///
    /// `"auth"` — отдельный случай, а не разновидность `"error"`: токен
    /// просрочен настолько, что обменять его на свежий сервер уже не даёт
    /// (30-дневный потолок на refresh), и добыть новый нечем — нужен пароль
    /// пользователя. Ретраи такое не лечат, поэтому UI по этому состоянию
    /// просит войти заново, а watcher перестаёт переподключаться.
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
    /// Our own outgoing message has landed in Sent.
    ///
    /// Deliberately separate from `NewMail`: no toast, no sound — the user just
    /// pressed Send. What it does mean is that the conversation the message
    /// belongs to now exists server-side, which is the one moment worth
    /// refetching. Sending is asynchronous, so this arrives well after the
    /// client was told «отправлено»; before it existed the client guessed with a
    /// 2.5s timer and a reply sent from a different address had nothing to jump
    /// to yet.
    MessageSent,
    /// A calendar changed server-side.
    CalendarUpdated { calendar_id: i64 },
    /// The set of accounts, calendar sources or contact sources changed
    /// server-side — a .mobileconfig import can add all three at once.
    ///
    /// Identities are cached and normally refreshed only on a full sync,
    /// because they change rarely. Without this event an account imported
    /// while the client was running stayed invisible — no sidebar tint, no
    /// from-picker entry, no calendar — until the next restart.
    IdentitiesChanged,
    /// The session token was refreshed (native provider); persist it.
    TokenRefreshed { account_id: String, token: String },
}

/// Sink the UI provides to receive engine events. Cheap to clone.
pub type Notifier = Arc<dyn Fn(EngineEvent) + Send + Sync>;

/// A no-op notifier for headless/testing use.
pub fn noop_notifier() -> Notifier {
    Arc::new(|_| {})
}
