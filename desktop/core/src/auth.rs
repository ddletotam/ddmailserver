//! Desktop login: username+password → JWT (native mode). Used by the
//! first-run login screen; after that the token lives in accounts.json and
//! NativeProvider auto-refreshes it on 401 (see native_provider.rs).

use serde::Deserialize;

/// What the login screen needs to build an account entry.
#[derive(Debug, Clone)]
pub struct LoginResult {
    pub token: String,
    pub username: String,
    /// Default identity email (falls back to the username when the
    /// identities call fails or returns nothing).
    pub email: String,
}

#[derive(Deserialize)]
struct LoginResponse {
    token: String,
    username: String,
}

#[derive(Deserialize)]
struct Identity {
    email: String,
    #[serde(default)]
    is_default: bool,
}

/// POST /auth/login, then GET /identities for the default sender address.
/// `server_url` is the bare origin, e.g. `https://mail.letotam.ru`.
pub async fn login(
    server_url: &str,
    username: &str,
    password: &str,
) -> Result<LoginResult, String> {
    let server_url = server_url.trim_end_matches('/');
    let http = reqwest::Client::builder()
        .timeout(std::time::Duration::from_secs(15))
        .build()
        .map_err(|e| format!("HTTP client: {e}"))?;

    let resp = http
        .post(format!("{server_url}/api/desktop/v1/auth/login"))
        .json(&serde_json::json!({ "username": username, "password": password }))
        .send()
        .await
        .map_err(|e| format!("Нет связи с сервером: {e}"))?;

    if resp.status() == reqwest::StatusCode::UNAUTHORIZED {
        return Err("Неверный логин или пароль".into());
    }
    if !resp.status().is_success() {
        return Err(format!("Сервер ответил {}", resp.status()));
    }
    let login: LoginResponse =
        resp.json().await.map_err(|e| format!("Ответ сервера не разобрать: {e}"))?;

    // Best-effort: the default identity gives us the user's real address for
    // display / self-detection. A failure here must not fail the login.
    let email = fetch_default_email(&http, server_url, &login.token)
        .await
        .unwrap_or_else(|| login.username.clone());

    Ok(LoginResult { token: login.token, username: login.username, email })
}

async fn fetch_default_email(
    http: &reqwest::Client,
    server_url: &str,
    token: &str,
) -> Option<String> {
    let resp = http
        .get(format!("{server_url}/api/desktop/v1/identities"))
        .bearer_auth(token)
        .send()
        .await
        .ok()?;
    if !resp.status().is_success() {
        return None;
    }
    let ids: Vec<Identity> = resp.json().await.ok()?;
    ids.iter()
        .find(|i| i.is_default)
        .or_else(|| ids.first())
        .map(|i| i.email.clone())
}
