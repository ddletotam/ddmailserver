use rusqlite::{Connection, params};
use std::path::PathBuf;
use std::sync::Mutex;
use serde::{Serialize, Deserialize};

use crate::imap::{Conversation, ContactInfo, MessageRef, MessageBody, Attachment, Identity};

pub struct Cache {
    conn: Mutex<Connection>,
}

impl Cache {
    pub fn new(app_dir: PathBuf) -> Result<Self, String> {
        std::fs::create_dir_all(&app_dir).map_err(|e| format!("mkdir: {e}"))?;
        let db_path = app_dir.join("cache.db");
        let conn = Connection::open(&db_path).map_err(|e| format!("SQLite open: {e}"))?;

        conn.execute_batch("
            PRAGMA journal_mode=WAL;
            PRAGMA synchronous=NORMAL;

            CREATE TABLE IF NOT EXISTS conversations (
                id TEXT PRIMARY KEY,
                account_key TEXT NOT NULL,
                label TEXT NOT NULL,
                avatar_hash TEXT NOT NULL DEFAULT '',
                counterpart_name TEXT NOT NULL DEFAULT '',
                counterpart_addr TEXT NOT NULL DEFAULT '',
                is_group INTEGER NOT NULL DEFAULT 0,
                last_date TEXT NOT NULL DEFAULT '',
                last_date_ts INTEGER NOT NULL DEFAULT 0,
                last_preview TEXT NOT NULL DEFAULT '',
                last_from TEXT NOT NULL DEFAULT '',
                unread_count INTEGER NOT NULL DEFAULT 0,
                total_count INTEGER NOT NULL DEFAULT 0,
                updated_at INTEGER NOT NULL DEFAULT 0
            );

            CREATE TABLE IF NOT EXISTS identities (
                email TEXT NOT NULL,
                account_key TEXT NOT NULL,
                name TEXT NOT NULL DEFAULT '',
                signature TEXT NOT NULL DEFAULT '',
                is_default INTEGER NOT NULL DEFAULT 0,
                color TEXT NOT NULL DEFAULT '',
                PRIMARY KEY(email, account_key)
            );

            CREATE TABLE IF NOT EXISTS avatar_cache (
                email TEXT PRIMARY KEY,
                png_data BLOB,
                cached_at INTEGER NOT NULL DEFAULT 0
            );

            -- Migrate: add avatar_hash if missing
            -- SQLite doesn't support IF NOT EXISTS for ALTER TABLE, so we try and ignore errors

            CREATE INDEX IF NOT EXISTS idx_conv_account ON conversations(account_key);
            CREATE INDEX IF NOT EXISTS idx_conv_date ON conversations(last_date_ts);

            CREATE TABLE IF NOT EXISTS conversation_messages (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                conversation_id TEXT NOT NULL,
                folder TEXT NOT NULL,
                uid INTEGER NOT NULL,
                UNIQUE(conversation_id, folder, uid)
            );

            CREATE TABLE IF NOT EXISTS message_bodies (
                folder TEXT NOT NULL,
                uid INTEGER NOT NULL,
                account_key TEXT NOT NULL,
                subject TEXT NOT NULL DEFAULT '',
                from_header TEXT NOT NULL DEFAULT '',
                from_addr TEXT NOT NULL DEFAULT '',
                to_header TEXT NOT NULL DEFAULT '',
                cc_header TEXT NOT NULL DEFAULT '',
                date_header TEXT NOT NULL DEFAULT '',
                date_ts INTEGER NOT NULL DEFAULT 0,
                html TEXT,
                text_body TEXT,
                attachments_json TEXT NOT NULL DEFAULT '[]',
                is_outgoing INTEGER NOT NULL DEFAULT 0,
                cached_at INTEGER NOT NULL DEFAULT 0,
                PRIMARY KEY(folder, uid, account_key)
            );
        ").map_err(|e| format!("SQLite init: {e}"))?;

        // Migrations for existing databases
        conn.execute("ALTER TABLE conversations ADD COLUMN avatar_hash TEXT NOT NULL DEFAULT ''", []).ok();
        conn.execute("ALTER TABLE conversations ADD COLUMN received_by TEXT NOT NULL DEFAULT ''", []).ok();

        Ok(Self { conn: Mutex::new(conn) })
    }

    /// Save conversations to cache (replaces all for this account).
    pub fn save_conversations(&self, account_key: &str, conversations: &[Conversation]) -> Result<(), String> {
        let conn = self.conn.lock().map_err(|e| format!("lock: {e}"))?;
        let now = chrono::Utc::now().timestamp();

        let tx = conn.unchecked_transaction().map_err(|e| format!("tx: {e}"))?;

        // Delete old conversations for this account
        tx.execute("DELETE FROM conversation_messages WHERE conversation_id IN \
            (SELECT id FROM conversations WHERE account_key = ?1)", params![account_key])
            .map_err(|e| format!("del msgs: {e}"))?;
        tx.execute("DELETE FROM conversations WHERE account_key = ?1", params![account_key])
            .map_err(|e| format!("del convs: {e}"))?;

        for conv in conversations {
            let cp_name = conv.counterparts.first().map(|c| c.name.as_str()).unwrap_or("");
            let cp_addr = conv.counterparts.first().map(|c| c.addr.as_str()).unwrap_or("");

            tx.execute(
                "INSERT INTO conversations (id, account_key, label, avatar_hash, received_by, counterpart_name, counterpart_addr, \
                 is_group, last_date, last_date_ts, last_preview, last_from, unread_count, total_count, updated_at) \
                 VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12, ?13, ?14, ?15)",
                params![
                    conv.id, account_key, conv.label, conv.avatar_hash, conv.received_by, cp_name, cp_addr,
                    conv.is_group as i32, conv.last_date, conv.last_date_ts,
                    conv.last_preview, conv.last_from, conv.unread_count, conv.total_count, now
                ],
            ).map_err(|e| format!("ins conv: {e}"))?;

            for mr in &conv.messages {
                tx.execute(
                    "INSERT OR IGNORE INTO conversation_messages (conversation_id, folder, uid) VALUES (?1, ?2, ?3)",
                    params![conv.id, mr.folder, mr.uid],
                ).map_err(|e| format!("ins msg ref: {e}"))?;
            }
        }

        tx.commit().map_err(|e| format!("commit: {e}"))?;
        Ok(())
    }

    /// Load cached conversations for an account.
    pub fn load_conversations(&self, account_key: &str) -> Result<Vec<Conversation>, String> {
        let conn = self.conn.lock().map_err(|e| format!("lock: {e}"))?;

        let mut stmt = conn.prepare(
            "SELECT id, label, avatar_hash, received_by, counterpart_name, counterpart_addr, is_group, \
             last_date, last_date_ts, last_preview, last_from, unread_count, total_count \
             FROM conversations WHERE account_key = ?1 ORDER BY last_date_ts DESC"
        ).map_err(|e| format!("prepare: {e}"))?;

        let rows = stmt.query_map(params![account_key], |row| {
            Ok((
                row.get::<_, String>(0)?,   // id
                row.get::<_, String>(1)?,   // label
                row.get::<_, String>(2)?,   // avatar_hash
                row.get::<_, String>(3)?,   // received_by
                row.get::<_, String>(4)?,   // cp_name
                row.get::<_, String>(5)?,   // cp_addr
                row.get::<_, bool>(6)?,     // is_group
                row.get::<_, String>(7)?,   // last_date
                row.get::<_, i64>(8)?,      // last_date_ts
                row.get::<_, String>(9)?,   // last_preview
                row.get::<_, String>(10)?,  // last_from
                row.get::<_, u32>(11)?,     // unread_count
                row.get::<_, u32>(12)?,     // total_count
            ))
        }).map_err(|e| format!("query: {e}"))?;

        let mut conversations = Vec::new();
        for row in rows {
            let (id, label, avatar_hash, received_by, cp_name, cp_addr, is_group, last_date, last_date_ts,
                 last_preview, last_from, unread_count, total_count) = row.map_err(|e| format!("row: {e}"))?;

            // Load message refs
            let mut msg_stmt = conn.prepare(
                "SELECT folder, uid FROM conversation_messages WHERE conversation_id = ?1"
            ).map_err(|e| format!("prepare msgs: {e}"))?;
            let messages: Vec<MessageRef> = msg_stmt.query_map(params![id], |r| {
                Ok(MessageRef { folder: r.get(0)?, uid: r.get(1)? })
            }).map_err(|e| format!("query msgs: {e}"))?
            .filter_map(|r| r.ok())
            .collect();

            conversations.push(Conversation {
                id,
                label,
                avatar_hash,
                received_by,
                counterparts: vec![ContactInfo { name: cp_name, addr: cp_addr }],
                is_group,
                last_date,
                last_date_ts,
                last_preview,
                last_from,
                unread_count,
                total_count,
                messages,
                draft: None, // Drafts not cached
            });
        }

        Ok(conversations)
    }

    /// Save message bodies to cache.
    pub fn save_message_bodies(&self, account_key: &str, bodies: &[MessageBody]) -> Result<(), String> {
        let conn = self.conn.lock().map_err(|e| format!("lock: {e}"))?;
        let now = chrono::Utc::now().timestamp();

        for body in bodies {
            let att_json = serde_json::to_string(&body.attachments).unwrap_or_else(|_| "[]".into());
            conn.execute(
                "INSERT OR REPLACE INTO message_bodies \
                 (folder, uid, account_key, subject, from_header, from_addr, to_header, cc_header, \
                  date_header, date_ts, html, text_body, attachments_json, is_outgoing, cached_at) \
                 VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12, ?13, ?14, ?15)",
                params![
                    body.folder, body.uid, account_key,
                    body.subject, body.from, body.from_addr,
                    body.to.join(", "), body.cc.join(", "),
                    body.date, body.date_ts,
                    body.html, body.text, att_json,
                    body.is_outgoing as i32, now
                ],
            ).map_err(|e| format!("ins body: {e}"))?;
        }
        Ok(())
    }

    /// Load cached message bodies.
    pub fn load_message_bodies(&self, account_key: &str, refs: &[MessageRef]) -> Result<Vec<MessageBody>, String> {
        let conn = self.conn.lock().map_err(|e| format!("lock: {e}"))?;

        let mut bodies = Vec::new();
        let mut stmt = conn.prepare(
            "SELECT folder, uid, subject, from_header, from_addr, to_header, cc_header, \
             date_header, date_ts, html, text_body, attachments_json, is_outgoing \
             FROM message_bodies WHERE folder = ?1 AND uid = ?2 AND account_key = ?3"
        ).map_err(|e| format!("prepare: {e}"))?;

        for mr in refs {
            let result = stmt.query_row(params![mr.folder, mr.uid, account_key], |row| {
                let to_str: String = row.get(5)?;
                let cc_str: String = row.get(6)?;
                let att_json: String = row.get(11)?;

                Ok(MessageBody {
                    folder: row.get(0)?,
                    uid: row.get(1)?,
                    subject: row.get(2)?,
                    from: row.get(3)?,
                    from_addr: row.get(4)?,
                    to: to_str.split(", ").filter(|s| !s.is_empty()).map(|s| s.to_string()).collect(),
                    cc: cc_str.split(", ").filter(|s| !s.is_empty()).map(|s| s.to_string()).collect(),
                    date: row.get(7)?,
                    date_ts: row.get(8)?,
                    html: row.get(9)?,
                    text: row.get(10)?,
                    attachments: serde_json::from_str(&att_json).unwrap_or_default(),
                    is_outgoing: row.get::<_, i32>(12)? != 0,
                })
            });

            if let Ok(body) = result {
                bodies.push(body);
            }
        }

        bodies.sort_by_key(|b| b.date_ts);
        Ok(bodies)
    }

    /// Save identities to cache.
    pub fn save_identities(&self, account_key: &str, identities: &[Identity]) -> Result<(), String> {
        let conn = self.conn.lock().map_err(|e| format!("lock: {e}"))?;
        // Clear old identities for this account
        conn.execute("DELETE FROM identities WHERE account_key = ?1", params![account_key])
            .map_err(|e| format!("del identities: {e}"))?;
        for id in identities {
            conn.execute(
                "INSERT INTO identities (email, account_key, name, signature, is_default, color) \
                 VALUES (?1, ?2, ?3, ?4, ?5, ?6)",
                params![id.email, account_key, id.name, id.signature, id.is_default as i32, id.color],
            ).map_err(|e| format!("ins identity: {e}"))?;
        }
        Ok(())
    }

    /// Load cached identities.
    pub fn load_identities(&self, account_key: &str) -> Result<Vec<Identity>, String> {
        let conn = self.conn.lock().map_err(|e| format!("lock: {e}"))?;
        let mut stmt = conn.prepare(
            "SELECT email, name, signature, is_default, color FROM identities WHERE account_key = ?1 ORDER BY is_default DESC"
        ).map_err(|e| format!("prepare: {e}"))?;

        let rows = stmt.query_map(params![account_key], |row| {
            Ok(Identity {
                email: row.get(0)?,
                name: row.get(1)?,
                signature: row.get(2)?,
                is_default: row.get::<_, i32>(3)? != 0,
                color: row.get(4)?,
            })
        }).map_err(|e| format!("query: {e}"))?;

        let mut identities = Vec::new();
        for row in rows {
            if let Ok(id) = row {
                identities.push(id);
            }
        }
        Ok(identities)
    }

    /// Get cached avatar PNG (if fresh enough — 7 days).
    pub fn get_avatar(&self, email: &str) -> Option<Vec<u8>> {
        let conn = self.conn.lock().ok()?;
        let week_ago = chrono::Utc::now().timestamp() - 7 * 86400;
        conn.query_row(
            "SELECT png_data FROM avatar_cache WHERE email = ?1 AND cached_at > ?2",
            params![email, week_ago],
            |row| row.get::<_, Vec<u8>>(0),
        ).ok()
    }

    /// Save avatar PNG to cache.
    pub fn save_avatar(&self, email: &str, png_data: &[u8]) -> Result<(), String> {
        let conn = self.conn.lock().map_err(|e| format!("lock: {e}"))?;
        let now = chrono::Utc::now().timestamp();
        conn.execute(
            "INSERT OR REPLACE INTO avatar_cache (email, png_data, cached_at) VALUES (?1, ?2, ?3)",
            params![email, png_data, now],
        ).map_err(|e| format!("ins avatar: {e}"))?;
        Ok(())
    }
}
