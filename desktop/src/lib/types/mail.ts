export interface Folder {
  name: string;
  delimiter: string;
  unread: number;
  total: number;
  special_use: string; // "\\Inbox", "\\Sent", "\\Drafts", etc.
}

export interface ContactInfo {
  name: string;
  addr: string;
}

export interface MessageRef {
  folder: string;
  uid: number;
}

export interface Conversation {
  id: string;
  label: string;
  avatar_hash: string;
  received_by: string;
  counterparts: ContactInfo[];
  is_group: boolean;
  last_date: string;
  last_date_ts: number;
  last_subject: string;
  unread_count: number;
  total_count: number;
  messages: MessageRef[];
  draft: MessageRef | null;
}

export interface MessageEnvelope {
  uid: number;
  folder: string;
  subject: string;
  from: string;
  from_addr: string;
  to: string[];
  to_addrs: string[];
  cc_addrs: string[];
  date: string;
  date_ts: number;
  seen: boolean;
  flagged: boolean;
  has_attachments: boolean;
  is_outgoing: boolean;
  message_id: string;
  in_reply_to: string;
  references: string[];
}

export interface MessageBody {
  uid: number;
  folder: string;
  subject: string;
  from: string;
  from_addr: string;
  to: string[];
  cc: string[];
  date: string;
  date_ts: number;
  html: string | null;
  text: string | null;
  attachments: Attachment[];
  is_outgoing: boolean;
  message_id: string;
  in_reply_to: string;
  references: string[];
}

export interface Attachment {
  filename: string;
  mime_type: string;
  size: number;
  index: number;
}

export interface Account {
  id: string;
  name: string;
  email: string;
  imap_host: string;
  imap_port: number;
  smtp_host: string;
  smtp_port: number;
  username: string;
  password: string;
  use_tls: boolean;
  // Provider abstraction (auto-detected)
  provider_type?: "imap" | "native";
  native_url?: string;
  native_token?: string;
}

export interface OutgoingMessage {
  from: string;
  to: string[];
  cc: string[];
  subject: string;
  html: string;
  text: string;
  in_reply_to: string | null;
  references: string | null;
  attachment_paths: string[];
}

export interface Contact {
  email: string;
  name: string;
  source: string; // "auto" | "carddav"
}

export interface Identity {
  email: string;
  name: string;
  signature: string;
  is_default: boolean;
  color: string;
}
