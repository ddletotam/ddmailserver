use rusqlite::{Connection, params};
use std::path::PathBuf;
use std::sync::Mutex;

use crate::types::*;

/// Parse a "Name <email>" header value into (name, addr). Returns ("", "") if no addr present.
fn parse_addr_pair(value: &str) -> (String, String) {
    let v = value.trim();
    if let (Some(start), Some(end)) = (v.rfind('<'), v.rfind('>')) {
        if start < end {
            let addr = v[start + 1..end].trim().to_string();
            let name = v[..start].trim().trim_matches('"').trim().to_string();
            return (name, addr);
        }
    }
    if v.contains('@') { return (String::new(), v.to_string()); }
    (String::new(), String::new())
}

/// Pull (name, addr) entries from a MessageBody's From/To/Cc fields.
fn collect_address_entries(body: &MessageBody) -> Vec<(String, String)> {
    let mut out: Vec<(String, String)> = Vec::new();
    let (fname, faddr) = parse_addr_pair(&body.from);
    let final_addr = if !faddr.is_empty() { faddr } else { body.from_addr.clone() };
    out.push((fname, final_addr));
    for h in body.to.iter().chain(body.cc.iter()) {
        let (n, a) = parse_addr_pair(h);
        if !a.is_empty() { out.push((n, a)); }
    }
    out
}

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
                counterparts_json TEXT NOT NULL DEFAULT '[]',
                is_group INTEGER NOT NULL DEFAULT 0,
                last_date TEXT NOT NULL DEFAULT '',
                last_date_ts INTEGER NOT NULL DEFAULT 0,
                last_subject TEXT NOT NULL DEFAULT '',
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
                mime TEXT NOT NULL DEFAULT '',
                cached_at INTEGER NOT NULL DEFAULT 0
            );
            -- Add mime column for installs that predate it (SQLite ignores
            -- the error if it already exists; we just don't want to write a
            -- separate version table for one column).

            CREATE TABLE IF NOT EXISTS contacts (
                account_key TEXT NOT NULL,
                email TEXT NOT NULL,
                name TEXT NOT NULL DEFAULT '',
                source TEXT NOT NULL DEFAULT 'auto',
                last_seen_ts INTEGER NOT NULL DEFAULT 0,
                PRIMARY KEY(account_key, email, source)
            );
            CREATE INDEX IF NOT EXISTS idx_contacts_email ON contacts(account_key, email);
            CREATE INDEX IF NOT EXISTS idx_contacts_name ON contacts(account_key, name);

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
                message_id TEXT NOT NULL DEFAULT '',
                in_reply_to TEXT NOT NULL DEFAULT '',
                references_json TEXT NOT NULL DEFAULT '[]',
                cached_at INTEGER NOT NULL DEFAULT 0,
                PRIMARY KEY(folder, uid, account_key)
            );

            -- Calendar reminders.
            --
            -- One row per (event, occurrence). The scheduler treats it as
            -- the source of truth so snoozes and acks survive app restarts.
            --
            -- status state machine:
            --   pending → fired   (notifier shows the toast)
            --   fired   → acked   (user clicked OK / Open)
            --   fired   → pending (user snoozed; fire_at_ms updated)
            --
            -- summary is denormalised so the toast can render without
            -- re-fetching the event from the server.
            CREATE TABLE IF NOT EXISTS event_reminders (
                event_id INTEGER NOT NULL,
                occurrence_start_ms INTEGER NOT NULL,
                fire_at_ms INTEGER NOT NULL,
                status TEXT NOT NULL DEFAULT 'pending',
                lead_min INTEGER NOT NULL DEFAULT 15,
                summary TEXT NOT NULL DEFAULT '',
                PRIMARY KEY (event_id, occurrence_start_ms)
            );
            CREATE INDEX IF NOT EXISTS idx_reminders_fire
                ON event_reminders(fire_at_ms);

            -- Small key/value store for sync bookkeeping (delta watermarks,
            -- last-full-sync timestamps). One row per key.
            CREATE TABLE IF NOT EXISTS meta (
                key TEXT PRIMARY KEY,
                value TEXT NOT NULL DEFAULT ''
            );
        ").map_err(|e| format!("SQLite init: {e}"))?;

        // Migrations for existing databases
        conn.execute("ALTER TABLE conversation_messages ADD COLUMN seen INTEGER NOT NULL DEFAULT 1", []).ok();
        conn.execute("ALTER TABLE conversations ADD COLUMN avatar_hash TEXT NOT NULL DEFAULT ''", []).ok();
        conn.execute("ALTER TABLE conversations ADD COLUMN received_by TEXT NOT NULL DEFAULT ''", []).ok();
        conn.execute("ALTER TABLE conversations ADD COLUMN last_subject TEXT NOT NULL DEFAULT ''", []).ok();
        conn.execute("ALTER TABLE conversations ADD COLUMN counterparts_json TEXT NOT NULL DEFAULT '[]'", []).ok();
        conn.execute("ALTER TABLE message_bodies ADD COLUMN message_id TEXT NOT NULL DEFAULT ''", []).ok();
        conn.execute("ALTER TABLE message_bodies ADD COLUMN in_reply_to TEXT NOT NULL DEFAULT ''", []).ok();
        conn.execute("ALTER TABLE message_bodies ADD COLUMN references_json TEXT NOT NULL DEFAULT '[]'", []).ok();
        conn.execute("ALTER TABLE avatar_cache ADD COLUMN mime TEXT NOT NULL DEFAULT ''", []).ok();
        // Existing rows pre-MIME stored Gravatar PNG bytes — purge so the
        // next lookup uses the new chain (and labels the result with a MIME).
        conn.execute("DELETE FROM avatar_cache WHERE mime = ''", []).ok();

        // Reminders dedup migration. The PRIMARY KEY (event_id,
        // occurrence_start_ms) doesn't dedupe across calendar resyncs
        // because CalDAV-side event ids are not stable: every sync mints
        // new ids, the frontend re-pushes reminders, and INSERT-OR-IGNORE
        // happily creates a fresh row per id. Result: one logical
        // occurrence ends up with 6-10 reminder rows, the scheduler fires
        // them one by one and the user sees the same toast nagging every
        // scan tick.
        //
        // The logical key is (occurrence_start_ms, summary): two events
        // can't legitimately share both. Dedupe rows along that key,
        // keeping the one that's furthest along — acked > fired > pending
        // — so the user's last action sticks. Then put a UNIQUE INDEX on
        // the same key, and switch the upsert below to an ON CONFLICT
        // path so new event_ids overwrite the old one in-place.
        conn.execute(
            "DELETE FROM event_reminders WHERE rowid IN (\
                SELECT rowid FROM (\
                    SELECT rowid, ROW_NUMBER() OVER (\
                        PARTITION BY occurrence_start_ms, summary \
                        ORDER BY CASE status \
                            WHEN 'acked' THEN 0 \
                            WHEN 'fired' THEN 1 \
                            ELSE 2 END, \
                        event_id DESC\
                    ) AS rn FROM event_reminders\
                ) WHERE rn > 1\
            )",
            [],
        ).ok();
        conn.execute(
            "CREATE UNIQUE INDEX IF NOT EXISTS idx_reminders_logical \
             ON event_reminders(occurrence_start_ms, summary)",
            [],
        ).ok();

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
            let cps_json = serde_json::to_string(&conv.counterparts)
                .map_err(|e| format!("serialize counterparts: {e}"))?;

            tx.execute(
                "INSERT INTO conversations (id, account_key, label, avatar_hash, received_by, counterpart_name, counterpart_addr, \
                 counterparts_json, is_group, last_date, last_date_ts, last_subject, unread_count, total_count, updated_at) \
                 VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12, ?13, ?14, ?15)",
                params![
                    conv.id, account_key, conv.label, conv.avatar_hash, conv.received_by, cp_name, cp_addr,
                    cps_json, conv.is_group as i32, conv.last_date, conv.last_date_ts,
                    conv.last_subject, conv.unread_count, conv.total_count, now
                ],
            ).map_err(|e| format!("ins conv: {e}"))?;

            for mr in &conv.messages {
                tx.execute(
                    "INSERT OR IGNORE INTO conversation_messages (conversation_id, folder, uid, seen) VALUES (?1, ?2, ?3, ?4)",
                    params![conv.id, mr.folder, mr.uid, mr.seen as i32],
                ).map_err(|e| format!("ins msg ref: {e}"))?;
            }

            // Auto-record the counterpart as a contact.
            if !cp_addr.is_empty() {
                let lc = cp_addr.to_lowercase();
                tx.execute(
                    "INSERT INTO contacts (account_key, email, name, source, last_seen_ts) \
                     VALUES (?1, ?2, ?3, 'auto', ?4) \
                     ON CONFLICT(account_key, email, source) DO UPDATE SET \
                       name = CASE WHEN excluded.name != '' THEN excluded.name ELSE name END, \
                       last_seen_ts = excluded.last_seen_ts",
                    params![account_key, lc, cp_name, conv.last_date_ts]
                ).map_err(|e| format!("auto-contact: {e}"))?;
            }
        }

        tx.commit().map_err(|e| format!("commit: {e}"))?;
        Ok(())
    }

    /// Upsert a partial set of conversations (delta sync): each conversation
    /// is replaced/inserted by id, its message refs rewritten; everything
    /// else stays untouched. Use save_conversations for a full replace.
    pub fn upsert_conversations(&self, account_key: &str, conversations: &[Conversation]) -> Result<(), String> {
        let conn = self.conn.lock().map_err(|e| format!("lock: {e}"))?;
        let now = chrono::Utc::now().timestamp();
        let tx = conn.unchecked_transaction().map_err(|e| format!("tx: {e}"))?;

        for conv in conversations {
            let cp_name = conv.counterparts.first().map(|c| c.name.as_str()).unwrap_or("");
            let cp_addr = conv.counterparts.first().map(|c| c.addr.as_str()).unwrap_or("");
            let cps_json = serde_json::to_string(&conv.counterparts)
                .map_err(|e| format!("serialize counterparts: {e}"))?;

            tx.execute(
                "INSERT OR REPLACE INTO conversations (id, account_key, label, avatar_hash, received_by, counterpart_name, \
                 counterpart_addr, counterparts_json, is_group, last_date, last_date_ts, last_subject, unread_count, \
                 total_count, updated_at) \
                 VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12, ?13, ?14, ?15)",
                params![
                    conv.id, account_key, conv.label, conv.avatar_hash, conv.received_by, cp_name, cp_addr,
                    cps_json, conv.is_group as i32, conv.last_date, conv.last_date_ts,
                    conv.last_subject, conv.unread_count, conv.total_count, now
                ],
            ).map_err(|e| format!("upsert conv: {e}"))?;

            tx.execute(
                "DELETE FROM conversation_messages WHERE conversation_id = ?1",
                params![conv.id],
            ).map_err(|e| format!("del msg refs: {e}"))?;
            for mr in &conv.messages {
                tx.execute(
                    "INSERT OR IGNORE INTO conversation_messages (conversation_id, folder, uid, seen) VALUES (?1, ?2, ?3, ?4)",
                    params![conv.id, mr.folder, mr.uid, mr.seen as i32],
                ).map_err(|e| format!("ins msg ref: {e}"))?;
            }
        }

        tx.commit().map_err(|e| format!("commit: {e}"))
    }

    /// Remove a conversation and everything that belongs to it: the row,
    /// its message refs, and the cached bodies of those refs. Used by the
    /// desktop "delete conversation" action so a restart can't resurrect
    /// the deleted thread from cache.
    pub fn delete_conversation(&self, account_key: &str, conversation_id: &str) -> Result<(), String> {
        let conn = self.conn.lock().map_err(|e| format!("lock: {e}"))?;
        let tx = conn.unchecked_transaction().map_err(|e| format!("tx: {e}"))?;
        tx.execute(
            "DELETE FROM message_bodies WHERE account_key = ?1 AND (folder, uid) IN \
             (SELECT folder, uid FROM conversation_messages WHERE conversation_id = ?2)",
            params![account_key, conversation_id],
        ).map_err(|e| format!("del bodies: {e}"))?;
        tx.execute(
            "DELETE FROM conversation_messages WHERE conversation_id = ?1",
            params![conversation_id],
        ).map_err(|e| format!("del refs: {e}"))?;
        tx.execute(
            "DELETE FROM conversations WHERE account_key = ?1 AND id = ?2",
            params![account_key, conversation_id],
        ).map_err(|e| format!("del conv: {e}"))?;
        tx.commit().map_err(|e| format!("commit: {e}"))
    }

    /// Read a sync-bookkeeping value (see the `meta` table).
    pub fn get_meta(&self, key: &str) -> Option<String> {
        let conn = self.conn.lock().ok()?;
        conn.query_row(
            "SELECT value FROM meta WHERE key = ?1",
            params![key],
            |r| r.get::<_, String>(0),
        ).ok()
    }

    /// Write a sync-bookkeeping value.
    pub fn set_meta(&self, key: &str, value: &str) -> Result<(), String> {
        let conn = self.conn.lock().map_err(|e| format!("lock: {e}"))?;
        conn.execute(
            "INSERT OR REPLACE INTO meta (key, value) VALUES (?1, ?2)",
            params![key, value],
        ).map_err(|e| format!("set meta: {e}"))?;
        Ok(())
    }

    /// Which of `refs` already have a cached body. Used by the engine's
    /// missing-only fetch: bodies are immutable, so a cached (folder, uid)
    /// never needs refetching.
    pub fn cached_body_refs(&self, account_key: &str, refs: &[MessageRef]) -> Result<Vec<MessageRef>, String> {
        let conn = self.conn.lock().map_err(|e| format!("lock: {e}"))?;
        let mut stmt = conn.prepare(
            "SELECT 1 FROM message_bodies WHERE folder = ?1 AND uid = ?2 AND account_key = ?3"
        ).map_err(|e| format!("prepare: {e}"))?;
        let mut out = Vec::new();
        for mr in refs {
            let hit: Result<i32, _> = stmt.query_row(params![mr.folder, mr.uid, account_key], |r| r.get(0));
            if hit.is_ok() {
                out.push(mr.clone());
            }
        }
        Ok(out)
    }

    /// Distinct account keys present in the conversations table.
    pub fn account_keys(&self) -> Result<Vec<String>, String> {
        let conn = self.conn.lock().map_err(|e| format!("lock: {e}"))?;
        let mut stmt = conn
            .prepare("SELECT DISTINCT account_key FROM conversations")
            .map_err(|e| format!("prepare: {e}"))?;
        let rows = stmt
            .query_map([], |r| r.get::<_, String>(0))
            .map_err(|e| format!("query: {e}"))?;
        Ok(rows.filter_map(|r| r.ok()).collect())
    }

    /// Load cached conversations for an account.
    pub fn load_conversations(&self, account_key: &str) -> Result<Vec<Conversation>, String> {
        let conn = self.conn.lock().map_err(|e| format!("lock: {e}"))?;

        let mut stmt = conn.prepare(
            "SELECT id, label, avatar_hash, received_by, counterpart_name, counterpart_addr, counterparts_json, is_group, \
             last_date, last_date_ts, last_subject, unread_count, total_count \
             FROM conversations WHERE account_key = ?1 ORDER BY last_date_ts DESC"
        ).map_err(|e| format!("prepare: {e}"))?;

        let rows = stmt.query_map(params![account_key], |row| {
            Ok((
                row.get::<_, String>(0)?,   // id
                row.get::<_, String>(1)?,   // label
                row.get::<_, String>(2)?,   // avatar_hash
                row.get::<_, String>(3)?,   // received_by
                row.get::<_, String>(4)?,   // cp_name (legacy)
                row.get::<_, String>(5)?,   // cp_addr (legacy)
                row.get::<_, String>(6)?,   // counterparts_json
                row.get::<_, bool>(7)?,     // is_group
                row.get::<_, String>(8)?,   // last_date
                row.get::<_, i64>(9)?,      // last_date_ts
                row.get::<_, String>(10)?,  // last_subject
                row.get::<_, u32>(11)?,     // unread_count
                row.get::<_, u32>(12)?,     // total_count
            ))
        }).map_err(|e| format!("query: {e}"))?;

        let mut conversations = Vec::new();
        for row in rows {
            let (id, label, avatar_hash, received_by, cp_name, cp_addr, cps_json, is_group, last_date, last_date_ts,
                 last_subject, unread_count, total_count) = row.map_err(|e| format!("row: {e}"))?;

            // Prefer the JSON column. Rows written before that migration will
            // store an empty array there — fall back to the legacy single-pair
            // columns for those.
            let counterparts: Vec<ContactInfo> = serde_json::from_str(&cps_json)
                .ok()
                .filter(|v: &Vec<ContactInfo>| !v.is_empty())
                .unwrap_or_else(|| vec![ContactInfo { name: cp_name, addr: cp_addr }]);

            // Load message refs
            let mut msg_stmt = conn.prepare(
                "SELECT folder, uid, COALESCE(seen, 1) FROM conversation_messages WHERE conversation_id = ?1"
            ).map_err(|e| format!("prepare msgs: {e}"))?;
            let messages: Vec<MessageRef> = msg_stmt.query_map(params![id], |r| {
                Ok(MessageRef {
                    folder: r.get(0)?,
                    uid: r.get(1)?,
                    seen: r.get::<_, i32>(2)? != 0,
                })
            }).map_err(|e| format!("query msgs: {e}"))?
            .filter_map(|r| r.ok())
            .collect();

            conversations.push(Conversation {
                id,
                label,
                avatar_hash,
                received_by,
                counterparts,
                is_group,
                last_date,
                last_date_ts,
                last_subject,
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

        // Auto-record contacts from From/To/Cc of each message.
        for body in bodies {
            let entries = collect_address_entries(body);
            for (name, addr) in entries {
                if addr.is_empty() { continue; }
                let lc = addr.to_lowercase();
                conn.execute(
                    "INSERT INTO contacts (account_key, email, name, source, last_seen_ts) \
                     VALUES (?1, ?2, ?3, 'auto', ?4) \
                     ON CONFLICT(account_key, email, source) DO UPDATE SET \
                       name = CASE WHEN excluded.name != '' THEN excluded.name ELSE name END, \
                       last_seen_ts = excluded.last_seen_ts",
                    params![account_key, lc, name, body.date_ts]
                ).map_err(|e| format!("auto-contact: {e}"))?;
            }
        }

        for body in bodies {
            let att_json = serde_json::to_string(&body.attachments).unwrap_or_else(|_| "[]".into());
            let refs_json = serde_json::to_string(&body.references).unwrap_or_else(|_| "[]".into());
            conn.execute(
                "INSERT OR REPLACE INTO message_bodies \
                 (folder, uid, account_key, subject, from_header, from_addr, to_header, cc_header, \
                  date_header, date_ts, html, text_body, attachments_json, is_outgoing, \
                  message_id, in_reply_to, references_json, cached_at) \
                 VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12, ?13, ?14, ?15, ?16, ?17, ?18)",
                params![
                    body.folder, body.uid, account_key,
                    body.subject, body.from, body.from_addr,
                    body.to.join(", "), body.cc.join(", "),
                    body.date, body.date_ts,
                    body.html, body.text, att_json,
                    body.is_outgoing as i32,
                    body.message_id, body.in_reply_to, refs_json, now
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
             date_header, date_ts, html, text_body, attachments_json, is_outgoing, \
             message_id, in_reply_to, references_json \
             FROM message_bodies WHERE folder = ?1 AND uid = ?2 AND account_key = ?3"
        ).map_err(|e| format!("prepare: {e}"))?;

        for mr in refs {
            let result = stmt.query_row(params![mr.folder, mr.uid, account_key], |row| {
                let to_str: String = row.get(5)?;
                let cc_str: String = row.get(6)?;
                let att_json: String = row.get(11)?;
                let refs_json: String = row.get(15)?;

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
                    message_id: row.get(13)?,
                    in_reply_to: row.get(14)?,
                    references: serde_json::from_str(&refs_json).unwrap_or_default(),
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

    /// Insert/update contacts in batch. Existing rows keep their non-empty names if a new
    /// row arrives with empty name.
    pub fn record_contacts(&self, account_key: &str, contacts: &[Contact]) -> Result<(), String> {
        let conn = self.conn.lock().map_err(|e| format!("lock: {e}"))?;
        let now = chrono::Utc::now().timestamp();
        let tx = conn.unchecked_transaction().map_err(|e| format!("tx: {e}"))?;
        for c in contacts {
            if c.email.is_empty() { continue; }
            let lc = c.email.to_lowercase();
            tx.execute(
                "INSERT INTO contacts (account_key, email, name, source, last_seen_ts) \
                 VALUES (?1, ?2, ?3, ?4, ?5) \
                 ON CONFLICT(account_key, email, source) DO UPDATE SET \
                   name = CASE WHEN excluded.name != '' THEN excluded.name ELSE name END, \
                   last_seen_ts = excluded.last_seen_ts",
                params![account_key, lc, c.name, c.source, now]
            ).map_err(|e| format!("upsert contact: {e}"))?;
        }
        tx.commit().map_err(|e| format!("commit: {e}"))
    }

    /// Search contacts by query (matches against email or name, case-insensitive).
    /// Deduped by email; carddav source wins over auto, then most recent.
    pub fn search_contacts(&self, account_key: &str, query: &str, limit: u32) -> Result<Vec<Contact>, String> {
        let conn = self.conn.lock().map_err(|e| format!("lock: {e}"))?;
        let pattern = format!("%{}%", query.to_lowercase());
        let mut stmt = conn.prepare(
            "SELECT email, name, source, last_seen_ts FROM contacts \
             WHERE account_key = ?1 \
             AND (LOWER(email) LIKE ?2 OR LOWER(name) LIKE ?2) \
             ORDER BY \
               CASE WHEN source='carddav' THEN 0 ELSE 1 END, \
               last_seen_ts DESC, \
               name"
        ).map_err(|e| format!("prepare: {e}"))?;
        let rows = stmt.query_map(params![account_key, pattern], |row| {
            Ok(Contact {
                email: row.get::<_, String>(0)?,
                name: row.get::<_, String>(1)?,
                source: row.get::<_, String>(2)?,
            })
        }).map_err(|e| format!("query: {e}"))?;

        let mut seen_emails: std::collections::HashSet<String> = std::collections::HashSet::new();
        let mut out: Vec<Contact> = Vec::new();
        for row in rows {
            if let Ok(c) = row {
                if !seen_emails.insert(c.email.clone()) { continue; }
                out.push(c);
                if out.len() as u32 >= limit { break; }
            }
        }
        Ok(out)
    }

    /// Get cached avatar bytes + MIME if fresh enough (7d positive / 1d negative).
    /// Returns None when the cache miss should trigger a refetch.
    pub fn get_avatar(&self, email: &str) -> Option<(Vec<u8>, String)> {
        let conn = self.conn.lock().ok()?;
        let now = chrono::Utc::now().timestamp();
        let week_ago = now - 7 * 86400;
        let day_ago = now - 86400;
        // Empty payload = negative cache; expire after 1 day so transient
        // failures (DNS hiccup, server restart) get re-tried sooner.
        let row = conn.query_row(
            "SELECT png_data, mime, cached_at FROM avatar_cache WHERE email = ?1",
            params![email],
            |row| Ok((row.get::<_, Vec<u8>>(0)?, row.get::<_, String>(1)?, row.get::<_, i64>(2)?)),
        ).ok()?;
        let (data, mime, cached_at) = row;
        let ttl_floor = if data.is_empty() { day_ago } else { week_ago };
        if cached_at <= ttl_floor {
            return None;
        }
        Some((data, mime))
    }

    /// Save avatar bytes + MIME to cache. Empty data = negative cache row.
    pub fn save_avatar(&self, email: &str, data: &[u8], mime: &str) -> Result<(), String> {
        let conn = self.conn.lock().map_err(|e| format!("lock: {e}"))?;
        let now = chrono::Utc::now().timestamp();
        conn.execute(
            "INSERT OR REPLACE INTO avatar_cache (email, png_data, mime, cached_at) VALUES (?1, ?2, ?3, ?4)",
            params![email, data, mime, now],
        ).map_err(|e| format!("ins avatar: {e}"))?;
        Ok(())
    }

    // ── Calendar reminders ──

    /// Insert a reminder for an event occurrence if no row exists yet.
    ///
    /// Dedup is on (occurrence_start_ms, summary), not (event_id, occ): the
    /// frontend re-pushes the whole schedule whenever the calendar list
    /// changes, and CalDAV resyncs reassign `event_id` so the same logical
    /// occurrence shows up with a different id each round. We treat
    /// (occ, summary) as the logical identity and update event_id in-place
    /// on conflict — the user's status / fire_at / lead_min stay put, so
    /// acks and snoozes survive the resync. The latest event_id wins
    /// because the open-event payload from the toast needs to route to
    /// whichever row the backend currently exposes.
    pub fn upsert_pending_reminder(
        &self,
        event_id: i64,
        occurrence_start_ms: i64,
        fire_at_ms: i64,
        lead_min: i32,
        summary: &str,
    ) -> Result<(), String> {
        let conn = self.conn.lock().map_err(|e| format!("lock: {e}"))?;
        conn.execute(
            "INSERT INTO event_reminders \
             (event_id, occurrence_start_ms, fire_at_ms, status, lead_min, summary) \
             VALUES (?1, ?2, ?3, 'pending', ?4, ?5) \
             ON CONFLICT(occurrence_start_ms, summary) DO UPDATE SET \
                event_id = excluded.event_id",
            params![event_id, occurrence_start_ms, fire_at_ms, lead_min, summary],
        ).map_err(|e| format!("ins reminder: {e}"))?;
        Ok(())
    }

    /// Reminders whose fire time has passed AND that haven't been shown
    /// yet. Status 'fired' rows stay in the table so we don't re-toast a
    /// notification the user dismissed deliberately.
    pub fn due_reminders(&self, now_ms: i64) -> Result<Vec<ReminderRow>, String> {
        let conn = self.conn.lock().map_err(|e| format!("lock: {e}"))?;
        let mut stmt = conn.prepare(
            "SELECT event_id, occurrence_start_ms, fire_at_ms, lead_min, summary \
             FROM event_reminders \
             WHERE status = 'pending' AND fire_at_ms <= ?1 \
             ORDER BY fire_at_ms ASC"
        ).map_err(|e| format!("prep: {e}"))?;
        let rows = stmt.query_map(params![now_ms], |r| {
            Ok(ReminderRow {
                event_id: r.get(0)?,
                occurrence_start_ms: r.get(1)?,
                fire_at_ms: r.get(2)?,
                lead_min: r.get(3)?,
                summary: r.get(4)?,
            })
        }).map_err(|e| format!("query: {e}"))?;
        let mut out = Vec::new();
        for r in rows { out.push(r.map_err(|e| format!("row: {e}"))?); }
        Ok(out)
    }

    pub fn mark_reminder_fired(&self, event_id: i64, occurrence_start_ms: i64) -> Result<(), String> {
        let conn = self.conn.lock().map_err(|e| format!("lock: {e}"))?;
        conn.execute(
            "UPDATE event_reminders SET status = 'fired' \
             WHERE event_id = ?1 AND occurrence_start_ms = ?2",
            params![event_id, occurrence_start_ms],
        ).map_err(|e| format!("upd fired: {e}"))?;
        Ok(())
    }

    /// Fetch a single reminder row by its logical id. Used by the
    /// snooze-config window to populate its UI without round-tripping
    /// the data through URL params.
    pub fn get_reminder(
        &self,
        event_id: i64,
        occurrence_start_ms: i64,
    ) -> Result<Option<ReminderRow>, String> {
        let conn = self.conn.lock().map_err(|e| format!("lock: {e}"))?;
        let mut stmt = conn.prepare(
            "SELECT event_id, occurrence_start_ms, fire_at_ms, lead_min, summary \
             FROM event_reminders WHERE event_id = ?1 AND occurrence_start_ms = ?2",
        ).map_err(|e| format!("prep: {e}"))?;
        let mut rows = stmt.query_map(params![event_id, occurrence_start_ms], |r| {
            Ok(ReminderRow {
                event_id: r.get(0)?,
                occurrence_start_ms: r.get(1)?,
                fire_at_ms: r.get(2)?,
                lead_min: r.get(3)?,
                summary: r.get(4)?,
            })
        }).map_err(|e| format!("query: {e}"))?;
        match rows.next() {
            Some(r) => Ok(Some(r.map_err(|e| format!("row: {e}"))?)),
            None => Ok(None),
        }
    }

    pub fn mark_reminder_acked(&self, event_id: i64, occurrence_start_ms: i64) -> Result<(), String> {
        let conn = self.conn.lock().map_err(|e| format!("lock: {e}"))?;
        conn.execute(
            "UPDATE event_reminders SET status = 'acked' \
             WHERE event_id = ?1 AND occurrence_start_ms = ?2",
            params![event_id, occurrence_start_ms],
        ).map_err(|e| format!("upd acked: {e}"))?;
        Ok(())
    }

    /// Snooze: reschedule fire_at and put status back to pending. Caller is
    /// responsible for computing the new absolute fire_at_ms.
    pub fn snooze_reminder(
        &self,
        event_id: i64,
        occurrence_start_ms: i64,
        new_fire_at_ms: i64,
        new_lead_min: i32,
    ) -> Result<(), String> {
        let conn = self.conn.lock().map_err(|e| format!("lock: {e}"))?;
        conn.execute(
            "UPDATE event_reminders SET status = 'pending', fire_at_ms = ?3, lead_min = ?4 \
             WHERE event_id = ?1 AND occurrence_start_ms = ?2",
            params![event_id, occurrence_start_ms, new_fire_at_ms, new_lead_min],
        ).map_err(|e| format!("upd snooze: {e}"))?;
        Ok(())
    }

    /// Drop reminders whose occurrence is well in the past so the table
    /// doesn't grow indefinitely. The cutoff is generous enough that we
    /// don't lose history a user might still want to investigate.
    pub fn prune_old_reminders(&self, cutoff_ms: i64) -> Result<(), String> {
        let conn = self.conn.lock().map_err(|e| format!("lock: {e}"))?;
        conn.execute(
            "DELETE FROM event_reminders WHERE occurrence_start_ms < ?1",
            params![cutoff_ms],
        ).map_err(|e| format!("prune: {e}"))?;
        Ok(())
    }
}

/// A scheduled reminder row, denormalised enough that the notifier can
/// render the toast without touching any other table. Serializable so a
/// reminder row can be pulled by id and shipped to the UI cheaply.
#[derive(Debug, Clone, serde::Serialize)]
pub struct ReminderRow {
    pub event_id: i64,
    pub occurrence_start_ms: i64,
    pub fire_at_ms: i64,
    pub lead_min: i32,
    pub summary: String,
}
