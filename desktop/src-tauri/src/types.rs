use serde::{Deserialize, Serialize};

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
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Conversation {
    pub id: String,
    pub label: String,
    pub avatar_hash: String,
    pub received_by: String, // which of our emails received this conversation's messages
    pub counterparts: Vec<ContactInfo>,
    pub is_group: bool,
    pub last_date: String,
    pub last_date_ts: i64,
    pub last_subject: String,
    pub unread_count: u32,
    pub total_count: u32,
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
    pub to: Vec<String>,
    pub to_addrs: Vec<String>,
    pub cc_addrs: Vec<String>,
    pub date: String,
    pub date_ts: i64,
    pub seen: bool,
    pub flagged: bool,
    pub has_attachments: bool,
    pub is_outgoing: bool,
    pub message_id: String,
    pub in_reply_to: String,
    pub references: Vec<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct MessageBody {
    pub uid: u32,
    pub folder: String,
    pub subject: String,
    pub from: String,
    pub from_addr: String,
    pub to: Vec<String>,
    pub cc: Vec<String>,
    pub date: String,
    pub date_ts: i64,
    pub html: Option<String>,
    pub text: Option<String>,
    pub attachments: Vec<Attachment>,
    pub is_outgoing: bool,
    pub message_id: String,
    pub in_reply_to: String,
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
    #[serde(default)]
    pub attachment_paths: Vec<String>,
}
