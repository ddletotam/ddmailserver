use serde::{Deserialize, Deserializer, Serialize};

/// Tolerate `null` for Vec fields. Native server returns `null` instead of `[]`
/// when a slice is empty (Go marshals nil slices as null), and Rust's default
/// `Vec` deserializer rejects null. Apply via `#[serde(default, deserialize_with = "null_as_empty_vec")]`.
fn null_as_empty_vec<'de, D, T>(d: D) -> Result<Vec<T>, D::Error>
where
    D: Deserializer<'de>,
    T: Deserialize<'de>,
{
    Option::<Vec<T>>::deserialize(d).map(|v| v.unwrap_or_default())
}

// ── Identity ──

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Identity {
    pub email: String,
    pub name: String,
    pub signature: String,
    pub is_default: bool,
    #[serde(default)]
    pub color: String, // assigned client-side, pastel
    /// Per-identity capabilities from the server: may the client offer
    /// creating a new event / contact "under" this address. The create-under
    /// picker is built only from identities where these are true.
    #[serde(default)]
    pub can_create_events: bool,
    #[serde(default)]
    pub can_create_contacts: bool,
}

// ── Folder ──

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Folder {
    pub name: String,
    pub delimiter: String,
    pub unread: u32,
    pub total: u32,
    #[serde(default)]
    pub special_use: String, // "\\Inbox", "\\Sent", "\\Drafts", "\\Trash", "\\Junk", "\\Archive", or ""
}

// ── Contact ──

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ContactInfo {
    pub name: String,
    pub addr: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Contact {
    pub email: String,
    pub name: String,
    pub source: String, // "auto" | "carddav"
}

/// One row of the unified address book served by the DDMail server
/// (`GET /contacts`). Aggregated across all the user's sources — membership
/// is deliberately not exposed.
#[derive(Debug, Clone, Serialize, Deserialize, Default)]
pub struct DesktopContact {
    pub id: i64,
    /// Which account this row came from (engine-tagged, not from the server) —
    /// so multi-account writes route back to the owning connection.
    #[serde(default)]
    pub account_key: String,
    /// vCard UID — populated by the standalone CardDAV client so edit/delete
    /// can resolve the resource; empty for the native path (which uses `id`).
    #[serde(default)]
    pub uid: String,
    #[serde(default)]
    pub full_name: String,
    #[serde(default, deserialize_with = "null_as_empty_vec")]
    pub emails: Vec<String>,
    #[serde(default, deserialize_with = "null_as_empty_vec")]
    pub phones: Vec<String>,
    #[serde(default)]
    pub organization: String,
    #[serde(default)]
    pub title: String,
    #[serde(default)]
    pub photo_url: String,
}

// ── Message ──

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct MessageRef {
    pub folder: String,
    pub uid: u32,
    /// Stable identity: the server's global key is (user_id, Message-ID). We
    /// echo it back on body/flag/delete so a row the server deleted+reinserted
    /// (upstream mirroring) still resolves — the volatile `uid` above can
    /// dangle. Empty for sources/rows without a Message-ID (degrades to `uid`).
    #[serde(default)]
    pub message_id: String,
    /// Read state at conversation-list time. Defaults to true ("read") for
    /// sources that don't track it (IMAP fallback, old cache rows) — the
    /// scroll-to-unread feature then degrades to scroll-to-end.
    #[serde(default = "default_seen")]
    pub seen: bool,
}

fn default_seen() -> bool {
    true
}

// ── Change journal (/changes) ──

/// One change-journal entry. `kind`: 1 = upsert (visible), 2 = delete tombstone.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ChangeEntry {
    pub seq: i64,
    pub message_id: String,
    pub kind: i32,
}

/// Response of GET /changes?since=seq. The client tracks `latest_seq` as its
/// cursor; `reset` means the cursor is unusable (new client / fell behind
/// retention) and a full conversation resync should adopt `latest_seq`.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ChangesResponse {
    #[serde(default, deserialize_with = "null_as_empty_vec")]
    pub entries: Vec<ChangeEntry>,
    pub latest_seq: i64,
    #[serde(default)]
    pub low_watermark: i64,
    #[serde(default)]
    pub reset: bool,
}

/// Journal kind constants mirroring the server (migration 043).
pub const CHANGE_KIND_UPSERT: i32 = 1;
pub const CHANGE_KIND_DELETE: i32 = 2;

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Conversation {
    pub id: String,
    pub label: String,
    pub avatar_hash: String,
    pub received_by: String, // which of our emails received this conversation's messages
    #[serde(default, deserialize_with = "null_as_empty_vec")]
    pub counterparts: Vec<ContactInfo>,
    pub is_group: bool,
    pub last_date: String,
    pub last_date_ts: i64,
    pub last_subject: String,
    pub unread_count: u32,
    pub total_count: u32,
    #[serde(default, deserialize_with = "null_as_empty_vec")]
    pub messages: Vec<MessageRef>,
    pub draft: Option<MessageRef>, // latest draft for this conversation
    /// Which account (server) this conversation came from. Empty from
    /// providers; the engine stamps it when merging the unified list, so the
    /// UI knows which account a reply / open / delete should target.
    #[serde(default)]
    pub account_key: String,
    /// Client-view flag: this row is a user-made merge of several source
    /// conversations (merges.json). Never serialized — providers and the
    /// cache only ever see raw conversations.
    #[serde(skip)]
    pub merged: bool,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct MessageEnvelope {
    pub uid: u32,
    pub folder: String,
    pub subject: String,
    pub from: String,
    pub from_addr: String,
    #[serde(default, deserialize_with = "null_as_empty_vec")]
    pub to: Vec<String>,
    #[serde(default, deserialize_with = "null_as_empty_vec")]
    pub to_addrs: Vec<String>,
    #[serde(default, deserialize_with = "null_as_empty_vec")]
    pub cc_addrs: Vec<String>,
    pub date: String,
    pub date_ts: i64,
    pub seen: bool,
    pub flagged: bool,
    pub has_attachments: bool,
    pub is_outgoing: bool,
    pub message_id: String,
    pub in_reply_to: String,
    #[serde(default, deserialize_with = "null_as_empty_vec")]
    pub references: Vec<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct MessageBody {
    pub uid: u32,
    pub folder: String,
    pub subject: String,
    pub from: String,
    pub from_addr: String,
    #[serde(default, deserialize_with = "null_as_empty_vec")]
    pub to: Vec<String>,
    #[serde(default, deserialize_with = "null_as_empty_vec")]
    pub cc: Vec<String>,
    pub date: String,
    pub date_ts: i64,
    pub html: Option<String>,
    pub text: Option<String>,
    #[serde(default, deserialize_with = "null_as_empty_vec")]
    pub attachments: Vec<Attachment>,
    pub is_outgoing: bool,
    pub message_id: String,
    pub in_reply_to: String,
    #[serde(default, deserialize_with = "null_as_empty_vec")]
    pub references: Vec<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Attachment {
    pub filename: String,
    pub mime_type: String,
    pub size: usize,
    pub index: usize,
}

// ── Outgoing ──

/// Attachment as a self-contained blob: filename + mime + raw bytes.
/// Serialised to JSON with the bytes base64-encoded (via serde_bytes), which
/// is how the native /send endpoint receives them. The IMAP path consumes the
/// same struct in-process — no encoding round-trip there.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct OutgoingAttachment {
    pub filename: String,
    pub mime_type: String,
    #[serde(with = "base64_bytes")]
    pub content: Vec<u8>,
    /// When set, the part is an inline body part (Content-ID + Content-Disposition: inline).
    /// HTML body is expected to reference it as `<img src="cid:{content_id}">`. The whole
    /// message is then framed as multipart/related so MUAs render the image in place,
    /// telegram-style. Empty/None = regular file attachment (multipart/mixed).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub content_id: Option<String>,
}

/// Serde codec that turns `Vec<u8>` into a base64 string (and back) for JSON
/// transport — matches Go's default `[]byte` JSON encoding so the native
/// /send endpoint can decode `attachments[].content` straight into `[]byte`.
mod base64_bytes {
    use base64::{engine::general_purpose::STANDARD, Engine as _};
    use serde::{Deserialize, Deserializer, Serializer};

    pub fn serialize<S: Serializer>(bytes: &[u8], s: S) -> Result<S::Ok, S::Error> {
        s.serialize_str(&STANDARD.encode(bytes))
    }

    pub fn deserialize<'de, D: Deserializer<'de>>(d: D) -> Result<Vec<u8>, D::Error> {
        let s = String::deserialize(d)?;
        STANDARD.decode(s.as_bytes()).map_err(serde::de::Error::custom)
    }
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct OutgoingMessage {
    pub from: String,
    pub to: Vec<String>,
    pub cc: Vec<String>,
    pub subject: String,
    pub html: String,
    pub text: String,
    pub in_reply_to: Option<String>,
    pub references: Option<String>,
    /// Filesystem paths picked by the user (composer dialog) for file-mode
    /// attachments. Populated by JS, resolved into `attachments` by the
    /// v2_send_message command. Providers themselves only look at `attachments`.
    #[serde(default)]
    pub attachment_paths: Vec<String>,
    /// Paths intended as inline images, paired with the cid the HTML body
    /// references via `<img src="cid:{content_id}">`. Resolved into
    /// `attachments` (with content_id set) by v2_send_message.
    #[serde(default)]
    pub inline_paths: Vec<InlineRef>,
    #[serde(default)]
    pub attachments: Vec<OutgoingAttachment>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct InlineRef {
    pub path: String,
    pub content_id: String,
}

/// One fetched inline part from a received message — bytes already base64-encoded
/// so the frontend can stuff it into a `data:` URL without round-tripping through
/// a binary IPC payload.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct InlinePart {
    pub mime_type: String,
    pub content_b64: String,
}

// ── Calendar ──

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct DesktopCalendar {
    pub id: i64,
    /// Engine-tagged owning account (multi-account routing).
    #[serde(default)]
    pub account_key: String,
    pub name: String,
    #[serde(default)]
    pub description: String,
    #[serde(default)]
    pub color: String,
    pub source_type: String, // "local" | "caldav" | "ics_import" | "ics_url"
    pub can_write: bool,
    pub enabled: bool,
    #[serde(default)]
    pub timezone: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct DesktopCalendarAttendee {
    pub email: String,
    #[serde(default)]
    pub name: String,
    #[serde(default)]
    pub role: String,
    #[serde(default)]
    pub partstat: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct DesktopCalendarEvent {
    pub id: i64,
    pub calendar_id: i64,
    /// Engine-tagged owning account (multi-account routing).
    #[serde(default)]
    pub account_key: String,
    pub uid: String,
    #[serde(default)]
    pub summary: String,
    #[serde(default)]
    pub description: String,
    #[serde(default)]
    pub location: String,
    pub dtstart: i64,        // ms since epoch
    pub dtend: Option<i64>,  // ms since epoch, may be null
    pub all_day: bool,
    #[serde(default)]
    pub organizer_email: String,
    #[serde(default)]
    pub organizer_name: String,
    #[serde(default)]
    pub status: String,
    #[serde(default)]
    pub rrule: String,
    #[serde(default)]
    pub recurrence_id: String,
    /// Deleted instances of a recurring event, as ms-since-epoch starts.
    /// The frontend filters these out when expanding `rrule` so removed
    /// occurrences don't reappear on the calendar.
    #[serde(default, deserialize_with = "null_as_empty_vec")]
    pub exdates: Vec<i64>,
    #[serde(default, deserialize_with = "null_as_empty_vec")]
    pub attendees: Vec<DesktopCalendarAttendee>,
    /// VALARM trigger as "minutes before start". 0 / missing → desktop
    /// should fall back to its global default lead-time.
    /// Deprecated in favour of `alarm_leads`; kept for older servers.
    #[serde(default)]
    pub alarm_lead_min: i32,
    /// Every VALARM as "minutes before start" in document order (0 = at
    /// start). Element 0 = primary reminder, the rest cascade — each fires
    /// only if the previous toast died by timeout.
    #[serde(default, deserialize_with = "null_as_empty_vec")]
    pub alarm_leads: Vec<i32>,
    /// Non-default VEVENT properties that aren't first-class fields —
    /// conference links, URL, CATEGORIES, CLASS etc. Uppercased names.
    #[serde(default, deserialize_with = "null_as_empty_vec")]
    pub extras: Vec<DesktopEventExtra>,
    /// Server-resolved presentation + capability so the source-blind client
    /// renders one calendar: the owning calendar's colour, whether THIS event
    /// may be edited/deleted, and the identity it belongs to. Advisory — the
    /// server re-checks on write and may still reject.
    #[serde(default)]
    pub color: String,
    #[serde(default)]
    pub editable: bool,
    #[serde(default)]
    pub deletable: bool,
    #[serde(default)]
    pub identity_email: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct DesktopEventExtra {
    pub name: String,
    pub value: String,
}
