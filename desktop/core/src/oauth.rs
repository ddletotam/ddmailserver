//! Interactive Google OAuth2 for standalone desktop accounts (no DDMail
//! server). Authorization-code flow with a loopback redirect: open the
//! browser to Google's consent screen, catch the code on 127.0.0.1, exchange
//! it for access+refresh tokens. Desktop clients carry a client_secret, so no
//! PKCE is required.
//!
//! PREREQUISITE (external, not code): a Google Cloud OAuth client of type
//! "Desktop app" — its client_id/secret are passed in here. The server's
//! web-app client won't work (loopback redirect isn't an authorized URI).
//!
//! End-to-end needs a real browser consent, so only the pure pieces
//! (URL building, token-response parsing) are unit-tested here.

use std::io::{Read, Write};
use std::net::TcpListener;

/// Scopes for mail (IMAP), calendar (CalDAV) and contacts (CardDAV).
const SCOPES: &str = "https://mail.google.com/ https://www.googleapis.com/auth/calendar https://www.googleapis.com/auth/carddav https://www.googleapis.com/auth/userinfo.email";

/// Google OAuth client credentials for this install. Kept OUT of the repo —
/// read from `%APPDATA%/ru.letotam.ddmail/google_oauth.json` (or `$HOME/...`),
/// a plain `{ "client_id": "...", "client_secret": "..." }`.
#[derive(Debug, Clone)]
pub struct ClientCreds {
    pub client_id: String,
    pub client_secret: String,
}

/// Load the client creds from the app config dir; None if the file is absent
/// or malformed (the onboarding UI then shows "Google not configured").
pub fn load_client_creds() -> Option<ClientCreds> {
    let base = std::env::var("APPDATA").or_else(|_| std::env::var("HOME")).ok()?;
    let path = std::path::Path::new(&base).join("ru.letotam.ddmail").join("google_oauth.json");
    let data = std::fs::read_to_string(path).ok()?;
    let v: serde_json::Value = serde_json::from_str(&data).ok()?;
    Some(ClientCreds {
        client_id: v.get("client_id")?.as_str()?.to_string(),
        client_secret: v.get("client_secret")?.as_str()?.to_string(),
    })
}

/// Fetch the account's primary email via the userinfo endpoint (so onboarding
/// doesn't have to ask the user to type it).
pub async fn fetch_email(access_token: &str) -> Result<String, String> {
    let http = reqwest::Client::new();
    let resp = http
        .get("https://www.googleapis.com/oauth2/v2/userinfo")
        .bearer_auth(access_token)
        .send()
        .await
        .map_err(|e| format!("userinfo: {e}"))?;
    if !resp.status().is_success() {
        return Err(format!("userinfo HTTP {}", resp.status()));
    }
    let v: serde_json::Value = resp.json().await.map_err(|e| format!("userinfo parse: {e}"))?;
    v.get("email")
        .and_then(|x| x.as_str())
        .map(String::from)
        .ok_or_else(|| "userinfo: no email".to_string())
}

#[derive(Debug, Clone)]
pub struct GoogleTokens {
    pub access_token: String,
    pub refresh_token: String,
    /// Unix seconds when the access token expires (best-effort).
    pub expires_at: i64,
}

/// Run the full interactive flow. Blocks until the user completes (or aborts)
/// the browser consent, so call it off the UI thread.
pub async fn google_login(
    client_id: &str,
    client_secret: &str,
    now_unix: i64,
) -> Result<GoogleTokens, String> {
    // Bind a loopback port first so we know the redirect URI.
    let listener = TcpListener::bind("127.0.0.1:0").map_err(|e| format!("loopback bind: {e}"))?;
    let port = listener.local_addr().map_err(|e| format!("addr: {e}"))?.port();
    let redirect = format!("http://127.0.0.1:{port}");

    let auth_url = build_auth_url(client_id, &redirect);
    open_browser(&auth_url)?;

    // Catch the single redirect request and pull ?code= out of the GET line.
    let code = wait_for_code(&listener)?;

    exchange_code(client_id, client_secret, &code, &redirect, now_unix).await
}

/// Build the consent URL. `access_type=offline` + `prompt=consent` ensures a
/// refresh token is returned.
pub fn build_auth_url(client_id: &str, redirect: &str) -> String {
    format!(
        "https://accounts.google.com/o/oauth2/v2/auth?client_id={}&redirect_uri={}&response_type=code&scope={}&access_type=offline&prompt=consent",
        urlencoding::encode(client_id),
        urlencoding::encode(redirect),
        urlencoding::encode(SCOPES),
    )
}

fn open_browser(url: &str) -> Result<(), String> {
    #[cfg(target_os = "windows")]
    let r = std::process::Command::new("cmd").args(["/C", "start", "", url]).spawn();
    #[cfg(target_os = "linux")]
    let r = std::process::Command::new("xdg-open").arg(url).spawn();
    #[cfg(target_os = "macos")]
    let r = std::process::Command::new("open").arg(url).spawn();
    r.map(|_| ()).map_err(|e| format!("open browser: {e}"))
}

/// Block on the loopback listener for the redirect, extract `code`, and reply
/// with a small page so the browser tab is dismissable.
fn wait_for_code(listener: &TcpListener) -> Result<String, String> {
    let (mut stream, _) = listener.accept().map_err(|e| format!("accept: {e}"))?;
    let mut buf = [0u8; 2048];
    let n = stream.read(&mut buf).map_err(|e| format!("read: {e}"))?;
    let req = String::from_utf8_lossy(&buf[..n]);
    // First line: "GET /?code=XYZ&scope=... HTTP/1.1"
    let target = req.lines().next().and_then(|l| l.split_whitespace().nth(1)).unwrap_or("");
    let code = parse_query_param(target, "code");
    let body = if code.is_some() {
        "ddmail: авторизация получена. Можно закрыть вкладку."
    } else {
        "ddmail: код не получен."
    };
    let resp = format!(
        "HTTP/1.1 200 OK\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{}",
        body.as_bytes().len(),
        body
    );
    let _ = stream.write_all(resp.as_bytes());
    code.ok_or_else(|| "no authorization code in redirect".to_string())
}

/// Extract a query parameter value from a request target like `/?a=1&code=X`.
pub fn parse_query_param(target: &str, key: &str) -> Option<String> {
    let q = target.split_once('?').map(|(_, q)| q).unwrap_or("");
    for pair in q.split('&') {
        if let Some((k, v)) = pair.split_once('=') {
            if k == key {
                return Some(urlencoding::decode(v).map(|c| c.into_owned()).unwrap_or_else(|_| v.to_string()));
            }
        }
    }
    None
}

async fn exchange_code(
    client_id: &str,
    client_secret: &str,
    code: &str,
    redirect: &str,
    now_unix: i64,
) -> Result<GoogleTokens, String> {
    let http = reqwest::Client::new();
    let resp = http
        .post("https://oauth2.googleapis.com/token")
        .form(&[
            ("client_id", client_id),
            ("client_secret", client_secret),
            ("code", code),
            ("redirect_uri", redirect),
            ("grant_type", "authorization_code"),
        ])
        .send()
        .await
        .map_err(|e| format!("token exchange: {e}"))?;
    if !resp.status().is_success() {
        return Err(format!("token exchange HTTP {}", resp.status()));
    }
    let body = resp.text().await.map_err(|e| format!("token body: {e}"))?;
    parse_token_response(&body, now_unix, "")
}

/// Exchange a refresh token for a fresh access token.
pub async fn refresh(
    client_id: &str,
    client_secret: &str,
    refresh_token: &str,
    now_unix: i64,
) -> Result<GoogleTokens, String> {
    let http = reqwest::Client::new();
    let resp = http
        .post("https://oauth2.googleapis.com/token")
        .form(&[
            ("client_id", client_id),
            ("client_secret", client_secret),
            ("refresh_token", refresh_token),
            ("grant_type", "refresh_token"),
        ])
        .send()
        .await
        .map_err(|e| format!("refresh: {e}"))?;
    if !resp.status().is_success() {
        return Err(format!("refresh HTTP {}", resp.status()));
    }
    let body = resp.text().await.map_err(|e| format!("refresh body: {e}"))?;
    // A refresh response usually omits refresh_token — keep the old one.
    parse_token_response(&body, now_unix, refresh_token)
}

/// Parse Google's JSON token response. `fallback_refresh` is used when the
/// response omits a refresh token (refresh grant).
pub fn parse_token_response(
    body: &str,
    now_unix: i64,
    fallback_refresh: &str,
) -> Result<GoogleTokens, String> {
    let v: serde_json::Value =
        serde_json::from_str(body).map_err(|e| format!("token parse: {e}"))?;
    let access_token = v
        .get("access_token")
        .and_then(|x| x.as_str())
        .ok_or("token response missing access_token")?
        .to_string();
    let refresh_token = v
        .get("refresh_token")
        .and_then(|x| x.as_str())
        .unwrap_or(fallback_refresh)
        .to_string();
    let expires_in = v.get("expires_in").and_then(|x| x.as_i64()).unwrap_or(3600);
    Ok(GoogleTokens {
        access_token,
        refresh_token,
        expires_at: now_unix + expires_in,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn auth_url_has_offline_and_scopes() {
        let u = build_auth_url("cid.apps.googleusercontent.com", "http://127.0.0.1:5000");
        assert!(u.contains("access_type=offline"));
        assert!(u.contains("prompt=consent"));
        assert!(u.contains("client_id=cid.apps.googleusercontent.com"));
        assert!(u.contains("mail.google.com"));
        assert!(u.contains("127.0.0.1%3A5000"));
    }

    #[test]
    fn parses_code_from_target() {
        assert_eq!(parse_query_param("/?code=abc123&scope=x", "code").as_deref(), Some("abc123"));
        assert_eq!(parse_query_param("/?error=denied", "code"), None);
    }

    #[test]
    fn parses_token_response_and_keeps_fallback_refresh() {
        let full = r#"{"access_token":"AT","refresh_token":"RT","expires_in":3599}"#;
        let t = parse_token_response(full, 1000, "old").unwrap();
        assert_eq!(t.access_token, "AT");
        assert_eq!(t.refresh_token, "RT");
        assert_eq!(t.expires_at, 1000 + 3599);

        let refreshed = r#"{"access_token":"AT2","expires_in":3600}"#;
        let t2 = parse_token_response(refreshed, 2000, "old").unwrap();
        assert_eq!(t2.access_token, "AT2");
        assert_eq!(t2.refresh_token, "old"); // fallback kept
    }
}
