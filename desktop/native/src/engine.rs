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
use ddmail_core::event::{EngineEvent, Notifier};
use ddmail_core::imap;
use ddmail_core::imap_provider::ImapProvider;
use ddmail_core::native_provider::NativeProvider;
use ddmail_core::provider::MailProvider;
use ddmail_core::session::SessionPool;
use ddmail_core::types::{
    Contact, Conversation, DesktopCalendar, DesktopCalendarEvent, DesktopContact, DesktopTask,
    MessageBody, MessageEnvelope, MessageRef, OutgoingAttachment, OutgoingMessage,
    CHANGE_KIND_DELETE,
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
    /// Optional CardDAV addressbook-collection URL (plain-server accounts).
    pub carddav_url: Option<String>,
    /// Optional CalDAV calendar-collection URL (plain-server accounts).
    pub caldav_url: Option<String>,
    /// Google OAuth refresh token (standalone Gmail accounts). Present ⇒ DAV
    /// uses Bearer and IMAP uses XOAUTH2; the access token is minted lazily.
    pub oauth_refresh_token: Option<String>,
}


/// Заменить парные маркеры на тег. Непарный хвост остаётся текстом.
fn apply_marker(text: &str, marker: &str, tag: &str) -> String {
    let mut out = String::with_capacity(text.len());
    let mut rest = text;
    while let Some(open) = rest.find(marker) {
        let after = &rest[open + marker.len()..];
        // Пустая пара (`****`) — не разметка, а просто звёздочки.
        match after.find(marker).filter(|end| *end > 0) {
            Some(end) => {
                out.push_str(&rest[..open]);
                out.push_str(&format!("<{tag}>{}</{tag}>", &after[..end]));
                rest = &after[end + marker.len()..];
            }
            None => break,
        }
    }
    out.push_str(rest);
    out
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
            carddav_url: std::env::var("DDMAIL_CARDDAV_URL").ok(),
            caldav_url: std::env::var("DDMAIL_CALDAV_URL").ok(),
            oauth_refresh_token: std::env::var("DDMAIL_OAUTH_REFRESH").ok(),
        })
    }

    /// Parse one account object (shared by account.json and accounts.json[]).
    /// `password` is optional for native-mode accounts (the login screen
    /// stores a token instead of the IMAP password).
    fn from_json(v: &serde_json::Value) -> Option<Self> {
        let host = v.get("host")?.as_str()?.to_string();
        let username = v.get("username")?.as_str()?.to_string();
        let native = v.get("native_url").is_some() && v.get("native_token").is_some();
        let password = match v.get("password").and_then(|x| x.as_str()) {
            Some(p) => p.to_string(),
            None if native => String::new(),
            None => return None,
        };
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
            carddav_url: v.get("carddav_url").and_then(|x| x.as_str()).map(String::from),
            caldav_url: v.get("caldav_url").and_then(|x| x.as_str()).map(String::from),
            oauth_refresh_token: v.get("oauth_refresh_token").and_then(|x| x.as_str()).map(String::from),
        })
    }

    /// `%APPDATA%/ru.letotam.ddmail` or `$HOME/ru.letotam.ddmail`.
    fn config_dir() -> Option<std::path::PathBuf> {
        let base = std::env::var("APPDATA").or_else(|_| std::env::var("HOME")).ok()?;
        Some(std::path::Path::new(&base).join("ru.letotam.ddmail"))
    }

    /// Load from the single-account `account.json` (legacy / fallback).
    pub fn from_file() -> Option<Self> {
        let path = Self::config_dir()?.join("account.json");
        let data = std::fs::read_to_string(&path).ok()?;
        let v: serde_json::Value = serde_json::from_str(&data).ok()?;
        Self::from_json(&v)
    }

    /// Env first (handy for dev), then the on-disk config file.
    pub fn load() -> Option<Self> {
        Self::from_env().or_else(Self::from_file)
    }

    /// All configured accounts. Source of truth is `accounts.json` (an array);
    /// falls back to an env override (dev, not persisted) or a single
    /// `account.json` which is then migrated into `accounts.json`.
    pub fn load_all() -> Vec<AccountConfig> {
        if let Some(dir) = Self::config_dir() {
            if let Ok(data) = std::fs::read_to_string(dir.join("accounts.json")) {
                if let Ok(serde_json::Value::Array(arr)) =
                    serde_json::from_str::<serde_json::Value>(&data)
                {
                    let v: Vec<AccountConfig> = arr.iter().filter_map(Self::from_json).collect();
                    if !v.is_empty() {
                        return v;
                    }
                }
            }
        }
        // Dev env override — used as-is, never written to disk.
        if let Some(cfg) = Self::from_env() {
            return vec![cfg];
        }
        // Legacy single account — migrate it into accounts.json going forward.
        if let Some(cfg) = Self::from_file() {
            cfg.migrate_to_accounts_json();
            return vec![cfg];
        }
        Vec::new()
    }

    fn to_json(&self) -> serde_json::Value {
        serde_json::json!({
            "host": self.host, "port": self.port, "use_tls": self.use_tls,
            "username": self.username, "password": self.password, "email": self.email,
            "smtp_host": self.smtp_host, "smtp_port": self.smtp_port,
            "native_url": self.native_url, "native_token": self.native_token,
            "carddav_url": self.carddav_url, "caldav_url": self.caldav_url,
            "oauth_refresh_token": self.oauth_refresh_token,
        })
    }

    /// Overwrite `accounts.json` with the given set. Best-effort.
    pub fn save_all(accounts: &[AccountConfig]) {
        let Some(dir) = Self::config_dir() else { return };
        let arr =
            serde_json::Value::Array(accounts.iter().map(|a| a.to_json()).collect());
        if let Ok(s) = serde_json::to_string_pretty(&arr) {
            let _ = std::fs::create_dir_all(&dir);
            let _ = std::fs::write(dir.join("accounts.json"), s);
        }
    }

    /// Add (or replace, matched by account_key) a connection in accounts.json.
    /// Used by the connections settings — never clobbers other accounts.
    pub fn add_account(cfg: &AccountConfig) {
        let key = cfg.account_key();
        let mut all: Vec<AccountConfig> =
            Self::load_all().into_iter().filter(|a| a.account_key() != key).collect();
        all.push(cfg.clone());
        Self::save_all(&all);
    }

    /// Remove a connection by account_key from accounts.json.
    pub fn remove_account(account_key: &str) {
        let all: Vec<AccountConfig> =
            Self::load_all().into_iter().filter(|a| a.account_key() != account_key).collect();
        Self::save_all(&all);
    }

    /// Persist a rotated JWT (native provider's auto-refresh) back into
    /// `accounts.json`, so the next launch starts from the fresh token
    /// instead of the stale one. `account_id` is the account email — the id
    /// the provider was built with (see build_provider).
    pub fn persist_native_token(account_id: &str, token: &str) {
        let mut accounts = Self::load_all();
        let mut changed = false;
        for a in accounts.iter_mut() {
            if a.email == account_id && a.native_token.is_some() {
                a.native_token = Some(token.to_string());
                changed = true;
            }
        }
        if changed {
            Self::save_all(&accounts);
        }
    }

    /// Write a one-element `accounts.json` from a legacy single account, unless
    /// one already exists. Best-effort.
    fn migrate_to_accounts_json(&self) {
        let Some(dir) = Self::config_dir() else { return };
        if dir.join("accounts.json").exists() {
            return;
        }
        Self::save_all(std::slice::from_ref(self));
    }

    pub fn account_key(&self) -> String {
        imap::account_key(&self.host, &self.username)
    }
}

/// Replace `cid:` inline-image references with `data:` URLs by pulling the
/// MIME parts from the provider. Runs once per body — the substituted HTML
/// is saved back to the cache, so the cost never repeats. Returns how many
/// bodies were rewritten. Provider errors leave the ref as-is (IMAP mode
/// doesn't implement inline parts yet).
fn resolve_inline_parts(
    rt: &tokio::runtime::Runtime,
    provider: &dyn MailProvider,
    bodies: &mut [MessageBody],
) -> usize {
    use std::sync::OnceLock;
    static CID_RE: OnceLock<regex::Regex> = OnceLock::new();
    let re = CID_RE.get_or_init(|| {
        regex::Regex::new(r#"(?i)cid:([^"'\s>)]+)"#).unwrap()
    });

    let mut rewritten = 0usize;
    for b in bodies.iter_mut() {
        let Some(html) = b.html.clone() else { continue };
        if !html.contains("cid:") {
            continue;
        }
        let cids: std::collections::HashSet<String> = re
            .captures_iter(&html)
            .map(|c| c[1].to_string())
            .collect();
        let mut replaced = html.clone();
        let mut any = false;
        for cid in cids {
            match rt.block_on(provider.fetch_inline_part(b.uid, &cid)) {
                Ok(part) => {
                    let data_url =
                        format!("data:{};base64,{}", part.mime_type, part.content_b64);
                    replaced = replaced.replace(&format!("cid:{cid}"), &data_url);
                    any = true;
                }
                Err(e) => eprintln!("inline part cid:{cid} (uid {}): {e}", b.uid),
            }
        }
        if any {
            b.html = Some(replaced);
            rewritten += 1;
        }
    }
    rewritten
}

/// A body with literally nothing to draw: no HTML, no text, no attachment
/// chips. Cached rows like this are the one case where the "bodies are
/// immutable, never refetch" rule bites: the bubble stays blank forever, and
/// a letter whose whole content WAS the attachment list (gov/EDMS mail: empty
/// text/plain part + PDFs) reads as "не отображается вообще" while the web UI
/// shows it fine. Server-side fixes to the attachment list or body extraction
/// can't reach such a row either (see docs/desktop-behavior-contract.md §4а),
/// so the fetch path treats it as missing and re-asks the server.
pub(crate) fn body_is_blank(b: &MessageBody) -> bool {
    b.attachments.is_empty()
        && b.html.as_deref().map(|s| s.trim().is_empty()).unwrap_or(true)
        && b.text.as_deref().map(|s| s.trim().is_empty()).unwrap_or(true)
}

fn build_provider(cfg: &AccountConfig) -> Arc<dyn MailProvider> {
    if let (Some(url), Some(token)) = (&cfg.native_url, &cfg.native_token) {
        Arc::new(NativeProvider::new(
            url.clone(),
            token.clone(),
            cfg.email.clone(),
            // Настоящий notifier провайдер получает в start_watching и ставит
            // себе сам. Заглушка здесь означала бы, что ротированный токен
            // некуда сообщить, и persist_native_token не вызовется никогда.
            None,
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
            carddav_url: cfg.carddav_url.clone(),
            caldav_url: cfg.caldav_url.clone(),
            caldav_event_uids: Arc::new(std::sync::Mutex::new(std::collections::HashMap::new())),
            caldav_collection: Arc::new(std::sync::Mutex::new(None)),
            carddav_collection: Arc::new(std::sync::Mutex::new(None)),
            carddav_contact_uids: Arc::new(std::sync::Mutex::new(std::collections::HashMap::new())),
            oauth_refresh_token: cfg.oauth_refresh_token.clone(),
            oauth_client: if cfg.oauth_refresh_token.is_some() {
                ddmail_core::oauth::load_client_creds()
            } else {
                None
            },
            oauth_access: Arc::new(std::sync::Mutex::new(None)),
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
    /// `generation` is the UI's conversation-open generation; it is echoed
    /// back in `EngineResult::Messages` so the UI can drop answers that
    /// arrive after the user already switched to another conversation.
    FetchMessages { messages: Vec<MessageRef>, generation: u64, account_key: String },
    StartWatching,
    FetchAvatar { email: String },
    SetFlags { messages: Vec<MessageRef>, flags: String, add: bool, account_key: String },
    Delete { messages: Vec<MessageRef>, account_key: String },
    /// Raw RFC-822 source of one message — feeds the «Показать →
    /// Заголовки / Исходник сообщения» views.
    /// `headers_only` fetches just the header block — the viewer that wants
    /// thirty lines should not pull every attachment down first.
    FetchSource { folder: String, uid: u32, account_key: String, headers_only: bool },
    /// «Спам»: blacklist the sender and purge their messages, including the
    /// given conversation rows. The real sender is resolved server-side from
    /// `message_ids`; `scope` = "address" | "domain"; `fallback_addr` is only
    /// used when the ids resolve no sender (IMAP).
    BlacklistAndPurge {
        scope: String,
        fallback_addr: String,
        message_ids: Vec<i64>,
        account_key: String,
    },
    /// `save_to = None` → в Downloads + автооткрытие (клик по чипу);
    /// `save_to = Some(path)` → в выбранный пользователем файл, без открытия.
    DownloadAttachment { folder: String, uid: u32, index: usize, filename: String, account_key: String, save_to: Option<String> },
    /// Live dropdown lookup for the search-as-compose bar: matching contacts
    /// (name/email) AND matching messages (subject/body) in one round-trip.
    /// The query is echoed back in the result so the UI can drop stale answers
    /// when typing faster than the engine answers.
    SearchDropdown { query: String, limit: u32 },
    /// Calendar list — populates the calendar-view sidebar. Cheap when
    /// the native provider is in use; ImapProvider returns an empty
    /// list (no calendar support there).
    FetchCalendars,
    /// Re-read the identity list from the server and overwrite the cache.
    ///
    /// The ordinary refresh happens inside FetchConversations, and only on a
    /// full sync — identities change rarely enough that asking on every delta
    /// would be waste. A profile import changes them out of band, which is the
    /// one case that needs to say so explicitly.
    RefreshIdentities,
    /// Events overlapping `[from_ms, to_ms)`. `calendar_ids` empty =
    /// fetch from all the user's calendars (the engine-default).
    ///
    /// `for_reminders` — фетч ради посева напоминаний, а не ради сетки: окно
    /// считается от `now`, а не от отображаемой недели, и результат в сетку не
    /// попадает (иначе он бы её перетёр чужим окном).
    FetchCalendarEvents {
        from_ms: i64,
        to_ms: i64,
        calendar_ids: Vec<i64>,
        for_reminders: bool,
    },
    /// The unified address book. Empty `query` = the full book; a non-empty
    /// query is autocomplete/search. `query` is echoed in the result so the UI
    /// can drop stale answers.
    FetchContacts { query: String, limit: u32 },
    /// Tasks (VTODO) across every account. Not windowed by time the way
    /// calendar events are: most tasks have no date at all, so a window would
    /// hide the bulk of a reminders list.
    FetchTasks { include_completed: bool },
    /// Tick a task off (or back on). The UI has already moved; this is the
    /// write that makes it stick.
    SetTaskCompletion { task_id: i64, completed: bool, account_key: String },
    /// Address-book writes. Bodies are server-shaped JSON
    /// (full_name/emails/phones/organization/title [+ from_identity on create]).
    CreateContact { body: serde_json::Value, account_key: String },
    UpdateContact { id: i64, body: serde_json::Value, account_key: String },
    DeleteContact { id: i64, account_key: String },
    /// Set the requesting user's PARTSTAT on an event (ACCEPTED/TENTATIVE/DECLINED).
    Rsvp { event_id: i64, partstat: String, account_key: String },
    /// Create a calendar event from a server-shaped JSON body.
    CreateEvent { body: serde_json::Value, account_key: String },
    /// Patch an existing event (body carries the changed fields + scope).
    PatchEvent { event_id: i64, body: serde_json::Value, account_key: String },
    /// Delete an event.
    DeleteEvent { event_id: i64, account_key: String },
    Send {
        to: Vec<String>,
        cc: Vec<String>,
        subject: String,
        body: String,
        /// HTML-версия тела из rich-text композера (пустая — уходит только
        /// text/plain-часть). Ссылается на inline-картинки через `cid:`.
        html: String,
        /// Inline-картинки тела: уже готовые вложения с `content_id`,
        /// на которые ссылается `html`. Отличаются от `attachments` (те —
        /// пути к файлам, которые читает движок).
        inline: Vec<OutgoingAttachment>,
        in_reply_to: Option<String>,
        references: Option<String>,
        /// Sending identity ("From"). None → the account's own email.
        /// The server matches it against the user's accounts/identities.
        from: Option<String>,
        /// Filesystem paths of files the user attached in the composer.
        /// Resolved to bytes (and a guessed MIME type) just before sending.
        attachments: Vec<String>,
        /// Forward mode: re-attach every attachment of this original
        /// message (downloaded from the provider) to the outgoing one.
        forward_attachments: Option<MessageRef>,
        /// Which account sends this message (reply → the conversation's
        /// account; new compose → the chosen "От кого").
        account_key: String,
    },
}

/// Results the engine sends back to the UI.
pub enum EngineResult {
    /// `partial == true`: only changed conversations (delta sync) — merge
    /// into the existing list instead of replacing it.
    Conversations { list: Vec<Conversation>, partial: bool },
    /// `generation` echoes FetchMessages' (stale-answer guard).
    Messages { bodies: Vec<MessageBody>, generation: u64 },
    /// Live-dropdown answer. `query` lets the UI drop responses for an old
    /// query string when the user has typed past it.
    SearchDropdown {
        query: String,
        contacts: Vec<Contact>,
        messages: Vec<MessageEnvelope>,
    },
    /// A push event from the provider's background watcher.
    Event(EngineEvent),
    /// Per-account connection state ("connecting" | "connected" | "error" |
    /// "auth" — токен мёртв, нужен повторный вход, см. `EngineEvent`),
    /// tagged with the account so the UI can drive the aggregate indicator.
    AccountState { account_key: String, state: String },
    /// A message was sent (server response / message-id).
    Sent(String),
    /// A mutating op (flags/delete) finished; UI should refresh.
    Done(String),
    /// A decoded avatar (RGBA) for an email address.
    Avatar { email: String, rgba: Vec<u8>, w: u32, h: u32 },
    /// An attachment was downloaded and saved at this path; UI opens it.
    AttachmentSaved(String),
    /// Вложение записано в явно выбранный пользователем путь («Сохранить
    /// как…») — подтверждаем инлайн-плашкой, файл НЕ открываем.
    AttachmentSavedTo(String),
    /// Raw RFC-822 source for a FetchSource request.
    Source { uid: u32, raw: String },
    /// Calendar list (sidebar) — echoed back from FetchCalendars. `complete`
    /// says every account answered: тот же принцип, что у CalendarEvents —
    /// отсутствие календарей упавшего аккаунта не означает, что их удалили.
    Calendars { list: Vec<DesktopCalendar>, complete: bool },
    /// Address-book rows — echoed back from FetchContacts. `query` lets the UI
    /// drop stale answers when typing faster than the engine answers.
    Contacts { query: String, list: Vec<DesktopContact> },
    /// Task rows — echoed back from FetchTasks.
    Tasks(Vec<DesktopTask>),
    /// Events for the currently displayed week — echoed back from
    /// FetchCalendarEvents. `from_ms`/`to_ms` echo the requested window and
    /// `complete` says every account answered — orphaned-reminder pruning
    /// must only trust a complete fetch (a failed account's events being
    /// absent is not a deletion). `for_reminders` echoes the request flag:
    /// такой результат только сеет напоминания, сетку не трогает.
    CalendarEvents {
        events: Vec<DesktopCalendarEvent>,
        from_ms: i64,
        to_ms: i64,
        complete: bool,
        for_reminders: bool,
    },
    /// A Send command failed — surfaced to the user (toast), unlike the
    /// generic Error which only logs. Carries the human-readable reason.
    SendFailed(String),
    /// DownloadAttachment failed (fetch or write) — surfaced to the user
    /// (toast), unlike the generic Error which only logs.
    AttachmentFailed(String),
    /// Spam blacklist-and-purge succeeded: what was blocked + rows removed.
    /// The client confirms with a «✓ …» plashka.
    SpamPurged { rule_type: String, rule_value: String, deleted: i64 },
    /// Spam blacklist-and-purge failed — surfaced to the user (toast).
    SpamFailed(String),
    Error(String),
}

/// Save attachment bytes into the user's Downloads dir (sanitized filename).
fn save_download(filename: &str, bytes: &[u8]) -> Result<String, String> {
    let home = std::env::var("USERPROFILE")
        .or_else(|_| std::env::var("HOME"))
        .map_err(|_| "no home dir".to_string())?;
    let dir = std::path::Path::new(&home).join("Downloads");
    std::fs::create_dir_all(&dir).map_err(|e| format!("mkdir: {e}"))?;
    // Долбанутые имена: помимо запрещённых на Windows символов, глушим
    // управляющие (переводы строк из криво разобранных MIME-заголовков) и
    // хвостовые точки/пробелы (Windows их молча отрезает — путь разъезжается).
    let safe: String = filename
        .chars()
        .map(|c| {
            if "\\/:*?\"<>|".contains(c) || c.is_control() {
                '_'
            } else {
                c
            }
        })
        .collect();
    let safe = safe.trim().trim_end_matches(['.', ' ']).to_string();
    let safe = if safe.is_empty() { "attachment".to_string() } else { safe };
    let path = dir.join(&safe);
    std::fs::write(&path, bytes).map_err(|e| format!("write: {e}"))?;
    Ok(path.to_string_lossy().into_owned())
}

/// Decode avatar bytes into straight-alpha RGBA. Raster formats go through
/// the `image` crate (incl. ico/bmp — the server's avatar chain ends at
/// /favicon.ico); SVG (BIMI brand logos) is rasterized via resvg at 96px.
/// The old Tauri build delegated all of this to Chromium's <img>, so the
/// native decoder has to match that breadth or avatars degrade to initials.
fn decode_avatar(bytes: &[u8], mime: &str) -> Option<(Vec<u8>, u32, u32)> {
    let looks_svg = mime.contains("svg")
        || bytes
            .get(..512)
            .map(|head| {
                let head = String::from_utf8_lossy(head);
                head.contains("<svg")
            })
            .unwrap_or(false);
    if looks_svg {
        return rasterize_svg(bytes, 96.0);
    }
    let img = image::load_from_memory(bytes).ok()?;
    let rgba = img.to_rgba8();
    let (w, h) = (rgba.width(), rgba.height());
    Some((rgba.into_raw(), w, h))
}

/// Render an SVG to RGBA at most `target` px on its longer side.
fn rasterize_svg(bytes: &[u8], target: f32) -> Option<(Vec<u8>, u32, u32)> {
    let opt = resvg::usvg::Options::default();
    let tree = resvg::usvg::Tree::from_data(bytes, &opt).ok()?;
    let size = tree.size();
    if size.width() <= 0.0 || size.height() <= 0.0 {
        return None;
    }
    let scale = target / size.width().max(size.height());
    let w = (size.width() * scale).ceil().max(1.0) as u32;
    let h = (size.height() * scale).ceil().max(1.0) as u32;
    let mut pixmap = resvg::tiny_skia::Pixmap::new(w, h)?;
    resvg::render(
        &tree,
        resvg::tiny_skia::Transform::from_scale(scale, scale),
        &mut pixmap.as_mut(),
    );
    // tiny-skia stores premultiplied RGBA; Slint expects straight alpha.
    let mut rgba = Vec::with_capacity((w * h * 4) as usize);
    for p in pixmap.pixels() {
        let c = p.demultiply();
        rgba.extend_from_slice(&[c.red(), c.green(), c.blue(), c.alpha()]);
    }
    Some((rgba, w, h))
}

/// Read each staged path into an `OutgoingAttachment`, guessing the MIME
/// type from the extension. Unreadable paths are logged and skipped rather
/// than failing the whole send.
fn resolve_attachments(paths: &[String]) -> Vec<OutgoingAttachment> {
    let mut out = Vec::with_capacity(paths.len());
    for p in paths {
        let path = std::path::Path::new(p);
        match std::fs::read(path) {
            Ok(bytes) => {
                let filename = path
                    .file_name()
                    .map(|n| n.to_string_lossy().into_owned())
                    .unwrap_or_else(|| "attachment".to_string());
                out.push(OutgoingAttachment {
                    filename,
                    mime_type: guess_mime(path),
                    content: bytes,
                    content_id: None,
                });
            }
            Err(e) => eprintln!("attachment: failed to read {p}: {e}"),
        }
    }
    out
}

/// Best-effort MIME type from a file extension. Falls back to the generic
/// binary type — the receiving MUA can still save the file by name.
fn guess_mime(path: &std::path::Path) -> String {
    let ext = path
        .extension()
        .and_then(|e| e.to_str())
        .unwrap_or("")
        .to_ascii_lowercase();
    match ext.as_str() {
        "png" => "image/png",
        "jpg" | "jpeg" => "image/jpeg",
        "gif" => "image/gif",
        "webp" => "image/webp",
        "svg" => "image/svg+xml",
        "pdf" => "application/pdf",
        "txt" | "log" | "md" => "text/plain",
        "html" | "htm" => "text/html",
        "csv" => "text/csv",
        "json" => "application/json",
        "xml" => "application/xml",
        "zip" => "application/zip",
        "gz" | "tgz" => "application/gzip",
        "doc" => "application/msword",
        "docx" => "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
        "xls" => "application/vnd.ms-excel",
        "xlsx" => "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
        "ppt" => "application/vnd.ms-powerpoint",
        "pptx" => "application/vnd.openxmlformats-officedocument.presentationml.presentation",
        _ => "application/octet-stream",
    }
    .to_string()
}

/// Spawn the engine thread. Returns a command sender; results arrive via `on_result`
/// (called from background threads — forward to the UI loop yourself).
/// One connected account: its cache key, config and provider. The orchestrator
/// holds a Vec of these — aggregating commands (conversations, calendars,
/// search) fan out across all; addressed commands route to one.
struct AccountConn {
    key: String,
    cfg: AccountConfig,
    provider: Arc<dyn MailProvider>,
}

/// Pick the account an addressed command targets. Falls back to the first
/// account when the key is empty/unknown (single-account, or a command issued
/// without a conversation context). `conns` is guaranteed non-empty.
fn route<'a>(conns: &'a [AccountConn], account_key: &str) -> &'a AccountConn {
    conns
        .iter()
        .find(|c| c.key == account_key)
        .unwrap_or(&conns[0])
}

pub fn spawn(
    accounts: Vec<AccountConfig>,
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
        let conns: Vec<AccountConn> = accounts
            .into_iter()
            .map(|cfg| AccountConn {
                key: cfg.account_key(),
                provider: build_provider(&cfg),
                cfg,
            })
            .collect();
        if conns.is_empty() {
            while rx.recv().is_ok() {}
            return;
        }
        // Addressed and aggregating commands now route per-account; only the
        // provider-agnostic avatar fetch still uses the first provider.
        let provider = conns[0].provider.clone();

        // Avatar fetches are bulk background work (one network round-trip
        // per cache miss, ~150 queued right after the first conversation
        // load). Everything else is interactive — a calendar/contacts click
        // must not queue behind that backlog, so avatars are parked and
        // served only while the channel is empty.
        let mut avatar_backlog: std::collections::VecDeque<String> = Default::default();
        // Последний удачно полученный список календарей на аккаунт. Нужен
        // ровно затем, чтобы разовая ошибка запроса не оставила UI без
        // календарей (см. FetchCalendars).
        let mut last_calendars: std::collections::HashMap<String, Vec<DesktopCalendar>> =
            Default::default();
        loop {
            let mut next: Option<EngineCmd> = None;
            let mut disconnected = false;
            loop {
                match rx.try_recv() {
                    Ok(EngineCmd::FetchAvatar { email }) => avatar_backlog.push_back(email),
                    Ok(c) => {
                        next = Some(c);
                        break;
                    }
                    Err(std::sync::mpsc::TryRecvError::Empty) => break,
                    Err(std::sync::mpsc::TryRecvError::Disconnected) => {
                        disconnected = true;
                        break;
                    }
                }
            }
            let cmd = match next {
                Some(c) => c,
                None => {
                    if let Some(email) = avatar_backlog.pop_front() {
                        EngineCmd::FetchAvatar { email }
                    } else if disconnected {
                        break;
                    } else {
                        // Nothing queued, no backlog — block for the next one.
                        match rx.recv() {
                            Ok(c) => c,
                            Err(_) => break,
                        }
                    }
                }
            };
            match cmd {
                EngineCmd::FetchConversations { limit } => {
                    // Fan out: each account syncs into ITS cache namespace
                    // (delta — native asks only for conversations changed since
                    // its server-clock watermark; a full resync runs on first
                    // start and every 24h; IMAP ignores `since` and comes back
                    // full). Then load the full set from every account's cache,
                    // tag each with its account_key, merge and sort by date —
                    // one unified list. A failed account is logged and skipped,
                    // so one down server doesn't blank the others.
                    let now_s = chrono::Utc::now().timestamp();
                    for conn in &conns {
                        // Change journal: pull the tail since our cursor and
                        // apply DELETE tombstones to the cache BEFORE the delta
                        // below. This is what makes deletions-elsewhere drop off
                        // without a full resync — the conversation delta only
                        // reports changed threads, never removed ones. Providers
                        // without a journal (plain IMAP) return None → skipped.
                        let seq_key = format!("journal_seq:{}", conn.key);
                        let cur_seq: i64 = cache
                            .get_meta(&seq_key)
                            .and_then(|v| v.parse().ok())
                            .unwrap_or(0);
                        match rt.block_on(conn.provider.fetch_changes(cur_seq)) {
                            Ok(Some(ch)) => {
                                let deletes: Vec<String> = ch
                                    .entries
                                    .iter()
                                    .filter(|e| e.kind == CHANGE_KIND_DELETE)
                                    .map(|e| e.message_id.clone())
                                    .collect();
                                if !deletes.is_empty() {
                                    match cache.apply_deletions(&conn.key, &deletes) {
                                        Ok(n) => println!(
                                            "journal [{}]: applied {n} deletions ({} tombstones)",
                                            conn.key,
                                            deletes.len()
                                        ),
                                        Err(e) => eprintln!("journal apply [{}]: {e}", conn.key),
                                    }
                                }
                                if ch.latest_seq > 0 {
                                    cache.set_meta(&seq_key, &ch.latest_seq.to_string()).ok();
                                }
                            }
                            Ok(None) => {} // no journal (IMAP) — rely on IMAP itself
                            Err(e) => eprintln!("journal fetch [{}]: {e}", conn.key),
                        }

                        let our = resolve_our_addrs(&cache, &conn.cfg);
                        let since_key = format!("conv_since:{}", conn.key);
                        let full_key = format!("conv_full_ts:{}", conn.key);
                        let last_full: i64 = cache
                            .get_meta(&full_key)
                            .and_then(|v| v.parse().ok())
                            .unwrap_or(0);
                        let since: i64 = if now_s - last_full > 24 * 3600 {
                            0
                        } else {
                            cache.get_meta(&since_key).and_then(|v| v.parse().ok()).unwrap_or(0)
                        };
                        match rt.block_on(conn.provider.fetch_conversations_delta(&our, limit, since)) {
                            Ok((convs, server_now, partial)) => {
                                if partial && since > 0 {
                                    cache.upsert_conversations(&conn.key, &convs).ok();
                                } else {
                                    cache.save_conversations(&conn.key, &convs).ok();
                                    cache.set_meta(&full_key, &now_s.to_string()).ok();
                                }
                                if server_now > 0 {
                                    cache.set_meta(&since_key, &server_now.to_string()).ok();
                                }
                            }
                            Err(e) => eprintln!("conversations sync [{}]: {e}", conn.key),
                        }

                        // Identities drive the sidebar row tints. Only the IMAP
                        // path used to persist them, so native mode showed grey
                        // rows whenever the cache lacked a prior IMAP-run's
                        // identities table (e.g. after a cache reset). Refresh
                        // on a full sync (identities change rarely) OR whenever
                        // the table is empty — the latter recovers the tints
                        // without waiting for the next 24h full cycle.
                        let need_idents = since == 0
                            || cache
                                .load_identities(&conn.key)
                                .map(|v| v.is_empty())
                                .unwrap_or(true);
                        if need_idents {
                            match rt.block_on(conn.provider.fetch_identities()) {
                                Ok(idents) => {
                                    cache.save_identities(&conn.key, &idents).ok();
                                }
                                Err(e) => eprintln!("identities sync [{}]: {e}", conn.key),
                            }
                        }
                    }
                    // Merge the full set across accounts (always a replace).
                    let mut merged: Vec<Conversation> = Vec::new();
                    for conn in &conns {
                        let mut convs = cache.load_conversations(&conn.key).unwrap_or_default();
                        for c in &mut convs {
                            c.account_key = conn.key.clone();
                        }
                        merged.extend(convs);
                    }
                    merged.sort_by(|a, b| b.last_date_ts.cmp(&a.last_date_ts));
                    on_result(EngineResult::Conversations { list: merged, partial: false });
                }
                EngineCmd::FetchMessages { messages, generation, account_key } => {
                    let conn = route(&conns, &account_key);
                    let provider = conn.provider.clone();
                    let key = conn.key.clone();
                    let cfg = conn.cfg.clone();
                    // Bodies are immutable: a cached (folder, uid) never needs
                    // refetching, so only the refs missing from SQLite go out
                    // on the wire. Fully-cached conversation = no network at all.
                    let mut cached =
                        cache.load_message_bodies(&key, &messages).unwrap_or_default();
                    // Heal pre-substitution cache entries: bodies cached before
                    // cid:-resolution keep broken inline-image refs forever
                    // (they never refetch) — resolve them in place and re-save.
                    let healed = resolve_inline_parts(&rt, provider.as_ref(), &mut cached);
                    if healed > 0 {
                        println!("engine: resolved inline parts in {healed} cached bodies");
                        cache.save_message_bodies(&key, &cached).ok();
                    }
                    // Blank cached rows don't count as "have" — refetching them
                    // is the only way a poisoned cache heals (see
                    // body_is_blank). Costs one request per open for a letter
                    // that really is empty; the alternative is a bubble that
                    // stays blank until someone deletes cache.db by hand.
                    let blank = cached.iter().filter(|b| body_is_blank(b)).count();
                    if blank > 0 {
                        println!("engine: {blank} cached bodies are blank — refetching them");
                    }
                    let have: std::collections::HashSet<(String, u32)> = cached
                        .iter()
                        .filter(|b| !body_is_blank(b))
                        .map(|b| (b.folder.clone(), b.uid))
                        .collect();
                    let missing: Vec<MessageRef> = messages
                        .iter()
                        .filter(|m| !have.contains(&(m.folder.clone(), m.uid)))
                        .cloned()
                        .collect();
                    if missing.is_empty() {
                        if healed > 0 {
                            // Re-emit so the UI re-renders the healed bodies.
                            on_result(EngineResult::Messages { bodies: cached, generation });
                        } else {
                            // The UI already rendered the cached bodies on open.
                            println!(
                                "engine: conversation fully cached ({} bodies) — no fetch",
                                messages.len()
                            );
                        }
                        continue;
                    }
                    println!(
                        "engine: fetching {}/{} missing bodies",
                        missing.len(),
                        messages.len()
                    );
                    let our = resolve_our_addrs(&cache, &cfg);
                    let r = rt.block_on(provider.fetch_conversation_messages(&our, &missing));
                    match r {
                        Ok(mut fetched) => {
                            resolve_inline_parts(&rt, provider.as_ref(), &mut fetched);
                            cache.save_message_bodies(&key, &fetched).ok();
                            // Hand the UI the merged full set (cache now has it all).
                            let bodies =
                                cache.load_message_bodies(&key, &messages).unwrap_or(fetched);
                            on_result(EngineResult::Messages { bodies, generation });
                        }
                        Err(e) => on_result(EngineResult::Error(e)),
                    }
                }
                EngineCmd::FetchAvatar { email } => {
                    // Cache first — a Some(empty) is a valid negative entry
                    // (the cache expires those after a day itself), so it
                    // must NOT trigger a refetch. On a miss, fetch and save
                    // whatever came back, empty included.
                    let (bytes, mime) = match cache.get_avatar(&email) {
                        Some(hit) => hit,
                        None => {
                            let (b, m) = rt
                                .block_on(provider.fetch_avatar(&email))
                                .unwrap_or_default();
                            cache.save_avatar(&email, &b, &m).ok();
                            (b, m)
                        }
                    };
                    if !bytes.is_empty() {
                        if let Some((rgba, w, h)) = decode_avatar(&bytes, &mime) {
                            on_result(EngineResult::Avatar { email, rgba, w, h });
                        } else {
                            eprintln!("avatar: undecodable image for {email} (mime {mime})");
                        }
                    }
                }
                EngineCmd::SetFlags { messages, flags, add, account_key } => {
                    let provider = route(&conns, &account_key).provider.clone();
                    match rt.block_on(provider.set_flags_batch(&messages, &flags, add)) {
                        Ok(()) => on_result(EngineResult::Done("flags".into())),
                        Err(e) => on_result(EngineResult::Error(e)),
                    }
                }
                EngineCmd::Delete { messages, account_key } => {
                    let conn = route(&conns, &account_key);
                    let provider = conn.provider.clone();
                    let key = conn.key.clone();
                    match rt.block_on(provider.delete_messages(&messages)) {
                        Ok(()) => {
                            // The conversations delta can only report CHANGED
                            // conversations — a fully-deleted one just vanishes
                            // from the full list. Reset the full-sync stamp so
                            // the refetch triggered by Done runs full.
                            cache.set_meta(&format!("conv_full_ts:{key}"), "0").ok();
                            on_result(EngineResult::Done("delete".into()));
                        }
                        Err(e) => on_result(EngineResult::Error(e)),
                    }
                }
                EngineCmd::FetchSource { folder, uid, account_key, headers_only } => {
                    let provider = route(&conns, &account_key).provider.clone();
                    let fetched = if headers_only {
                        rt.block_on(provider.fetch_message_headers(&folder, uid))
                    } else {
                        rt.block_on(provider.fetch_message_source(&folder, uid))
                    };
                    match fetched {
                        Ok(raw) => on_result(EngineResult::Source { uid, raw }),
                        Err(e) => on_result(EngineResult::Error(e)),
                    }
                }
                EngineCmd::BlacklistAndPurge { scope, fallback_addr, message_ids, account_key } => {
                    let conn = route(&conns, &account_key);
                    let provider = conn.provider.clone();
                    let key = conn.key.clone();
                    match rt.block_on(provider.blacklist_and_purge(&scope, &fallback_addr, &message_ids)) {
                        Ok(outcome) => {
                            println!(
                                "engine: spam purge blocked {}={} ({} rule(s)), removed {} messages",
                                outcome.rule_type, outcome.rule_value, outcome.rule_count, outcome.deleted
                            );
                            // Conversations vanish — force the next list
                            // refetch to run full (same as Delete).
                            cache.set_meta(&format!("conv_full_ts:{key}"), "0").ok();
                            on_result(EngineResult::SpamPurged {
                                rule_type: outcome.rule_type,
                                rule_value: outcome.rule_value,
                                deleted: outcome.deleted,
                            });
                        }
                        Err(e) => on_result(EngineResult::SpamFailed(e)),
                    }
                }
                EngineCmd::DownloadAttachment { folder, uid, index, filename, account_key, save_to } => {
                    let provider = route(&conns, &account_key).provider.clone();
                    match rt.block_on(provider.fetch_attachment(&folder, uid, index)) {
                        Ok((bytes, _mime)) => match save_to {
                            Some(path) => match std::fs::write(&path, &bytes) {
                                Ok(()) => on_result(EngineResult::AttachmentSavedTo(path)),
                                Err(e) => on_result(EngineResult::AttachmentFailed(format!(
                                    "запись {path}: {e}"
                                ))),
                            },
                            None => match save_download(&filename, &bytes) {
                                Ok(path) => on_result(EngineResult::AttachmentSaved(path)),
                                Err(e) => on_result(EngineResult::AttachmentFailed(e)),
                            },
                        },
                        Err(e) => on_result(EngineResult::AttachmentFailed(e)),
                    }
                }
                EngineCmd::SearchDropdown { query, limit } => {
                    // Contacts come from the local cache (instant); messages
                    // hit the provider. Failures on either side degrade
                    // gracefully so a missing contact index doesn't blank
                    // the dropdown.
                    // Fan out across all accounts and merge (each cache is
                    // namespaced by key; each provider searches its own mail),
                    // capped at the total limit.
                    let mut contacts = Vec::new();
                    let mut messages = Vec::new();
                    for conn in &conns {
                        if contacts.len() < limit as usize {
                            if let Ok(c) = cache.search_contacts(&conn.key, &query, limit) {
                                contacts.extend(c);
                            }
                        }
                        if let Ok(m) = rt.block_on(conn.provider.search_messages(&conn.cfg.email, &query)) {
                            messages.extend(m);
                        }
                    }
                    contacts.truncate(limit as usize);
                    on_result(EngineResult::SearchDropdown {
                        query,
                        contacts,
                        messages,
                    });
                }
                EngineCmd::RefreshIdentities => {
                    // Unconditional, unlike the cached check in
                    // FetchConversations: the whole point of this command is
                    // that the caller already knows the cache is stale.
                    for conn in &conns {
                        match rt.block_on(conn.provider.fetch_identities()) {
                            Ok(idents) => {
                                println!("[idents] refreshed {} for {}", idents.len(), conn.key);
                                cache.save_identities(&conn.key, &idents).ok();
                            }
                            Err(e) => eprintln!("identities refresh [{}]: {e}", conn.key),
                        }
                    }
                }
                EngineCmd::FetchCalendars => {
                    // Fan out across every account and tag each calendar with
                    // its owner — one unified list, no primary account.
                    let t0 = std::time::Instant::now();
                    let mut all = Vec::new();
                    let mut complete = true;
                    for conn in &conns {
                        match rt.block_on(conn.provider.list_calendars()) {
                            Ok(mut cals) => {
                                for c in cals.iter_mut() {
                                    c.account_key = conn.key.clone();
                                }
                                last_calendars.insert(conn.key.clone(), cals.clone());
                                all.extend(cals);
                            }
                            Err(e) => {
                                eprintln!("list_calendars [{}]: {e}", conn.key);
                                complete = false;
                                // Ошибка запроса — это не «календари удалили».
                                // Отдать вместо них пустоту значит сломать
                                // форму создания события: сохранять станет
                                // некуда, причём молча. Держим последний
                                // удачный список этого аккаунта.
                                if let Some(prev) = last_calendars.get(&conn.key) {
                                    all.extend(prev.iter().cloned());
                                }
                            }
                        }
                    }
                    println!(
                        "[cal] FetchCalendars: {} cals in {}ms{}",
                        all.len(),
                        t0.elapsed().as_millis(),
                        if complete { "" } else { " (partial)" }
                    );
                    on_result(EngineResult::Calendars { list: all, complete });
                }
                EngineCmd::FetchContacts { query, limit } => {
                    let mut all = Vec::new();
                    for conn in &conns {
                        let r = if query.trim().is_empty() {
                            rt.block_on(conn.provider.list_contacts(limit))
                        } else {
                            rt.block_on(conn.provider.search_contacts(&query, limit))
                        };
                        match r {
                            Ok(mut list) => {
                                for c in list.iter_mut() {
                                    c.account_key = conn.key.clone();
                                }
                                all.extend(list);
                            }
                            Err(e) => eprintln!("fetch_contacts [{}]: {e}", conn.key),
                        }
                    }
                    on_result(EngineResult::Contacts { query, list: all });
                }
                EngineCmd::FetchTasks { include_completed } => {
                    let mut all = Vec::new();
                    for conn in &conns {
                        match rt.block_on(conn.provider.fetch_tasks(include_completed)) {
                            Ok(mut list) => {
                                for t in list.iter_mut() {
                                    t.account_key = conn.key.clone();
                                }
                                all.extend(list);
                            }
                            Err(e) => eprintln!("fetch_tasks [{}]: {e}", conn.key),
                        }
                    }
                    // Undated tasks sort last: they are the ones with no
                    // urgency to convey, and putting them on top would bury
                    // whatever actually has a deadline.
                    all.sort_by(|a, b| match (a.due, b.due) {
                        (Some(x), Some(y)) => x.cmp(&y),
                        (Some(_), None) => std::cmp::Ordering::Less,
                        (None, Some(_)) => std::cmp::Ordering::Greater,
                        (None, None) => a.summary.cmp(&b.summary),
                    });
                    on_result(EngineResult::Tasks(all));
                }
                EngineCmd::SetTaskCompletion { task_id, completed, account_key } => {
                    let provider = route(&conns, &account_key).provider.clone();
                    if let Err(e) = rt.block_on(provider.set_task_completion(task_id, completed)) {
                        eprintln!("set_task_completion {task_id}: {e}");
                        // Re-read rather than guess: the optimistic tick in the
                        // UI is now wrong, and the server's answer is the only
                        // one worth showing.
                        on_result(EngineResult::Error(format!("Не удалось изменить задачу: {e}")));
                    }
                }
                EngineCmd::CreateContact { body, account_key } => {
                    let provider = route(&conns, &account_key).provider.clone();
                    match rt.block_on(provider.create_contact(body)) {
                        Ok(_) => on_result(EngineResult::Done("contact".into())),
                        Err(e) => on_result(EngineResult::Error(format!("create_contact: {e}"))),
                    }
                }
                EngineCmd::UpdateContact { id, body, account_key } => {
                    let provider = route(&conns, &account_key).provider.clone();
                    match rt.block_on(provider.update_contact(id, body)) {
                        Ok(_) => on_result(EngineResult::Done("contact".into())),
                        Err(e) => on_result(EngineResult::Error(format!("update_contact: {e}"))),
                    }
                }
                EngineCmd::DeleteContact { id, account_key } => {
                    let provider = route(&conns, &account_key).provider.clone();
                    match rt.block_on(provider.delete_contact(id)) {
                        Ok(_) => on_result(EngineResult::Done("contact".into())),
                        Err(e) => on_result(EngineResult::Error(format!("delete_contact: {e}"))),
                    }
                }
                EngineCmd::FetchCalendarEvents { from_ms, to_ms, calendar_ids, for_reminders } => {
                    let t0 = std::time::Instant::now();
                    let mut all = Vec::new();
                    // A calendar-filtered fetch is a subset by construction —
                    // never let the orphan pruning treat it as the full truth.
                    // Ни одного подключения — тоже не истина: пустой ответ
                    // «полного» фетча вычистил бы все напоминания окна, хотя
                    // мы просто ничего не спросили (фетч до коннекта).
                    let mut complete = calendar_ids.is_empty() && !conns.is_empty();
                    for conn in &conns {
                        match rt.block_on(conn.provider.fetch_calendar_events(from_ms, to_ms, &calendar_ids)) {
                            Ok(mut evs) => {
                                for e in evs.iter_mut() {
                                    e.account_key = conn.key.clone();
                                }
                                all.extend(evs);
                            }
                            Err(e) => {
                                complete = false;
                                eprintln!("fetch_calendar_events [{}]: {e}", conn.key);
                            }
                        }
                    }
                    println!(
                        "[cal] FetchCalendarEvents{}: {} events in {}ms{}",
                        if for_reminders { " (посев)" } else { "" },
                        all.len(),
                        t0.elapsed().as_millis(),
                        if complete { "" } else { " (partial)" }
                    );
                    on_result(EngineResult::CalendarEvents {
                        events: all,
                        from_ms,
                        to_ms,
                        complete,
                        for_reminders,
                    });
                }
                EngineCmd::Rsvp { event_id, partstat, account_key } => {
                    let provider = route(&conns, &account_key).provider.clone();
                    match rt.block_on(provider.rsvp_event(event_id, &partstat)) {
                        Ok(_) => on_result(EngineResult::Done("rsvp".into())),
                        Err(e) => on_result(EngineResult::Error(format!("rsvp: {e}"))),
                    }
                }
                EngineCmd::CreateEvent { body, account_key } => {
                    let provider = route(&conns, &account_key).provider.clone();
                    match rt.block_on(provider.create_event(body)) {
                        Ok(_) => on_result(EngineResult::Done("rsvp".into())),
                        Err(e) => on_result(EngineResult::Error(format!("create_event: {e}"))),
                    }
                }
                EngineCmd::PatchEvent { event_id, body, account_key } => {
                    let provider = route(&conns, &account_key).provider.clone();
                    match rt.block_on(provider.patch_event(event_id, body)) {
                        Ok(_) => on_result(EngineResult::Done("rsvp".into())),
                        Err(e) => on_result(EngineResult::Error(format!("patch_event: {e}"))),
                    }
                }
                EngineCmd::DeleteEvent { event_id, account_key } => {
                    let provider = route(&conns, &account_key).provider.clone();
                    match rt.block_on(provider.delete_event(event_id)) {
                        Ok(_) => on_result(EngineResult::Done("rsvp".into())),
                        Err(e) => on_result(EngineResult::Error(format!("delete_event: {e}"))),
                    }
                }
                EngineCmd::Send { to, cc, subject, body, html, inline, in_reply_to, references, from, attachments, forward_attachments, account_key } => {
                    let conn = route(&conns, &account_key);
                    let provider = conn.provider.clone();
                    let key = conn.key.clone();
                    let cfg = conn.cfg.clone();
                    let mut outgoing = resolve_attachments(&attachments);
                    // Inline-картинки тела идут теми же вложениями, но с
                    // content_id — SMTP-слой сам завернёт их в multipart/related.
                    outgoing.extend(inline);
                    // Forward mode: pull the original's attachments from the
                    // provider and re-attach them as-is. Failures of single
                    // parts are logged, not fatal — the text still goes out.
                    if let Some(orig) = forward_attachments {
                        let metas = cache
                            .load_message_bodies(&key, std::slice::from_ref(&orig))
                            .ok()
                            .and_then(|mut v| v.pop())
                            .map(|b| b.attachments)
                            .unwrap_or_default();
                        for meta in metas.iter() {
                            match rt.block_on(provider.fetch_attachment(&orig.folder, orig.uid, meta.index)) {
                                Ok((bytes, mime)) => outgoing.push(OutgoingAttachment {
                                    filename: meta.filename.clone(),
                                    mime_type: if mime.is_empty() {
                                        "application/octet-stream".into()
                                    } else {
                                        mime
                                    },
                                    content: bytes,
                                    content_id: None,
                                }),
                                Err(e) => eprintln!(
                                    "forward: attachment {} ({}) failed: {e}",
                                    meta.index, meta.filename
                                ),
                            }
                        }
                    }
                    let msg = OutgoingMessage {
                        from: from.unwrap_or_else(|| cfg.email.clone()),
                        to,
                        cc,
                        subject,
                        html,
                        text: body,
                        in_reply_to,
                        references,
                        attachment_paths: Vec::new(),
                        inline_paths: Vec::new(),
                        attachments: outgoing,
                    };
                    let r = rt.block_on(provider.send_message(
                        &cfg.smtp_host,
                        cfg.smtp_port,
                        &msg,
                    ));
                    match r {
                        Ok(id) => on_result(EngineResult::Sent(id)),
                        // Distinct from the generic Error: the UI toasts this
                        // so a failed send is never silently swallowed.
                        Err(e) => on_result(EngineResult::SendFailed(e)),
                    }
                }
                EngineCmd::StartWatching => {
                    // Start a watcher per account. start_watching spawns its
                    // worker on the runtime and returns, so the loop stays
                    // responsive. The notifier tags ConnectionState events with
                    // the account so the UI's aggregate indicator knows the source.
                    for conn in &conns {
                        let akey = conn.key.clone();
                        let sink = on_result.clone();
                        let notifier: Notifier = Arc::new(move |ev| match ev {
                            EngineEvent::ConnectionState { state, .. } => {
                                sink(EngineResult::AccountState {
                                    account_key: akey.clone(),
                                    state,
                                })
                            }
                            other => sink(EngineResult::Event(other)),
                        });
                        if let Err(e) = rt.block_on(conn.provider.start_watching(notifier)) {
                            eprintln!("watch [{}]: {e}", conn.key);
                            on_result(EngineResult::AccountState {
                                account_key: conn.key.clone(),
                                state: "error".into(),
                            });
                        } else if conn.cfg.native_url.is_none() {
                            // IMAP doesn't report connection state; assume
                            // connected once the idle watcher started (real
                            // tracking comes with P3 reconnect).
                            on_result(EngineResult::AccountState {
                                account_key: conn.key.clone(),
                                state: "connected".into(),
                            });
                        }
                    }
                }
            }
        }
    });
    tx
}
