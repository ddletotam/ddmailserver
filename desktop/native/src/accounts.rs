//! Connections: the local cache they share, the add/edit connection window,
//! rebuilding the mail engine when the account list changes, and the
//! per-connection status shown in the sidebar (contract §5д, §5д-бис).

use super::*;

pub(crate) fn cache_db_path() -> Option<std::path::PathBuf> {
    // Pick the per-OS location of the existing cache dir so the client
    // reads the same cache.db the user already has.
    //   * Windows: %APPDATA%\ru.letotam.ddmail\cache.db
    //   * macOS:   ~/Library/Application Support/ru.letotam.ddmail/cache.db
    //   * Linux:   $XDG_DATA_HOME/ru.letotam.ddmail/cache.db,
    //              or ~/.local/share/ru.letotam.ddmail/cache.db
    #[cfg(target_os = "windows")]
    {
        let appdata = std::env::var("APPDATA").ok()?;
        return Some(std::path::PathBuf::from(appdata).join("ru.letotam.ddmail").join("cache.db"));
    }
    #[cfg(target_os = "macos")]
    {
        let home = std::env::var("HOME").ok()?;
        return Some(
            std::path::PathBuf::from(home)
                .join("Library/Application Support/ru.letotam.ddmail/cache.db"),
        );
    }
    #[cfg(target_os = "linux")]
    {
        let base =
            std::env::var("XDG_DATA_HOME").ok().map(std::path::PathBuf::from).or_else(|| {
                std::env::var("HOME").ok().map(|h| std::path::PathBuf::from(h).join(".local/share"))
            })?;
        return Some(base.join("ru.letotam.ddmail").join("cache.db"));
    }
    #[allow(unreachable_code)]
    None
}

pub(crate) fn open_cache() -> Option<Arc<Cache>> {
    let path = cache_db_path()?;
    let dir = path.parent()?.to_path_buf();
    Cache::new(dir).ok().map(Arc::new)
}

pub(crate) fn open_account() -> Option<(Arc<Cache>, String, Vec<Conversation>)> {
    let path = cache_db_path()?;
    if !path.exists() {
        println!("cache.db not found at {}", path.display());
        return None;
    }
    let cache = open_cache()?;
    // Ключ основной (первой) учётки, а не первый попавшийся в кэше:
    // `SELECT DISTINCT` отдаёт ключи в порядке индекса, и с внешней
    // учёткой «denis…@imap.mail.ru» вставал впереди «lucky@mail.letotam.ru».
    // Айдентики живут под ключом основной — с чужим ключом их карта
    // пустела, и все строки списка становились серыми.
    let cached = cache.account_keys().ok()?;
    let key = engine::AccountConfig::load_all()
        .first()
        .map(|a| a.account_key())
        .filter(|k| cached.contains(k))
        .or_else(|| cached.into_iter().next())?;
    let convs = cache.load_conversations(&key).ok()?;
    if convs.is_empty() {
        return None;
    }
    println!("loaded {} real conversations (account {key})", convs.len());
    Some((cache, key, convs))
}

#[cfg(test)]
mod reauth_tests {
    use super::note_account_state;

    /// «Нужен вход» держится, пока сессию не подтвердили заново: watcher
    /// после отказа успевает крикнуть "error"/"connecting", и если считать
    /// это выздоровлением, плашка мигнёт и исчезнет.
    #[test]
    fn auth_is_sticky_until_connected() {
        let mut r = Vec::new();
        note_account_state(&mut r, "mail.letotam.ru|lucky", "auth");
        note_account_state(&mut r, "mail.letotam.ru|lucky", "error");
        note_account_state(&mut r, "mail.letotam.ru|lucky", "connecting");
        assert_eq!(r, vec!["mail.letotam.ru|lucky".to_string()]);

        note_account_state(&mut r, "mail.letotam.ru|lucky", "connected");
        assert!(r.is_empty());
    }

    /// Каждый 401 по любому запросу шлёт своё событие; в списке учётка одна.
    #[test]
    fn repeated_auth_reports_do_not_duplicate() {
        let mut r = Vec::new();
        for _ in 0..5 {
            note_account_state(&mut r, "a|u", "auth");
        }
        assert_eq!(r.len(), 1);
    }

    /// Порядок отказов сохраняется, а чужой коннект не снимает свой приговор:
    /// индекс для формы входа считается по этому списку.
    #[test]
    fn accounts_are_independent_and_ordered() {
        let mut r = Vec::new();
        note_account_state(&mut r, "a|u", "auth");
        note_account_state(&mut r, "b|u", "auth");
        note_account_state(&mut r, "b|u", "connected");
        assert_eq!(r, vec!["a|u".to_string()]);

        note_account_state(&mut r, "b|u", "auth");
        assert_eq!(r, vec!["a|u".to_string(), "b|u".to_string()]);
    }
}

/// `https://mail.letotam.ru:8443/x` → `mail.letotam.ru` — the host part only,
/// used as the account-key host for native-mode accounts.
pub(crate) fn host_from_url(url: &str) -> String {
    let s = url.trim();
    let s = s.strip_prefix("https://").or_else(|| s.strip_prefix("http://")).unwrap_or(s);
    s.split(['/', ':']).next().unwrap_or(s).to_string()
}

/// Close the add-connection window `generation` — and only it: a window opened after
/// it must survive a late close of the old one. Deferred to the event loop,
/// since this runs from the window's own callbacks and the handle must not be
/// dropped inside them.
fn close_add_connection(generation: u64) {
    let _ = slint::invoke_from_event_loop(move || {
        SHARED.with(|s| {
            if let Some(sh) = s.borrow().as_ref() {
                let mut slot = sh.accounts.add_conn_window.borrow_mut();
                if slot.as_ref().is_some_and(|(g, _)| *g == generation) {
                    if let Some((_, lw)) = slot.take() {
                        let _ = lw.hide();
                    }
                }
            }
        });
    });
}

/// Close the add-connection window and rebuild the engine from the updated
/// accounts.json. Safe to call from any thread — it hops to the UI loop.
pub(crate) fn finish_add_connection(main_weak: slint::Weak<MainWindow>, generation: u64) {
    close_add_connection(generation);
    let _ = slint::invoke_from_event_loop(move || {
        SHARED.with(|s| {
            if let Some(sh) = s.borrow().as_ref() {
                if let Some(m) = main_weak.upgrade() {
                    rebuild_engine(&m, sh);
                    refresh_connections(&m, sh);
                }
            }
        });
    });
}

/// Open the add-connection window from the running app. Native (server/login/
/// pass → JWT), standalone IMAP/SMTP (+optional CalDAV/CardDAV), or Google
/// OAuth. A new connection is APPENDED to accounts.json (never clobbering the
/// others); an edited one is written in place of the original, even when the
/// edit changed its key. The engine rebuilds after. The window is retained in
/// Shared while open (contract §5г-бис).
///
/// Nothing is saved until the login actually worked: a standalone account is
/// checked over IMAP and SMTP first. Before that, a mistyped password went
/// straight into accounts.json and surfaced later as «сессия истекла».
pub(crate) fn open_add_connection(
    main_weak: slint::Weak<MainWindow>,
    prefill: Option<engine::AccountConfig>,
) {
    // Одно окно на всё приложение. Идёт проверка — поднимаем его: вторая
    // форма поверх первой означала гонку, в которой сохранялась не та учётка
    // и «Отмена» закрывала чужое окно. Не идёт — старое уступает новому
    // (например, «Войти» с плашки для другой учётки).
    let mut generation = 0;
    let raised = SHARED.with(|s| {
        let s = s.borrow();
        let Some(sh) = s.as_ref() else { return false };
        let mut slot = sh.accounts.add_conn_window.borrow_mut();
        if let Some((_, lw)) = slot.as_ref().filter(|(_, lw)| lw.get_busy()) {
            // Вывести окно вперёд и дать фокус Slint не умеет; хотя бы
            // развернуть, если свёрнуто.
            lw.window().set_minimized(false);
            let _ = lw.show();
            return true;
        }
        if let Some((_, old)) = slot.take() {
            let _ = old.hide();
        }
        generation = sh.accounts.add_conn_seq.get() + 1;
        sh.accounts.add_conn_seq.set(generation);
        false
    });
    if raised {
        return;
    }
    let Ok(lw) = LoginWindow::new() else { return };
    // Shortcuts — глобал на компонент: у отдельного окна свой экземпляр, и
    // без этого Ctrl+V под ЙЦУКЕН здесь не вставлял — а вставляют сюда как
    // раз 16-символьный пароль приложения.
    lw.global::<Shortcuts>().on_action(|text, held| shortcut_action(text.as_str(), held));

    // Куда сохранять: новая учётка (None) или на место прежней. Повторный
    // вход (сессия истекла, учётка Google) обязан вернуть ту же учётку:
    // выбрал в браузере другой аккаунт Google или вошёл на сервер другим
    // логином — прежняя молча подменилась бы, а её секреты стёрлись.
    let mut target: Option<SaveTarget> = None;
    if let Some(c) = &prefill {
        let key = c.account_key();
        let needs_login = SHARED.with(|s| {
            s.borrow().as_ref().is_some_and(|sh| sh.accounts.reauth.borrow().contains(&key))
        });
        target = Some(SaveTarget {
            key: key.clone(),
            same_account: needs_login || c.oauth_refresh_token.is_some(),
        });
        lw.set_heading(
            if needs_login {
                "Вход в учётную запись"
            } else {
                "Изменить подключение"
            }
            .into(),
        );
        // Вид учётки при правке не меняют: это была бы другая учётка.
        lw.set_kind_locked(true);
        // Пароль не хранится (native) или его и пришли поменять.
        lw.set_focus_field("password".into());
        if c.native_url.is_some() {
            lw.set_mode(0);
            lw.set_server_url(c.native_url.clone().unwrap_or_default().into());
            lw.set_username(c.username.clone().into());
        } else if c.oauth_refresh_token.is_some() {
            // Google OAuth: пароля нет, править нечего — только войти заново.
            // Парольная форма сохранила бы учётку без токена, и CalDAV Google
            // по паролю уже не работал бы.
            lw.set_mode(1);
            lw.set_oauth_only(true);
            lw.set_email(c.email.clone().into());
        } else {
            lw.set_mode(1);
            lw.set_email(c.email.clone().into());
            lw.set_imap_host(c.host.clone().into());
            lw.set_imap_port(c.port.to_string().into());
            lw.set_smtp_host(c.smtp_host.clone().into());
            lw.set_smtp_port(c.smtp_port.to_string().into());
            // Логин, совпадающий с email, не показываем: пустое поле и
            // значит «как email».
            if c.username != c.email {
                lw.set_imap_username(c.username.clone().into());
            }
            lw.set_password(c.password.clone().into());
            lw.set_carddav_url(c.carddav_url.clone().unwrap_or_default().into());
            lw.set_caldav_url(c.caldav_url.clone().unwrap_or_default().into());
            // Сохранённые серверы — это решение пользователя, а не догадка:
            // показываем их и по email больше не перезаписываем.
            lw.set_servers_touched(true);
            lw.set_servers_open(true);
            if let Some(g) = mail_presets::guess(&c.email) {
                lw.set_password_hint(g.password_hint.into());
            }
        }
    }

    // Email → серверы и подсказка про пароль, пока серверы не правили руками.
    let weak = lw.as_weak();
    lw.on_email_edited(move |email| {
        let Some(lw) = weak.upgrade() else { return };
        let guess = mail_presets::guess(&email);
        lw.set_password_hint(guess.as_ref().map(|g| g.password_hint).unwrap_or("").into());
        if lw.get_servers_touched() {
            return;
        }
        match guess {
            Some(g) => {
                lw.set_imap_host(g.imap_host.into());
                lw.set_imap_port(g.imap_port.to_string().into());
                lw.set_smtp_host(g.smtp_host.into());
                lw.set_smtp_port(g.smtp_port.to_string().into());
                lw.set_servers_known(g.provider.is_some());
                lw.set_provider_name(g.provider.unwrap_or("").into());
            }
            None => {
                lw.set_imap_host("".into());
                lw.set_smtp_host("".into());
                lw.set_servers_known(false);
            }
        }
    });

    // «Отмена», Esc и крестик окна — одно и то же. Window gone from Shared ⇒
    // a check still running in the background finds nothing to report to and
    // saves nothing (see `save_if_open`).
    lw.on_cancel(move || close_add_connection(generation));
    lw.window().on_close_requested(move || {
        close_add_connection(generation);
        slint::CloseRequestResponse::HideWindow
    });

    let weak = lw.as_weak();
    let mw_submit = main_weak.clone();
    let key_submit = target.clone();
    lw.on_submit(move || {
        let Some(lw) = weak.upgrade() else { return };
        if lw.get_busy() {
            return;
        }
        if lw.get_oauth_only() {
            // Enter из окна идёт сюда мимо выключенной кнопки.
            if lw.get_google_available() {
                lw.invoke_google_login();
            }
            return;
        }
        let password = lw.get_password().to_string();

        // ── Standalone IMAP/SMTP: check the login, then persist. ──
        if lw.get_mode() == 1 {
            let cfg = match standalone_config(&lw, password) {
                Ok(cfg) => cfg,
                Err(e) => {
                    // Ошибка в серверах — раскрыть их, иначе исправлять негде.
                    if e.contains("IMAP") || e.contains("SMTP") || e.contains("Логин") {
                        lw.set_servers_open(true);
                    }
                    lw.set_error(e.into());
                    return;
                }
            };
            lw.set_error("".into());
            lw.set_busy(true);
            lw.set_busy_text("Проверяем вход…".into());
            let weak = lw.as_weak();
            let mw = mw_submit.clone();
            let key = key_submit.clone();
            std::thread::spawn(move || {
                let result = tokio::runtime::Runtime::new()
                    .map_err(|e| format!("tokio: {e}"))
                    .and_then(|rt| rt.block_on(check_standalone(&cfg)));
                save_if_open(weak, mw, generation, key, result.map(|()| cfg));
            });
            return;
        }

        // ── Native (our server): try username+password → JWT. On failure the
        // user can switch to the «Другой ящик (IMAP)» tab (the manual fallback). ──
        let username = lw.get_username().trim().to_string();
        let server = lw.get_server_url().trim().trim_end_matches('/').to_string();
        if server.is_empty() || username.is_empty() || password.is_empty() {
            lw.set_error("Заполните адрес сервера, логин и пароль.".into());
            return;
        }
        let server = if server.starts_with("http://") || server.starts_with("https://") {
            server
        } else {
            format!("https://{server}")
        };
        if let Err(e) = require_https(&server, "Адрес сервера") {
            lw.set_error(e.into());
            return;
        }
        lw.set_error("".into());
        lw.set_busy(true);
        lw.set_busy_text("Входим…".into());

        let weak = lw.as_weak();
        let mw = mw_submit.clone();
        let key = key_submit.clone();
        std::thread::spawn(move || {
            let result =
                tokio::runtime::Runtime::new().map_err(|e| format!("tokio: {e}")).and_then(|rt| {
                    rt.block_on(ddmail_core::auth::login(&server, &username, &password))
                });
            let result = result
                .map(|login| engine::AccountConfig {
                    host: host_from_url(&server),
                    port: 993,
                    username: login.username.clone(),
                    password: String::new(),
                    use_tls: true,
                    email: login.email.clone(),
                    smtp_host: host_from_url(&server),
                    smtp_port: 465,
                    native_url: Some(server.clone()),
                    native_token: Some(login.token.clone()),
                    carddav_url: None,
                    caldav_url: None,
                    oauth_refresh_token: None,
                })
                .map_err(|e| {
                    // Неверный пароль — сервер точно ddmail, совет про
                    // другую вкладку только сбил бы с толку.
                    if e == "Неверный логин или пароль" {
                        e
                    } else {
                        format!("{e}. Если это не сервер ddmail — вкладка «Другой ящик (IMAP)».")
                    }
                });
            save_if_open(weak, mw, generation, key, result);
        });
    });

    lw.set_google_available(ddmail_core::oauth::load_client_creds().is_some());
    let gweak = lw.as_weak();
    let mw_google = main_weak.clone();
    let key_google = target.clone();
    lw.on_google_login(move || {
        let Some(lw) = gweak.upgrade() else { return };
        if lw.get_busy() || !lw.get_google_available() {
            return;
        }
        lw.set_error("".into());
        lw.set_busy(true);
        lw.set_busy_text("Ждём браузер…".into());
        let weak = lw.as_weak();
        let mw = mw_google.clone();
        let key = key_google.clone();
        std::thread::spawn(move || {
            let result =
                tokio::runtime::Runtime::new().map_err(|e| format!("tokio: {e}")).and_then(|rt| {
                    rt.block_on(async {
                        let creds = ddmail_core::oauth::load_client_creds()
                            .ok_or("google_oauth.json missing")?;
                        let now = chrono::Local::now().timestamp();
                        let tokens = ddmail_core::oauth::google_login(
                            &creds.client_id,
                            &creds.client_secret,
                            now,
                        )
                        .await?;
                        let email = ddmail_core::oauth::fetch_email(&tokens.access_token).await?;
                        Ok::<_, String>((tokens, email))
                    })
                });
            let result = result.map(|(tokens, email)| engine::AccountConfig {
                host: "imap.gmail.com".into(),
                port: 993,
                username: email.clone(),
                password: String::new(),
                use_tls: true,
                email: email.clone(),
                smtp_host: "smtp.gmail.com".into(),
                smtp_port: 465,
                native_url: None,
                native_token: None,
                carddav_url: Some("https://www.googleapis.com/.well-known/carddav".into()),
                caldav_url: Some(format!(
                    "https://apidata.googleusercontent.com/caldav/v2/{email}/events"
                )),
                oauth_refresh_token: Some(tokens.refresh_token),
            });
            save_if_open(weak, mw, generation, key, result);
        });
    });

    let _ = lw.show();
    SHARED.with(|s| {
        if let Some(sh) = s.borrow().as_ref() {
            *sh.accounts.add_conn_window.borrow_mut() = Some((generation, lw));
        }
    });
}

/// Пароль по `http://` уходит открытым текстом: и вход на сервер ddmail, и
/// Basic-авторизация CalDAV/CardDAV. Исключение — своя машина.
fn require_https(url: &str, what: &str) -> Result<(), String> {
    let Some(rest) = url.strip_prefix("http://") else { return Ok(()) };
    let host = rest.split(['/', ':']).next().unwrap_or("");
    if matches!(host, "localhost" | "127.0.0.1" | "[::1]") {
        return Ok(());
    }
    Err(format!("{what}: нужен https:// — по http:// пароль ушёл бы открытым текстом."))
}

/// Standalone account from the form, or what is wrong with the form.
fn standalone_config(lw: &LoginWindow, password: String) -> Result<engine::AccountConfig, String> {
    let email = lw.get_email().trim().to_string();
    if !email.contains('@') {
        return Err("Введите email.".into());
    }
    if password.is_empty() {
        return Err("Введите пароль.".into());
    }
    let host = lw.get_imap_host().trim().to_string();
    let smtp_host = lw.get_smtp_host().trim().to_string();
    if host.is_empty() || smtp_host.is_empty() {
        return Err("Укажите серверы IMAP и SMTP.".into());
    }
    // Опечатка в порту — ошибка, а не молчаливые 993: так учётка уезжала
    // на чужой порт и «не подключалась» без объяснений.
    let port = |s: slint::SharedString, what: &str| {
        s.trim()
            .parse::<u16>()
            .ok()
            .filter(|p| *p > 0)
            .ok_or(format!("Порт {what} — число от 1 до 65535."))
    };
    let imap_port = port(lw.get_imap_port(), "IMAP")?;
    let smtp_port = port(lw.get_smtp_port(), "SMTP")?;
    let username = lw.get_imap_username().trim().to_string();
    let opt = |s: slint::SharedString| {
        let t = s.trim().to_string();
        if t.is_empty() { None } else { Some(t) }
    };
    let caldav_url = opt(lw.get_caldav_url());
    let carddav_url = opt(lw.get_carddav_url());
    for (url, what) in [(&caldav_url, "CalDAV"), (&carddav_url, "CardDAV")] {
        if let Some(u) = url {
            require_https(u, what)?;
        }
    }
    Ok(engine::AccountConfig {
        host,
        port: imap_port,
        username: if username.is_empty() { email.clone() } else { username },
        password,
        use_tls: true,
        email,
        smtp_host,
        smtp_port,
        native_url: None,
        native_token: None,
        carddav_url,
        caldav_url,
        oauth_refresh_token: None,
    })
}

/// Ошибки IMAP/SMTP приходят из библиотек по-английски; самые частые —
/// по-русски, чтобы не было «IMAP … — Invalid username or password» посреди
/// русской фразы. Остальное как есть: там важны подробности.
fn ru_login_error(e: String) -> String {
    let lower = e.to_ascii_lowercase();
    if lower.contains("invalid username or password")
        || lower.contains("authentication failed")
        || lower.contains("authentication credentials invalid")
        || lower.contains("(535)")
    {
        return "неверный логин или пароль".into();
    }
    if let Some(rest) = e.strip_prefix("TCP: ") {
        return format!("нет соединения ({rest})");
    }
    e
}

/// Log in over IMAP, then SMTP. Each failure names the server it came from:
/// «неверный пароль» from SMTP after IMAP succeeded means a different thing
/// (a separate SMTP password, or SMTP disabled) than from IMAP.
async fn check_standalone(cfg: &engine::AccountConfig) -> Result<(), String> {
    let timeout = std::time::Duration::from_secs(25);
    let imap = ddmail_core::imap::check_login(
        &cfg.host,
        cfg.port,
        &cfg.username,
        &cfg.password,
        cfg.use_tls,
    );
    match tokio::time::timeout(timeout, imap).await {
        Err(_) => return Err(format!("IMAP {}:{} не отвечает.", cfg.host, cfg.port)),
        Ok(Err(e)) => {
            return Err(format!("IMAP {}:{} — {}", cfg.host, cfg.port, ru_login_error(e)));
        }
        Ok(Ok(())) => {}
    }
    let smtp = ddmail_core::smtp::test_login(
        &cfg.smtp_host,
        cfg.smtp_port,
        &cfg.username,
        &cfg.password,
        cfg.use_tls,
    );
    match tokio::time::timeout(timeout, smtp).await {
        Err(_) => Err(format!("SMTP {}:{} не отвечает.", cfg.smtp_host, cfg.smtp_port)),
        Ok(Err(e)) => Err(format!(
            "Вход по IMAP прошёл, но SMTP {}:{} — {}",
            cfg.smtp_host,
            cfg.smtp_port,
            ru_login_error(e)
        )),
        Ok(Ok(())) => Ok(()),
    }
}

/// Where an edited account goes: in place of `key`. `same_account` — повторный
/// вход: результат обязан быть той же учёткой (тот же ключ).
#[derive(Clone)]
struct SaveTarget {
    key: String,
    same_account: bool,
}

/// Back on the UI thread: save the account and close, or show the error —
/// but only while window `generation` is still the open one. Cancelled mid-check (or
/// replaced by a newer window) ⇒ nothing is saved. An edited account is
/// written in place of `target.key`.
fn save_if_open(
    weak: slint::Weak<LoginWindow>,
    mw: slint::Weak<MainWindow>,
    generation: u64,
    target: Option<SaveTarget>,
    result: Result<engine::AccountConfig, String>,
) {
    let _ = slint::invoke_from_event_loop(move || {
        let open = SHARED.with(|s| {
            s.borrow().as_ref().is_some_and(|sh| {
                sh.accounts.add_conn_window.borrow().as_ref().is_some_and(|(g, _)| *g == generation)
            })
        });
        let Some(lw) = weak.upgrade() else { return };
        if !open {
            return;
        }
        let saved = result.and_then(|cfg| match &target {
            Some(t) if t.same_account && cfg.account_key() != t.key => Err(format!(
                "Вы вошли как {} — это другая учётка. Войдите в ту же или добавьте эту \
                 через «Добавить подключение».",
                if cfg.email.is_empty() { cfg.account_key() } else { cfg.email.clone() }
            )),
            Some(t) => engine::AccountConfig::replace_account(&t.key, &cfg),
            None => {
                engine::AccountConfig::add_account(&cfg);
                Ok(())
            }
        });
        match saved {
            Ok(()) => finish_add_connection(mw, generation),
            Err(e) => {
                lw.set_busy(false);
                lw.set_error(e.into());
            }
        }
    });
}

#[cfg(test)]
mod form_tests {
    use super::{require_https, ru_login_error};

    #[test]
    fn http_is_refused_except_loopback() {
        assert!(require_https("https://mail.example.ru", "x").is_ok());
        assert!(require_https("http://mail.example.ru/dav", "x").is_err());
        assert!(require_https("http://localhost:8080/", "x").is_ok());
        assert!(require_https("http://127.0.0.1/", "x").is_ok());
    }

    #[test]
    fn common_login_errors_are_russian() {
        assert_eq!(
            ru_login_error("Invalid username or password".into()),
            "неверный логин или пароль"
        );
        assert_eq!(
            ru_login_error(
                "permanent error (535): 5.7.8 Authentication credentials invalid".into()
            ),
            "неверный логин или пароль"
        );
        assert_eq!(ru_login_error("TCP: refused".into()), "нет соединения (refused)");
        assert_eq!(ru_login_error("TLS: bad cert".into()), "TLS: bad cert");
    }
}

/// (Re)spawn the mail engine from the current `accounts.json`. Tears down any
/// running engine first (dropping the command Sender ends its thread), then
/// spawns a fresh one over the current account set and kicks the initial
/// fetch. Called at startup and whenever connections change; a reloading
/// marker is shown until the first conversations arrive.
pub(crate) fn rebuild_engine(ui: &MainWindow, shared: &Rc<Shared>) {
    // Tear down the previous engine (its thread exits when the Sender drops).
    shared.engine_tx.borrow_mut().take();

    let Some(cache) = open_cache() else { return };
    let mut accounts = engine::AccountConfig::load_all();
    let live = !accounts.is_empty();
    ui.set_has_connections(live);

    if accounts.is_empty() && !shared.key.is_empty() {
        // Dev cache-only fallback: reconstruct a placeholder so cache reads
        // resolve to the existing namespace (no live provider).
        let key = shared.key.clone();
        let (username, host) = key
            .rsplit_once('@')
            .map(|(u, h)| (u.to_string(), h.to_string()))
            .unwrap_or_else(|| (key.clone(), String::new()));
        accounts.push(engine::AccountConfig {
            host: host.clone(),
            port: 993,
            username,
            password: String::new(),
            use_tls: true,
            email: key,
            smtp_host: host,
            smtp_port: 465,
            native_url: None,
            native_token: None,
            carddav_url: None,
            caldav_url: None,
            oauth_refresh_token: None,
        });
    }

    {
        let keys: Vec<String> = accounts.iter().map(|a| a.account_key()).collect();
        let mut st = shared.accounts.account_states.borrow_mut();
        st.clear();
        for k in &keys {
            st.insert(k.clone(), "connecting".into());
        }
        drop(st);
        *shared.accounts.account_keys.borrow_mut() = keys;
        // Пересборка движка — это и повторный вход в том числе: заново
        // выданный токен ещё ничего не подтвердил, но старый приговор с него
        // снимать надо, иначе плашка останется висеть после успешного входа.
        shared.accounts.reauth.borrow_mut().clear();
    }

    let ui_weak_eng = ui.as_weak();
    let etx = engine::spawn(accounts, cache, move |res| {
        let _ = ui_weak_eng.upgrade_in_event_loop(move |ui| handle_engine_result(&ui, res));
    });
    if live {
        ui.set_engine_reloading(true);
        let _ = etx.send(engine::EngineCmd::FetchConversations { limit: CONV_FETCH_LIMIT });
        let _ = etx.send(engine::EngineCmd::StartWatching);
        let _ = etx.send(engine::EngineCmd::FetchDialogFolders);
    } else {
        ui.set_engine_reloading(false);
    }
    *shared.engine_tx.borrow_mut() = Some(etx);
}

/// Rebuild the Settings → Подключения list from accounts.json, keeping a
/// parallel key list for edit/delete-by-index.
pub(crate) fn refresh_connections(ui: &MainWindow, shared: &Shared) {
    let accounts = engine::AccountConfig::load_all();
    let mut rows: Vec<ConnRow> = Vec::with_capacity(accounts.len());
    let mut keys: Vec<String> = Vec::with_capacity(accounts.len());
    let reauth = shared.accounts.reauth.borrow();
    for a in &accounts {
        let key = a.account_key();
        let title = if a.email.is_empty() { key.clone() } else { a.email.clone() };
        let needs_login = reauth.iter().any(|k| *k == key);
        let subtitle = if needs_login {
            // Кнопка рядом на этой строке — «Войти»: открывает форму входа с
            // подставленным сервером и логином.
            "сессия истекла — нужен пароль".to_string()
        } else {
            format!("{} · {}", a.host, account_mode(a))
        };
        rows.push(ConnRow { title: title.into(), subtitle: subtitle.into(), needs_login });
        keys.push(key);
    }
    drop(reauth);
    // Тот же список — модель не трогаем: новая модель сбрасывала бы
    // подтверждение удаления (`changed connections`), а при открытой модалке
    // сюда приходит каждое событие связи.
    let current = ui.get_connections();
    use slint::Model as _;
    let same = current.row_count() == rows.len()
        && rows.iter().enumerate().all(|(i, r)| current.row_data(i).as_ref() == Some(r));
    *shared.accounts.settings_conn_keys.borrow_mut() = keys;
    if !same {
        ui.set_connections(ModelRc::new(VecModel::from(rows)));
    }
}

/// Учесть новое состояние аккаунта в списке «ждут пароля».
///
/// `"auth"` добавляет (без дублей, порядок отказов сохраняется), удачный
/// коннект снимает. `"connecting"`/`"error"` НЕ снимают намеренно: мёртвая
/// сессия продолжает отдавать 401, и watcher по пути к молчанию успевает ещё
/// раз сказать «переподключаюсь» — приняв это за выздоровление, плашка
/// мигнула бы и исчезла, оставив пользователя с тем же пустым календарём.
pub(crate) fn note_account_state(reauth: &mut Vec<String>, key: &str, state: &str) {
    match state {
        "auth" => {
            if !reauth.iter().any(|k| k == key) {
                reauth.push(key.to_string());
            }
        }
        "connected" => reauth.retain(|k| k != key),
        _ => {}
    }
}

/// Apply an engine result on the UI thread (reaches Shared via the thread-local).
/// Recompute the aggregate connection light from per-account states:
/// 2 = green (all connected), 1 = yellow (some down), 0 = red (none connected).
pub(crate) fn apply_conn_status(ui: &MainWindow, sh: &Shared) {
    let states = sh.accounts.account_states.borrow();
    let keys = sh.accounts.account_keys.borrow();
    let total = keys.len();
    let connected =
        keys.iter().filter(|k| states.get(*k).map(|s| s == "connected").unwrap_or(false)).count();
    let status = if total == 0 || connected == total {
        2
    } else if connected > 0 {
        1
    } else {
        0
    };
    ui.set_conn_status(status);

    // Тот же список, что в настройках, но с бинарным состоянием на строку:
    // агрегат «частично» уже несёт сама точка. Исключение — мёртвая сессия:
    // «не подключён» тут ничего не объясняет, поэтому строка говорит прямо.
    let accounts = engine::AccountConfig::load_all();
    let reauth = sh.accounts.reauth.borrow();
    let rows: Vec<ConnDotRow> = accounts
        .iter()
        .map(|a| {
            let key = a.account_key();
            let title = if a.email.is_empty() { key.clone() } else { a.email.clone() };
            let needs_login = reauth.iter().any(|k| *k == key);
            let subtitle = if needs_login {
                "сессия истекла — нужен пароль".to_string()
            } else {
                format!("{} · {}", a.host, account_mode(a))
            };
            ConnDotRow {
                title: title.into(),
                subtitle: subtitle.into(),
                ok: states.get(&key).map(|s| s == "connected").unwrap_or(false),
                needs_login,
            }
        })
        .collect();
    // Индексы строк списка = порядок accounts.json; `relogin(i)` разрешает
    // индекс через этот же список, а не через список настроек: тот
    // наполняется только при открытии модалки настроек.
    *sh.accounts.conn_dot_keys.borrow_mut() = accounts.iter().map(|a| a.account_key()).collect();
    ui.set_conn_accounts(ModelRc::new(VecModel::from(rows)));

    // Плашка «сессия истекла»: первая по порядку accounts.json учётка,
    // ждущая пароля, плюс сколько ещё таких же.
    match accounts.iter().position(|a| reauth.iter().any(|k| *k == a.account_key())) {
        Some(idx) => {
            let a = &accounts[idx];
            let title = if a.email.is_empty() { a.account_key() } else { a.email.clone() };
            let rest = reauth.len().saturating_sub(1);
            let note = if rest > 0 {
                format!("{title} и ещё {rest}: почта, календарь и задачи не обновляются")
            } else {
                format!("{title}: почта, календарь и задачи не обновляются")
            };
            ui.set_reauth_index(idx as i32);
            ui.set_reauth_note(note.into());
        }
        None => {
            ui.set_reauth_index(-1);
            ui.set_reauth_note("".into());
        }
    }
    // Открытые настройки показывают «сессия истекла» сразу, а не после
    // повторного открытия.
    if ui.get_settings_visible() {
        refresh_connections(ui, sh);
    }
}

/// Как учётка ходит на сервер — подпись для строки в списках подключений.
pub(crate) fn account_mode(a: &engine::AccountConfig) -> &'static str {
    if a.native_url.is_some() {
        "сервер ddmail"
    } else if a.oauth_refresh_token.is_some() {
        "Google OAuth"
    } else {
        "IMAP/SMTP"
    }
}

/// Settings modal and connections: add / edit / re-login / delete a
/// connection, open settings, the global media switches.
pub(crate) fn wire_settings(ui: &MainWindow, shared: &Rc<Shared>) {
    // Settings modal: refresh the connections list and open on the first tab.
    let ui_weak_set = ui.as_weak();
    let sh_set = shared.clone();
    // Empty-state CTA → open the add-connection modal.
    let ui_weak_afc = ui.as_weak();
    ui.on_add_first_connection(move || {
        open_add_connection(ui_weak_afc.clone(), None);
    });

    // Settings → Подключения: add / edit / delete.
    let ui_weak_addc = ui.as_weak();
    ui.on_add_connection(move || {
        open_add_connection(ui_weak_addc.clone(), None);
    });
    let ui_weak_editc = ui.as_weak();
    let sh_editc = shared.clone();
    ui.on_edit_connection(move |idx| {
        let key = sh_editc.accounts.settings_conn_keys.borrow().get(idx.max(0) as usize).cloned();
        let Some(key) = key else { return };
        let cfg = engine::AccountConfig::load_all().into_iter().find(|a| a.account_key() == key);
        open_add_connection(ui_weak_editc.clone(), cfg);
    });
    // Плашка «сессия истекла» и строка под индикатором связи: тот же вход,
    // что «Изменить», но по индексу списка учёток (он существует с первого
    // события связи, тогда как список настроек наполняется только при
    // открытии модалки).
    let ui_weak_relog = ui.as_weak();
    let sh_relog = shared.clone();
    ui.on_relogin(move |idx| {
        let key = sh_relog.accounts.conn_dot_keys.borrow().get(idx.max(0) as usize).cloned();
        let Some(key) = key else { return };
        let cfg = engine::AccountConfig::load_all().into_iter().find(|a| a.account_key() == key);
        open_add_connection(ui_weak_relog.clone(), cfg);
    });

    let ui_weak_delc = ui.as_weak();
    let sh_delc = shared.clone();
    ui.on_delete_connection(move |idx| {
        let Some(ui) = ui_weak_delc.upgrade() else { return };
        let key = sh_delc.accounts.settings_conn_keys.borrow().get(idx.max(0) as usize).cloned();
        let Some(key) = key else { return };
        engine::AccountConfig::remove_account(&key);
        rebuild_engine(&ui, &sh_delc);
        refresh_connections(&ui, &sh_delc);
    });

    // Настройки открываются не только шестерёнкой: строка в списке индикатора
    // связи и правый клик по табу папки ставят `settings-visible` прямо в
    // Slint. Список подключений обновляется на любое открытие — иначе там
    // висело «Добавить первое подключение» при живых учётках, а «Изменить»
    // молча ничего не находило.
    let ui_weak_shown = ui.as_weak();
    let sh_shown = shared.clone();
    ui.on_settings_shown(move || {
        let Some(ui) = ui_weak_shown.upgrade() else { return };
        refresh_connections(&ui, &sh_shown);
    });

    ui.on_open_settings(move || {
        let Some(ui) = ui_weak_set.upgrade() else { return };
        refresh_connections(&ui, &sh_set);
        ui.set_settings_tab(0);
        ui.set_settings_visible(true);
    });
    // Global media-policy toggles from the settings «Внешний контент» tab. Same
    // effect as the per-message «Медиа…» allow-alls, minus the row context,
    // so no body is needed.
    let ui_weak_mg = ui.as_weak();
    let sh_mg = shared.clone();
    ui.on_set_media_global(move |which| {
        let gen_now = {
            let mut p = sh_mg.policy.borrow_mut();
            match which.as_str() {
                "allow-all" => p.allow_all = !p.allow_all,
                "images-all" => p.allow_all_media = !p.allow_all_media,
                other => {
                    println!("media global {other} — not wired");
                    return;
                }
            }
            // Generation must change atomically with the policy so the
            // texture cache key invalidates exactly the affected rows.
            p.generation += 1;
            let g = p.generation;
            policy::save(&p);
            g
        };
        sh_mg.policy_gen.set(gen_now);
        if let Some(ui) = ui_weak_mg.upgrade() {
            sync_media_globals(&ui, &sh_mg.policy.borrow());
        }
        // Repaint the open conversation under the new policy — no refetch.
        let bodies = sh_mg.current_bodies.borrow().clone();
        send_render_job(&sh_mg, bodies, None);
    });
}
