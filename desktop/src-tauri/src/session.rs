use std::collections::HashMap;
use tokio::sync::Mutex;
use serde::{Deserialize, Serialize};
use tauri::{AppHandle, Emitter, Runtime};
use tokio_util::compat::TokioAsyncReadCompatExt;
use log::{info, warn};

#[derive(Debug, Clone, Hash, Eq, PartialEq, Serialize, Deserialize)]
pub struct Credentials {
    pub host: String,
    pub port: u16,
    pub username: String,
    pub password: String,
    pub use_tls: bool,
    pub user_email: String,
}

#[derive(Debug, Clone, Serialize)]
pub struct NewMailEvent {
    pub folder: String,
    pub count: u32,
}

#[derive(Debug, Clone, Serialize)]
pub struct ConnectionStateEvent {
    pub state: String,
    pub message: Option<String>,
}

pub struct SessionPool {
    idle_handles: Mutex<HashMap<String, tokio::task::JoinHandle<()>>>,
}

/// Turn an async_imap login error into a clean, user-facing message.
/// async_imap stuffs the raw debug-formatted response into `Error::No(String)`,
/// e.g. `code: None, info: Some("invalid credentials")` — we extract the info
/// portion and map a few known phrases.
pub fn friendly_login_error(e: async_imap::error::Error) -> String {
    use async_imap::error::Error;
    match e {
        Error::No(s) | Error::Bad(s) => extract_info(&s)
            .map(|info| humanize_login_info(&info))
            .unwrap_or_else(|| s),
        Error::Io(io) => format!("Connection error: {io}"),
        Error::ConnectionLost => "Connection lost".to_string(),
        other => other.to_string(),
    }
}

fn extract_info(s: &str) -> Option<String> {
    let key = "info: Some(\"";
    let start = s.find(key)? + key.len();
    let rest = &s[start..];
    let end = rest.rfind("\")")?;
    Some(rest[..end].replace("\\\"", "\"").replace("\\\\", "\\"))
}

fn humanize_login_info(info: &str) -> String {
    let lower = info.trim().to_ascii_lowercase();
    if lower.contains("invalid credentials")
        || lower.contains("authentication failed")
        || lower.contains("authenticationfailed")
        || lower.contains("login failed")
    {
        return "Invalid username or password".to_string();
    }
    let trimmed = info.trim();
    let mut chars = trimmed.chars();
    match chars.next() {
        Some(first) => first.to_uppercase().chain(chars).collect(),
        None => String::new(),
    }
}

impl SessionPool {
    pub fn new() -> Self {
        Self { idle_handles: Mutex::new(HashMap::new()) }
    }

    /// Return an Arc-wrapped new pool that shares nothing with this one.
    /// Used to give ImapProvider its own pool instance.
    pub fn clone_inner(&self) -> std::sync::Arc<SessionPool> {
        std::sync::Arc::new(SessionPool::new())
    }

    pub async fn start_idle<R: Runtime>(&self, app: AppHandle<R>, creds: Credentials) {
        let key = format!("{}@{}:{}", creds.username, creds.host, creds.port);
        {
            let mut handles = self.idle_handles.lock().await;
            if let Some(handle) = handles.remove(&key) { handle.abort(); }
        }
        let key_clone = key.clone();
        let handle = tokio::spawn(async move { idle_loop(app, creds).await; });
        self.idle_handles.lock().await.insert(key_clone, handle);
    }
}

async fn idle_loop<R: Runtime>(app: AppHandle<R>, creds: Credentials) {
    loop {
        app.emit("connection-state", ConnectionStateEvent { state: "connecting".into(), message: None }).ok();

        let result = if creds.use_tls {
            run_idle_tls(&app, &creds).await
        } else {
            run_idle_plain(&app, &creds).await
        };

        match result {
            Ok(()) => { info!("IDLE cycle completed, restarting"); }
            Err(e) => {
                warn!("IDLE error: {e}, reconnecting in 30s");
                app.emit("connection-state", ConnectionStateEvent { state: "error".into(), message: Some(e) }).ok();
                tokio::time::sleep(std::time::Duration::from_secs(30)).await;
            }
        }
    }
}

async fn run_idle_tls<R: Runtime>(app: &AppHandle<R>, creds: &Credentials) -> Result<(), String> {
    let tls = async_native_tls::TlsConnector::new();
    let tcp = tokio::net::TcpStream::connect((creds.host.as_str(), creds.port))
        .await.map_err(|e| format!("TCP: {e}"))?;
    let tls_stream = tls.connect(&creds.host, tcp.compat())
        .await.map_err(|e| format!("TLS: {e}"))?;
    let client = async_imap::Client::new(tls_stream);
    let session = client.login(&creds.username, &creds.password)
        .await.map_err(|e| friendly_login_error(e.0))?;
    do_idle(app, session).await
}

async fn run_idle_plain<R: Runtime>(app: &AppHandle<R>, creds: &Credentials) -> Result<(), String> {
    let tcp = tokio::net::TcpStream::connect((creds.host.as_str(), creds.port))
        .await.map_err(|e| format!("TCP: {e}"))?;
    let client = async_imap::Client::new(tcp.compat());
    let session = client.login(&creds.username, &creds.password)
        .await.map_err(|e| friendly_login_error(e.0))?;
    do_idle(app, session).await
}

async fn do_idle<R: Runtime, T>(
    app: &AppHandle<R>,
    mut session: async_imap::Session<T>,
) -> Result<(), String>
where
    T: futures::AsyncRead + futures::AsyncWrite + Unpin + Send + std::fmt::Debug,
{
    app.emit("connection-state", ConnectionStateEvent { state: "connected".into(), message: None }).ok();

    let mailbox = session.select("INBOX").await.map_err(|e| format!("SELECT: {e}"))?;
    info!("IDLE starting on INBOX ({} messages)", mailbox.exists);

    let mut idle = session.idle();
    idle.init().await.map_err(|e| format!("IDLE init: {e}"))?;

    let timeout = std::time::Duration::from_secs(25 * 60);
    let (wait_fut, _stop) = idle.wait_with_timeout(timeout);
    let response = wait_fut.await.map_err(|e| format!("IDLE wait: {e}"))?;

    use async_imap::extensions::idle::IdleResponse;
    match response {
        IdleResponse::NewData(_) => {
            info!("IDLE: new data");
            app.emit("new-mail", NewMailEvent { folder: "INBOX".into(), count: 1 }).ok();
        }
        IdleResponse::Timeout => { info!("IDLE: timeout"); }
        IdleResponse::ManualInterrupt => { info!("IDLE: interrupted"); }
    }

    idle.done().await.map_err(|e| format!("IDLE done: {e}"))?;
    Ok(())
}
