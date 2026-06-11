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

// ── Message ──

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct MessageRef {
    pub folder: String,
    pub uid: u32,
    /// Read state at conversation-list time. Defaults to true ("read") for
    /// sources that don't track it (IMAP fallback, old cache rows) — the
    /// scroll-to-unread feature then degrades to scroll-to-end.
    #[serde(default = "default_seen")]
    pub seen: bool,
}

fn default_seen() -> bool {
    true
}

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
    #[serde(default)]
    pub alarm_lead_min: i32,
}
