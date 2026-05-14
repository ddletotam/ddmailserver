use std::sync::Arc;

use async_trait::async_trait;
use reqwest::{Client, RequestBuilder, Response, StatusCode};
use tauri::{AppHandle, Emitter};
use tokio::sync::{Mutex, RwLock};
use tokio_tungstenite::tungstenite;

use crate::provider::MailProvider;
use crate::session::{ConnectionStateEvent, NewMailEvent};
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
    app: Option<AppHandle>,
    account_id: String,
    // Serializes concurrent refreshes so parallel 401s don't fan out into N
    // refresh round trips (each using the same stale token, with only the
    // first having well-defined behaviour). Whoever wins the mutex performs
    // the actual exchange; latecomers observe the already-rotated token
    // and short-circuit.
    refresh_lock: Arc<Mutex<()>>,
}

impl NativeProvider {
    pub fn new(
        server_url: String,
        token: String,
        user_email: String,
        app: Option<AppHandle>,
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
            app,
            account_id,
            refresh_lock: Arc::new(Mutex::new(())),
        }
    }

    fn api_url(&self, path: &str) -> String {
        format!("{}/api/desktop/v1{}", self.server_url, path)
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
        let _guard = self.refresh_lock.lock().await;

        // Under the lock — has someone already refreshed for us?
        {
            let current = self.token.read().await;
            if current.as_str() != seen_token {
                return Ok(());
            }
        }

        let resp = self
            .http
            .post(format!("{}/api/desktop/v1/auth/refresh", self.server_url))
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

        *self.token.write().await = new_token.clone();

        if let Some(app) = &self.app {
            let _ = app.emit(
                "token-refreshed",
                serde_json::json!({
                    "account_id": self.account_id,
                    "token": new_token,
                }),
            );
        }
        log::info!("NativeProvider: token refreshed for account {}", self.account_id);
        Ok(())
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

    async fn start_watching(&self, app: AppHandle) -> Result<(), String> {
        let ws_base = self
            .server_url
            .replace("https://", "wss://")
            .replace("http://", "ws://");
        let user_email = self.user_email.clone();
        let token = self.token.clone();

        tokio::spawn(async move {
            log::info!("NativeProvider: connecting WebSocket for {user_email}");
            app.emit(
                "connection-state",
                ConnectionStateEvent {
                    state: "connecting".into(),
                    message: None,
                },
            )
            .ok();

            loop {
                let current_token = token.read().await.clone();
                let ws_url = format!("{ws_base}/api/desktop/v1/ws?token={current_token}");
                match tokio_tungstenite::connect_async(&ws_url).await {
                    Ok((ws_stream, _)) => {
                        log::info!("NativeProvider: WebSocket connected for {user_email}");
                        app.emit(
                            "connection-state",
                            ConnectionStateEvent {
                                state: "connected".into(),
                                message: None,
                            },
                        )
                        .ok();

                        use futures::StreamExt;
                        let (_, mut read) = ws_stream.split();

                        while let Some(msg) = read.next().await {
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
                                            app.emit(
                                                "new-mail",
                                                NewMailEvent {
                                                    folder: folder.to_string(),
                                                    count,
                                                },
                                            )
                                            .ok();
                                        } else if event_type == "calendar_updated" {
                                            let calendar_id = event
                                                .get("calendar_id")
                                                .and_then(|v| v.as_i64())
                                                .unwrap_or(0);
                                            app.emit(
                                                "calendar-updated",
                                                serde_json::json!({ "calendar_id": calendar_id }),
                                            )
                                            .ok();
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
                        app.emit(
                            "connection-state",
                            ConnectionStateEvent {
                                state: "error".into(),
                                message: Some(e.to_string()),
                            },
                        )
                        .ok();
                    }
                }

                // Reconnect after 30s
                log::info!("NativeProvider: reconnecting in 30s...");
                tokio::time::sleep(std::time::Duration::from_secs(30)).await;
            }
        });

        Ok(())
    }

    fn provider_type(&self) -> &'static str {
        "native"
    }
}
