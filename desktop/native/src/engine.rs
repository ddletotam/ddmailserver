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
use ddmail_core::event::{noop_notifier, EngineEvent, Notifier};
use ddmail_core::imap;
use ddmail_core::imap_provider::ImapProvider;
use ddmail_core::native_provider::NativeProvider;
use ddmail_core::provider::MailProvider;
use ddmail_core::session::SessionPool;
use ddmail_core::types::{
    Contact, Conversation, DesktopCalendar, DesktopCalendarEvent, MessageBody, MessageEnvelope,
    MessageRef, OutgoingMessage,
};

#[derive(Clone)]
pub struct AccountConfig {
    pub host: String,
    pub port: u16,
    pub username: String,
    pub password: String,
    pub use_tls: bool,
    pub email: String,
    pub smtp_host: String,
    pub smtp_port: u16,
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
        let smtp_host = std::env::var("DDMAIL_SMTP_HOST").unwrap_or_else(|_| host.clone());
        let smtp_port = std::env::var("DDMAIL_SMTP_PORT")
            .ok()
            .and_then(|s| s.parse().ok())
            .unwrap_or(465);
        Some(AccountConfig {
            host,
            port,
            username,
            password,
            use_tls,
            email,
            smtp_host,
            smtp_port,
            native_url: std::env::var("DDMAIL_NATIVE_URL").ok(),
            native_token: std::env::var("DDMAIL_NATIVE_TOKEN").ok(),
        })
    }

    /// Load from `%APPDATA%/ru.letotam.ddmail/account.json` (fallback when env
    /// isn't set). Plaintext for now — same trust level as the env approach;
    /// a real login screen + keyring comes later.
    pub fn from_file() -> Option<Self> {
        let base = std::env::var("APPDATA").or_else(|_| std::env::var("HOME")).ok()?;
        let path = std::path::Path::new(&base).join("ru.letotam.ddmail").join("account.json");
        let data = std::fs::read_to_string(&path).ok()?;
        let v: serde_json::Value = serde_json::from_str(&data).ok()?;
        let host = v.get("host")?.as_str()?.to_string();
        let username = v.get("username")?.as_str()?.to_string();
        let password = v.get("password")?.as_str()?.to_string();
        let email = v.get("email").and_then(|x| x.as_str()).unwrap_or(&username).to_string();
        let smtp_host = v.get("smtp_host").and_then(|x| x.as_str()).unwrap_or(&host).to_string();
        Some(AccountConfig {
            host,
            port: v.get("port").and_then(|x| x.as_u64()).unwrap_or(993) as u16,
            username,
            password,
            use_tls: v.get("use_tls").and_then(|x| x.as_bool()).unwrap_or(true),
            email,
            smtp_host,
            smtp_port: v.get("smtp_port").and_then(|x| x.as_u64()).unwrap_or(465) as u16,
            native_url: v.get("native_url").and_then(|x| x.as_str()).map(String::from),
            native_token: v.get("native_token").and_then(|x| x.as_str()).map(String::from),
        })
    }

    /// Env first (handy for dev), then the on-disk config file.
    pub fn load() -> Option<Self> {
        Self::from_env().or_else(Self::from_file)
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
    StartWatching,
    FetchAvatar { email: String },
    SetFlags { messages: Vec<MessageRef>, flags: String, add: bool },
    Delete { messages: Vec<MessageRef> },
    DownloadAttachment { folder: String, uid: u32, index: usize, filename: String },
    /// Live dropdown lookup for the search-as-compose bar: matching contacts
    /// (name/email) AND matching messages (subject/body) in one round-trip.
    /// The query is echoed back in the result so the UI can drop stale answers
    /// when typing faster than the engine answers.
    SearchDropdown { query: String, limit: u32 },
    /// Calendar list — populates the calendar-view sidebar. Cheap when
    /// the native provider is in use; ImapProvider returns an empty
    /// list (no calendar support there).
    FetchCalendars,
    /// Events overlapping `[from_ms, to_ms)`. `calendar_ids` empty =
    /// fetch from all the user's calendars (the engine-default).
    FetchCalendarEvents {
        from_ms: i64,
        to_ms: i64,
        calendar_ids: Vec<i64>,
    },
    /// Set the requesting user's PARTSTAT on an event (ACCEPTED/TENTATIVE/DECLINED).
    Rsvp { event_id: i64, partstat: String },
    /// Create a calendar event from a server-shaped JSON body.
    CreateEvent { body: serde_json::Value },
    /// Patch an existing event (body carries the changed fields + scope).
    PatchEvent { event_id: i64, body: serde_json::Value },
    Send {
        to: Vec<String>,
        cc: Vec<String>,
        subject: String,
        body: String,
        in_reply_to: Option<String>,
        references: Option<String>,
    },
}

/// Results the engine sends back to the UI.
pub enum EngineResult {
    Conversations(Vec<Conversation>),
    Messages(Vec<MessageBody>),
    /// Live-dropdown answer. `query` lets the UI drop responses for an old
    /// query string when the user has typed past it.
    SearchDropdown {
        query: String,
        contacts: Vec<Contact>,
        messages: Vec<MessageEnvelope>,
    },
    /// A push event from the provider's background watcher.
    Event(EngineEvent),
    /// A message was sent (server response / message-id).
    Sent(String),
    /// A mutating op (flags/delete) finished; UI should refresh.
    Done(String),
    /// A decoded avatar (RGBA) for an email address.
    Avatar { email: String, rgba: Vec<u8>, w: u32, h: u32 },
    /// An attachment was downloaded and saved at this path; UI opens it.
    AttachmentSaved(String),
    /// Calendar list (sidebar) — echoed back from FetchCalendars.
    Calendars(Vec<DesktopCalendar>),
    /// Events for the currently displayed week — echoed back from
    /// FetchCalendarEvents.
    CalendarEvents(Vec<DesktopCalendarEvent>),
    Error(String),
}

/// Save attachment bytes into the user's Downloads dir (sanitized filename).
fn save_download(filename: &str, bytes: &[u8]) -> Result<String, String> {
    let home = std::env::var("USERPROFILE")
        .or_else(|_| std::env::var("HOME"))
        .map_err(|_| "no home dir".to_string())?;
    let dir = std::path::Path::new(&home).join("Downloads");
    std::fs::create_dir_all(&dir).map_err(|e| format!("mkdir: {e}"))?;
    let safe: String = filename
        .chars()
        .map(|c| if "\\/:*?\"<>|".contains(c) { '_' } else { c })
        .collect();
    let safe = if safe.trim().is_empty() { "attachment".to_string() } else { safe };
    let path = dir.join(&safe);
    std::fs::write(&path, bytes).map_err(|e| format!("write: {e}"))?;
    Ok(path.to_string_lossy().into_owned())
}

/// Spawn the engine thread. Returns a command sender; results arrive via `on_result`
/// (called from background threads — forward to the UI loop yourself).
pub fn spawn(
    cfg: AccountConfig,
    cache: Arc<Cache>,
    on_result: impl Fn(EngineResult) + Send + Sync + 'static,
) -> mpsc::Sender<EngineCmd> {
    let (tx, rx) = mpsc::channel::<EngineCmd>();
    let on_result = Arc::new(on_result);
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
                EngineCmd::FetchAvatar { email } => {
                    // Cache first, else fetch from the provider and cache it.
                    let bytes = match cache.get_avatar(&email) {
                        Some((b, _)) if !b.is_empty() => b,
                        _ => match rt.block_on(provider.fetch_avatar(&email)) {
                            Ok((b, m)) if !b.is_empty() => {
                                cache.save_avatar(&email, &b, &m).ok();
                                b
                            }
                            _ => Vec::new(),
                        },
                    };
                    if !bytes.is_empty() {
                        if let Ok(img) = image::load_from_memory(&bytes) {
                            let rgba = img.to_rgba8();
                            let (w, h) = (rgba.width(), rgba.height());
                            on_result(EngineResult::Avatar {
                                email,
                                rgba: rgba.into_raw(),
                                w,
                                h,
                            });
                        }
                    }
                }
                EngineCmd::SetFlags { messages, flags, add } => {
                    match rt.block_on(provider.set_flags_batch(&messages, &flags, add)) {
                        Ok(()) => on_result(EngineResult::Done("flags".into())),
                        Err(e) => on_result(EngineResult::Error(e)),
                    }
                }
                EngineCmd::Delete { messages } => {
                    match rt.block_on(provider.delete_messages(&messages)) {
                        Ok(()) => on_result(EngineResult::Done("delete".into())),
                        Err(e) => on_result(EngineResult::Error(e)),
                    }
                }
                EngineCmd::DownloadAttachment { folder, uid, index, filename } => {
                    match rt.block_on(provider.fetch_attachment(&folder, uid, index)) {
                        Ok((bytes, _mime)) => match save_download(&filename, &bytes) {
                            Ok(path) => on_result(EngineResult::AttachmentSaved(path)),
                            Err(e) => on_result(EngineResult::Error(e)),
                        },
                        Err(e) => on_result(EngineResult::Error(e)),
                    }
                }
                EngineCmd::SearchDropdown { query, limit } => {
                    // Contacts come from the local cache (instant); messages
                    // hit the provider. Failures on either side degrade
                    // gracefully so a missing contact index doesn't blank
                    // the dropdown.
                    let contacts = cache
                        .search_contacts(&key, &query, limit)
                        .unwrap_or_default();
                    let messages = rt
                        .block_on(provider.search_messages(&cfg.email, &query))
                        .unwrap_or_default();
                    on_result(EngineResult::SearchDropdown {
                        query,
                        contacts,
                        messages,
                    });
                }
                EngineCmd::FetchCalendars => {
                    match rt.block_on(provider.list_calendars()) {
                        Ok(cals) => on_result(EngineResult::Calendars(cals)),
                        Err(e) => on_result(EngineResult::Error(format!("list_calendars: {e}"))),
                    }
                }
                EngineCmd::FetchCalendarEvents { from_ms, to_ms, calendar_ids } => {
                    match rt.block_on(provider.fetch_calendar_events(from_ms, to_ms, &calendar_ids)) {
                        Ok(events) => on_result(EngineResult::CalendarEvents(events)),
                        Err(e) => on_result(EngineResult::Error(format!("fetch_calendar_events: {e}"))),
                    }
                }
                EngineCmd::Rsvp { event_id, partstat } => {
                    match rt.block_on(provider.rsvp_event(event_id, &partstat)) {
                        Ok(_) => on_result(EngineResult::Done("rsvp".into())),
                        Err(e) => on_result(EngineResult::Error(format!("rsvp: {e}"))),
                    }
                }
                EngineCmd::CreateEvent { body } => {
                    match rt.block_on(provider.create_event(body)) {
                        Ok(_) => on_result(EngineResult::Done("rsvp".into())),
                        Err(e) => on_result(EngineResult::Error(format!("create_event: {e}"))),
                    }
                }
                EngineCmd::PatchEvent { event_id, body } => {
                    match rt.block_on(provider.patch_event(event_id, body)) {
                        Ok(_) => on_result(EngineResult::Done("rsvp".into())),
                        Err(e) => on_result(EngineResult::Error(format!("patch_event: {e}"))),
                    }
                }
                EngineCmd::Send { to, cc, subject, body, in_reply_to, references } => {
                    let msg = OutgoingMessage {
                        from: cfg.email.clone(),
                        to,
                        cc,
                        subject,
                        html: String::new(),
                        text: body,
                        in_reply_to,
                        references,
                        attachment_paths: Vec::new(),
                        inline_paths: Vec::new(),
                        attachments: Vec::new(),
                    };
                    let r = rt.block_on(provider.send_message(
                        &cfg.smtp_host,
                        cfg.smtp_port,
                        &msg,
                    ));
                    match r {
                        Ok(id) => on_result(EngineResult::Sent(id)),
                        Err(e) => on_result(EngineResult::Error(e)),
                    }
                }
                EngineCmd::StartWatching => {
                    // Bridge provider events into EngineResult::Event. The
                    // multi-threaded runtime keeps the spawned watcher alive
                    // while this thread blocks on the next command.
                    let sink = on_result.clone();
                    let notifier: Notifier =
                        Arc::new(move |ev| sink(EngineResult::Event(ev)));
                    if let Err(e) = rt.block_on(provider.start_watching(notifier)) {
                        on_result(EngineResult::Error(format!("watch: {e}")));
                    }
                }
            }
        }
    });
    tx
}
