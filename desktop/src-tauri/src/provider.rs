use async_trait::async_trait;

use crate::types::*;

/// Unified interface for mail operations.
///
/// Two implementations:
/// - `ImapProvider` — standard IMAP/SMTP for third-party servers
/// - `NativeProvider` — HTTP/2 + WebSocket for DDMail server
#[async_trait]
pub trait MailProvider: Send + Sync {
    /// List all folders with unread/total counts.
    async fn list_folders(&self) -> Result<Vec<Folder>, String>;

    /// Fetch conversations (threaded message groups).
    async fn fetch_conversations(
        &self,
        our_addrs: &[String],
        limit: u32,
    ) -> Result<Vec<Conversation>, String>;

    /// Fetch full message bodies for the given refs.
    async fn fetch_conversation_messages(
        &self,
        our_addrs: &[String],
        messages: &[MessageRef],
    ) -> Result<Vec<MessageBody>, String>;

    /// Search messages by query string.
    async fn search_messages(
        &self,
        user_email: &str,
        query: &str,
    ) -> Result<Vec<MessageEnvelope>, String>;

    /// Set/clear flags on a single message.
    async fn set_flags(
        &self,
        folder: &str,
        uid: u32,
        flags: &str,
        add: bool,
    ) -> Result<(), String>;

    /// Set/clear flags on multiple messages in one session.
    async fn set_flags_batch(
        &self,
        messages: &[MessageRef],
        flags: &str,
        add: bool,
    ) -> Result<(), String>;

    /// Fetch raw RFC-822 source of a message.
    async fn fetch_message_source(
        &self,
        folder: &str,
        uid: u32,
    ) -> Result<String, String>;

    /// Fetch one inline body part (e.g. an inline image referenced as `cid:…`
    /// in the HTML body). Used by SandboxedEmail to substitute `cid:` URLs
    /// with renderable `data:` URLs before mounting the shadow DOM.
    async fn fetch_inline_part(
        &self,
        message_id: u32,
        content_id: &str,
    ) -> Result<InlinePart, String>;

    /// Fetch raw message bytes (for attachment extraction).
    async fn fetch_raw_message(
        &self,
        folder: &str,
        uid: u32,
    ) -> Result<Vec<u8>, String>;

    /// Fetch user identities (email aliases).
    async fn fetch_identities(&self) -> Result<Vec<Identity>, String>;

    /// Send a message via SMTP or HTTP.
    async fn send_message(
        &self,
        smtp_host: &str,
        smtp_port: u16,
        message: &OutgoingMessage,
    ) -> Result<String, String>;

    /// Start background push/IDLE listener. Emits Tauri events.
    async fn start_watching(
        &self,
        app: tauri::AppHandle,
    ) -> Result<(), String>;

    /// Fetch the avatar bytes for a remote email address.
    /// Returns an empty Vec when no source has anything (Caller renders the
    /// initial-bubble fallback). Native providers go through their server's
    /// chain (CardDAV → Libravatar → Gravatar → BIMI → favicon); IMAP-only
    /// providers fall back to Gravatar.
    async fn fetch_avatar(&self, email: &str) -> Result<Vec<u8>, String>;

    /// List the user's calendars. Native-only feature; IMAP-only providers
    /// return an error explaining the limitation.
    async fn list_calendars(&self) -> Result<Vec<DesktopCalendar>, String>;

    /// Fetch calendar events that overlap [from_ms, to_ms). When
    /// `calendar_ids` is empty the server returns events for all of the
    /// user's calendars (used on first paint before settings are loaded).
    async fn fetch_calendar_events(
        &self,
        from_ms: i64,
        to_ms: i64,
        calendar_ids: &[i64],
    ) -> Result<Vec<DesktopCalendarEvent>, String>;

    /// Provider type identifier.
    fn provider_type(&self) -> &'static str;
}
