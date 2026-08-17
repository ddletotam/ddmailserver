use std::sync::Arc;

use async_trait::async_trait;
use reqwest::{Client, RequestBuilder, Response, StatusCode};
use tokio::sync::{Mutex, RwLock};
use tokio_tungstenite::tungstenite;

use crate::event::{EngineEvent, Notifier};
use crate::provider::MailProvider;
use crate::types::*;

/// DDMail native provider — communicates with our server via HTTP/2 + WebSocket
/// instead of IMAP. Faster, richer features (server search, push for all folders).
///
/// The JWT is held behind an `RwLock` so it can be swapped in place when a 401
/// triggers an auto-refresh. After a successful refresh the new token is
/// emitted to the frontend (event `token-refreshed`) so it can be persisted to
/// localStorage and survive app restarts.
pub struct NativeProvider {
    server_url: String,
    token: Arc<RwLock<String>>,
    user_email: String,
    http: Client,
    /// Куда уходит `TokenRefreshed` — фронтенд по этому событию кладёт
    /// ротированный JWT на диск. Слот меняемый, потому что провайдер строится
    /// раньше, чем существует настоящий notifier: он приезжает первым же
    /// `start_watching` и там же встаёт сюда. Пока слот пуст, ротация живёт
    /// только в памяти — и следующий холодный старт поднимает токен с диска,
    /// который за 30 суток протухает без права на refresh.
    notifier: Arc<std::sync::Mutex<Option<Notifier>>>,
    account_id: String,
    // Serializes concurrent refreshes so parallel 401s don't fan out into N
    // refresh round trips (each using the same stale token, with only the
    // first having well-defined behaviour). Whoever wins the mutex performs
    // the actual exchange; latecomers observe the already-rotated token
    // and short-circuit.
    refresh_lock: Arc<Mutex<()>>,
}

/// Single-flight token refresh, callable from tasks that can't hold &self
/// (the WebSocket watcher outlives any borrow of the provider). Semantics:
/// under the lock, if the current token no longer equals `seen_token`,
/// someone already rotated it — succeed without a round trip. The server
/// accepts signature-valid expired tokens up to 30 days old.
#[allow(clippy::too_many_arguments)]
async fn refresh_token_standalone(
    http: &Client,
    server_url: &str,
    token: &Arc<RwLock<String>>,
    refresh_lock: &Arc<Mutex<()>>,
    notifier: Option<&Notifier>,
    account_id: &str,
    seen_token: &str,
) -> Result<(), String> {
    let _guard = refresh_lock.lock().await;

    // Under the lock — has someone already refreshed for us?
    {
        let current = token.read().await;
        if current.as_str() != seen_token {
            return Ok(());
        }
    }

    let resp = http
        .post(format!("{server_url}/api/desktop/v1/auth/refresh"))
        .bearer_auth(seen_token)
        .send()
        .await
        .map_err(|e| format!("Refresh request: {e}"))?;

    if !resp.status().is_success() {
        let status = resp.status();
        let body = resp.text().await.unwrap_or_default();
        return Err(format!("Refresh failed HTTP {status}: {body}"));
    }

    let data: serde_json::Value =
        resp.json().await.map_err(|e| format!("Refresh parse: {e}"))?;
    let new_token = data
        .get("token")
        .and_then(|v| v.as_str())
        .ok_or_else(|| "Refresh: missing token in response".to_string())?
        .to_string();

    *token.write().await = new_token.clone();

    if let Some(notifier) = notifier {
        notifier(EngineEvent::TokenRefreshed {
            account_id: account_id.to_string(),
            token: new_token.clone(),
        });
    }
    log::info!("NativeProvider: token refreshed for account {account_id}");
    Ok(())
}

impl NativeProvider {
    pub fn new(
        server_url: String,
        token: String,
        user_email: String,
        notifier: Option<Notifier>,
        account_id: String,
    ) -> Self {
        let http = Client::builder()
            .timeout(std::time::Duration::from_secs(30))
            .build()
            .unwrap_or_default();
        Self {
            server_url,
            token: Arc::new(RwLock::new(token)),
            user_email,
            http,
            notifier: Arc::new(std::sync::Mutex::new(notifier)),
            account_id,
            refresh_lock: Arc::new(Mutex::new(())),
        }
    }

    fn api_url(&self, path: &str) -> String {
        format!("{}/api/desktop/v1{}", self.server_url, path)
    }

    /// Снимок текущего notifier'а. Владеющая копия (Notifier — это Arc), чтобы
    /// не держать sync-гард через await.
    fn current_notifier(&self) -> Option<Notifier> {
        self.notifier.lock().ok().and_then(|slot| slot.clone())
    }

    /// Exchange the current (possibly expired) token for a fresh one.
    /// Updates the in-memory token and notifies the frontend on success.
    ///
    /// `seen_token` is what the caller had in hand when its request hit 401.
    /// We acquire the refresh lock and, *under the lock*, compare seen vs.
    /// current; if a parallel caller already rotated, we return success
    /// immediately. Otherwise we perform the exchange. This is the standard
    /// single-flight pattern: N concurrent 401s produce one refresh.
    async fn refresh_token(&self, seen_token: &str) -> Result<(), String> {
        let notifier = self.current_notifier();
        refresh_token_standalone(
            &self.http,
            &self.server_url,
            &self.token,
            &self.refresh_lock,
            notifier.as_ref(),
            &self.account_id,
            seen_token,
        )
        .await
    }

    /// Send a request with auto-refresh on 401. The closure is called once
    /// with the current token; if the response is 401, the token is refreshed
    /// and the closure is invoked again with the new token.
    async fn send_authed<F>(&self, build: F) -> Result<Response, String>
    where
        F: Fn(&Client, &str) -> RequestBuilder,
    {
        let token = self.token.read().await.clone();
        let resp = build(&self.http, &token)
            .send()
            .await
            .map_err(|e| format!("HTTP: {e}"))?;

        if resp.status() != StatusCode::UNAUTHORIZED {
            return Ok(resp);
        }

        // Pass the token that hit 401 so refresh_token can short-circuit if
        // a concurrent request already rotated under us.
        self.refresh_token(&token).await?;
        let new_token = self.token.read().await.clone();
        build(&self.http, &new_token)
            .send()
            .await
            .map_err(|e| format!("HTTP: {e}"))
    }

    async fn get<T: serde::de::DeserializeOwned>(&self, path: &str) -> Result<T, String> {
        let url = self.api_url(path);
        let resp = self
            .send_authed(|http, token| http.get(&url).bearer_auth(token))
            .await?;

        if !resp.status().is_success() {
            let status = resp.status();
            let body = resp.text().await.unwrap_or_default();
            return Err(format!("HTTP {status}: {body}"));
        }

        resp.json::<T>()
            .await
            .map_err(|e| format!("JSON decode {path}: {e}"))
    }

    async fn post<B: serde::Serialize, T: serde::de::DeserializeOwned>(
        &self,
        path: &str,
        body: &B,
    ) -> Result<T, String> {
        let url = self.api_url(path);
        let body_json = serde_json::to_value(body).map_err(|e| format!("JSON encode: {e}"))?;
        let resp = self
            .send_authed(|http, token| http.post(&url).bearer_auth(token).json(&body_json))
            .await?;

        if !resp.status().is_success() {
            let status = resp.status();
            let body = resp.text().await.unwrap_or_default();
            return Err(format!("HTTP {status}: {body}"));
        }

        resp.json::<T>()
            .await
            .map_err(|e| format!("JSON decode {path}: {e}"))
    }
}

#[async_trait]
impl MailProvider for NativeProvider {
    async fn list_folders(&self) -> Result<Vec<Folder>, String> {
        self.get("/folders").await
    }

    async fn fetch_conversations(
        &self,
        _our_addrs: &[String],
        limit: u32,
    ) -> Result<Vec<Conversation>, String> {
        // Server groups conversations for us — much faster than IMAP
        self.get(&format!("/conversations?limit={limit}")).await
    }

    async fn fetch_conversations_delta(
        &self,
        _our_addrs: &[String],
        limit: u32,
        since_ms: i64,
    ) -> Result<(Vec<Conversation>, i64, bool), String> {
        // ?since= switches the server to the delta envelope: only changed
        // conversations + server_now_ms watermark for the next call.
        // since=0 is a full sync that still yields a watermark.
        #[derive(serde::Deserialize)]
        struct DeltaResp {
            server_now_ms: i64,
            conversations: Vec<Conversation>,
        }
        let since = since_ms.max(0);
        let resp: DeltaResp = self
            .get(&format!("/conversations?limit={limit}&since={since}"))
            .await?;
        Ok((resp.conversations, resp.server_now_ms, since > 0))
    }

    async fn fetch_changes(&self, since: i64) -> Result<Option<ChangesResponse>, String> {
        let resp: ChangesResponse = self
            .get(&format!("/changes?since={}", since.max(0)))
            .await?;
        Ok(Some(resp))
    }

    async fn fetch_conversation_messages(
        &self,
        _our_addrs: &[String],
        messages: &[MessageRef],
    ) -> Result<Vec<MessageBody>, String> {
        // POST message refs, server returns full bodies
        self.post("/conversations/messages", &messages).await
    }

    async fn search_messages(
        &self,
        _user_email: &str,
        query: &str,
    ) -> Result<Vec<MessageEnvelope>, String> {
        let encoded = urlencoding::encode(query);
        self.get(&format!("/search?q={encoded}")).await
    }

    async fn set_flags(
        &self,
        folder: &str,
        uid: u32,
        flags: &str,
        add: bool,
    ) -> Result<(), String> {
        let body = serde_json::json!({
            "messages": [{"folder": folder, "uid": uid}],
            "flags": flags,
            "add": add,
        });
        let _: serde_json::Value = self.post("/messages/flags", &body).await?;
        Ok(())
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
        let body = serde_json::json!({
            "messages": messages,
            "flags": flags,
            "add": add,
        });
        let _: serde_json::Value = self.post("/messages/flags", &body).await?;
        Ok(())
    }

    async fn delete_messages(
        &self,
        messages: &[MessageRef],
    ) -> Result<(), String> {
        if messages.is_empty() {
            return Ok(());
        }
        let body = serde_json::json!({ "messages": messages });
        let _: serde_json::Value = self.post("/messages/delete", &body).await?;
        Ok(())
    }

    async fn mark_spam_by_domain(
        &self,
        domain: &str,
        messages: &[MessageRef],
    ) -> Result<(), String> {
        if domain.is_empty() {
            return Err("empty domain".into());
        }
        let body = serde_json::json!({ "domain": domain, "messages": messages });
        let _: serde_json::Value = self.post("/spam/mark-domain", &body).await?;
        Ok(())
    }

    async fn blacklist_and_purge(&self, scope: &str, fallback_addr: &str, message_ids: &[i64]) -> Result<PurgeOutcome, String> {
        if message_ids.is_empty() && fallback_addr.is_empty() {
            return Err("no sender to block".into());
        }
        // Server resolves the real sender from message_ids; `address` is only
        // a fallback for when they resolve nothing. `scope` picks address vs
        // domain blocking.
        let body = serde_json::json!({
            "scope": scope,
            "address": fallback_addr,
            "message_ids": message_ids,
        });
        let resp: PurgeOutcome = self.post("/spam/blacklist-and-purge", &body).await?;
        Ok(resp)
    }

    async fn fetch_message_source(
        &self,
        _folder: &str,
        uid: u32,
    ) -> Result<String, String> {
        // uid here is actually the server message ID for native provider
        let url = self.api_url(&format!("/messages/{uid}/source"));
        let resp = self
            .send_authed(|http, token| http.get(&url).bearer_auth(token))
            .await?;

        if !resp.status().is_success() {
            return Err(format!("HTTP {}", resp.status()));
        }

        resp.text()
            .await
            .map_err(|e| format!("Read body: {e}"))
    }

    async fn fetch_raw_message(
        &self,
        _folder: &str,
        uid: u32,
    ) -> Result<Vec<u8>, String> {
        let url = self.api_url(&format!("/messages/{uid}/source"));
        let resp = self
            .send_authed(|http, token| http.get(&url).bearer_auth(token))
            .await?;

        if !resp.status().is_success() {
            return Err(format!("HTTP {}", resp.status()));
        }

        resp.bytes()
            .await
            .map(|b| b.to_vec())
            .map_err(|e| format!("Read body: {e}"))
    }

    async fn fetch_attachment(
        &self,
        _folder: &str,
        uid: u32,
        index: usize,
    ) -> Result<(Vec<u8>, String), String> {
        // `uid` here is messages.id in native mode (see comment on
        // fetch_message_source).
        let url = self.api_url(&format!("/messages/{uid}/attachments/{index}"));
        let resp = self
            .send_authed(|http, token| http.get(&url).bearer_auth(token))
            .await?;
        if !resp.status().is_success() {
            return Err(format!("HTTP {}", resp.status()));
        }
        let mime = resp
            .headers()
            .get("content-type")
            .and_then(|v| v.to_str().ok())
            .map(|s| s.split(';').next().unwrap_or("").trim().to_string())
            .unwrap_or_else(|| "application/octet-stream".to_string());
        let bytes = resp.bytes().await.map_err(|e| format!("read: {e}"))?;
        Ok((bytes.to_vec(), mime))
    }

    async fn fetch_identities(&self) -> Result<Vec<Identity>, String> {
        self.get("/identities").await
    }

    async fn fetch_avatar(&self, email: &str) -> Result<(Vec<u8>, String), String> {
        // Server walks the source chain and returns bytes (or 204 None).
        // Email goes through as a query param — keeps `@`, `+`, dots etc.
        // safe across nginx and gorilla/mux without depending on path encoding.
        let encoded = urlencoding::encode(email.trim());
        let url = self.api_url(&format!("/avatars?email={encoded}"));
        let resp = self
            .send_authed(|http, token| http.get(&url).bearer_auth(token))
            .await?;
        let status = resp.status();
        if status.as_u16() == 204 {
            return Ok((Vec::new(), String::new()));
        }
        if !status.is_success() {
            return Err(format!("HTTP {status}"));
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
        self.get("/calendars").await
    }

    async fn list_contacts(&self, limit: u32) -> Result<Vec<DesktopContact>, String> {
        self.get(&format!("/contacts?limit={limit}")).await
    }

    async fn search_contacts(&self, query: &str, limit: u32) -> Result<Vec<DesktopContact>, String> {
        let q = urlencoding::encode(query);
        self.get(&format!("/contacts/search?q={q}&limit={limit}")).await
    }

    async fn create_contact(&self, body: serde_json::Value) -> Result<serde_json::Value, String> {
        self.post("/contacts", &body).await
    }

    async fn update_contact(&self, id: i64, body: serde_json::Value) -> Result<(), String> {
        let url = self.api_url(&format!("/contacts/{id}"));
        let resp = self
            .send_authed(|http, token| http.patch(&url).bearer_auth(token).json(&body))
            .await?;
        if !resp.status().is_success() {
            let status = resp.status();
            let body = resp.text().await.unwrap_or_default();
            return Err(format!("HTTP {status}: {body}"));
        }
        Ok(())
    }

    async fn delete_contact(&self, id: i64) -> Result<(), String> {
        let url = self.api_url(&format!("/contacts/{id}"));
        let resp = self
            .send_authed(|http, token| http.delete(&url).bearer_auth(token))
            .await?;
        if !resp.status().is_success() {
            let status = resp.status();
            let body = resp.text().await.unwrap_or_default();
            return Err(format!("HTTP {status}: {body}"));
        }
        Ok(())
    }

    async fn fetch_calendar_events(
        &self,
        from_ms: i64,
        to_ms: i64,
        calendar_ids: &[i64],
    ) -> Result<Vec<DesktopCalendarEvent>, String> {
        let mut path = format!("/calendar-events?from={from_ms}&to={to_ms}");
        if !calendar_ids.is_empty() {
            let ids = calendar_ids
                .iter()
                .map(i64::to_string)
                .collect::<Vec<_>>()
                .join(",");
            path.push_str(&format!("&ids={ids}"));
        }
        self.get(&path).await
    }

    async fn rsvp_event(&self, event_id: i64, partstat: &str) -> Result<String, String> {
        let body = serde_json::json!({ "partstat": partstat });
        let resp: serde_json::Value = self.post(&format!("/events/{event_id}/rsvp"), &body).await?;
        Ok(resp
            .get("partstat")
            .and_then(|v| v.as_str())
            .unwrap_or(partstat)
            .to_string())
    }

    async fn patch_event(&self, event_id: i64, body: serde_json::Value) -> Result<(), String> {
        let url = self.api_url(&format!("/events/{event_id}"));
        let resp = self
            .send_authed(|http, token| {
                http.patch(&url).bearer_auth(token).json(&body)
            })
            .await?;
        if !resp.status().is_success() {
            let status = resp.status();
            let body = resp.text().await.unwrap_or_default();
            return Err(format!("HTTP {status}: {body}"));
        }
        Ok(())
    }

    async fn create_event(&self, body: serde_json::Value) -> Result<serde_json::Value, String> {
        self.post("/events", &body).await
    }

    async fn delete_event(&self, event_id: i64) -> Result<(), String> {
        let url = self.api_url(&format!("/events/{event_id}"));
        let resp = self
            .send_authed(|http, token| http.delete(&url).bearer_auth(token))
            .await?;
        if !resp.status().is_success() {
            let status = resp.status();
            let body = resp.text().await.unwrap_or_default();
            return Err(format!("HTTP {status}: {body}"));
        }
        Ok(())
    }

    async fn fetch_inline_part(
        &self,
        message_id: u32,
        content_id: &str,
    ) -> Result<InlinePart, String> {
        let cid_enc = urlencoding::encode(content_id);
        let url = self.api_url(&format!("/messages/{message_id}/parts/{cid_enc}"));
        let resp = self
            .send_authed(|http, token| http.get(&url).bearer_auth(token))
            .await?;

        if !resp.status().is_success() {
            return Err(format!("HTTP {}", resp.status()));
        }

        let mime_type = resp
            .headers()
            .get("content-type")
            .and_then(|v| v.to_str().ok())
            .map(|s| s.to_string())
            .unwrap_or_else(|| "application/octet-stream".to_string());

        let bytes = resp
            .bytes()
            .await
            .map_err(|e| format!("Read body: {e}"))?;

        use base64::Engine as _;
        let content_b64 = base64::engine::general_purpose::STANDARD.encode(&bytes);
        Ok(InlinePart { mime_type, content_b64 })
    }

    async fn send_message(
        &self,
        _smtp_host: &str,
        _smtp_port: u16,
        message: &OutgoingMessage,
    ) -> Result<String, String> {
        let resp: serde_json::Value = self.post("/send", message).await?;
        Ok(resp
            .get("status")
            .and_then(|v| v.as_str())
            .unwrap_or("queued")
            .to_string())
    }

    async fn start_watching(&self, notifier: Notifier) -> Result<(), String> {
        // Настоящий notifier появляется только здесь — с этого момента и
        // REST-путь, и watcher шлют `TokenRefreshed` фронтенду, а тот пишет
        // токен в accounts.json. До этой строки слот держал заглушку из
        // build_provider, и ротация никуда не сообщалась.
        if let Ok(mut slot) = self.notifier.lock() {
            *slot = Some(notifier.clone());
        }
        let ws_base = self
            .server_url
            .replace("https://", "wss://")
            .replace("http://", "ws://");
        let user_email = self.user_email.clone();
        let token = self.token.clone();
        // Pieces for in-loop token refresh: the watcher is the only network
        // activity in push-driven idle periods, so if IT doesn't refresh an
        // expired JWT, nothing does — the client stays in "error" forever.
        let http = self.http.clone();
        let server_url = self.server_url.clone();
        let refresh_lock = self.refresh_lock.clone();
        let self_notifier = self.notifier.clone();
        let account_id = self.account_id.clone();

        tokio::spawn(async move {
            log::info!("NativeProvider: connecting WebSocket for {user_email}");
            notifier(EngineEvent::ConnectionState {
                state: "connecting".into(),
                message: None,
            });

            // Short first retry, doubling to a 30s ceiling; reset on success.
            let mut backoff_secs: u64 = 2;

            loop {
                let current_token = token.read().await.clone();
                let ws_url = format!("{ws_base}/api/desktop/v1/ws?token={current_token}");
                match tokio_tungstenite::connect_async(&ws_url).await {
                    Ok((ws_stream, _)) => {
                        backoff_secs = 2;
                        log::info!("NativeProvider: WebSocket connected for {user_email}");
                        notifier(EngineEvent::ConnectionState {
                            state: "connected".into(),
                            message: None,
                        });

                        use futures::StreamExt;
                        let (_, mut read) = ws_stream.split();

                        // Read watchdog: the server pings every 30s, so a
                        // healthy connection always produces SOME frame
                        // within a minute. A half-open TCP session (NAT
                        // reset, server restart the FIN of which never
                        // arrived) otherwise parks read.next() forever —
                        // the loop never reconnects and push goes silent.
                        loop {
                            let msg = match tokio::time::timeout(
                                std::time::Duration::from_secs(90),
                                read.next(),
                            )
                            .await
                            {
                                Err(_) => {
                                    log::warn!(
                                        "NativeProvider: no frames for 90s — dropping dead WebSocket"
                                    );
                                    break;
                                }
                                Ok(None) => break, // stream ended
                                Ok(Some(m)) => m,
                            };
                            match msg {
                                Ok(tungstenite::Message::Text(text)) => {
                                    if let Ok(event) =
                                        serde_json::from_str::<serde_json::Value>(&text)
                                    {
                                        let event_type =
                                            event.get("type").and_then(|v| v.as_str()).unwrap_or("");
                                        let folder = event
                                            .get("folder")
                                            .and_then(|v| v.as_str())
                                            .unwrap_or("INBOX");
                                        let count =
                                            event.get("count").and_then(|v| v.as_u64()).unwrap_or(1)
                                                as u32;

                                        if event_type == "new_message" {
                                            notifier(EngineEvent::NewMail {
                                                folder: folder.to_string(),
                                                count,
                                                new_count: event
                                                    .get("new_count")
                                                    .and_then(|v| v.as_u64())
                                                    .unwrap_or(1)
                                                    as u32,
                                                from: event
                                                    .get("from")
                                                    .and_then(|v| v.as_str())
                                                    .unwrap_or_default()
                                                    .to_string(),
                                                subject: event
                                                    .get("subject")
                                                    .and_then(|v| v.as_str())
                                                    .unwrap_or_default()
                                                    .to_string(),
                                                message_id: event
                                                    .get("message_id")
                                                    .and_then(|v| v.as_i64())
                                                    .unwrap_or(0),
                                            });
                                        } else if event_type == "message_sent" {
                                            // Our own message reached Sent — the
                                            // conversation it belongs to exists
                                            // now. Quiet by design: no toast.
                                            notifier(EngineEvent::MessageSent);
                                        } else if event_type == "calendar_updated" {
                                            let calendar_id = event
                                                .get("calendar_id")
                                                .and_then(|v| v.as_i64())
                                                .unwrap_or(0);
                                            notifier(EngineEvent::CalendarUpdated { calendar_id });
                                        } else if event_type == "expunge" {
                                            // Messages deleted elsewhere (another
                                            // client, spam purge). Tell the engine
                                            // to drop the now-dangling conversations.
                                            notifier(EngineEvent::Expunged {
                                                folder: folder.to_string(),
                                            });
                                        } else if event_type == "flags_changed" {
                                            // Read/starred elsewhere — refresh
                                            // unread state via a delta fetch.
                                            notifier(EngineEvent::FlagsChanged {
                                                folder: folder.to_string(),
                                            });
                                        }
                                        log::info!(
                                            "NativeProvider: event {event_type} folder={folder}"
                                        );
                                    }
                                }
                                Ok(tungstenite::Message::Close(_)) => {
                                    log::info!("NativeProvider: WebSocket closed by server");
                                    break;
                                }
                                Err(e) => {
                                    log::warn!("NativeProvider: WebSocket error: {e}");
                                    break;
                                }
                                _ => {} // Ping/Pong handled by tungstenite
                            }
                        }
                    }
                    Err(e) => {
                        log::warn!("NativeProvider: WebSocket connect failed: {e}");
                        notifier(EngineEvent::ConnectionState {
                            state: "error".into(),
                            message: Some(e.to_string()),
                        });
                        // The usual cause after long uptime is an expired JWT
                        // (the handshake is rejected with 401 before upgrade).
                        // Try a refresh with the token we just failed on; the
                        // next iteration reads the rotated token from the lock.
                        let watcher_notifier =
                            self_notifier.lock().ok().and_then(|slot| slot.clone());
                        if let Err(re) = refresh_token_standalone(
                            &http,
                            &server_url,
                            &token,
                            &refresh_lock,
                            watcher_notifier.as_ref(),
                            &account_id,
                            &current_token,
                        )
                        .await
                        {
                            log::warn!("NativeProvider: watcher token refresh failed: {re}");
                        }
                    }
                }

                log::info!("NativeProvider: reconnecting in {backoff_secs}s...");
                tokio::time::sleep(std::time::Duration::from_secs(backoff_secs)).await;
                backoff_secs = (backoff_secs * 2).min(30);
                // Flapping or reconnecting — show truthful state while retrying.
                notifier(EngineEvent::ConnectionState {
                    state: "connecting".into(),
                    message: None,
                });
            }
        });

        Ok(())
    }

    fn provider_type(&self) -> &'static str {
        "native"
    }
}
