//! Read-only access to the desktop SQLite cache (`cache.db`) written by the
//! mail client. Query logic mirrors the Tauri crate's `cache.rs`.

use std::path::Path;

use rusqlite::{params, Connection};

use crate::types::*;

pub struct Cache {
    conn: Connection,
}

impl Cache {
    pub fn open(db_path: &Path) -> Result<Self, String> {
        let conn = Connection::open(db_path).map_err(|e| format!("SQLite open: {e}"))?;
        Ok(Self { conn })
    }

    /// Distinct account keys present in the cache.
    pub fn account_keys(&self) -> Result<Vec<String>, String> {
        let mut stmt = self
            .conn
            .prepare("SELECT DISTINCT account_key FROM conversations")
            .map_err(|e| format!("prepare: {e}"))?;
        let rows = stmt
            .query_map([], |r| r.get::<_, String>(0))
            .map_err(|e| format!("query: {e}"))?;
        Ok(rows.filter_map(|r| r.ok()).collect())
    }

    pub fn load_conversations(&self, account_key: &str) -> Result<Vec<Conversation>, String> {
        // 1) Conversation rows (collected owned so the statement is dropped
        // before we prepare the per-conversation message-refs query).
        type Row = (
            String, String, String, String, String, String, String, bool, String, i64, String, u32, u32,
        );
        let rows: Vec<Row> = {
            let mut stmt = self
                .conn
                .prepare(
                    "SELECT id, label, avatar_hash, received_by, counterpart_name, counterpart_addr, \
                     counterparts_json, is_group, last_date, last_date_ts, last_subject, unread_count, total_count \
                     FROM conversations WHERE account_key = ?1 ORDER BY last_date_ts DESC",
                )
                .map_err(|e| format!("prepare: {e}"))?;
            let it = stmt
                .query_map(params![account_key], |row| {
                    Ok((
                        row.get::<_, String>(0)?,
                        row.get::<_, String>(1)?,
                        row.get::<_, String>(2)?,
                        row.get::<_, String>(3)?,
                        row.get::<_, String>(4)?,
                        row.get::<_, String>(5)?,
                        row.get::<_, String>(6)?,
                        row.get::<_, bool>(7)?,
                        row.get::<_, String>(8)?,
                        row.get::<_, i64>(9)?,
                        row.get::<_, String>(10)?,
                        row.get::<_, u32>(11)?,
                        row.get::<_, u32>(12)?,
                    ))
                })
                .map_err(|e| format!("query: {e}"))?;
            it.filter_map(|r| r.ok()).collect()
        };

        // 2) Message refs per conversation.
        let mut msg_stmt = self
            .conn
            .prepare("SELECT folder, uid FROM conversation_messages WHERE conversation_id = ?1")
            .map_err(|e| format!("prepare msgs: {e}"))?;

        let mut out = Vec::new();
        for (id, label, avatar_hash, received_by, cp_name, cp_addr, cps_json, is_group, last_date, last_date_ts, last_subject, unread_count, total_count) in rows {
            let counterparts: Vec<ContactInfo> = serde_json::from_str(&cps_json)
                .ok()
                .filter(|v: &Vec<ContactInfo>| !v.is_empty())
                .unwrap_or_else(|| vec![ContactInfo { name: cp_name, addr: cp_addr }]);

            let messages: Vec<MessageRef> = msg_stmt
                .query_map(params![id], |r| Ok(MessageRef { folder: r.get(0)?, uid: r.get(1)? }))
                .map_err(|e| format!("query msgs: {e}"))?
                .filter_map(|r| r.ok())
                .collect();

            out.push(Conversation {
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
                draft: None,
            });
        }
        Ok(out)
    }

    /// Load cached message bodies for the given refs, sorted oldest-first.
    pub fn load_message_bodies(
        &self,
        account_key: &str,
        refs: &[MessageRef],
    ) -> Result<Vec<MessageBody>, String> {
        let mut stmt = self
            .conn
            .prepare(
                "SELECT folder, uid, subject, from_header, from_addr, to_header, cc_header, \
                 date_header, date_ts, html, text_body, attachments_json, is_outgoing, \
                 message_id, in_reply_to, references_json \
                 FROM message_bodies WHERE folder = ?1 AND uid = ?2 AND account_key = ?3",
            )
            .map_err(|e| format!("prepare: {e}"))?;

        let mut bodies = Vec::new();
        for mr in refs {
            let r = stmt.query_row(params![mr.folder, mr.uid, account_key], |row| {
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
            if let Ok(b) = r {
                bodies.push(b);
            }
        }
        bodies.sort_by_key(|b| b.date_ts);
        Ok(bodies)
    }
}
