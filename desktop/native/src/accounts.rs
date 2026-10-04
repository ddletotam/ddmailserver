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
    let key = cache.account_keys().ok()?.into_iter().next()?;
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

/// Close the add-connection modal and rebuild the engine from the updated
/// accounts.json. Safe to call from any thread — it hops to the UI loop.
pub(crate) fn finish_add_connection(main_weak: slint::Weak<MainWindow>) {
    let _ = slint::invoke_from_event_loop(move || {
        SHARED.with(|s| {
            if let Some(sh) = s.borrow().as_ref() {
                if let Some(lw) = sh.accounts.add_conn_window.borrow_mut().take() {
                    let _ = lw.hide();
                }
                if let Some(m) = main_weak.upgrade() {
                    rebuild_engine(&m, sh);
                    refresh_connections(&m, sh);
                }
            }
        });
    });
}

/// Open the add-connection modal from the running app. Native (server/login/
/// pass → JWT), standalone IMAP/SMTP (+optional CalDAV/CardDAV), or Google
/// OAuth. On success the connection is APPENDED to accounts.json (never
/// clobbering the others) and the engine rebuilds. The window is retained in
/// Shared while open.
pub(crate) fn open_add_connection(
    main_weak: slint::Weak<MainWindow>,
    prefill: Option<engine::AccountConfig>,
) {
    let Ok(lw) = LoginWindow::new() else { return };

    // Editing: pre-fill the form. (Native passwords/tokens aren't stored, so
    // the user re-enters the password to re-authenticate; a changed host/login
    // creates a new key — same host/login replaces in place.)
    if let Some(c) = &prefill {
        if c.native_url.is_some() {
            lw.set_mode(0);
            lw.set_server_url(c.native_url.clone().unwrap_or_default().into());
            lw.set_username(c.username.clone().into());
        } else {
            lw.set_mode(1);
            lw.set_email(c.email.clone().into());
            lw.set_imap_host(c.host.clone().into());
            lw.set_imap_port(c.port.to_string().into());
            lw.set_username(c.username.clone().into());
            lw.set_password(c.password.clone().into());
            lw.set_carddav_url(c.carddav_url.clone().unwrap_or_default().into());
            lw.set_caldav_url(c.caldav_url.clone().unwrap_or_default().into());
        }
    }

    let weak = lw.as_weak();
    let mw_submit = main_weak.clone();
    lw.on_submit(move || {
        let Some(lw) = weak.upgrade() else { return };
        if lw.get_busy() {
            return;
        }
        let username = lw.get_username().trim().to_string();
        let password = lw.get_password().to_string();

        // ── Standalone IMAP/SMTP: persist directly, no server login. ──
        if lw.get_mode() == 1 {
            let email = lw.get_email().trim().to_string();
            let host = lw.get_imap_host().trim().to_string();
            if email.is_empty() || host.is_empty() || username.is_empty() || password.is_empty() {
                lw.set_error("Заполните email, IMAP-сервер, логин и пароль".into());
                return;
            }
            let port: u16 = lw.get_imap_port().trim().parse().unwrap_or(993);
            let opt = |s: String| {
                let t = s.trim().to_string();
                if t.is_empty() { None } else { Some(t) }
            };
            let cfg = engine::AccountConfig {
                host: host.clone(),
                port,
                username,
                password,
                use_tls: true,
                email,
                smtp_host: host,
                smtp_port: 465,
                native_url: None,
                native_token: None,
                carddav_url: opt(lw.get_carddav_url().to_string()),
                caldav_url: opt(lw.get_caldav_url().to_string()),
                oauth_refresh_token: None,
            };
            engine::AccountConfig::add_account(&cfg);
            finish_add_connection(mw_submit.clone());
            return;
        }

        // ── Native (our server): try username+password → JWT. On failure the
        // user can switch to the "Другой (IMAP)" tab (the manual fallback). ──
        let server = lw.get_server_url().trim().trim_end_matches('/').to_string();
        if server.is_empty() || username.is_empty() || password.is_empty() {
            lw.set_error("Заполните все поля".into());
            return;
        }
        let server = if server.starts_with("http://") || server.starts_with("https://") {
            server
        } else {
            format!("https://{server}")
        };
        lw.set_error("".into());
        lw.set_busy(true);

        let weak = lw.as_weak();
        let mw = mw_submit.clone();
        std::thread::spawn(move || {
            let result =
                tokio::runtime::Runtime::new().map_err(|e| format!("tokio: {e}")).and_then(|rt| {
                    rt.block_on(ddmail_core::auth::login(&server, &username, &password))
                });
            match result {
                Ok(login) => {
                    let cfg = engine::AccountConfig {
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
                    };
                    engine::AccountConfig::add_account(&cfg);
                    finish_add_connection(mw);
                }
                Err(e) => {
                    // Fallback hint: connect failed → offer the manual IMAP form.
                    let _ = weak.upgrade_in_event_loop(move |lw| {
                        lw.set_busy(false);
                        lw.set_error(format!("{e} — попробуйте вкладку «Другой (IMAP)»").into());
                    });
                }
            }
        });
    });

    lw.set_google_available(ddmail_core::oauth::load_client_creds().is_some());
    let gweak = lw.as_weak();
    let mw_google = main_weak.clone();
    lw.on_google_login(move || {
        let Some(lw) = gweak.upgrade() else { return };
        if lw.get_busy() {
            return;
        }
        lw.set_error("".into());
        lw.set_busy(true);
        let weak = lw.as_weak();
        let mw = mw_google.clone();
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
            match result {
                Ok((tokens, email)) => {
                    let cfg = engine::AccountConfig {
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
                    };
                    engine::AccountConfig::add_account(&cfg);
                    finish_add_connection(mw);
                }
                Err(e) => {
                    let _ = weak.upgrade_in_event_loop(move |lw| {
                        lw.set_busy(false);
                        lw.set_error(e.into());
                    });
                }
            }
        });
    });

    let _ = lw.show();
    SHARED.with(|s| {
        if let Some(sh) = s.borrow().as_ref() {
            *sh.accounts.add_conn_window.borrow_mut() = Some(lw);
        }
    });
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
    } else {
        ui.set_engine_reloading(false);
    }
    *shared.engine_tx.borrow_mut() = Some(etx);
}

/// Rebuild the Settings → Подключения list from accounts.json, keeping a
/// parallel key list for edit/delete-by-index.
pub(crate) fn refresh_connections(ui: &MainWindow, shared: &Rc<Shared>) {
    let accounts = engine::AccountConfig::load_all();
    let mut rows: Vec<ConnRow> = Vec::with_capacity(accounts.len());
    let mut keys: Vec<String> = Vec::with_capacity(accounts.len());
    let reauth = shared.accounts.reauth.borrow();
    for a in &accounts {
        let key = a.account_key();
        let title = if a.email.is_empty() { key.clone() } else { a.email.clone() };
        let needs_login = reauth.iter().any(|k| *k == key);
        let subtitle = if needs_login {
            // Кнопка «Изменить» рядом открывает форму входа с подставленным
            // сервером и логином — строка говорит, что от неё нужно.
            "сессия истекла — «Изменить» и ввести пароль".to_string()
        } else {
            format!("{} · {}", a.host, account_mode(a))
        };
        rows.push(ConnRow { title: title.into(), subtitle: subtitle.into(), needs_login });
        keys.push(key);
    }
    drop(reauth);
    *shared.accounts.settings_conn_keys.borrow_mut() = keys;
    ui.set_connections(ModelRc::new(VecModel::from(rows)));
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
                "сессия истекла — войти заново".to_string()
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
}

/// Как учётка ходит на сервер — подпись для строки в списках подключений.
pub(crate) fn account_mode(a: &engine::AccountConfig) -> &'static str {
    if a.native_url.is_some() {
        "наш сервер"
    } else if a.oauth_refresh_token.is_some() {
        "Google OAuth"
    } else {
        "IMAP/SMTP"
    }
}

/// Settings modal and connections: add / edit / re-login / delete a
/// connection, open settings, the global media switches.
pub(crate) fn wire_settings(ui: &MainWindow, shared: &Rc<Shared>) {
    // Settings modal: populate the read-only connection section from the
    // live config (env first, then on-disk profile) and show it.
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

    ui.on_open_settings(move || {
        let Some(ui) = ui_weak_set.upgrade() else { return };
        let cfg = engine::AccountConfig::load_all().into_iter().next();
        match &cfg {
            Some(c) => {
                let account = if c.email.is_empty() {
                    format!("{}@{}", c.username, c.host)
                } else {
                    c.email.clone()
                };
                ui.set_conn_account(account.into());
                ui.set_conn_mode("Онлайн — IMAP/SMTP".into());
                ui.set_conn_imap(
                    format!(
                        "{}:{} · {}",
                        c.host,
                        c.port,
                        if c.use_tls { "TLS" } else { "без TLS" }
                    )
                    .into(),
                );
                ui.set_conn_smtp(format!("{}:{}", c.smtp_host, c.smtp_port).into());
                ui.set_conn_native(c.native_url.clone().unwrap_or_default().into());
            }
            None => {
                ui.set_conn_account(sh_set.key.clone().into());
                ui.set_conn_mode("Только локальный кэш (IMAP не настроен)".into());
                ui.set_conn_imap("".into());
                ui.set_conn_smtp("".into());
                ui.set_conn_native("".into());
            }
        }
        refresh_connections(&ui, &sh_set);
        ui.set_settings_tab(0);
        ui.set_settings_visible(true);
    });
    // Global media-policy toggles from the settings «Контент» tab. Same
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
