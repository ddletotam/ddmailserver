use std::sync::Arc;

use serde::{Deserialize, Serialize};

use crate::cache::Cache;
use crate::imap;
use crate::imap_provider::ImapProvider;
use crate::native_provider::NativeProvider;
use crate::provider::MailProvider;
use crate::registry::ProviderRegistry;
use crate::session::SessionPool;
use crate::types::*;

// ── Server detection ──

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct DetectResult {
    pub server_url: String,
    pub api_base: String,
    pub ws_path: String,
    pub features: Vec<String>,
}

/// Probe a host for DDMail native protocol support.
/// Probes https and http in parallel (3s timeout) — returns first success.
#[tauri::command]
pub async fn detect_server(host: String) -> Result<Option<DetectResult>, String> {
    let client = reqwest::Client::builder()
        .timeout(std::time::Duration::from_secs(3))
        .build()
        .map_err(|e| e.to_string())?;

    async fn try_scheme(
        client: &reqwest::Client,
        host: &str,
        scheme: &str,
    ) -> Option<DetectResult> {
        let url = format!("{scheme}://{host}/.well-known/ddmail");
        let resp = client.get(&url).send().await.ok()?;
        if !resp.status().is_success() {
            return None;
        }
        let info: serde_json::Value = resp.json().await.ok()?;
        if info.get("ddmail").and_then(|v| v.as_bool()) != Some(true) {
            return None;
        }
        Some(DetectResult {
            server_url: format!("{scheme}://{host}"),
            api_base: info["api_base"]
                .as_str()
                .unwrap_or("/api/desktop/v1")
                .to_string(),
            ws_path: info["ws_path"]
                .as_str()
                .unwrap_or("/api/desktop/v1/ws")
                .to_string(),
            features: info["features"]
                .as_array()
                .map(|arr| {
                    arr.iter()
                        .filter_map(|v| v.as_str().map(String::from))
                        .collect()
                })
                .unwrap_or_default(),
        })
    }

    // Probe HTTPS and HTTP in parallel, prefer HTTPS
    let (https_result, http_result) = tokio::join!(
        try_scheme(&client, &host, "https"),
        try_scheme(&client, &host, "http"),
    );

    Ok(https_result.or(http_result))
}

// ── Account activation ──

/// Authenticate with the DDMail server and get a JWT token.
#[tauri::command]
pub async fn native_login(
    server_url: String,
    username: String,
    password: String,
) -> Result<String, String> {
    let client = reqwest::Client::builder()
        .timeout(std::time::Duration::from_secs(10))
        .build()
        .map_err(|e| e.to_string())?;

    let resp = client
        .post(format!("{server_url}/api/desktop/v1/auth/login"))
        .json(&serde_json::json!({
            "username": username,
            "password": password,
        }))
        .send()
        .await
        .map_err(|e| format!("Login request failed: {e}"))?;

    if !resp.status().is_success() {
        let body = resp.text().await.unwrap_or_default();
        return Err(format!("Login failed: {body}"));
    }

    let data: serde_json::Value = resp.json().await.map_err(|e| format!("Parse: {e}"))?;
    data.get("token")
        .and_then(|v| v.as_str())
        .map(String::from)
        .ok_or_else(|| "No token in response".to_string())
}

/// Activate an account — creates the right provider (IMAP or Native) and
/// registers it in the ProviderRegistry. Subsequent v2 commands use account_id.
#[tauri::command]
pub async fn activate_account(
    app: tauri::AppHandle,
    registry: tauri::State<'_, ProviderRegistry>,
    pool: tauri::State<'_, SessionPool>,
    account_id: String,
    // IMAP credentials (always provided)
    imap_host: String,
    imap_port: u16,
    username: String,
    password: String,
    use_tls: bool,
    email: String,
    // Native mode (optional — set if detect_server found DDMail)
    native_url: Option<String>,
    native_token: Option<String>,
) -> Result<String, String> {
    let provider: Arc<dyn MailProvider> =
        if let (Some(url), Some(token)) = (native_url, native_token) {
            Arc::new(NativeProvider::new(
                url,
                token,
                email,
                Some(app.clone()),
                account_id.clone(),
            ))
        } else {
            Arc::new(ImapProvider {
                host: imap_host,
                port: imap_port,
                username,
                password,
                use_tls,
                user_email: email,
                pool: pool.clone_inner(),
            })
        };

    let provider_type = provider.provider_type().to_string();
    registry.register(&account_id, provider).await;
    Ok(provider_type)
}

// ── v2 commands (dispatch through registry) ──

async fn get_provider(
    registry: &ProviderRegistry,
    account_id: &str,
) -> Result<Arc<dyn MailProvider>, String> {
    registry
        .get(account_id)
        .await
        .ok_or_else(|| "Account not activated — call activate_account first".to_string())
}

/// Build the list of "our" email addresses for conversation threading.
fn resolve_our_addrs(cache: &Cache, host: &str, username: &str, user_email: &str) -> Vec<String> {
    let key = imap::account_key(host, username);
    let mut addrs: Vec<String> = cache
        .load_identities(&key)
        .map(|ids| ids.into_iter().map(|i| i.email.to_lowercase()).collect())
        .unwrap_or_default();
    let user_addr = user_email.to_lowercase();
    if !addrs.iter().any(|a| a == &user_addr) {
        addrs.push(user_addr);
    }
    addrs
}

#[tauri::command]
pub async fn v2_list_folders(
    registry: tauri::State<'_, ProviderRegistry>,
    account_id: String,
) -> Result<Vec<Folder>, String> {
    get_provider(&registry, &account_id).await?.list_folders().await
}

#[tauri::command]
pub async fn v2_fetch_conversations(
    registry: tauri::State<'_, ProviderRegistry>,
    cache: tauri::State<'_, std::sync::Arc<Cache>>,
    account_id: String,
    host: String,
    username: String,
    user_email: String,
    limit: u32,
) -> Result<Vec<Conversation>, String> {
    let provider = get_provider(&registry, &account_id).await?;
    let key = imap::account_key(&host, &username);
    let our_addrs = resolve_our_addrs(&cache, &host, &username, &user_email);

    let result = provider.fetch_conversations(&our_addrs, limit).await;
    if let Ok(ref convs) = result {
        cache.save_conversations(&key, convs).ok();
    }
    result
}

#[tauri::command]
pub async fn v2_fetch_conversation_messages(
    registry: tauri::State<'_, ProviderRegistry>,
    cache: tauri::State<'_, std::sync::Arc<Cache>>,
    account_id: String,
    host: String,
    username: String,
    user_email: String,
    messages: Vec<MessageRef>,
) -> Result<Vec<MessageBody>, String> {
    let provider = get_provider(&registry, &account_id).await?;
    let key = imap::account_key(&host, &username);
    let our_addrs = resolve_our_addrs(&cache, &host, &username, &user_email);

    let result = provider
        .fetch_conversation_messages(&our_addrs, &messages)
        .await;
    if let Ok(ref bodies) = result {
        cache.save_message_bodies(&key, bodies).ok();
    }
    result
}

#[tauri::command]
pub async fn v2_search_messages(
    registry: tauri::State<'_, ProviderRegistry>,
    account_id: String,
    user_email: String,
    query: String,
) -> Result<Vec<MessageEnvelope>, String> {
    get_provider(&registry, &account_id)
        .await?
        .search_messages(&user_email, &query)
        .await
}

#[tauri::command]
pub async fn v2_set_flags(
    registry: tauri::State<'_, ProviderRegistry>,
    account_id: String,
    folder: String,
    uid: u32,
    flags: String,
    add: bool,
) -> Result<(), String> {
    get_provider(&registry, &account_id)
        .await?
        .set_flags(&folder, uid, &flags, add)
        .await
}

#[tauri::command]
pub async fn v2_set_flags_batch(
    registry: tauri::State<'_, ProviderRegistry>,
    account_id: String,
    messages: Vec<MessageRef>,
    flags: String,
    add: bool,
) -> Result<(), String> {
    get_provider(&registry, &account_id)
        .await?
        .set_flags_batch(&messages, &flags, add)
        .await
}

#[tauri::command]
pub async fn v2_delete_messages(
    registry: tauri::State<'_, ProviderRegistry>,
    account_id: String,
    messages: Vec<MessageRef>,
) -> Result<(), String> {
    get_provider(&registry, &account_id)
        .await?
        .delete_messages(&messages)
        .await
}

#[tauri::command]
pub async fn v2_mark_spam_by_domain(
    registry: tauri::State<'_, ProviderRegistry>,
    account_id: String,
    domain: String,
    messages: Vec<MessageRef>,
) -> Result<(), String> {
    get_provider(&registry, &account_id)
        .await?
        .mark_spam_by_domain(&domain, &messages)
        .await
}

#[tauri::command]
pub async fn v2_blacklist_and_purge(
    registry: tauri::State<'_, ProviderRegistry>,
    account_id: String,
    domain: String,
    address: String,
) -> Result<i64, String> {
    get_provider(&registry, &account_id)
        .await?
        .blacklist_and_purge(&domain, &address)
        .await
}

#[tauri::command]
pub async fn v2_fetch_inline_part(
    registry: tauri::State<'_, ProviderRegistry>,
    account_id: String,
    message_id: u32,
    content_id: String,
) -> Result<InlinePart, String> {
    get_provider(&registry, &account_id)
        .await?
        .fetch_inline_part(message_id, &content_id)
        .await
}

#[tauri::command]
pub async fn v2_fetch_message_source(
    registry: tauri::State<'_, ProviderRegistry>,
    account_id: String,
    folder: String,
    uid: u32,
) -> Result<String, String> {
    get_provider(&registry, &account_id)
        .await?
        .fetch_message_source(&folder, uid)
        .await
}

#[tauri::command]
pub async fn v2_fetch_identities(
    registry: tauri::State<'_, ProviderRegistry>,
    cache: tauri::State<'_, std::sync::Arc<Cache>>,
    account_id: String,
    host: String,
    username: String,
) -> Result<Vec<Identity>, String> {
    let provider = get_provider(&registry, &account_id).await?;
    let key = imap::account_key(&host, &username);

    match provider.fetch_identities().await {
        Ok(identities) => {
            if !identities.is_empty() {
                cache.save_identities(&key, &identities).ok();
            }
            Ok(identities)
        }
        Err(_) => cache.load_identities(&key),
    }
}

#[tauri::command]
pub async fn v2_send_message(
    registry: tauri::State<'_, ProviderRegistry>,
    account_id: String,
    smtp_host: String,
    smtp_port: u16,
    mut message: OutgoingMessage,
) -> Result<String, String> {
    // Resolve filesystem paths into self-contained blobs once, here, so each
    // provider receives a fully-formed message and never touches the disk.
    // JS only knows the picked paths; turning them into bytes lives at the
    // command boundary.
    for path_str in std::mem::take(&mut message.attachment_paths) {
        message.attachments.push(read_attachment(&path_str, None)?);
    }
    for inline in std::mem::take(&mut message.inline_paths) {
        message.attachments.push(read_attachment(&inline.path, Some(inline.content_id))?);
    }

    get_provider(&registry, &account_id)
        .await?
        .send_message(&smtp_host, smtp_port, &message)
        .await
}

fn read_attachment(path_str: &str, content_id: Option<String>) -> Result<OutgoingAttachment, String> {
    let path = std::path::Path::new(path_str);
    let content = std::fs::read(path)
        .map_err(|e| format!("read attachment {path_str}: {e}"))?;
    let filename = path
        .file_name()
        .map(|n| n.to_string_lossy().to_string())
        .unwrap_or_else(|| "attachment".into());
    Ok(OutgoingAttachment {
        mime_type: guess_mime(&filename).to_string(),
        filename,
        content,
        content_id,
    })
}

/// MIME-type lookup by filename extension. Lives in commands.rs so both the
/// path-resolution above and any future ad-hoc inline-image guessing can share
/// it without depending on smtp.rs.
fn guess_mime(filename: &str) -> &'static str {
    let lower = filename.to_lowercase();
    let ext = lower.rsplit_once('.').map(|(_, e)| e).unwrap_or("");
    match ext {
        "pdf" => "application/pdf",
        "png" => "image/png",
        "jpg" | "jpeg" => "image/jpeg",
        "gif" => "image/gif",
        "webp" => "image/webp",
        "svg" => "image/svg+xml",
        "txt" | "log" => "text/plain",
        "csv" => "text/csv",
        "html" | "htm" => "text/html",
        "json" => "application/json",
        "xml" => "application/xml",
        "zip" => "application/zip",
        "doc" => "application/msword",
        "docx" => "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
        "xls" => "application/vnd.ms-excel",
        "xlsx" => "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
        "ppt" => "application/vnd.ms-powerpoint",
        "pptx" => "application/vnd.openxmlformats-officedocument.presentationml.presentation",
        _ => "application/octet-stream",
    }
}

#[tauri::command]
pub async fn v2_start_watching(
    registry: tauri::State<'_, ProviderRegistry>,
    app: tauri::AppHandle,
    account_id: String,
) -> Result<(), String> {
    get_provider(&registry, &account_id)
        .await?
        .start_watching(app)
        .await
}

/// Fetch an avatar via the active provider with a thin Tauri-side cache.
/// Returns `{data, mime}` — empty `data` means no source had anything and
/// the caller should render an initial bubble. MIME is required so the
/// frontend can put it in a valid `data:` URL (image/* doesn't render in
/// Chromium).
#[derive(serde::Serialize)]
pub struct AvatarResult {
    pub data: String, // base64
    pub mime: String,
}

#[tauri::command]
pub async fn v2_fetch_avatar(
    registry: tauri::State<'_, ProviderRegistry>,
    cache: tauri::State<'_, std::sync::Arc<Cache>>,
    account_id: String,
    email: String,
) -> Result<AvatarResult, String> {
    let lower = email.trim().to_lowercase();
    if lower.is_empty() {
        return Ok(AvatarResult { data: String::new(), mime: String::new() });
    }
    if let Some((bytes, mime)) = cache.get_avatar(&lower) {
        if bytes.is_empty() {
            return Ok(AvatarResult { data: String::new(), mime: String::new() });
        }
        use base64::Engine as _;
        return Ok(AvatarResult {
            data: base64::engine::general_purpose::STANDARD.encode(&bytes),
            mime,
        });
    }
    let provider = get_provider(&registry, &account_id).await?;
    let (bytes, mime) = provider
        .fetch_avatar(&lower)
        .await
        .unwrap_or_else(|_| (Vec::new(), String::new()));
    let _ = cache.save_avatar(&lower, &bytes, &mime);
    if bytes.is_empty() {
        return Ok(AvatarResult { data: String::new(), mime: String::new() });
    }
    use base64::Engine as _;
    Ok(AvatarResult {
        data: base64::engine::general_purpose::STANDARD.encode(&bytes),
        mime,
    })
}

#[tauri::command]
pub async fn v2_list_calendars(
    registry: tauri::State<'_, ProviderRegistry>,
    account_id: String,
) -> Result<Vec<DesktopCalendar>, String> {
    get_provider(&registry, &account_id)
        .await?
        .list_calendars()
        .await
}

#[tauri::command]
pub async fn v2_fetch_calendar_events(
    registry: tauri::State<'_, ProviderRegistry>,
    account_id: String,
    from_ms: i64,
    to_ms: i64,
    calendar_ids: Vec<i64>,
) -> Result<Vec<DesktopCalendarEvent>, String> {
    get_provider(&registry, &account_id)
        .await?
        .fetch_calendar_events(from_ms, to_ms, &calendar_ids)
        .await
}

#[tauri::command]
pub async fn v2_rsvp_event(
    registry: tauri::State<'_, ProviderRegistry>,
    account_id: String,
    event_id: i64,
    partstat: String,
) -> Result<String, String> {
    get_provider(&registry, &account_id)
        .await?
        .rsvp_event(event_id, &partstat)
        .await
}

#[tauri::command]
pub async fn v2_patch_event(
    registry: tauri::State<'_, ProviderRegistry>,
    account_id: String,
    event_id: i64,
    body: serde_json::Value,
) -> Result<(), String> {
    get_provider(&registry, &account_id)
        .await?
        .patch_event(event_id, body)
        .await
}

#[tauri::command]
pub async fn v2_create_event(
    registry: tauri::State<'_, ProviderRegistry>,
    account_id: String,
    body: serde_json::Value,
) -> Result<serde_json::Value, String> {
    get_provider(&registry, &account_id)
        .await?
        .create_event(body)
        .await
}

#[tauri::command]
pub async fn v2_delete_event(
    registry: tauri::State<'_, ProviderRegistry>,
    account_id: String,
    event_id: i64,
) -> Result<(), String> {
    get_provider(&registry, &account_id)
        .await?
        .delete_event(event_id)
        .await
}
