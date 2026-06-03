//! Live mail engine for the native client. Runs the ddmail-core providers on a
//! dedicated tokio runtime/thread, talking to the UI via command/result
//! channels. Kept off the Ultralight render thread (which is single-threaded).
//!
//! Account config comes from env vars for now (no login UI yet):
//!   DDMAIL_IMAP_HOST, DDMAIL_IMAP_PORT, DDMAIL_IMAP_USER, DDMAIL_IMAP_PASS,
//!   DDMAIL_IMAP_TLS (1/0), DDMAIL_EMAIL
//!   optional native mode: DDMAIL_NATIVE_URL, DDMAIL_NATIVE_TOKEN

use std::sync::mpsc;
use std::sync::Arc;

use ddmail_core::cache::Cache;
use ddmail_core::event::noop_notifier;
use ddmail_core::imap;
use ddmail_core::imap_provider::ImapProvider;
use ddmail_core::native_provider::NativeProvider;
use ddmail_core::provider::MailProvider;
use ddmail_core::session::SessionPool;
use ddmail_core::types::{Conversation, MessageBody, MessageRef};

#[derive(Clone)]
pub struct AccountConfig {
    pub host: String,
    pub port: u16,
    pub username: String,
    pub password: String,
    pub use_tls: bool,
    pub email: String,
    pub native_url: Option<String>,
    pub native_token: Option<String>,
}

impl AccountConfig {
    /// Load from environment. Returns None if the IMAP basics aren't set.
    pub fn from_env() -> Option<Self> {
        let host = std::env::var("DDMAIL_IMAP_HOST").ok()?;
        let username = std::env::var("DDMAIL_IMAP_USER").ok()?;
        let password = std::env::var("DDMAIL_IMAP_PASS").ok()?;
        let email = std::env::var("DDMAIL_EMAIL").unwrap_or_else(|_| username.clone());
        let port = std::env::var("DDMAIL_IMAP_PORT")
            .ok()
            .and_then(|s| s.parse().ok())
            .unwrap_or(993);
        let use_tls = std::env::var("DDMAIL_IMAP_TLS").map(|s| s != "0").unwrap_or(true);
        Some(AccountConfig {
            host,
            port,
            username,
            password,
            use_tls,
            email,
            native_url: std::env::var("DDMAIL_NATIVE_URL").ok(),
            native_token: std::env::var("DDMAIL_NATIVE_TOKEN").ok(),
        })
    }

    pub fn account_key(&self) -> String {
        imap::account_key(&self.host, &self.username)
    }
}

fn build_provider(cfg: &AccountConfig) -> Arc<dyn MailProvider> {
    if let (Some(url), Some(token)) = (&cfg.native_url, &cfg.native_token) {
        Arc::new(NativeProvider::new(
            url.clone(),
            token.clone(),
            cfg.email.clone(),
            Some(noop_notifier()),
            cfg.email.clone(),
        ))
    } else {
        Arc::new(ImapProvider {
            host: cfg.host.clone(),
            port: cfg.port,
            username: cfg.username.clone(),
            password: cfg.password.clone(),
            use_tls: cfg.use_tls,
            user_email: cfg.email.clone(),
            pool: Arc::new(SessionPool::new()),
        })
    }
}

/// "Our" addresses for threading: cached identities + the account email.
fn resolve_our_addrs(cache: &Cache, cfg: &AccountConfig) -> Vec<String> {
    let key = cfg.account_key();
    let mut addrs: Vec<String> = cache
        .load_identities(&key)
        .map(|ids| ids.into_iter().map(|i| i.email.to_lowercase()).collect())
        .unwrap_or_default();
    let me = cfg.email.to_lowercase();
    if !addrs.iter().any(|a| a == &me) {
        addrs.push(me);
    }
    addrs
}

/// Commands the UI sends to the engine thread.
pub enum EngineCmd {
    FetchConversations { limit: u32 },
    FetchMessages { messages: Vec<MessageRef> },
}

/// Results the engine sends back to the UI.
pub enum EngineResult {
    Conversations(Vec<Conversation>),
    Messages(Vec<MessageBody>),
    Error(String),
}

/// Spawn the engine thread. Returns a command sender; results arrive via `on_result`
/// (called on the engine thread — forward to the UI loop yourself).
pub fn spawn(
    cfg: AccountConfig,
    cache: Arc<Cache>,
    on_result: impl Fn(EngineResult) + Send + 'static,
) -> mpsc::Sender<EngineCmd> {
    let (tx, rx) = mpsc::channel::<EngineCmd>();
    std::thread::spawn(move || {
        let rt = match tokio::runtime::Runtime::new() {
            Ok(rt) => rt,
            Err(e) => {
                on_result(EngineResult::Error(format!("runtime: {e}")));
                return;
            }
        };
        let provider = build_provider(&cfg);
        let key = cfg.account_key();

        while let Ok(cmd) = rx.recv() {
            match cmd {
                EngineCmd::FetchConversations { limit } => {
                    let our = resolve_our_addrs(&cache, &cfg);
                    let r = rt.block_on(provider.fetch_conversations(&our, limit));
                    match r {
                        Ok(convs) => {
                            cache.save_conversations(&key, &convs).ok();
                            on_result(EngineResult::Conversations(convs));
                        }
                        Err(e) => on_result(EngineResult::Error(e)),
                    }
                }
                EngineCmd::FetchMessages { messages } => {
                    let our = resolve_our_addrs(&cache, &cfg);
                    let r = rt.block_on(provider.fetch_conversation_messages(&our, &messages));
                    match r {
                        Ok(bodies) => {
                            cache.save_message_bodies(&key, &bodies).ok();
                            on_result(EngineResult::Messages(bodies));
                        }
                        Err(e) => on_result(EngineResult::Error(e)),
                    }
                }
            }
        }
    });
    tx
}
