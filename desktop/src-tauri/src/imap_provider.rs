use async_trait::async_trait;
use md5::Digest;
use tauri::AppHandle;

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
}

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
        app: AppHandle,
    ) -> Result<(), String> {
        let creds = Credentials {
            host: self.host.clone(),
            port: self.port,
            username: self.username.clone(),
            password: self.password.clone(),
            use_tls: self.use_tls,
            user_email: self.user_email.clone(),
        };
        self.pool.start_idle(app, creds).await;
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
        Err("Calendars require a DDMail server. Add an account on a server that supports the native protocol, or wait for standalone CalDAV support.".into())
    }

    async fn fetch_calendar_events(
        &self,
        _from_ms: i64,
        _to_ms: i64,
        _calendar_ids: &[i64],
    ) -> Result<Vec<DesktopCalendarEvent>, String> {
        Err("Calendars require a DDMail server.".into())
    }

    async fn rsvp_event(&self, _event_id: i64, _partstat: &str) -> Result<String, String> {
        Err("RSVP requires a DDMail server.".into())
    }

    async fn patch_event(&self, _event_id: i64, _body: serde_json::Value) -> Result<(), String> {
        Err("Editing events requires a DDMail server.".into())
    }

    fn provider_type(&self) -> &'static str {
        "imap"
    }
}
