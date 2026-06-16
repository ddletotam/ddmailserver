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
    MessageRef, OutgoingAttachment, OutgoingMessage,
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

    /// Parse one account object (shared by account.json and accounts.json[]).
    fn from_json(v: &serde_json::Value) -> Option<Self> {
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

    /// Write a one-element `accounts.json` from a legacy single account, unless
    /// one already exists. Best-effort.
    fn migrate_to_accounts_json(&self) {
        let Some(dir) = Self::config_dir() else { return };
        let path = dir.join("accounts.json");
        if path.exists() {
            return;
        }
        let arr = serde_json::json!([{
            "host": self.host, "port": self.port, "use_tls": self.use_tls,
            "username": self.username, "password": self.password, "email": self.email,
            "smtp_host": self.smtp_host, "smtp_port": self.smtp_port,
            "native_url": self.native_url, "native_token": self.native_token,
        }]);
        if let Ok(s) = serde_json::to_string_pretty(&arr) {
            let _ = std::fs::create_dir_all(&dir);
            let _ = std::fs::write(&path, s);
        }
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
    /// `generation` is the UI's conversation-open generation; it is echoed
    /// back in `EngineResult::Messages` so the UI can drop answers that
    /// arrive after the user already switched to another conversation.
    FetchMessages { messages: Vec<MessageRef>, generation: u64 },
    StartWatching,
    FetchAvatar { email: String },
    SetFlags { messages: Vec<MessageRef>, flags: String, add: bool },
    Delete { messages: Vec<MessageRef> },
    /// Raw RFC-822 source of one message — feeds the «Показать →
    /// Заголовки / Исходник сообщения» views.
    FetchSource { folder: String, uid: u32 },
    /// «Спам»: blacklist the sender (domain rule) and purge all their
    /// messages, including the given conversation rows.
    BlacklistAndPurge {
        domain: String,
        address: String,
        message_ids: Vec<i64>,
    },
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
    /// Delete an event.
    DeleteEvent { event_id: i64 },
    Send {
        to: Vec<String>,
        cc: Vec<String>,
        subject: String,
        body: String,
        in_reply_to: Option<String>,
        references: Option<String>,
        /// Filesystem paths of files the user attached in the composer.
        /// Resolved to bytes (and a guessed MIME type) just before sending.
        attachments: Vec<String>,
        /// Forward mode: re-attach every attachment of this original
        /// message (downloaded from the provider) to the outgoing one.
        forward_attachments: Option<MessageRef>,
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
    /// A message was sent (server response / message-id).
    Sent(String),
    /// A mutating op (flags/delete) finished; UI should refresh.
    Done(String),
    /// A decoded avatar (RGBA) for an email address.
    Avatar { email: String, rgba: Vec<u8>, w: u32, h: u32 },
    /// An attachment was downloaded and saved at this path; UI opens it.
    AttachmentSaved(String),
    /// Raw RFC-822 source for a FetchSource request.
    Source { uid: u32, raw: String },
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
        // Primary = first account. Addressed commands (body/flags/delete/
        // source/attachment/send) and calendar commands use it until
        // per-account routing lands (1b-ii); aggregating commands fan out.
        let provider = conns[0].provider.clone();
        let key = conns[0].key.clone();
        let cfg = conns[0].cfg.clone();

        while let Ok(cmd) = rx.recv() {
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
                EngineCmd::FetchMessages { messages, generation } => {
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
                    let have: std::collections::HashSet<(String, u32)> = cached
                        .iter()
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
                EngineCmd::SetFlags { messages, flags, add } => {
                    match rt.block_on(provider.set_flags_batch(&messages, &flags, add)) {
                        Ok(()) => on_result(EngineResult::Done("flags".into())),
                        Err(e) => on_result(EngineResult::Error(e)),
                    }
                }
                EngineCmd::Delete { messages } => {
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
                EngineCmd::FetchSource { folder, uid } => {
                    match rt.block_on(provider.fetch_message_source(&folder, uid)) {
                        Ok(raw) => on_result(EngineResult::Source { uid, raw }),
                        Err(e) => on_result(EngineResult::Error(e)),
                    }
                }
                EngineCmd::BlacklistAndPurge { domain, address, message_ids } => {
                    match rt.block_on(provider.blacklist_and_purge(&domain, &address, &message_ids)) {
                        Ok(n) => {
                            println!("engine: spam purge of {address} removed {n} messages");
                            // Conversations vanish — force the next list
                            // refetch to run full (same as Delete).
                            cache.set_meta(&format!("conv_full_ts:{key}"), "0").ok();
                            on_result(EngineResult::Done("delete".into()));
                        }
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
                EngineCmd::DeleteEvent { event_id } => {
                    match rt.block_on(provider.delete_event(event_id)) {
                        Ok(_) => on_result(EngineResult::Done("rsvp".into())),
                        Err(e) => on_result(EngineResult::Error(format!("delete_event: {e}"))),
                    }
                }
                EngineCmd::Send { to, cc, subject, body, in_reply_to, references, attachments, forward_attachments } => {
                    let mut outgoing = resolve_attachments(&attachments);
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
                        attachments: outgoing,
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
                    // Start a watcher per account. start_watching spawns its
                    // worker on the runtime and returns, so the loop stays
                    // responsive. Events aren't account-tagged yet (P2).
                    for conn in &conns {
                        let sink = on_result.clone();
                        let notifier: Notifier =
                            Arc::new(move |ev| sink(EngineResult::Event(ev)));
                        if let Err(e) = rt.block_on(conn.provider.start_watching(notifier)) {
                            eprintln!("watch [{}]: {e}", conn.key);
                        }
                    }
                }
            }
        }
    });
    tx
}
