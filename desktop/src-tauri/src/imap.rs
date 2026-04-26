use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use tauri::{AppHandle, Manager, Runtime};
use tokio_util::compat::TokioAsyncReadCompatExt;
use futures::TryStreamExt;

use md5::{Md5, Digest};

use crate::cache::Cache;
use crate::session::{Credentials, SessionPool};

// ── Identity ──

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Identity {
    pub email: String,
    pub name: String,
    pub signature: String,
    pub is_default: bool,
    #[serde(default)]
    pub color: String, // assigned client-side, pastel
}

fn gravatar_hash(email: &str) -> String {
    let trimmed = email.trim().to_lowercase();
    let hash = Md5::digest(trimmed.as_bytes());
    format!("{:x}", hash)
}

fn account_key(host: &str, username: &str) -> String {
    format!("{username}@{host}")
}

// ── Types ──

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Folder {
    pub name: String,
    pub delimiter: String,
    pub unread: u32,
    pub total: u32,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ContactInfo {
    pub name: String,
    pub addr: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct MessageRef {
    pub folder: String,
    pub uid: u32,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Conversation {
    pub id: String,
    pub label: String,
    pub avatar_hash: String,
    pub received_by: String, // which of our emails received this conversation's messages
    pub counterparts: Vec<ContactInfo>,
    pub is_group: bool,
    pub last_date: String,
    pub last_date_ts: i64,
    pub last_preview: String,
    pub last_from: String,
    pub unread_count: u32,
    pub total_count: u32,
    pub messages: Vec<MessageRef>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct MessageEnvelope {
    pub uid: u32,
    pub folder: String,
    pub subject: String,
    pub from: String,
    pub from_addr: String,
    pub to: Vec<String>,
    pub to_addrs: Vec<String>,
    pub cc_addrs: Vec<String>,
    pub date: String,
    pub date_ts: i64,
    pub seen: bool,
    pub flagged: bool,
    pub has_attachments: bool,
    pub is_outgoing: bool,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct MessageBody {
    pub uid: u32,
    pub folder: String,
    pub subject: String,
    pub from: String,
    pub from_addr: String,
    pub to: Vec<String>,
    pub cc: Vec<String>,
    pub date: String,
    pub date_ts: i64,
    pub html: Option<String>,
    pub text: Option<String>,
    pub attachments: Vec<Attachment>,
    pub is_outgoing: bool,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Attachment {
    pub filename: String,
    pub mime_type: String,
    pub size: usize,
    pub index: usize,
}

// ── Internal helpers ──

struct RawEnvelope {
    uid: u32,
    folder: String,
    subject: String,
    from_name: String,
    from_addr: String,
    to_names: Vec<String>,
    to_addrs: Vec<String>,
    cc_addrs: Vec<String>,
    date: String,
    date_ts: i64,
    seen: bool,
    flagged: bool,
    has_attachments: bool,
}

fn parse_date_to_ts(date_str: &str) -> i64 {
    chrono::DateTime::parse_from_rfc2822(date_str)
        .map(|d| d.timestamp())
        .unwrap_or(0)
}

/// Decode RFC 2047 encoded-words (=?utf-8?q?...?= / =?utf-8?b?...?=) in IMAP envelope fields.
fn decode_mime(raw: &[u8]) -> String {
    // Wrap raw bytes as a fake header so mailparse can decode RFC 2047 words
    let mut fake = b"X: ".to_vec();
    fake.extend_from_slice(raw);
    fake.push(b'\n');
    match mailparse::parse_header(&fake) {
        Ok((hdr, _)) => hdr.get_value(),
        Err(_) => String::from_utf8_lossy(raw).to_string(),
    }
}

fn addr_str(a: &async_imap::imap_proto::types::Address<'_>) -> String {
    let mbox = a.mailbox.as_ref()
        .and_then(|m| String::from_utf8(m.to_vec()).ok())
        .unwrap_or_default()
        .replace(['<', '>'], "");
    let host = a.host.as_ref()
        .and_then(|h| String::from_utf8(h.to_vec()).ok())
        .unwrap_or_default()
        .replace(['<', '>'], "");
    let email = format!("{mbox}@{host}").to_lowercase();
    // Remove trailing @ if host was empty
    email.trim_end_matches('@').to_string()
}

fn name_str(a: &async_imap::imap_proto::types::Address<'_>) -> String {
    a.name.as_ref()
        .map(|n| decode_mime(n))
        .unwrap_or_default()
}

/// Strip email parts from display name.
/// "John Doe <john@example.com>" → "John Doe"
/// "John Doe (john@example.com)" → "John Doe"
/// "john@example.com" → "" (pure email, no name)
fn clean_display_name(s: &str) -> String {
    let mut result = s.to_string();
    // Remove <...> part
    if let Some(idx) = result.find('<') {
        result = result[..idx].to_string();
    }
    // Remove (...) part if it contains @
    if let Some(start) = result.find('(') {
        if let Some(end) = result.find(')') {
            if result[start..end].contains('@') {
                result = result[..start].to_string();
            }
        }
    }
    let trimmed = result.trim().to_string();
    // If what's left looks like an email, it's not a name
    if trimmed.contains('@') {
        return String::new();
    }
    trimmed
}

fn extract_envelope(msg: &async_imap::types::Fetch, folder: &str) -> Option<RawEnvelope> {
    let uid = msg.uid?;
    let env = msg.envelope()?;

    let subject = env.subject.as_ref()
        .map(|s| decode_mime(s))
        .unwrap_or_default();

    // Parse raw FROM header for proper-case name (ENVELOPE may lowercase)
    let raw_headers = msg.header()
        .map(|h| String::from_utf8_lossy(h).to_string())
        .unwrap_or_default();
    let (from_name, from_addr) = parse_from_header(&raw_headers)
        .unwrap_or_else(|| {
            // Fallback to envelope
            let name = env.from.as_ref()
                .and_then(|a| a.first()).map(|a| name_str(a)).unwrap_or_default();
            let addr = env.from.as_ref()
                .and_then(|a| a.first()).map(|a| addr_str(a)).unwrap_or_default();
            (name, addr)
        });

    let to_names: Vec<String> = env.to.as_ref()
        .map(|addrs| addrs.iter().map(|a| { let n = name_str(a); if n.is_empty() { addr_str(a) } else { n } }).collect())
        .unwrap_or_default();
    let to_addrs: Vec<String> = env.to.as_ref()
        .map(|addrs| addrs.iter().map(|a| addr_str(a)).collect()).unwrap_or_default();
    let cc_addrs: Vec<String> = env.cc.as_ref()
        .map(|addrs| addrs.iter().map(|a| addr_str(a)).collect()).unwrap_or_default();
    let date = env.date.as_ref()
        .and_then(|d| String::from_utf8(d.to_vec()).ok()).unwrap_or_default();
    let date_ts = parse_date_to_ts(&date);
    let flags: Vec<_> = msg.flags().collect();
    let seen = flags.iter().any(|f| matches!(f, async_imap::types::Flag::Seen));
    let flagged = flags.iter().any(|f| matches!(f, async_imap::types::Flag::Flagged));
    let has_attachments = raw_headers.to_lowercase().contains("multipart/mixed");

    Some(RawEnvelope { uid, folder: folder.to_string(), subject, from_name, from_addr,
        to_names, to_addrs, cc_addrs, date, date_ts, seen, flagged, has_attachments })
}

fn clean_subject(subject: &str) -> String {
    let mut s = subject.trim();
    loop {
        let lower = s.to_lowercase();
        if lower.starts_with("re:") { s = s[3..].trim_start(); }
        else if lower.starts_with("fwd:") { s = s[4..].trim_start(); }
        else if lower.starts_with("fw:") { s = s[3..].trim_start(); }
        else { break; }
    }
    s.to_string()
}

/// Parse raw "From: ..." header to get (display_name, email).
/// Uses mailparse for proper RFC 2047 decoding with correct case.
fn parse_from_header(raw_headers: &str) -> Option<(String, String)> {
    // Find "From:" line in raw headers
    let headers_bytes = raw_headers.as_bytes();
    let parsed = mailparse::parse_headers(headers_bytes).ok()?.0;
    let from_hdr = parsed.iter().find(|h| h.get_key().eq_ignore_ascii_case("from"))?;
    let from_value = from_hdr.get_value();

    // Parse address: "Name <email>" or just "email"
    let (name, addr) = if let Some(lt) = from_value.rfind('<') {
        if let Some(gt) = from_value.rfind('>') {
            let name = from_value[..lt].trim().trim_matches('"').to_string();
            let addr = from_value[lt + 1..gt].trim().to_lowercase();
            (name, addr)
        } else {
            (String::new(), from_value.trim().to_lowercase())
        }
    } else {
        (String::new(), from_value.trim().to_lowercase())
    };

    if addr.is_empty() { return None; }
    Some((name, addr))
}

fn extract_addr_from_header(header: &str) -> String {
    if let Some(start) = header.rfind('<') {
        if let Some(end) = header.rfind('>') {
            return header[start + 1..end].trim().to_lowercase();
        }
    }
    header.trim().to_lowercase()
}

fn find_sent_folder(folders: &[Folder]) -> Option<String> {
    let candidates = ["Sent", "INBOX.Sent", "Sent Messages", "Sent Items"];
    for c in &candidates {
        if let Some(f) = folders.iter().find(|f| f.name.eq_ignore_ascii_case(c)) {
            return Some(f.name.clone());
        }
    }
    folders.iter().find(|f| f.name.to_lowercase().contains("sent")).map(|f| f.name.clone())
}

// Helper: fetch envelopes from a folder using an existing session
async fn fetch_folder_envelopes<T>(
    session: &mut async_imap::Session<T>,
    folder: &str,
    limit: u32,
) -> Result<Vec<RawEnvelope>, String>
where
    T: futures::AsyncRead + futures::AsyncWrite + Unpin + Send + std::fmt::Debug,
{
    let mailbox = session.select(folder).await
        .map_err(|e| format!("SELECT {folder}: {e}"))?;
    let total = mailbox.exists;
    if total == 0 { return Ok(vec![]); }

    let start = total.saturating_sub(limit).max(1);
    let range = format!("{start}:{total}");
    let messages = session
        .fetch(&range, "(UID FLAGS ENVELOPE BODY.PEEK[HEADER.FIELDS (CONTENT-TYPE FROM)])")
        .await.map_err(|e| format!("FETCH {folder}: {e}"))?;

    let mut envelopes = Vec::new();
    let collected: Vec<_> = messages.try_collect::<Vec<_>>()
        .await.map_err(|e| format!("Collect: {e}"))?;
    for msg in &collected {
        if let Some(env) = extract_envelope(msg, folder) {
            envelopes.push(env);
        }
    }
    Ok(envelopes)
}

// Macro-like helper: connect, login, get session (TLS)
async fn connect_tls(host: &str, port: u16, username: &str, password: &str)
    -> Result<async_imap::Session<async_native_tls::TlsStream<tokio_util::compat::Compat<tokio::net::TcpStream>>>, String>
{
    let tls = async_native_tls::TlsConnector::new();
    let tcp = tokio::net::TcpStream::connect((host, port))
        .await.map_err(|e| format!("TCP: {e}"))?;
    let tls_stream = tls.connect(host, tcp.compat())
        .await.map_err(|e| format!("TLS: {e}"))?;
    let client = async_imap::Client::new(tls_stream);
    client.login(username, password).await.map_err(|e| format!("Login: {:?}", e.0))
}

async fn connect_plain(host: &str, port: u16, username: &str, password: &str)
    -> Result<async_imap::Session<tokio_util::compat::Compat<tokio::net::TcpStream>>, String>
{
    let tcp = tokio::net::TcpStream::connect((host, port))
        .await.map_err(|e| format!("TCP: {e}"))?;
    let client = async_imap::Client::new(tcp.compat());
    client.login(username, password).await.map_err(|e| format!("Login: {:?}", e.0))
}

// ── Commands ──

#[tauri::command]
pub async fn connect(
    host: String, port: u16, username: String, password: String, use_tls: bool,
) -> Result<Vec<Folder>, String> {
    if use_tls {
        let mut session = connect_tls(&host, port, &username, &password).await?;
        let result = list_folders_impl(&mut session).await;
        session.logout().await.ok();
        result
    } else {
        let mut session = connect_plain(&host, port, &username, &password).await?;
        let result = list_folders_impl(&mut session).await;
        session.logout().await.ok();
        result
    }
}

async fn list_folders_impl<T>(session: &mut async_imap::Session<T>) -> Result<Vec<Folder>, String>
where
    T: futures::AsyncRead + futures::AsyncWrite + Unpin + Send + std::fmt::Debug,
{
    let mailboxes = session.list(Some(""), Some("*"))
        .await.map_err(|e| format!("LIST: {e}"))?;
    let collected: Vec<_> = mailboxes.try_collect::<Vec<_>>()
        .await.map_err(|e| format!("Collect: {e}"))?;

    let mut folders = Vec::new();
    for mailbox in &collected {
        let name = mailbox.name().to_string();
        let delimiter = mailbox.delimiter().unwrap_or("/").to_string();
        let (total, unread) = match session.status(&name, "(MESSAGES UNSEEN)").await {
            Ok(s) => (s.exists, s.unseen.unwrap_or(0)),
            Err(_) => (0, 0),
        };
        folders.push(Folder { name, delimiter, unread, total });
    }
    Ok(folders)
}

#[tauri::command]
pub async fn list_folders(
    host: String, port: u16, username: String, password: String, use_tls: bool,
) -> Result<Vec<Folder>, String> {
    connect(host, port, username, password, use_tls).await
}

#[tauri::command]
pub async fn fetch_conversations(
    cache: tauri::State<'_, Cache>,
    host: String, port: u16, username: String, password: String, use_tls: bool,
    user_email: String, limit: u32,
) -> Result<Vec<Conversation>, String> {
    let user_addr = user_email.to_lowercase();
    let key = account_key(&host, &username);

    let result = if use_tls {
        let mut session = connect_tls(&host, port, &username, &password).await?;
        let r = fetch_conversations_impl(&mut session, &user_addr, limit).await;
        session.logout().await.ok();
        r
    } else {
        let mut session = connect_plain(&host, port, &username, &password).await?;
        let r = fetch_conversations_impl(&mut session, &user_addr, limit).await;
        session.logout().await.ok();
        r
    };

    // Save to cache on success
    if let Ok(ref convs) = result {
        cache.save_conversations(&key, convs).ok();
    }

    result
}

async fn fetch_conversations_impl<T>(
    session: &mut async_imap::Session<T>,
    user_addr: &str,
    limit: u32,
) -> Result<Vec<Conversation>, String>
where
    T: futures::AsyncRead + futures::AsyncWrite + Unpin + Send + std::fmt::Debug,
{
    // Get folder list
    let mailboxes = session.list(Some(""), Some("*"))
        .await.map_err(|e| format!("LIST: {e}"))?;
    let collected: Vec<_> = mailboxes.try_collect::<Vec<_>>()
        .await.map_err(|e| format!("Collect: {e}"))?;
    let folder_list: Vec<Folder> = collected.iter().map(|m| Folder {
        name: m.name().to_string(),
        delimiter: m.delimiter().unwrap_or("/").to_string(),
        unread: 0, total: 0,
    }).collect();

    // Fetch INBOX
    let mut all_envelopes = fetch_folder_envelopes(session, "INBOX", limit).await?;

    // Fetch Sent
    if let Some(sent_name) = find_sent_folder(&folder_list) {
        if let Ok(sent_envs) = fetch_folder_envelopes(session, &sent_name, limit / 2).await {
            all_envelopes.extend(sent_envs);
        }
    }

    // Group into conversations.
    // Telegram-like: counterpart = sender (for received) or first recipient (for sent).
    // CC is ignored for grouping — every conversation is 1-on-1 with the primary counterpart.
    let mut conv_map: HashMap<String, Vec<RawEnvelope>> = HashMap::new();

    for env in all_envelopes {
        let key = if env.from_addr == user_addr {
            // Outgoing: counterpart = first To recipient
            env.to_addrs.first().cloned().unwrap_or_else(|| user_addr.to_string())
        } else {
            // Incoming: counterpart = sender
            env.from_addr.clone()
        };
        conv_map.entry(key).or_default().push(env);
    }

    let mut conversations: Vec<Conversation> = Vec::new();

    for (key, mut msgs) in conv_map {
        msgs.sort_by_key(|m| m.date_ts);

        // All conversations are 1-on-1 (grouped by single counterpart)
        let is_group = false;

        // Label = counterpart's display name, fallback to email
        let cp_addr = &key;

        // Try to find display name from FROM (when they sent us mail)
        let cp_name = msgs.iter()
            .find(|m| m.from_addr == *cp_addr && !m.from_name.is_empty())
            .map(|m| clean_display_name(&m.from_name))
            .filter(|n| !n.is_empty() && !n.contains('@'))
            // Fallback: look in TO names (when we sent mail to them)
            .or_else(|| msgs.iter().flat_map(|m|
                m.to_addrs.iter().zip(m.to_names.iter())
                    .filter(|(a, _)| a.as_str() == cp_addr.as_str())
                    .map(|(_, n)| clean_display_name(n))
            ).find(|n| !n.is_empty() && !n.contains('@')))
            .unwrap_or_default();

        let label = if cp_name.is_empty() {
            // No display name — show full email
            cp_addr.clone()
        } else {
            cp_name.clone()
        };

        let counterparts = vec![ContactInfo {
            name: cp_name,
            addr: cp_addr.clone(),
        }];

        let last = msgs.last().unwrap();
        let last_from = if last.from_addr == user_addr { "You".into() } else { last.from_name.clone() };

        // Which of our email addresses received this conversation?
        let received_by = msgs.iter().rev()
            .find(|m| m.from_addr != user_addr)
            .and_then(|m| {
                m.to_addrs.iter().chain(m.cc_addrs.iter())
                    .find(|a| a.as_str() != cp_addr.as_str())
                    .cloned()
            })
            .unwrap_or_else(|| user_addr.to_string());

        conversations.push(Conversation {
            id: key,
            label,
            avatar_hash: gravatar_hash(&counterparts[0].addr),
            received_by,
            counterparts,
            is_group,
            last_date: last.date.clone(),
            last_date_ts: last.date_ts,
            last_preview: last.subject.clone(),
            last_from,
            unread_count: msgs.iter().filter(|m| !m.seen).count() as u32,
            total_count: msgs.len() as u32,
            messages: msgs.iter().map(|m| MessageRef { folder: m.folder.clone(), uid: m.uid }).collect(),
        });
    }

    conversations.sort_by(|a, b| b.last_date_ts.cmp(&a.last_date_ts));
    Ok(conversations)
}

#[tauri::command]
pub async fn fetch_conversation_messages(
    cache: tauri::State<'_, Cache>,
    host: String, port: u16, username: String, password: String, use_tls: bool,
    user_email: String, messages: Vec<MessageRef>,
) -> Result<Vec<MessageBody>, String> {
    let user_addr = user_email.to_lowercase();
    let key = account_key(&host, &username);

    let result = if use_tls {
        let mut session = connect_tls(&host, port, &username, &password).await?;
        let r = fetch_bodies_impl(&mut session, &user_addr, &messages).await;
        session.logout().await.ok();
        r
    } else {
        let mut session = connect_plain(&host, port, &username, &password).await?;
        let r = fetch_bodies_impl(&mut session, &user_addr, &messages).await;
        session.logout().await.ok();
        r
    };

    // Cache bodies
    if let Ok(ref bodies) = result {
        cache.save_message_bodies(&key, bodies).ok();
    }

    result
}

async fn fetch_bodies_impl<T>(
    session: &mut async_imap::Session<T>,
    user_addr: &str,
    message_refs: &[MessageRef],
) -> Result<Vec<MessageBody>, String>
where
    T: futures::AsyncRead + futures::AsyncWrite + Unpin + Send + std::fmt::Debug,
{
    let mut by_folder: HashMap<String, Vec<u32>> = HashMap::new();
    for mr in message_refs {
        by_folder.entry(mr.folder.clone()).or_default().push(mr.uid);
    }

    let mut bodies: Vec<MessageBody> = Vec::new();

    for (folder, uids) in by_folder {
        session.select(&folder).await.map_err(|e| format!("SELECT {folder}: {e}"))?;
        let uid_set = uids.iter().map(|u| u.to_string()).collect::<Vec<_>>().join(",");
        let fetched = session.uid_fetch(&uid_set, "(BODY.PEEK[] FLAGS)")
            .await.map_err(|e| format!("FETCH {folder}: {e}"))?;

        let collected: Vec<_> = fetched.try_collect::<Vec<_>>()
            .await.map_err(|e| format!("Collect: {e}"))?;

        for msg in &collected {
            let uid = msg.uid.unwrap_or(0);
            let body_raw = match msg.body() { Some(b) => b, None => continue };
            let parsed = match mailparse::parse_mail(body_raw) { Ok(p) => p, Err(_) => continue };

            let subject = parsed.headers.iter()
                .find(|h| h.get_key().eq_ignore_ascii_case("subject"))
                .map(|h| h.get_value()).unwrap_or_default();
            let from = parsed.headers.iter()
                .find(|h| h.get_key().eq_ignore_ascii_case("from"))
                .map(|h| h.get_value()).unwrap_or_default();
            let from_addr = extract_addr_from_header(&from);
            let to: Vec<String> = parsed.headers.iter()
                .find(|h| h.get_key().eq_ignore_ascii_case("to"))
                .map(|h| h.get_value().split(',').map(|s| s.trim().to_string()).collect())
                .unwrap_or_default();
            let cc: Vec<String> = parsed.headers.iter()
                .find(|h| h.get_key().eq_ignore_ascii_case("cc"))
                .map(|h| h.get_value().split(',').map(|s| s.trim().to_string()).collect())
                .unwrap_or_default();
            let date = parsed.headers.iter()
                .find(|h| h.get_key().eq_ignore_ascii_case("date"))
                .map(|h| h.get_value()).unwrap_or_default();
            let date_ts = parse_date_to_ts(&date);

            let mut html = None;
            let mut text = None;
            let mut attachments = Vec::new();
            walk_parts(&parsed, &mut html, &mut text, &mut attachments, &mut 0);

            bodies.push(MessageBody {
                uid, folder: folder.clone(), subject, from, from_addr: from_addr.clone(),
                to, cc, date, date_ts, html, text, attachments,
                is_outgoing: from_addr == user_addr,
            });
        }
    }

    bodies.sort_by_key(|b| b.date_ts);
    Ok(bodies)
}

fn walk_parts(part: &mailparse::ParsedMail, html: &mut Option<String>, text: &mut Option<String>, attachments: &mut Vec<Attachment>, idx: &mut usize) {
    let ct = part.ctype.mimetype.to_lowercase();
    if part.subparts.is_empty() {
        let disp = part.get_content_disposition();
        if matches!(disp.disposition, mailparse::DispositionType::Attachment) {
            let filename = disp.params.get("filename").cloned().unwrap_or_else(|| format!("attachment_{idx}"));
            let size = part.get_body_raw().map(|b| b.len()).unwrap_or(0);
            attachments.push(Attachment { filename, mime_type: ct, size, index: *idx });
            *idx += 1;
            return;
        }
        if ct == "text/html" && html.is_none() { *html = part.get_body().ok(); }
        else if ct == "text/plain" && text.is_none() { *text = part.get_body().ok(); }
    }
    for sub in &part.subparts { walk_parts(sub, html, text, attachments, idx); }
}

#[tauri::command]
pub async fn search_messages(
    host: String, port: u16, username: String, password: String, use_tls: bool,
    user_email: String, query: String,
) -> Result<Vec<MessageEnvelope>, String> {
    let user_addr = user_email.to_lowercase();
    if use_tls {
        let mut session = connect_tls(&host, port, &username, &password).await?;
        let result = search_impl(&mut session, &user_addr, &query).await;
        session.logout().await.ok();
        result
    } else {
        let mut session = connect_plain(&host, port, &username, &password).await?;
        let result = search_impl(&mut session, &user_addr, &query).await;
        session.logout().await.ok();
        result
    }
}

async fn search_impl<T>(
    session: &mut async_imap::Session<T>,
    user_addr: &str,
    query: &str,
) -> Result<Vec<MessageEnvelope>, String>
where
    T: futures::AsyncRead + futures::AsyncWrite + Unpin + Send + std::fmt::Debug,
{
    session.select("INBOX").await.map_err(|e| format!("SELECT: {e}"))?;
    let search_query = format!("TEXT \"{query}\"");
    let uids = session.search(&search_query).await.map_err(|e| format!("SEARCH: {e}"))?;
    if uids.is_empty() { return Ok(vec![]); }

    let mut uid_vec: Vec<u32> = uids.into_iter().collect();
    uid_vec.sort_unstable();
    uid_vec.reverse();
    let uid_list: Vec<String> = uid_vec.iter().take(30).map(|u| u.to_string()).collect();
    let uid_set = uid_list.join(",");
    let messages = session.uid_fetch(&uid_set, "(UID FLAGS ENVELOPE)")
        .await.map_err(|e| format!("FETCH: {e}"))?;
    let collected: Vec<_> = messages.try_collect::<Vec<_>>()
        .await.map_err(|e| format!("Collect: {e}"))?;

    let mut envelopes = Vec::new();
    for msg in &collected {
        if let Some(raw) = extract_envelope(msg, "INBOX") {
            let is_outgoing = raw.from_addr == user_addr;
            envelopes.push(MessageEnvelope {
                uid: raw.uid, folder: raw.folder, subject: raw.subject,
                from: raw.from_name, from_addr: raw.from_addr,
                to: raw.to_names, to_addrs: raw.to_addrs, cc_addrs: raw.cc_addrs,
                date: raw.date, date_ts: raw.date_ts,
                seen: raw.seen, flagged: raw.flagged,
                has_attachments: raw.has_attachments, is_outgoing,
            });
        }
    }
    Ok(envelopes)
}

async fn store_flags_impl<T>(
    session: &mut async_imap::Session<T>,
    folder: &str, uid: u32, flags: &str, add: bool,
) -> Result<(), String>
where
    T: futures::AsyncRead + futures::AsyncWrite + Unpin + Send + std::fmt::Debug,
{
    session.select(folder).await.map_err(|e| format!("SELECT: {e}"))?;
    let op = if add { "+FLAGS" } else { "-FLAGS" };
    let store_result = session.uid_store(uid.to_string(), &format!("{op} ({flags})"))
        .await.map_err(|e| format!("STORE: {e}"))?;
    let _: Vec<_> = store_result.try_collect::<Vec<_>>()
        .await.map_err(|e| format!("STORE collect: {e}"))?;
    Ok(())
}

#[tauri::command]
pub async fn set_flags(
    host: String, port: u16, username: String, password: String, use_tls: bool,
    folder: String, uid: u32, flags: String, add: bool,
) -> Result<(), String> {
    if use_tls {
        let mut session = connect_tls(&host, port, &username, &password).await?;
        store_flags_impl(&mut session, &folder, uid, &flags, add).await?;
        session.logout().await.ok();
    } else {
        let mut session = connect_plain(&host, port, &username, &password).await?;
        store_flags_impl(&mut session, &folder, uid, &flags, add).await?;
        session.logout().await.ok();
    }
    Ok(())
}

/// Fetch raw RFC-822 source of a single message.
#[tauri::command]
pub async fn fetch_message_source(
    host: String, port: u16, username: String, password: String, use_tls: bool,
    folder: String, uid: u32,
) -> Result<String, String> {
    if use_tls {
        let mut session = connect_tls(&host, port, &username, &password).await?;
        let r = fetch_source_impl(&mut session, &folder, uid).await;
        session.logout().await.ok();
        r
    } else {
        let mut session = connect_plain(&host, port, &username, &password).await?;
        let r = fetch_source_impl(&mut session, &folder, uid).await;
        session.logout().await.ok();
        r
    }
}

async fn fetch_source_impl<T>(
    session: &mut async_imap::Session<T>,
    folder: &str,
    uid: u32,
) -> Result<String, String>
where
    T: futures::AsyncRead + futures::AsyncWrite + Unpin + Send + std::fmt::Debug,
{
    session.select(folder).await.map_err(|e| format!("SELECT {folder}: {e}"))?;
    let fetched = session.uid_fetch(uid.to_string(), "BODY.PEEK[]")
        .await.map_err(|e| format!("FETCH: {e}"))?;
    let msgs: Vec<_> = fetched.try_collect().await.map_err(|e| format!("Collect: {e}"))?;
    let msg = msgs.first().ok_or("Message not found")?;
    let body = msg.body().ok_or("Empty body")?;
    String::from_utf8(body.to_vec()).map_err(|_| "Non-UTF8 source".into())
}

// ── Cache commands ──

/// Load conversations from local SQLite cache (instant).
#[tauri::command]
pub async fn load_cached_conversations(
    cache: tauri::State<'_, Cache>,
    host: String, username: String,
) -> Result<Vec<Conversation>, String> {
    let key = account_key(&host, &username);
    cache.load_conversations(&key)
}

/// Load message bodies from local cache (instant).
#[tauri::command]
pub async fn load_cached_messages(
    cache: tauri::State<'_, Cache>,
    host: String, username: String, messages: Vec<MessageRef>,
) -> Result<Vec<MessageBody>, String> {
    let key = account_key(&host, &username);
    cache.load_message_bodies(&key, &messages)
}

/// Fetch Gravatar avatar: cache-first, then HTTP, cache for 7 days.
/// Returns base64-encoded PNG or empty string if no avatar.
/// Check CAPABILITY for METADATA, fetch identities via GETMETADATA.
/// Returns empty vec if not our server.
#[tauri::command]
pub async fn fetch_identities(
    cache: tauri::State<'_, Cache>,
    host: String, port: u16, username: String, password: String, use_tls: bool,
) -> Result<Vec<Identity>, String> {
    let key = account_key(&host, &username);

    // Always fetch fresh from server; fall back to cache on error
    let result = if use_tls {
        let mut session = connect_tls(&host, port, &username, &password).await?;
        let ids = fetch_identities_impl(&mut session).await;
        session.logout().await.ok();
        ids
    } else {
        let mut session = connect_plain(&host, port, &username, &password).await?;
        let ids = fetch_identities_impl(&mut session).await;
        session.logout().await.ok();
        ids
    };

    match result {
        Ok(identities) => {
            if !identities.is_empty() {
                cache.save_identities(&key, &identities).ok();
            }
            Ok(identities)
        }
        Err(_) => {
            // Server failed — use cache as fallback
            cache.load_identities(&key)
        }
    }
}

async fn fetch_identities_impl<T>(
    session: &mut async_imap::Session<T>,
) -> Result<Vec<Identity>, String>
where
    T: futures::AsyncRead + futures::AsyncWrite + Unpin + Send + std::fmt::Debug,
{
    // 1. Check CAPABILITY for METADATA
    let caps = session.capabilities().await.map_err(|e| format!("CAPABILITY: {e}"))?;
    let has_metadata = caps.has(&async_imap::types::Capability::Atom("METADATA".to_string()));
    if !has_metadata {
        log::info!("Server does not support METADATA — not our server");
        return Ok(vec![]);
    }

    // 2. Send GETMETADATA command
    let _tag = session.run_command("GETMETADATA \"\" /shared/vendor/ddmail/identities")
        .await.map_err(|e| format!("GETMETADATA send: {e}"))?;

    // 3. Read responses — extract JSON from raw bytes
    let mut json_data = String::new();
    loop {
        match session.read_response().await {
            Some(Ok(resp)) => {
                // Get raw bytes of the IMAP response line
                let raw_bytes = resp.borrow_owner();
                let raw_text = String::from_utf8_lossy(raw_bytes);
                log::debug!("GETMETADATA raw: {}", raw_text);

                // Find JSON array in the raw response
                if json_data.is_empty() {
                    if let Some(start) = raw_text.find("[{") {
                        if let Some(end) = raw_text.rfind("}]") {
                            json_data = raw_text[start..=end + 1].to_string();
                        }
                    }
                }

                // Tagged response = done
                if resp.request_id().is_some() {
                    break;
                }
            }
            Some(Err(e)) => {
                return Err(format!("GETMETADATA read: {e}"));
            }
            None => break,
        }
    }

    if json_data.is_empty() {
        log::warn!("GETMETADATA: no JSON data found in response");
        return Ok(vec![]);
    }

    log::info!("GETMETADATA JSON ({} bytes): {}...", json_data.len(),
        &json_data[..json_data.len().min(100)]);

    // 4. Parse JSON
    let mut identities: Vec<Identity> = serde_json::from_str(&json_data)
        .map_err(|e| format!("Parse identities JSON: {e}"))?;

    // 5. Assign pastel colors to identities without colors
    let pastel_colors = [
        "#FFE4E1", "#E8F5E9", "#E3F2FD", "#FFF9C4", "#F3E5F5",
        "#E0F7FA", "#FBE9E7", "#F1F8E9", "#EDE7F6", "#E8EAF6",
        "#FCE4EC", "#E0F2F1", "#FFF3E0", "#F9FBE7", "#EFEBE9",
    ];
    for (i, identity) in identities.iter_mut().enumerate() {
        if identity.color.is_empty() {
            identity.color = pastel_colors[i % pastel_colors.len()].to_string();
        }
    }

    Ok(identities)
}

#[tauri::command]
pub async fn fetch_avatar(
    cache: tauri::State<'_, Cache>,
    email: String,
) -> Result<String, String> {
    let email_lower = email.trim().to_lowercase();

    // Check cache first
    if let Some(data) = cache.get_avatar(&email_lower) {
        use base64::Engine;
        return Ok(base64::engine::general_purpose::STANDARD.encode(&data));
    }

    // Fetch from Gravatar
    let hash = gravatar_hash(&email_lower);
    let url = format!("https://www.gravatar.com/avatar/{hash}?d=404&s=96");

    let response = reqwest::get(&url).await.map_err(|e| format!("HTTP: {e}"))?;
    if !response.status().is_success() {
        return Ok(String::new()); // No gravatar
    }

    let bytes = response.bytes().await.map_err(|e| format!("read: {e}"))?;

    // Cache it
    cache.save_avatar(&email_lower, &bytes).ok();

    use base64::Engine;
    Ok(base64::engine::general_purpose::STANDARD.encode(&bytes))
}

#[tauri::command]
pub async fn start_watching(
    app: AppHandle<impl Runtime>,
    pool: tauri::State<'_, SessionPool>,
    host: String, port: u16, username: String, password: String, use_tls: bool,
    user_email: String,
) -> Result<(), String> {
    let creds = Credentials { host, port, username, password, use_tls, user_email };
    pool.start_idle(app, creds).await;
    Ok(())
}
