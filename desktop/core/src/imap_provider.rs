use async_trait::async_trait;
use md5::Digest;

use crate::event::Notifier;
use crate::imap;
use crate::provider::MailProvider;
use crate::session::{Credentials, SessionPool};
use crate::types::*;

/// IMAP/SMTP provider for third-party mail servers.
///
/// Each operation opens a fresh connection, delegates to the existing
/// `imap::*_impl` functions, and logs out. This matches the current
/// stateless-per-command architecture.
pub struct ImapProvider {
    pub host: String,
    pub port: u16,
    pub username: String,
    pub password: String,
    pub use_tls: bool,
    pub user_email: String,
    pub pool: std::sync::Arc<SessionPool>,
    /// Optional CardDAV addressbook-collection URL for this account. When set,
    /// the address book is served from it (Basic auth with the IMAP
    /// credentials by default); otherwise contacts are empty. No autodiscovery
    /// yet — a direct collection URL (Phase 4, slice 1).
    pub carddav_url: Option<String>,
    /// Optional CalDAV calendar-collection URL. When set, list_calendars
    /// returns one calendar and fetch_calendar_events reads from it; with
    /// write support the event create/patch/delete also target it (Basic
    /// auth). Direct collection URL, no discovery.
    pub caldav_url: Option<String>,
    /// Maps our synthetic numeric event id → iCal UID for the last-fetched
    /// CalDAV events, so patch/delete (which the UI addresses by i64 id) can
    /// resolve the resource. The provider is long-lived per account, so this
    /// survives between fetch and edit.
    pub caldav_event_uids: std::sync::Arc<std::sync::Mutex<std::collections::HashMap<i64, String>>>,
    /// Cached result of resolving `caldav_url` to a concrete calendar
    /// collection (discovery runs once per account, then this is reused).
    pub caldav_collection: std::sync::Arc<std::sync::Mutex<Option<String>>>,
    /// Same, for the CardDAV addressbook collection.
    pub carddav_collection: std::sync::Arc<std::sync::Mutex<Option<String>>>,
}

impl ImapProvider {
    /// The addressbook-collection URL to operate on: `carddav_url` resolved
    /// through discovery if it's a base, cached after the first call.
    async fn carddav_collection_url(&self) -> Result<Option<String>, String> {
        let Some(configured) = &self.carddav_url else {
            return Ok(None);
        };
        if let Some(cached) = self.carddav_collection.lock().map_err(|e| format!("lock: {e}"))?.clone() {
            return Ok(Some(cached));
        }
        let resolved = crate::carddav_client::resolve_addressbook_collection(
            configured,
            &self.username,
            &self.password,
        )
        .await
        .unwrap_or_else(|_| configured.clone());
        *self.carddav_collection.lock().map_err(|e| format!("lock: {e}"))? = Some(resolved.clone());
        Ok(Some(resolved))
    }

    /// The calendar-collection URL to operate on: the configured `caldav_url`
    /// resolved through discovery (RFC 6764) if it's a server/principal base,
    /// cached after the first call. `None` when no CalDAV is configured.
    async fn caldav_collection_url(&self) -> Result<Option<String>, String> {
        let Some(configured) = &self.caldav_url else {
            return Ok(None);
        };
        if let Some(cached) = self.caldav_collection.lock().map_err(|e| format!("lock: {e}"))?.clone() {
            return Ok(Some(cached));
        }
        let resolved = crate::caldav_client::resolve_calendar_collection(
            configured,
            &self.username,
            &self.password,
        )
        .await
        .unwrap_or_else(|_| configured.clone());
        *self.caldav_collection.lock().map_err(|e| format!("lock: {e}"))? = Some(resolved.clone());
        Ok(Some(resolved))
    }
}

/// Synthetic calendar id for a standalone CalDAV collection (there is exactly
/// one per plain-server account, so a fixed id is fine).
const STANDALONE_CALDAV_CAL_ID: i64 = 1;

/// Helper macro: connect → run closure → logout.
macro_rules! with_session {
    ($self:expr, |$s:ident| $body:expr) => {{
        if $self.use_tls {
            let mut $s = imap::connect_tls(&$self.host, $self.port, &$self.username, &$self.password).await?;
            let result = $body;
            $s.logout().await.ok();
            result
        } else {
            let mut $s = imap::connect_plain(&$self.host, $self.port, &$self.username, &$self.password).await?;
            let result = $body;
            $s.logout().await.ok();
            result
        }
    }};
}

#[async_trait]
impl MailProvider for ImapProvider {
    async fn list_folders(&self) -> Result<Vec<Folder>, String> {
        with_session!(self, |session| {
            imap::list_folders_impl(&mut session).await
        })
    }

    async fn fetch_conversations(
        &self,
        our_addrs: &[String],
        limit: u32,
    ) -> Result<Vec<Conversation>, String> {
        with_session!(self, |session| {
            imap::fetch_conversations_impl(&mut session, &self.user_email, our_addrs, limit).await
        })
    }

    async fn fetch_conversation_messages(
        &self,
        our_addrs: &[String],
        messages: &[MessageRef],
    ) -> Result<Vec<MessageBody>, String> {
        with_session!(self, |session| {
            imap::fetch_bodies_impl(&mut session, our_addrs, messages).await
        })
    }

    async fn search_messages(
        &self,
        user_email: &str,
        query: &str,
    ) -> Result<Vec<MessageEnvelope>, String> {
        with_session!(self, |session| {
            imap::search_impl(&mut session, user_email, query).await
        })
    }

    async fn set_flags(
        &self,
        folder: &str,
        uid: u32,
        flags: &str,
        add: bool,
    ) -> Result<(), String> {
        with_session!(self, |session| {
            imap::store_flags_impl(&mut session, folder, uid, flags, add).await
        })
    }

    async fn set_flags_batch(
        &self,
        messages: &[MessageRef],
        flags: &str,
        add: bool,
    ) -> Result<(), String> {
        if messages.is_empty() {
            return Ok(());
        }
        with_session!(self, |session| {
            imap::store_flags_batch_impl(&mut session, messages, flags, add).await
        })
    }

    async fn delete_messages(
        &self,
        messages: &[MessageRef],
    ) -> Result<(), String> {
        if messages.is_empty() {
            return Ok(());
        }
        with_session!(self, |session| {
            imap::delete_messages_impl(&mut session, messages).await
        })
    }

    async fn mark_spam_by_domain(
        &self,
        _domain: &str,
        messages: &[MessageRef],
    ) -> Result<(), String> {
        // No native spam-rule concept on third-party IMAP servers — degrade
        // to the same destructive flow as delete_messages so the conv at
        // least disappears from the client. Callers should warn the user that
        // future deliveries from this domain won't be auto-spammed.
        if messages.is_empty() {
            return Ok(());
        }
        with_session!(self, |session| {
            imap::delete_messages_impl(&mut session, messages).await
        })
    }

    async fn blacklist_and_purge(&self, _domain: &str, _address: &str, _message_ids: &[i64]) -> Result<i64, String> {
        Err("Blacklist requires a DDMail server.".into())
    }

    async fn fetch_message_source(
        &self,
        folder: &str,
        uid: u32,
    ) -> Result<String, String> {
        with_session!(self, |session| {
            imap::fetch_source_impl(&mut session, folder, uid).await
        })
    }

    async fn fetch_raw_message(
        &self,
        folder: &str,
        uid: u32,
    ) -> Result<Vec<u8>, String> {
        with_session!(self, |session| {
            imap::fetch_raw_message(&mut session, folder, uid).await
        })
    }

    async fn fetch_attachment(
        &self,
        folder: &str,
        uid: u32,
        index: usize,
    ) -> Result<(Vec<u8>, String), String> {
        let raw = with_session!(self, |session| {
            imap::fetch_raw_message(&mut session, folder, uid).await
        })?;
        let parsed = mailparse::parse_mail(&raw).map_err(|e| format!("parse mail: {e}"))?;
        let (bytes, mime) = imap::find_attachment(&parsed, index, &mut 0)
            .ok_or_else(|| format!("attachment {index} not found"))?;
        Ok((bytes, mime))
    }

    async fn fetch_identities(&self) -> Result<Vec<Identity>, String> {
        with_session!(self, |session| {
            imap::fetch_identities_impl(&mut session).await
        })
    }

    async fn fetch_inline_part(
        &self,
        _message_id: u32,
        _content_id: &str,
    ) -> Result<InlinePart, String> {
        // Inline parts via IMAP would require fetching the raw message bytes,
        // running a MIME parser, and walking parts to find the matching
        // Content-ID. Not yet implemented — third-party IMAP accounts will
        // see broken cid: refs in inline images for now. Native path covers
        // our own server.
        Err("inline parts not implemented for IMAP provider".into())
    }

    async fn send_message(
        &self,
        smtp_host: &str,
        smtp_port: u16,
        message: &OutgoingMessage,
    ) -> Result<String, String> {
        // Delegate to the existing smtp module logic.
        // We re-use the same credentials (username/password) for SMTP.
        crate::smtp::send_message_impl(
            smtp_host,
            smtp_port,
            &self.username,
            &self.password,
            self.use_tls,
            message,
        )
        .await
    }

    async fn start_watching(
        &self,
        notifier: Notifier,
    ) -> Result<(), String> {
        let creds = Credentials {
            host: self.host.clone(),
            port: self.port,
            username: self.username.clone(),
            password: self.password.clone(),
            use_tls: self.use_tls,
            user_email: self.user_email.clone(),
        };
        self.pool.start_idle(notifier, creds).await;
        Ok(())
    }

    async fn fetch_avatar(&self, email: &str) -> Result<(Vec<u8>, String), String> {
        // IMAP-only mode: Gravatar by md5(email).
        let trimmed = email.trim().to_lowercase();
        let hash = format!("{:x}", md5::Md5::new().chain_update(trimmed.as_bytes()).finalize());
        let url = format!("https://www.gravatar.com/avatar/{hash}?d=404&s=96");
        let client = reqwest::Client::builder()
            .timeout(std::time::Duration::from_secs(3))
            .build()
            .map_err(|e| format!("client: {e}"))?;
        let resp = client.get(&url).send().await.map_err(|e| format!("HTTP: {e}"))?;
        if !resp.status().is_success() {
            return Ok((Vec::new(), String::new()));
        }
        let mime = resp
            .headers()
            .get("content-type")
            .and_then(|v| v.to_str().ok())
            .map(|s| s.split(';').next().unwrap_or("").trim().to_string())
            .unwrap_or_else(|| "image/png".to_string());
        let bytes = resp.bytes().await.map_err(|e| format!("read: {e}"))?;
        Ok((bytes.to_vec(), mime))
    }

    async fn list_calendars(&self) -> Result<Vec<DesktopCalendar>, String> {
        // A direct CalDAV collection URL exposes exactly one (read-only)
        // calendar; without one, a plain IMAP account simply has no calendars.
        let Some(_) = &self.caldav_url else {
            return Ok(Vec::new());
        };
        Ok(vec![DesktopCalendar {
            id: STANDALONE_CALDAV_CAL_ID,
            name: self.user_email.clone(),
            description: String::new(),
            color: "#3a6df0".into(),
            source_type: "caldav".into(),
            can_write: true,
            enabled: true,
            timezone: String::new(),
        }])
    }

    async fn fetch_calendar_events(
        &self,
        from_ms: i64,
        to_ms: i64,
        _calendar_ids: &[i64],
    ) -> Result<Vec<DesktopCalendarEvent>, String> {
        let Some(url) = self.caldav_collection_url().await? else {
            return Ok(Vec::new());
        };
        let url = url.as_str();
        let mut events =
            crate::caldav_client::fetch_events(url, &self.username, &self.password, from_ms, to_ms)
                .await?;
        // Assign a stable numeric id per UID and remember the mapping so
        // patch/delete (addressed by id) can resolve the CalDAV resource.
        let mut map = self.caldav_event_uids.lock().map_err(|e| format!("lock: {e}"))?;
        for e in events.iter_mut() {
            let id = crate::caldav_client::event_id_from_uid(&e.uid);
            e.id = id;
            e.calendar_id = STANDALONE_CALDAV_CAL_ID;
            e.editable = true;
            e.deletable = true;
            e.identity_email = self.user_email.clone();
            map.insert(id, e.uid.clone());
        }
        Ok(events)
    }

    async fn rsvp_event(&self, _event_id: i64, _partstat: &str) -> Result<String, String> {
        Err("RSVP requires a DDMail server.".into())
    }

    async fn create_event(&self, body: serde_json::Value) -> Result<serde_json::Value, String> {
        let Some(url) = self.caldav_collection_url().await? else {
            return Err("Creating events requires a CalDAV URL or a DDMail server.".into());
        };
        let url = url.as_str();
        let summary = body.get("summary").and_then(|v| v.as_str()).unwrap_or("");
        let description = body.get("description").and_then(|v| v.as_str()).unwrap_or("");
        let location = body.get("location").and_then(|v| v.as_str()).unwrap_or("");
        let all_day = body.get("all_day").and_then(|v| v.as_bool()).unwrap_or(false);
        let dtstart = body
            .get("dtstart")
            .and_then(|v| v.as_i64())
            .ok_or("dtstart required")?;
        let dtend = body.get("dtend").and_then(|v| v.as_i64()).filter(|&v| v != 0);
        // UID from start + a hash of the summary (no RNG in core); the server
        // treats a repeat PUT of the same UID as an update, which is benign.
        let uid = format!(
            "{}-{:x}@ddmail",
            dtstart,
            crate::caldav_client::event_id_from_uid(summary)
        );
        let ical = crate::caldav_client::build_ical(
            &uid, summary, description, location, dtstart, dtend, all_day,
        );
        crate::caldav_client::put_event(
            url,
            &self.username,
            &self.password,
            &uid,
            &ical,
            crate::caldav_client::Precondition::IfNew,
        )
        .await?;
        let id = crate::caldav_client::event_id_from_uid(&uid);
        self.caldav_event_uids
            .lock()
            .map_err(|e| format!("lock: {e}"))?
            .insert(id, uid.clone());
        Ok(serde_json::json!({
            "id": id, "uid": uid, "calendar_id": STANDALONE_CALDAV_CAL_ID
        }))
    }

    async fn patch_event(&self, event_id: i64, body: serde_json::Value) -> Result<(), String> {
        let Some(url) = self.caldav_collection_url().await? else {
            return Err("Editing events requires a CalDAV URL or a DDMail server.".into());
        };
        let url = url.as_str();
        let uid = self
            .caldav_event_uids
            .lock()
            .map_err(|e| format!("lock: {e}"))?
            .get(&event_id)
            .cloned()
            .ok_or("unknown event id — reopen the calendar and retry")?;
        // Fetch-merge-put so recurrence and other unedited properties survive;
        // the fetched ETag guards the PUT against a concurrent change.
        let (existing, etag) =
            crate::caldav_client::get_event_raw(url, &self.username, &self.password, &uid).await?;
        let summary = body.get("summary").and_then(|v| v.as_str()).unwrap_or("");
        let description = body.get("description").and_then(|v| v.as_str()).unwrap_or("");
        let location = body.get("location").and_then(|v| v.as_str()).unwrap_or("");
        let all_day = body.get("all_day").and_then(|v| v.as_bool()).unwrap_or(false);
        let dtstart = body
            .get("dtstart")
            .and_then(|v| v.as_i64())
            .ok_or("dtstart required")?;
        let dtend = body.get("dtend").and_then(|v| v.as_i64()).filter(|&v| v != 0);
        let merged = crate::caldav_client::merge_ical(
            &existing, summary, description, location, dtstart, dtend, all_day,
        );
        let pre = match etag.as_deref() {
            Some(tag) => crate::caldav_client::Precondition::IfMatch(tag),
            None => crate::caldav_client::Precondition::None,
        };
        crate::caldav_client::put_event(url, &self.username, &self.password, &uid, &merged, pre)
            .await
    }

    async fn delete_event(&self, event_id: i64) -> Result<(), String> {
        let Some(url) = self.caldav_collection_url().await? else {
            return Err("Deleting events requires a CalDAV URL or a DDMail server.".into());
        };
        let url = url.as_str();
        let uid = self
            .caldav_event_uids
            .lock()
            .map_err(|e| format!("lock: {e}"))?
            .get(&event_id)
            .cloned()
            .ok_or("unknown event id — reopen the calendar and retry")?;
        // Grab the current ETag so the delete can't clobber a newer version.
        let if_match = crate::caldav_client::get_event_raw(url, &self.username, &self.password, &uid)
            .await
            .ok()
            .and_then(|(_, tag)| tag);
        crate::caldav_client::delete_event(
            url,
            &self.username,
            &self.password,
            &uid,
            if_match.as_deref(),
        )
        .await
    }

    async fn list_contacts(&self, limit: u32) -> Result<Vec<DesktopContact>, String> {
        let Some(url) = self.carddav_collection_url().await? else {
            return Ok(Vec::new());
        };
        crate::carddav_client::fetch_contacts(
            &url,
            &self.username,
            &self.password,
            None,
            limit as usize,
        )
        .await
    }

    async fn search_contacts(&self, query: &str, limit: u32) -> Result<Vec<DesktopContact>, String> {
        let Some(url) = self.carddav_collection_url().await? else {
            return Ok(Vec::new());
        };
        crate::carddav_client::fetch_contacts(
            &url,
            &self.username,
            &self.password,
            Some(query),
            limit as usize,
        )
        .await
    }

    fn provider_type(&self) -> &'static str {
        "imap"
    }
}
