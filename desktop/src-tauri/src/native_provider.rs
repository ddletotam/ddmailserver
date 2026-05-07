use async_trait::async_trait;
use reqwest::Client;
use tauri::{AppHandle, Emitter};
use tokio_tungstenite::tungstenite;

use crate::provider::MailProvider;
use crate::session::{ConnectionStateEvent, NewMailEvent};
use crate::types::*;

/// DDMail native provider — communicates with our server via HTTP/2 + WebSocket
/// instead of IMAP. Faster, richer features (server search, push for all folders).
pub struct NativeProvider {
    server_url: String,
    token: String,
    user_email: String,
    http: Client,
}

impl NativeProvider {
    pub fn new(server_url: String, token: String, user_email: String) -> Self {
        let http = Client::builder()
            .timeout(std::time::Duration::from_secs(30))
            .build()
            .unwrap_or_default();
        Self {
            server_url,
            token,
            user_email,
            http,
        }
    }

    fn api_url(&self, path: &str) -> String {
        format!("{}/api/desktop/v1{}", self.server_url, path)
    }

    async fn get<T: serde::de::DeserializeOwned>(&self, path: &str) -> Result<T, String> {
        let resp = self
            .http
            .get(self.api_url(path))
            .bearer_auth(&self.token)
            .send()
            .await
            .map_err(|e| format!("HTTP GET {path}: {e}"))?;

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
        let resp = self
            .http
            .post(self.api_url(path))
            .bearer_auth(&self.token)
            .json(body)
            .send()
            .await
            .map_err(|e| format!("HTTP POST {path}: {e}"))?;

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

    async fn fetch_message_source(
        &self,
        _folder: &str,
        uid: u32,
    ) -> Result<String, String> {
        // uid here is actually the server message ID for native provider
        let resp = self
            .http
            .get(self.api_url(&format!("/messages/{uid}/source")))
            .bearer_auth(&self.token)
            .send()
            .await
            .map_err(|e| format!("HTTP: {e}"))?;

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
        let resp = self
            .http
            .get(self.api_url(&format!("/messages/{uid}/source")))
            .bearer_auth(&self.token)
            .send()
            .await
            .map_err(|e| format!("HTTP: {e}"))?;

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

    async fn fetch_inline_part(
        &self,
        message_id: u32,
        content_id: &str,
    ) -> Result<InlinePart, String> {
        let cid_enc = urlencoding::encode(content_id);
        let resp = self
            .http
            .get(self.api_url(&format!("/messages/{message_id}/parts/{cid_enc}")))
            .bearer_auth(&self.token)
            .send()
            .await
            .map_err(|e| format!("HTTP: {e}"))?;

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
        let ws_url = format!(
            "{}/api/desktop/v1/ws?token={}",
            self.server_url.replace("https://", "wss://").replace("http://", "ws://"),
            self.token,
        );
        let user_email = self.user_email.clone();

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
