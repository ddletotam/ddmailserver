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

    /// Delete a batch of messages. Native: soft-deletes server-side. IMAP:
    /// STORE \Deleted then EXPUNGE.
    async fn delete_messages(
        &self,
        messages: &[MessageRef],
    ) -> Result<(), String>;

    /// Mark a conversation as spam by domain. Native: posts the domain rule
    /// + flags the messages on the server. IMAP fallback: STORE \Deleted +
    /// EXPUNGE — closest behaviour without server-side rules.
    async fn mark_spam_by_domain(
        &self,
        domain: &str,
        messages: &[MessageRef],
    ) -> Result<(), String>;

    /// Blacklist a sender (domain or address) AND hard-DELETE every
    /// message from them for the active user. The chat-header "Spam"
    /// quick-action: harder than `mark_spam_by_domain` (which only
    /// soft-deletes the listed conversation), this removes the rows
    /// entirely so they're not in the spam vault either. Native only.
    ///
    /// `message_ids` are the local server PKs from the visible
    /// conversation — the server also deletes those rows by id so
    /// outgoing-from-us threads (where from_addr is OUR address,
    /// not the counterpart's) actually disappear instead of just
    /// the rule being created and the row left to re-sync back.
    async fn blacklist_and_purge(&self, domain: &str, address: &str, message_ids: &[i64]) -> Result<i64, String>;

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

    /// Fetch one non-inline attachment by index. Returns the raw bytes and
    /// a best-effort MIME type — the caller is responsible for choosing
    /// a filename and writing to disk.
    async fn fetch_attachment(
        &self,
        folder: &str,
        uid: u32,
        index: usize,
    ) -> Result<(Vec<u8>, String), String>;

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

    /// Fetch an avatar for the given email. Returns `(bytes, mime)`; bytes
    /// are empty when no source has anything (the caller renders an initial-
    /// bubble fallback). MIME flows through unchanged so the frontend can put
    /// it in a `data:` URL — generic `image/*` doesn't render in Chromium.
    async fn fetch_avatar(&self, email: &str) -> Result<(Vec<u8>, String), String>;

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

    /// Update the requesting user's PARTSTAT on an event. PartStat must be
    /// one of ACCEPTED / DECLINED / TENTATIVE. Returns the value the server
    /// persisted so the caller can reconcile.
    async fn rsvp_event(&self, event_id: i64, partstat: &str) -> Result<String, String>;

    /// Edit a calendar event. The body is forwarded as-is — the server
    /// validates scope and applied fields. ImapProvider returns "not
    /// supported" since calendar editing requires the native backend.
    async fn patch_event(&self, event_id: i64, body: serde_json::Value) -> Result<(), String>;

    /// Create a calendar event on the named calendar (passed inside `body`).
    /// Returns the canonical server row as JSON; the desktop refreshes the
    /// view from cache + a fresh fetch anyway, but receiving the new id
    /// avoids the "the row I just made" flicker.
    async fn create_event(&self, body: serde_json::Value) -> Result<serde_json::Value, String>;

    /// Delete a calendar event — the entire row, for v1. The server also
    /// queues a reverse-sync so the upstream CalDAV server drops its copy.
    /// ImapProvider returns "not supported".
    async fn delete_event(&self, event_id: i64) -> Result<(), String>;

    /// Provider type identifier.
    fn provider_type(&self) -> &'static str;
}
