use std::collections::HashMap;
use crate::event::Notifier;
use tokio_util::compat::TokioAsyncReadCompatExt;
use futures::TryStreamExt;

use md5::{Md5, Digest};

use crate::cache::Cache;
use crate::session::{Credentials, SessionPool};
use crate::types::*;

pub(crate) fn gravatar_hash(email: &str) -> String {
    let trimmed = email.trim().to_lowercase();
    let hash = Md5::digest(trimmed.as_bytes());
    format!("{:x}", hash)
}

pub fn account_key(host: &str, username: &str) -> String {
    format!("{username}@{host}")
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
    is_draft: bool,
    message_id: String,
    in_reply_to: String,
    references: Vec<String>,
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
    let is_draft = flags.iter().any(|f| matches!(f, async_imap::types::Flag::Draft));
    let has_attachments = raw_headers.to_lowercase().contains("multipart/mixed");

    let message_id = env.message_id.as_ref()
        .map(|b| String::from_utf8_lossy(b).to_string())
        .map(|s| normalize_msg_id(&s))
        .unwrap_or_default();
    let in_reply_to = env.in_reply_to.as_ref()
        .map(|b| String::from_utf8_lossy(b).to_string())
        .map(|s| normalize_msg_id(&s))
        .unwrap_or_default();
    let references = parse_references(&raw_headers);

    Some(RawEnvelope { uid, folder: folder.to_string(), subject, from_name, from_addr,
        to_names, to_addrs, cc_addrs, date, date_ts, seen, flagged, has_attachments, is_draft,
        message_id, in_reply_to, references })
}

/// Normalize a single Message-Id: strip surrounding `<>` and whitespace.
fn normalize_msg_id(s: &str) -> String {
    s.trim().trim_start_matches('<').trim_end_matches('>').trim().to_string()
}

/// Extract a list of Message-Ids (e.g. from a References header value).
fn extract_msg_ids(value: &str) -> Vec<String> {
    let mut out = Vec::new();
    let mut current = String::new();
    let mut depth = 0i32;
    for ch in value.chars() {
        match ch {
            '<' => { depth += 1; current.clear(); }
            '>' => {
                if depth > 0 {
                    depth -= 1;
                    let id = current.trim().to_string();
                    if !id.is_empty() { out.push(id); }
                    current.clear();
                }
            }
            _ => { if depth > 0 { current.push(ch); } }
        }
    }
    out
}

/// Parse the References header into a list of normalized Message-Ids.
fn parse_references(raw_headers: &str) -> Vec<String> {
    let mut in_ref = false;
    let mut buf = String::new();
    for line in raw_headers.lines() {
        if line.to_ascii_lowercase().starts_with("references:") {
            in_ref = true;
            buf.push_str(line["references:".len()..].trim_start());
            continue;
        }
        if in_ref {
            // Continuation lines start with whitespace
            if line.starts_with(' ') || line.starts_with('\t') {
                buf.push(' ');
                buf.push_str(line.trim());
            } else {
                break;
            }
        }
    }
    extract_msg_ids(&buf)
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
        .fetch(&range, "(UID FLAGS ENVELOPE BODY.PEEK[HEADER.FIELDS (CONTENT-TYPE FROM REFERENCES)])")
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
pub(crate) async fn connect_tls(host: &str, port: u16, username: &str, password: &str)
    -> Result<async_imap::Session<async_native_tls::TlsStream<tokio_util::compat::Compat<tokio::net::TcpStream>>>, String>
{
    let tls = async_native_tls::TlsConnector::new();
    let tcp = tokio::net::TcpStream::connect((host, port))
        .await.map_err(|e| format!("TCP: {e}"))?;
    let tls_stream = tls.connect(host, tcp.compat())
        .await.map_err(|e| format!("TLS: {e}"))?;
    let client = async_imap::Client::new(tls_stream);
    client.login(username, password).await.map_err(|e| crate::session::friendly_login_error(e.0))
}

pub(crate) async fn connect_plain(host: &str, port: u16, username: &str, password: &str)
    -> Result<async_imap::Session<tokio_util::compat::Compat<tokio::net::TcpStream>>, String>
{
    let tcp = tokio::net::TcpStream::connect((host, port))
        .await.map_err(|e| format!("TCP: {e}"))?;
    let client = async_imap::Client::new(tcp.compat());
    client.login(username, password).await.map_err(|e| crate::session::friendly_login_error(e.0))
}

// ── Commands ──

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

pub(crate) async fn list_folders_impl<T>(session: &mut async_imap::Session<T>) -> Result<Vec<Folder>, String>
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
        // Extract Special-Use attribute
        let special_use = mailbox.attributes().iter()
            .find_map(|attr| {
                let s = format!("{:?}", attr);
                // async-imap represents attributes as NameAttribute variants
                if s.contains("Sent") { Some("\\Sent".to_string()) }
                else if s.contains("Trash") { Some("\\Trash".to_string()) }
                else if s.contains("Drafts") { Some("\\Drafts".to_string()) }
                else if s.contains("Junk") { Some("\\Junk".to_string()) }
                else if s.contains("Archive") { Some("\\Archive".to_string()) }
                else { None }
            })
            .unwrap_or_default();
        // INBOX is identified by name, not attribute
        let special_use = if name.eq_ignore_ascii_case("INBOX") { "\\Inbox".to_string() } else { special_use };
        folders.push(Folder { name, delimiter, unread, total, special_use });
    }
    Ok(folders)
}

pub async fn list_folders(
    host: String, port: u16, username: String, password: String, use_tls: bool,
) -> Result<Vec<Folder>, String> {
    connect(host, port, username, password, use_tls).await
}

pub async fn fetch_conversations(
    cache: &Cache,
    host: String, port: u16, username: String, password: String, use_tls: bool,
    user_email: String, limit: u32,
) -> Result<Vec<Conversation>, String> {
    let user_addr = user_email.to_lowercase();
    let key = account_key(&host, &username);

    // Our identity addresses come from the cached identity list; fall back to user_addr only.
    let mut our_addrs: Vec<String> = cache.load_identities(&key)
        .map(|ids| ids.into_iter().map(|i| i.email.to_lowercase()).collect::<Vec<_>>())
        .unwrap_or_default();
    if !our_addrs.iter().any(|a| a == &user_addr) {
        our_addrs.push(user_addr.clone());
    }

    let result = if use_tls {
        let mut session = connect_tls(&host, port, &username, &password).await?;
        let r = fetch_conversations_impl(&mut session, &user_addr, &our_addrs, limit).await;
        session.logout().await.ok();
        r
    } else {
        let mut session = connect_plain(&host, port, &username, &password).await?;
        let r = fetch_conversations_impl(&mut session, &user_addr, &our_addrs, limit).await;
        session.logout().await.ok();
        r
    };

    // Save to cache on success
    if let Ok(ref convs) = result {
        cache.save_conversations(&key, convs).ok();
    }

    result
}

pub(crate) async fn fetch_conversations_impl<T>(
    session: &mut async_imap::Session<T>,
    user_addr: &str,
    our_addrs: &[String],
    limit: u32,
) -> Result<Vec<Conversation>, String>
where
    T: futures::AsyncRead + futures::AsyncWrite + Unpin + Send + std::fmt::Debug,
{
    // Get folder list and detect Special-Use folders by attributes
    let mailboxes = session.list(Some(""), Some("*"))
        .await.map_err(|e| format!("LIST: {e}"))?;
    let collected: Vec<_> = mailboxes.try_collect::<Vec<_>>()
        .await.map_err(|e| format!("Collect: {e}"))?;

    let mut inbox_name = "INBOX".to_string();
    let mut sent_name: Option<String> = None;
    let mut drafts_name: Option<String> = None;

    for m in &collected {
        let name = m.name().to_string();
        let attrs: Vec<String> = m.attributes().iter().map(|a| format!("{:?}", a)).collect();
        let attrs_joined = attrs.join(" ");

        if name.eq_ignore_ascii_case("INBOX") {
            inbox_name = name;
        } else if attrs_joined.contains("Sent") {
            sent_name = Some(name);
        } else if attrs_joined.contains("Drafts") {
            drafts_name = Some(name);
        }
    }

    // Fallback: find Sent by name if no attribute
    if sent_name.is_none() {
        let folder_list: Vec<Folder> = collected.iter().map(|m| Folder {
            name: m.name().to_string(),
            delimiter: m.delimiter().unwrap_or("/").to_string(),
            unread: 0, total: 0, special_use: String::new(),
        }).collect();
        sent_name = find_sent_folder(&folder_list);
    }

    // Fetch INBOX
    let mut all_envelopes = fetch_folder_envelopes(session, &inbox_name, limit).await?;

    // Fetch Sent
    if let Some(ref sname) = sent_name {
        if let Ok(sent_envs) = fetch_folder_envelopes(session, sname, limit / 2).await {
            all_envelopes.extend(sent_envs);
        }
    }

    // Fetch Drafts
    if let Some(ref dname) = drafts_name {
        if let Ok(draft_envs) = fetch_folder_envelopes(session, dname, limit / 4).await {
            all_envelopes.extend(draft_envs);
        }
    }

    // Group into conversations by (my_id, sorted_counterparts). 1:1 mail produces
    // a single-counterpart key — collapsing to the historical "{my_id}|{cp}" id
    // string so pinned-conversation localStorage stays valid. Multi-recipient
    // mail (To+Cc has >1 non-self address) keeps the full participant set in
    // the key so each unique group is its own row.
    let is_ours = |a: &str| {
        let lc = a.to_lowercase();
        our_addrs.iter().any(|o| *o == lc)
    };

    let mut conv_map: HashMap<(String, String), (Vec<String>, Vec<RawEnvelope>)> = HashMap::new();
    for env in all_envelopes {
        let from_lc = env.from_addr.to_lowercase();

        // Pick myID: outgoing → sender, incoming → first of our addrs in To/Cc.
        let my_id = if is_ours(&from_lc) {
            from_lc.clone()
        } else {
            env.to_addrs.iter().chain(env.cc_addrs.iter())
                .map(|a| a.to_lowercase())
                .find(|a| is_ours(a))
                .unwrap_or_else(|| user_addr.to_string())
        };

        // Counterparts = unique sorted (from ∪ to ∪ cc) minus our identities.
        let mut cps: Vec<String> = Vec::new();
        let mut seen: std::collections::HashSet<String> = std::collections::HashSet::new();
        let mut push = |a: &str| {
            let lc = a.to_lowercase();
            if lc.is_empty() || is_ours(&lc) { return None; }
            if seen.insert(lc.clone()) { Some(lc) } else { None }
        };
        if let Some(v) = push(&from_lc) { cps.push(v); }
        for a in env.to_addrs.iter().chain(env.cc_addrs.iter()) {
            if let Some(v) = push(a) { cps.push(v); }
        }
        if cps.is_empty() { continue; }
        cps.sort();

        let cps_key = cps.join(",");
        conv_map.entry((my_id, cps_key))
            .or_insert_with(|| (cps.clone(), Vec::new()))
            .1
            .push(env);
    }

    let mut conversations: Vec<Conversation> = Vec::new();

    for ((my_id, cps_key), (cp_addrs, mut msgs)) in conv_map {
        msgs.sort_by_key(|m| m.date_ts);

        // Same physical message can appear in multiple folders (e.g. self-mail lands in
        // both Sent and INBOX when both endpoints are local). Dedup by Message-Id, keeping
        // the Sent copy when there's a choice — that's the canonical "I wrote this" view.
        if msgs.iter().any(|m| !m.message_id.is_empty()) {
            let sent_lc = sent_name.as_ref().map(|s| s.to_lowercase());
            let prefer_folder = |folder: &str| -> u8 {
                let lc = folder.to_lowercase();
                if Some(lc.as_str()) == sent_lc.as_deref() { 0 } else { 1 }
            };
            let mut seen: HashMap<String, usize> = HashMap::new();
            let mut keep = vec![true; msgs.len()];
            for (i, m) in msgs.iter().enumerate() {
                if m.message_id.is_empty() { continue; }
                if let Some(&j) = seen.get(&m.message_id) {
                    if prefer_folder(&m.folder) < prefer_folder(&msgs[j].folder) {
                        keep[j] = false;
                        seen.insert(m.message_id.clone(), i);
                    } else {
                        keep[i] = false;
                    }
                } else {
                    seen.insert(m.message_id.clone(), i);
                }
            }
            let mut iter = keep.into_iter();
            msgs.retain(|_| iter.next().unwrap_or(true));
        }

        // Display name for each counterpart: scan the thread for FROM names
        // (when they wrote to us) or To/Cc names (when we wrote to them).
        // Drop names that look like raw addresses — those are useless labels.
        let name_for = |addr: &str| -> String {
            msgs.iter()
                .find(|m| m.from_addr.eq_ignore_ascii_case(addr) && !m.from_name.is_empty())
                .map(|m| clean_display_name(&m.from_name))
                .filter(|n| !n.is_empty() && !n.contains('@'))
                .or_else(|| msgs.iter().flat_map(|m|
                    m.to_addrs.iter().zip(m.to_names.iter())
                        .filter(|(a, _)| a.eq_ignore_ascii_case(addr))
                        .map(|(_, n)| clean_display_name(n))
                ).find(|n| !n.is_empty() && !n.contains('@')))
                .unwrap_or_default()
        };

        let counterparts: Vec<ContactInfo> = cp_addrs.iter()
            .map(|a| ContactInfo { name: name_for(a), addr: a.clone() })
            .collect();

        // Separate drafts from regular messages.
        let draft = msgs.iter().rev().find(|m| m.is_draft)
            .map(|m| MessageRef { folder: m.folder.clone(), uid: m.uid });
        let regular_msgs: Vec<&RawEnvelope> = msgs.iter().filter(|m| !m.is_draft).collect();

        // Drafts alone don't make a conversation — see HandleDesktopConversations
        // in the Go server for the matching server-side rule.
        if regular_msgs.is_empty() { continue; }

        let first = regular_msgs.first().copied().unwrap();
        let last = regular_msgs.last().copied().unwrap();

        let is_group = cp_addrs.len() > 1;

        // 1:1: label = counterpart name (or addr).
        // Group: label = first message subject + " (N чел)" where N counts all
        // human participants (counterparts + me).
        let (id, label) = if is_group {
            let subject = if first.subject.trim().is_empty() {
                "(без темы)".to_string()
            } else {
                first.subject.clone()
            };
            (
                format!("{}|group:{}", my_id, cps_key),
                format!("{} ({} чел)", subject, cp_addrs.len() + 1),
            )
        } else {
            let cp_name = &counterparts[0].name;
            let label = if cp_name.is_empty() { cp_addrs[0].clone() } else { cp_name.clone() };
            (format!("{}|{}", my_id, cps_key), label)
        };

        // Avatar seed: for both 1:1 and groups, use the first counterpart for now.
        let avatar_seed = &cp_addrs[0];

        conversations.push(Conversation {
            id,
            label,
            avatar_hash: gravatar_hash(avatar_seed),
            received_by: my_id,
            counterparts,
            is_group,
            last_date: last.date.clone(),
            last_date_ts: last.date_ts,
            last_subject: last.subject.clone(),
            unread_count: regular_msgs.iter().filter(|m| !m.seen).count() as u32,
            total_count: regular_msgs.len() as u32,
            messages: regular_msgs.iter().map(|m| MessageRef { folder: m.folder.clone(), uid: m.uid }).collect(),
            draft,
        });
    }

    conversations.sort_by(|a, b| b.last_date_ts.cmp(&a.last_date_ts));
    Ok(conversations)
}

pub async fn fetch_conversation_messages(
    cache: &Cache,
    host: String, port: u16, username: String, password: String, use_tls: bool,
    user_email: String, messages: Vec<MessageRef>,
) -> Result<Vec<MessageBody>, String> {
    let user_addr = user_email.to_lowercase();
    let key = account_key(&host, &username);

    let mut our_addrs: Vec<String> = cache.load_identities(&key)
        .map(|ids| ids.into_iter().map(|i| i.email.to_lowercase()).collect::<Vec<_>>())
        .unwrap_or_default();
    if !our_addrs.iter().any(|a| a == &user_addr) {
        our_addrs.push(user_addr.clone());
    }

    let result = if use_tls {
        let mut session = connect_tls(&host, port, &username, &password).await?;
        let r = fetch_bodies_impl(&mut session, &our_addrs, &messages).await;
        session.logout().await.ok();
        r
    } else {
        let mut session = connect_plain(&host, port, &username, &password).await?;
        let r = fetch_bodies_impl(&mut session, &our_addrs, &messages).await;
        session.logout().await.ok();
        r
    };

    // Cache bodies
    if let Ok(ref bodies) = result {
        cache.save_message_bodies(&key, bodies).ok();
    }

    result
}

pub(crate) async fn fetch_bodies_impl<T>(
    session: &mut async_imap::Session<T>,
    our_addrs: &[String],
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

            let message_id = parsed.headers.iter()
                .find(|h| h.get_key().eq_ignore_ascii_case("message-id"))
                .map(|h| normalize_msg_id(&h.get_value())).unwrap_or_default();
            let in_reply_to = parsed.headers.iter()
                .find(|h| h.get_key().eq_ignore_ascii_case("in-reply-to"))
                .map(|h| normalize_msg_id(&h.get_value())).unwrap_or_default();
            let references = parsed.headers.iter()
                .find(|h| h.get_key().eq_ignore_ascii_case("references"))
                .map(|h| extract_msg_ids(&h.get_value())).unwrap_or_default();

            let mut html = None;
            let mut text = None;
            let mut attachments = Vec::new();
            walk_parts(&parsed, &mut html, &mut text, &mut attachments, &mut 0);

            let from_addr_lc = from_addr.to_lowercase();
            let is_outgoing = our_addrs.iter().any(|a| a == &from_addr_lc);

            bodies.push(MessageBody {
                uid, folder: folder.clone(), subject, from, from_addr: from_addr.clone(),
                to, cc, date, date_ts, html, text, attachments,
                is_outgoing,
                message_id, in_reply_to, references,
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

pub(crate) async fn search_impl<T>(
    session: &mut async_imap::Session<T>,
    user_addr: &str,
    query: &str,
) -> Result<Vec<MessageEnvelope>, String>
where
    T: futures::AsyncRead + futures::AsyncWrite + Unpin + Send + std::fmt::Debug,
{
    // Find INBOX and the Sent folder via LIST attributes; search both.
    let mailboxes = session.list(Some(""), Some("*"))
        .await.map_err(|e| format!("LIST: {e}"))?;
    let collected: Vec<_> = mailboxes.try_collect::<Vec<_>>()
        .await.map_err(|e| format!("Collect: {e}"))?;
    let mut sent_name: Option<String> = None;
    for m in &collected {
        let attrs: Vec<String> = m.attributes().iter().map(|a| format!("{:?}", a)).collect();
        if attrs.join(" ").contains("Sent") { sent_name = Some(m.name().to_string()); break; }
    }
    let folder_list: Vec<Folder> = collected.iter().map(|m| Folder {
        name: m.name().to_string(),
        delimiter: m.delimiter().unwrap_or("/").to_string(),
        unread: 0, total: 0, special_use: String::new(),
    }).collect();
    if sent_name.is_none() { sent_name = find_sent_folder(&folder_list); }

    let mut envelopes: Vec<MessageEnvelope> = Vec::new();
    let folders_to_search: Vec<String> = std::iter::once("INBOX".to_string())
        .chain(sent_name.into_iter())
        .collect();

    let escaped: String = query.chars()
        .map(|c| if c == '\\' || c == '"' { format!("\\{}", c) } else { c.to_string() })
        .collect();

    for folder in &folders_to_search {
        if session.select(folder).await.is_err() { continue; }
        // Server's Meilisearch index covers only subject+body. We pass both criteria;
        // the server's `extractTextQuery` joins them into a single full-text query.
        // CHARSET UTF-8 lets non-ASCII text match; fall back to a bare query for
        // servers that reject the CHARSET argument.
        let crit = format!("OR SUBJECT \"{}\" BODY \"{}\"", escaped, escaped);
        let with_charset = format!("CHARSET UTF-8 {}", crit);
        // UID SEARCH (vs plain SEARCH) so the server returns UIDs directly — sequence
        // numbers depend on the SELECTed mailbox state and our server currently mis-
        // computes them, which made FETCH-by-seqno return the wrong message.
        let uids = match session.uid_search(&with_charset).await {
            Ok(u) => u,
            Err(_) => match session.uid_search(&crit).await {
                Ok(u) => u,
                Err(_) => continue,
            },
        };

        let mut uid_vec: Vec<u32> = uids.into_iter().collect();
        uid_vec.sort_unstable();
        uid_vec.reverse();
        let uid_list: Vec<String> = uid_vec.iter().take(30).map(|u| u.to_string()).collect();
        let uid_set = uid_list.join(",");
        let messages = match session.uid_fetch(&uid_set, "(UID FLAGS ENVELOPE)").await {
            Ok(m) => m,
            Err(_) => continue,
        };
        let collected: Vec<_> = messages.try_collect::<Vec<_>>()
            .await.unwrap_or_default();

        for msg in &collected {
            if let Some(raw) = extract_envelope(msg, folder) {
                let is_outgoing = raw.from_addr == user_addr;
                envelopes.push(MessageEnvelope {
                    uid: raw.uid, folder: raw.folder, subject: raw.subject,
                    from: raw.from_name, from_addr: raw.from_addr,
                    to: raw.to_names, to_addrs: raw.to_addrs, cc_addrs: raw.cc_addrs,
                    date: raw.date, date_ts: raw.date_ts,
                    seen: raw.seen, flagged: raw.flagged,
                    has_attachments: raw.has_attachments, is_outgoing,
                    message_id: raw.message_id, in_reply_to: raw.in_reply_to, references: raw.references,
                });
            }
        }
    }

    envelopes.sort_by(|a, b| b.date_ts.cmp(&a.date_ts));
    envelopes.truncate(30);
    Ok(envelopes)
}

pub(crate) async fn store_flags_impl<T>(
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

/// Apply flag changes to many messages in a single IMAP session — avoids the
/// thunder-on-handshake of one TLS connection per message.
pub async fn set_flags_batch(
    host: String, port: u16, username: String, password: String, use_tls: bool,
    messages: Vec<MessageRef>, flags: String, add: bool,
) -> Result<(), String> {
    if messages.is_empty() { return Ok(()); }
    if use_tls {
        let mut session = connect_tls(&host, port, &username, &password).await?;
        let r = store_flags_batch_impl(&mut session, &messages, &flags, add).await;
        session.logout().await.ok();
        r
    } else {
        let mut session = connect_plain(&host, port, &username, &password).await?;
        let r = store_flags_batch_impl(&mut session, &messages, &flags, add).await;
        session.logout().await.ok();
        r
    }
}

pub(crate) async fn store_flags_batch_impl<T>(
    session: &mut async_imap::Session<T>,
    messages: &[MessageRef], flags: &str, add: bool,
) -> Result<(), String>
where
    T: futures::AsyncRead + futures::AsyncWrite + Unpin + Send + std::fmt::Debug,
{
    let mut by_folder: HashMap<String, Vec<u32>> = HashMap::new();
    for mr in messages {
        if mr.uid == 0 { continue; }
        by_folder.entry(mr.folder.clone()).or_default().push(mr.uid);
    }
    let op = if add { "+FLAGS" } else { "-FLAGS" };
    for (folder, uids) in by_folder {
        if let Err(e) = session.select(&folder).await {
            log::warn!("set_flags_batch: SELECT {folder} failed: {e}");
            continue;
        }
        let uid_set = uids.iter().map(|u| u.to_string()).collect::<Vec<_>>().join(",");
        match session.uid_store(uid_set, format!("{op} ({flags})")).await {
            Ok(stream) => { let _: Vec<_> = stream.try_collect::<Vec<_>>().await.unwrap_or_default(); }
            Err(e) => log::warn!("set_flags_batch: STORE in {folder}: {e}"),
        }
    }
    Ok(())
}

/// Soft-delete via IMAP: STORE +FLAGS (\Deleted) on the UIDs in each folder,
/// then EXPUNGE that folder. Folders are processed sequentially so a partial
/// failure on one mailbox doesn't poison the rest.
pub(crate) async fn delete_messages_impl<T>(
    session: &mut async_imap::Session<T>,
    messages: &[MessageRef],
) -> Result<(), String>
where
    T: futures::AsyncRead + futures::AsyncWrite + Unpin + Send + std::fmt::Debug,
{
    let mut by_folder: HashMap<String, Vec<u32>> = HashMap::new();
    for mr in messages {
        if mr.uid == 0 { continue; }
        by_folder.entry(mr.folder.clone()).or_default().push(mr.uid);
    }
    for (folder, uids) in by_folder {
        if let Err(e) = session.select(&folder).await {
            log::warn!("delete_messages: SELECT {folder} failed: {e}");
            continue;
        }
        let uid_set = uids.iter().map(|u| u.to_string()).collect::<Vec<_>>().join(",");
        match session.uid_store(uid_set, "+FLAGS (\\Deleted)").await {
            Ok(stream) => { let _: Vec<_> = stream.try_collect::<Vec<_>>().await.unwrap_or_default(); }
            Err(e) => { log::warn!("delete_messages: STORE in {folder}: {e}"); continue; }
        }
        match session.expunge().await {
            Ok(stream) => { let _: Vec<_> = stream.try_collect::<Vec<_>>().await.unwrap_or_default(); }
            Err(e) => log::warn!("delete_messages: EXPUNGE in {folder}: {e}"),
        }
    }
    Ok(())
}

/// Fetch raw RFC-822 source of a single message.
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

pub(crate) async fn fetch_source_impl<T>(
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

pub(crate) fn open_in_default_app_pub(path: &std::path::Path) {
    open_in_default_app(path);
}

pub(crate) fn sanitize_filename_pub(name: &str) -> String {
    sanitize_filename(name)
}

pub(crate) fn unique_path_pub(dir: &std::path::Path, filename: &str) -> std::path::PathBuf {
    unique_path(dir, filename)
}

fn open_in_default_app(path: &std::path::Path) {
    let path_str = path.to_string_lossy();
    eprintln!("[attachment] opening: {path_str}");

    // PowerShell Invoke-Item is the exact equivalent of double-clicking in
    // Explorer — it calls ShellExecuteW with the registered default verb.
    // rundll32/url.dll only handles URL schemes, not arbitrary file types.
    #[cfg(target_os = "windows")]
    let result = std::process::Command::new("powershell")
        .args([
            "-NoProfile",
            "-NonInteractive",
            "-Command",
            &format!("Invoke-Item '{}'", path_str.replace('\'', "''")),
        ])
        .spawn();

    #[cfg(target_os = "macos")]
    let result = std::process::Command::new("open").arg(path).spawn();

    // On Linux prefer `xdg-open`. Detach by spawning it: if no default
    // handler is registered, xdg-open opens its own MIME-association
    // chooser (which is the "choose app" dialog the user expects when
    // there's nothing set). If even that fails we fall back to `gio
    // open` and finally just log the failure — there's nowhere good
    // to surface it from this synchronous helper.
    #[cfg(target_os = "linux")]
    let result = std::process::Command::new("xdg-open")
        .arg(path)
        .spawn()
        .or_else(|_| std::process::Command::new("gio").args(["open", &path_str]).spawn());

    match result {
        Ok(_) => {}
        Err(e) => eprintln!("[attachment] open failed: {e}"),
    }
}

pub(crate) async fn fetch_raw_message<T>(
    session: &mut async_imap::Session<T>, folder: &str, uid: u32,
) -> Result<Vec<u8>, String>
where
    T: futures::AsyncRead + futures::AsyncWrite + Unpin + Send + std::fmt::Debug,
{
    session.select(folder).await.map_err(|e| format!("SELECT {folder}: {e}"))?;
    let fetched = session.uid_fetch(uid.to_string(), "BODY.PEEK[]")
        .await.map_err(|e| format!("FETCH: {e}"))?;
    let msgs: Vec<_> = fetched.try_collect().await.map_err(|e| format!("Collect: {e}"))?;
    let msg = msgs.first().ok_or("Message not found")?;
    let body = msg.body().ok_or("Empty body")?;
    Ok(body.to_vec())
}

pub(crate) fn find_attachment(
    part: &mailparse::ParsedMail,
    target: usize,
    idx: &mut usize,
) -> Option<(Vec<u8>, String)> {
    if part.subparts.is_empty() {
        let disp = part.get_content_disposition();
        if matches!(disp.disposition, mailparse::DispositionType::Attachment) {
            if *idx == target {
                let bytes = part.get_body_raw().ok()?;
                let mime = part.ctype.mimetype.clone();
                return Some((bytes, mime));
            }
            *idx += 1;
        }
        return None;
    }
    for sub in &part.subparts {
        if let Some(b) = find_attachment(sub, target, idx) { return Some(b); }
    }
    None
}

fn sanitize_filename(name: &str) -> String {
    let cleaned: String = name.chars()
        .map(|c| if matches!(c, '/' | '\\' | ':' | '*' | '?' | '"' | '<' | '>' | '|' | '\0') { '_' } else { c })
        .collect();
    let trimmed = cleaned.trim().trim_matches('.');
    if trimmed.is_empty() { "attachment".into() } else { trimmed.to_string() }
}

fn unique_path(dir: &std::path::Path, filename: &str) -> std::path::PathBuf {
    let candidate = dir.join(filename);
    if !candidate.exists() { return candidate; }
    let (stem, ext) = match filename.rsplit_once('.') {
        Some((s, e)) if !s.is_empty() => (s.to_string(), format!(".{e}")),
        _ => (filename.to_string(), String::new()),
    };
    for n in 1..1000 {
        let try_path = dir.join(format!("{stem} ({n}){ext}"));
        if !try_path.exists() { return try_path; }
    }
    candidate
}

// ── Contacts ──

pub async fn search_contacts(
    cache: &Cache,
    host: String, username: String, query: String, limit: u32,
) -> Result<Vec<Contact>, String> {
    let key = account_key(&host, &username);
    cache.search_contacts(&key, &query, limit)
}

// ── Cache commands ──

/// Load conversations from local SQLite cache (instant).
pub async fn load_cached_conversations(
    cache: &Cache,
    host: String, username: String,
) -> Result<Vec<Conversation>, String> {
    let key = account_key(&host, &username);
    cache.load_conversations(&key)
}

/// Load message bodies from local cache (instant).
pub async fn load_cached_messages(
    cache: &Cache,
    host: String, username: String, messages: Vec<MessageRef>,
) -> Result<Vec<MessageBody>, String> {
    let key = account_key(&host, &username);
    cache.load_message_bodies(&key, &messages)
}

/// Fetch Gravatar avatar: cache-first, then HTTP, cache for 7 days.
/// Returns base64-encoded PNG or empty string if no avatar.
/// Check CAPABILITY for METADATA, fetch identities via GETMETADATA.
/// Returns empty vec if not our server.
pub async fn fetch_identities(
    cache: &Cache,
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

/// Extract the first top-level JSON array `[ ... ]` from a string, respecting
/// string literals and escapes — i.e. `[`, `]`, `{`, `}` inside a JSON string don't
/// affect bracket counting. Returns `None` if no balanced array is found.
fn extract_top_level_array(s: &str) -> Option<String> {
    let bytes = s.as_bytes();
    let start = bytes.iter().position(|&b| b == b'[')?;
    let mut depth = 0i32;
    let mut in_str = false;
    let mut esc = false;
    for (i, &b) in bytes.iter().enumerate().skip(start) {
        if in_str {
            if esc { esc = false; }
            else if b == b'\\' { esc = true; }
            else if b == b'"' { in_str = false; }
            continue;
        }
        match b {
            b'"' => in_str = true,
            b'[' => depth += 1,
            b']' => {
                depth -= 1;
                if depth == 0 {
                    return std::str::from_utf8(&bytes[start..=i]).ok().map(|s| s.to_string());
                }
            }
            _ => {}
        }
    }
    None
}

pub(crate) async fn fetch_identities_impl<T>(
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

    // 3. Accumulate every raw response chunk (literal-form values can span multiple
    // reads), then extract the JSON array using bracket-matching that respects
    // strings and escapes.
    let mut buf = Vec::<u8>::new();
    loop {
        match session.read_response().await {
            Some(Ok(resp)) => {
                buf.extend_from_slice(resp.borrow_owner());
                if resp.request_id().is_some() { break; }
            }
            Some(Err(e)) => return Err(format!("GETMETADATA read: {e}")),
            None => break,
        }
    }

    let raw = String::from_utf8_lossy(&buf);
    let json_data = match extract_top_level_array(&raw) {
        Some(s) => s,
        None => {
            log::warn!("GETMETADATA: no JSON array in response (len={})", raw.len());
            return Ok(vec![]);
        }
    };

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

pub async fn start_watching(
    notifier: Notifier,
    pool: &SessionPool,
    host: String, port: u16, username: String, password: String, use_tls: bool,
    user_email: String,
) -> Result<(), String> {
    let creds = Credentials { host, port, username, password, use_tls, user_email };
    pool.start_idle(notifier, creds).await;
    Ok(())
}
