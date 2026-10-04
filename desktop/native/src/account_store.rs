//! Хранение учёток: несекретное — в `accounts.json`, секреты — в хранилище ОС.
//!
//! Секреты учётки — пароль IMAP, JWT нативного сервера и refresh-токен Google —
//! живут в keyring (Windows Credential Manager / Secret Service на Linux) под
//! service [`SERVICE`] и именем `<account_key>:<поле>`. В файле остаётся
//! только то, что не жалко показать: хост, логин, адреса, порты и `auth` —
//! какой из секретов учётке нужен, чтобы пропажа секрета из keyring не меняла
//! тип учётки (нативная без токена должна дойти до плашки «войдите заново», а
//! не молча стать IMAP-учёткой с пустым паролем).
//!
//! Поля секретов в [`StoredAccount`] — `skip_serializing`: старые файлы с
//! паролями открытым текстом читаются, но записать секрет этими полями нельзя
//! физически. Единственный путь секрета на диск — явный блок
//! `plaintext_secrets`, и только когда keyring отказал (headless Linux без
//! Secret Service, переполненный blob Credential Manager): учётка без секрета
//! хуже учётки с секретом в файле, а терять учётки нельзя.
//!
//! Миграция — побочный эффект любой загрузки: всё, что лежит в файле открытым
//! текстом, кладётся в keyring, и файл переписывается без него. Запись —
//! атомарная (tmp в той же папке + fsync + rename), а чтение-правка-запись
//! сериализованы мьютексом процесса и файловой блокировкой `accounts.lock`
//! (второй процесс клиента single-instance не пускает, но его гард молча
//! пропускается, если сам не создался).

use std::collections::HashMap;
use std::io::Write;
use std::path::{Path, PathBuf};
use std::sync::Mutex;
use std::sync::atomic::{AtomicBool, Ordering};

use serde::{Deserialize, Serialize};

use crate::engine::AccountConfig;

/// Service, под которым секреты лежат в keyring.
pub const SERVICE: &str = "ru.letotam.ddmail";

const ACCOUNTS_FILE: &str = "accounts.json";
const LEGACY_FILE: &str = "account.json";
const LOCK_FILE: &str = "accounts.lock";

/// Какой секрет учётки.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum SecretKind {
    Password,
    NativeToken,
    OauthRefresh,
}

impl SecretKind {
    const ALL: [SecretKind; 3] =
        [SecretKind::Password, SecretKind::NativeToken, SecretKind::OauthRefresh];

    fn name(self) -> &'static str {
        match self {
            SecretKind::Password => "password",
            SecretKind::NativeToken => "native_token",
            SecretKind::OauthRefresh => "oauth_refresh_token",
        }
    }
}

/// Имя секрета в keyring.
pub fn secret_id(account_key: &str, kind: SecretKind) -> String {
    format!("{account_key}:{}", kind.name())
}

#[derive(Debug, Clone)]
pub struct StoreError {
    pub msg: String,
}

impl std::fmt::Display for StoreError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.msg)
    }
}

/// Хранилище секретов. Боевое — [`KeyringStore`], в тестах — in-memory.
pub trait SecretStore: Send + Sync {
    /// `Ok(None)` — записи нет; `Err` — спросить не удалось.
    fn get(&self, id: &str) -> Result<Option<String>, StoreError>;
    fn set(&self, id: &str, value: &str) -> Result<(), StoreError>;
    /// Удаление отсутствующей записи — не ошибка.
    fn delete(&self, id: &str) -> Result<(), StoreError>;
}

/// keyring ОС. После первого отказа «хранилища нет» больше не дёргается до
/// конца процесса: на headless Linux каждая попытка — это поход в D-Bus, а
/// `load_all` зовут на каждый чих UI.
pub struct KeyringStore {
    down: AtomicBool,
}

impl KeyringStore {
    pub const fn new() -> Self {
        Self { down: AtomicBool::new(false) }
    }

    fn fail(&self, op: &str, e: keyring::Error) -> StoreError {
        use keyring::Error as E;
        let unavailable =
            matches!(e, E::NoDefaultStore | E::NoStorageAccess(_) | E::PlatformFailure(_));
        if unavailable && !self.down.swap(true, Ordering::SeqCst) {
            log::warn!(
                "keyring недоступен ({op}: {e}); секреты учёток остаются в accounts.json открытым текстом до следующего запуска"
            );
        }
        StoreError { msg: format!("keyring {op}: {e}") }
    }

    fn entry(&self, id: &str) -> Result<keyring::Entry, StoreError> {
        if self.down.load(Ordering::SeqCst) {
            return Err(StoreError { msg: "keyring недоступен".into() });
        }
        keyring::Entry::new(SERVICE, id).map_err(|e| self.fail("open", e))
    }
}

impl SecretStore for KeyringStore {
    fn get(&self, id: &str) -> Result<Option<String>, StoreError> {
        // Байты, а не get_password: Credential Manager хранит «пароль» в
        // UTF-16, и лимит blob'а (2560 байт) съедался бы вдвое быстрее.
        match self.entry(id)?.get_secret() {
            Ok(b) => String::from_utf8(b)
                .map(Some)
                .map_err(|_| StoreError { msg: format!("{id}: не UTF-8") }),
            Err(keyring::Error::NoEntry) => Ok(None),
            Err(e) => Err(self.fail("get", e)),
        }
    }

    fn set(&self, id: &str, value: &str) -> Result<(), StoreError> {
        let entry = self.entry(id)?;
        entry.set_secret(value.as_bytes()).map_err(|e| self.fail("set", e))?;
        // Сверка: секрет уходит из файла только если keyring вернул ровно его.
        match entry.get_secret() {
            Ok(b) if b == value.as_bytes() => Ok(()),
            Ok(_) => Err(StoreError { msg: format!("{id}: keyring вернул не то") }),
            Err(e) => Err(self.fail("verify", e)),
        }
    }

    fn delete(&self, id: &str) -> Result<(), StoreError> {
        match self.entry(id)?.delete_credential() {
            Ok(()) | Err(keyring::Error::NoEntry) => Ok(()),
            Err(e) => Err(self.fail("delete", e)),
        }
    }
}

/// Кэш поверх хранилища: секрет спрашивается у ОС один раз за процесс.
/// `None` в карте — «точно нет записи». Ошибки не кэшируются.
pub struct CachedStore<S> {
    inner: S,
    cache: Mutex<HashMap<String, Option<String>>>,
}

impl<S: SecretStore> CachedStore<S> {
    pub fn new(inner: S) -> Self {
        Self { inner, cache: Mutex::new(HashMap::new()) }
    }

    fn cached(&self, id: &str) -> Option<Option<String>> {
        self.cache.lock().ok()?.get(id).cloned()
    }

    fn remember(&self, id: &str, v: Option<String>) {
        if let Ok(mut c) = self.cache.lock() {
            c.insert(id.to_string(), v);
        }
    }
}

impl<S: SecretStore> SecretStore for CachedStore<S> {
    fn get(&self, id: &str) -> Result<Option<String>, StoreError> {
        if let Some(v) = self.cached(id) {
            return Ok(v);
        }
        let v = self.inner.get(id)?;
        self.remember(id, v.clone());
        Ok(v)
    }

    fn set(&self, id: &str, value: &str) -> Result<(), StoreError> {
        if self.cached(id).flatten().as_deref() == Some(value) {
            return Ok(());
        }
        self.inner.set(id, value)?;
        self.remember(id, Some(value.to_string()));
        Ok(())
    }

    fn delete(&self, id: &str) -> Result<(), StoreError> {
        self.inner.delete(id)?;
        self.remember(id, None);
        Ok(())
    }
}

/// Боевое хранилище процесса.
pub fn os_store() -> &'static dyn SecretStore {
    static STORE: std::sync::OnceLock<CachedStore<KeyringStore>> = std::sync::OnceLock::new();
    STORE.get_or_init(|| CachedStore::new(KeyringStore::new()))
}

// ── Формат файла ──

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
enum AuthKind {
    Password,
    Native,
    Oauth,
}

impl AuthKind {
    fn secret(self) -> SecretKind {
        match self {
            AuthKind::Password => SecretKind::Password,
            AuthKind::Native => SecretKind::NativeToken,
            AuthKind::Oauth => SecretKind::OauthRefresh,
        }
    }

    fn of(cfg: &AccountConfig) -> Self {
        if cfg.oauth_refresh_token.is_some() {
            AuthKind::Oauth
        } else if cfg.native_url.is_some() && cfg.native_token.is_some() {
            AuthKind::Native
        } else {
            AuthKind::Password
        }
    }
}

#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
struct PlainSecrets {
    #[serde(default, skip_serializing_if = "Option::is_none")]
    password: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    native_token: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    oauth_refresh_token: Option<String>,
}

impl PlainSecrets {
    fn slot(&mut self, kind: SecretKind) -> &mut Option<String> {
        match kind {
            SecretKind::Password => &mut self.password,
            SecretKind::NativeToken => &mut self.native_token,
            SecretKind::OauthRefresh => &mut self.oauth_refresh_token,
        }
    }

    fn is_empty(&self) -> bool {
        self.password.is_none() && self.native_token.is_none() && self.oauth_refresh_token.is_none()
    }
}

fn d_imap_port() -> u16 {
    993
}
fn d_smtp_port() -> u16 {
    465
}
fn d_true() -> bool {
    true
}

/// Запись учётки на диске.
#[derive(Clone, Debug, Serialize, Deserialize)]
struct StoredAccount {
    host: String,
    #[serde(default = "d_imap_port")]
    port: u16,
    #[serde(default = "d_true")]
    use_tls: bool,
    username: String,
    #[serde(default)]
    email: Option<String>,
    #[serde(default)]
    smtp_host: Option<String>,
    #[serde(default = "d_smtp_port")]
    smtp_port: u16,
    #[serde(default)]
    native_url: Option<String>,
    #[serde(default)]
    carddav_url: Option<String>,
    #[serde(default)]
    caldav_url: Option<String>,
    /// Нет у файлов до keyring — тогда выводится из того, какие секреты есть.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    auth: Option<AuthKind>,

    // Секреты старого формата: читаются для миграции, не пишутся НИКОГДА.
    #[serde(default, skip_serializing)]
    password: Option<String>,
    #[serde(default, skip_serializing)]
    native_token: Option<String>,
    #[serde(default, skip_serializing)]
    oauth_refresh_token: Option<String>,

    /// Деградация: keyring отказал, секрет лежит здесь открытым текстом.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    plaintext_secrets: Option<PlainSecrets>,
}

impl StoredAccount {
    fn account_key(&self) -> String {
        ddmail_core::imap::account_key(&self.host, &self.username)
    }

    /// Секрет, лежащий в самом файле (старое поле или блок деградации).
    fn inline(&self, kind: SecretKind) -> Option<String> {
        let legacy = match kind {
            SecretKind::Password => &self.password,
            SecretKind::NativeToken => &self.native_token,
            SecretKind::OauthRefresh => &self.oauth_refresh_token,
        };
        let plain = self.plaintext_secrets.as_ref().and_then(|p| match kind {
            SecretKind::Password => p.password.clone(),
            SecretKind::NativeToken => p.native_token.clone(),
            SecretKind::OauthRefresh => p.oauth_refresh_token.clone(),
        });
        legacy.clone().or(plain).filter(|s| !s.is_empty())
    }

    fn has_inline_secrets(&self) -> bool {
        SecretKind::ALL.iter().any(|k| self.inline(*k).is_some())
    }

    /// Тип входа: записанный, а для старого файла — по тем же признакам, что
    /// и раньше (refresh-токен ⇒ OAuth, адрес сервера + токен ⇒ нативная).
    /// `None` — старая запись без единого секрета, её и раньше пропускали.
    fn auth_kind(&self) -> Option<AuthKind> {
        if let Some(a) = self.auth {
            return Some(a);
        }
        if self.oauth_refresh_token.is_some() {
            Some(AuthKind::Oauth)
        } else if self.native_url.is_some() && self.native_token.is_some() {
            Some(AuthKind::Native)
        } else if self.password.is_some() {
            Some(AuthKind::Password)
        } else {
            None
        }
    }

    fn from_config(cfg: &AccountConfig) -> Self {
        StoredAccount {
            host: cfg.host.clone(),
            port: cfg.port,
            use_tls: cfg.use_tls,
            username: cfg.username.clone(),
            email: Some(cfg.email.clone()),
            smtp_host: Some(cfg.smtp_host.clone()),
            smtp_port: cfg.smtp_port,
            native_url: cfg.native_url.clone(),
            carddav_url: cfg.carddav_url.clone(),
            caldav_url: cfg.caldav_url.clone(),
            auth: Some(AuthKind::of(cfg)),
            password: None,
            native_token: None,
            oauth_refresh_token: None,
            plaintext_secrets: None,
        }
    }

    /// Собрать конфиг: секрет — из файла, иначе из хранилища. Пропавший секрет
    /// — пустая строка того же типа входа: учётка остаётся в списке и
    /// попросит войти заново, а не исчезнет.
    fn to_config(&self, store: &dyn SecretStore) -> Option<AccountConfig> {
        let auth = self.auth_kind()?;
        let key = self.account_key();
        let kind = auth.secret();
        let secret = self.inline(kind).or_else(|| match store.get(&secret_id(&key, kind)) {
            Ok(v) => v,
            Err(e) => {
                log::warn!("секрет {} для {key}: {e}", kind.name());
                None
            }
        });
        let secret = secret.unwrap_or_else(|| {
            log::warn!("для {key} нет секрета {} — понадобится вход заново", kind.name());
            String::new()
        });
        let mut password = String::new();
        let mut native_token = None;
        let mut oauth_refresh_token = None;
        match auth {
            AuthKind::Password => password = secret,
            AuthKind::Native => native_token = Some(secret),
            AuthKind::Oauth => oauth_refresh_token = Some(secret),
        }
        Some(AccountConfig {
            host: self.host.clone(),
            port: self.port,
            username: self.username.clone(),
            password,
            use_tls: self.use_tls,
            email: self.email.clone().unwrap_or_else(|| self.username.clone()),
            smtp_host: self.smtp_host.clone().unwrap_or_else(|| self.host.clone()),
            smtp_port: self.smtp_port,
            native_url: self.native_url.clone(),
            native_token,
            carddav_url: self.carddav_url.clone(),
            caldav_url: self.caldav_url.clone(),
            oauth_refresh_token,
        })
    }
}

/// Секреты конфига, которые есть смысл хранить.
fn secrets_of(cfg: &AccountConfig) -> Vec<(SecretKind, &str)> {
    let mut v = Vec::new();
    if !cfg.password.is_empty() {
        v.push((SecretKind::Password, cfg.password.as_str()));
    }
    if let Some(t) = cfg.native_token.as_deref().filter(|t| !t.is_empty()) {
        v.push((SecretKind::NativeToken, t));
    }
    if let Some(t) = cfg.oauth_refresh_token.as_deref().filter(|t| !t.is_empty()) {
        v.push((SecretKind::OauthRefresh, t));
    }
    v
}

/// Запись для диска: секреты — в хранилище, а что не влезло — в
/// `plaintext_secrets`. Пустые секреты из хранилища НЕ удаляются: пустая
/// строка в памяти бывает и от того, что keyring не ответил на чтении, и
/// удаление тогда стёрло бы живой секрет. Чистит только `remove`.
fn stored_with_secrets(cfg: &AccountConfig, store: &dyn SecretStore) -> StoredAccount {
    let mut rec = StoredAccount::from_config(cfg);
    let key = rec.account_key();
    let mut plain = PlainSecrets::default();
    for (kind, value) in secrets_of(cfg) {
        if let Err(e) = store.set(&secret_id(&key, kind), value) {
            log::warn!("{key}: {} не лёг в keyring ({e}) — остаётся в файле", kind.name());
            *plain.slot(kind) = Some(value.to_string());
        }
    }
    if !plain.is_empty() {
        rec.plaintext_secrets = Some(plain);
    }
    rec
}

// ── Файл ──
/// Папка конфига клиента: `%APPDATA%/ru.letotam.ddmail` или `$HOME/ru.letotam.ddmail`.
pub fn config_dir() -> Option<PathBuf> {
    let base = std::env::var("APPDATA").or_else(|_| std::env::var("HOME")).ok()?;
    Some(Path::new(&base).join("ru.letotam.ddmail"))
}

/// Атомарная запись: tmp в той же папке (rename между томами не атомарен),
/// fsync, rename поверх. Сбой посреди оставляет старый файл целым.
pub(crate) fn write_atomic(path: &Path, data: &[u8]) -> std::io::Result<()> {
    let dir = path.parent().ok_or_else(|| std::io::Error::other("нет родительской папки"))?;
    std::fs::create_dir_all(dir)?;
    let name = path.file_name().and_then(|n| n.to_str()).unwrap_or("file");
    // pid + счётчик: два потока одного процесса не делят tmp даже без замка.
    static SEQ: std::sync::atomic::AtomicU64 = std::sync::atomic::AtomicU64::new(0);
    let seq = SEQ.fetch_add(1, Ordering::Relaxed);
    let tmp = dir.join(format!(".{name}.tmp-{}-{seq}", std::process::id()));
    let res = (|| {
        let mut opts = std::fs::OpenOptions::new();
        opts.write(true).create(true).truncate(true);
        #[cfg(unix)]
        {
            use std::os::unix::fs::OpenOptionsExt;
            opts.mode(0o600);
        }
        let mut f = opts.open(&tmp)?;
        f.write_all(data)?;
        f.sync_all()?;
        drop(f);
        // Windows: rename поверх файла, который держит открытым антивирус или
        // индексатор, падает с «отказано в доступе» — пара повторов.
        let mut last = None;
        for _ in 0..5 {
            match std::fs::rename(&tmp, path) {
                Ok(()) => {
                    last = None;
                    break;
                }
                Err(e) => {
                    last = Some(e);
                    std::thread::sleep(std::time::Duration::from_millis(30));
                }
            }
        }
        if let Some(e) = last {
            return Err(e);
        }
        #[cfg(unix)]
        if let Ok(d) = std::fs::File::open(dir) {
            let _ = d.sync_all();
        }
        Ok(())
    })();
    if res.is_err() {
        let _ = std::fs::remove_file(&tmp);
    }
    res
}

fn to_text(records: &[StoredAccount]) -> Option<String> {
    serde_json::to_string_pretty(records).ok()
}

/// Записать, только если содержимое изменилось.
fn write_if_changed(path: &Path, text: &str) {
    if std::fs::read_to_string(path).ok().as_deref() == Some(text) {
        return;
    }
    if let Err(e) = write_atomic(path, text.as_bytes()) {
        log::warn!("{}: запись не удалась: {e}", path.display());
    }
}

/// Записи из `accounts.json`. Битая запись пропускается, как и раньше.
fn read_records(path: &Path) -> Option<Vec<StoredAccount>> {
    let data = std::fs::read_to_string(path).ok()?;
    let serde_json::Value::Array(arr) = serde_json::from_str(&data).ok()? else { return None };
    Some(arr.into_iter().filter_map(|v| serde_json::from_value(v).ok()).collect())
}

/// Сериализация чтения-правки-записи: мьютекс между потоками процесса и
/// блокировка файла между процессами.
pub struct Locked {
    _thread: std::sync::MutexGuard<'static, ()>,
    _file: Option<std::fs::File>,
}

pub fn lock(dir: &Path) -> Locked {
    static LOCK: Mutex<()> = Mutex::new(());
    let guard = LOCK.lock().unwrap_or_else(|p| p.into_inner());
    let _ = std::fs::create_dir_all(dir);
    let file = std::fs::OpenOptions::new()
        .create(true)
        .truncate(false)
        .write(true)
        .open(dir.join(LOCK_FILE))
        .ok()
        .filter(|f| f.lock().is_ok());
    Locked { _thread: guard, _file: file }
}

/// Загрузить все учётки из `dir`, попутно переложив секреты из файла в
/// хранилище. Вызывать под [`lock`].
pub fn load_all_in(dir: &Path, store: &dyn SecretStore) -> Vec<AccountConfig> {
    let path = dir.join(ACCOUNTS_FILE);
    let Some(records) = read_records(&path) else { return Vec::new() };
    let accounts: Vec<AccountConfig> = records.iter().filter_map(|r| r.to_config(store)).collect();
    if records.iter().any(StoredAccount::has_inline_secrets) {
        // Пересохранение = миграция: секреты уходят в хранилище, файл
        // переписывается без них (или с блоком деградации — тогда байты те
        // же и записи не будет). Битые и бессекретные старые записи при этом
        // выпадают — их и раньше никто не видел.
        save_all_in(dir, &accounts, store);
        scrub_backups(dir);
    }
    accounts
}

/// Перезаписать `accounts.json` набором. Вызывать под [`lock`].
pub fn save_all_in(dir: &Path, accounts: &[AccountConfig], store: &dyn SecretStore) {
    let records: Vec<StoredAccount> =
        accounts.iter().map(|a| stored_with_secrets(a, store)).collect();
    if let Some(text) = to_text(&records) {
        write_if_changed(&dir.join(ACCOUNTS_FILE), &text);
    }
}

/// Удалить учётку и её секреты. Вызывать под [`lock`].
pub fn remove_in(dir: &Path, account_key: &str, store: &dyn SecretStore) {
    let all: Vec<AccountConfig> =
        load_all_in(dir, store).into_iter().filter(|a| a.account_key() != account_key).collect();
    save_all_in(dir, &all, store);
    forget_secrets(account_key, store);
}

fn forget_secrets(account_key: &str, store: &dyn SecretStore) {
    for kind in SecretKind::ALL {
        if let Err(e) = store.delete(&secret_id(account_key, kind)) {
            log::warn!("{account_key}: {} не удалён из keyring: {e}", kind.name());
        }
    }
}

/// Стереть из хранилища секреты всех учёток `dir` (деинсталляция). Файлы не
/// трогает — их удаляет сам деинсталлятор.
pub fn forget_all_in(dir: &Path, store: &dyn SecretStore) {
    let mut keys: Vec<String> = read_records(&dir.join(ACCOUNTS_FILE))
        .unwrap_or_default()
        .iter()
        .map(StoredAccount::account_key)
        .collect();
    if let Some(r) = read_legacy(dir) {
        keys.push(r.account_key());
    }
    keys.sort();
    keys.dedup();
    for k in keys {
        forget_secrets(&k, store);
    }
}

// ── Старый одиночный account.json ──

fn read_legacy(dir: &Path) -> Option<StoredAccount> {
    let data = std::fs::read_to_string(dir.join(LEGACY_FILE)).ok()?;
    serde_json::from_str(&data).ok()
}

/// Учётка из старого одиночного `account.json`. Секрет из него переносится в
/// хранилище, а сам файл переписывается без секрета. Вызывать под [`lock`].
pub fn load_legacy_in(dir: &Path, store: &dyn SecretStore) -> Option<AccountConfig> {
    let rec = read_legacy(dir)?;
    let cfg = rec.to_config(store)?;
    if rec.has_inline_secrets() {
        let stored = stored_with_secrets(&cfg, store);
        if let Ok(text) = serde_json::to_string_pretty(&stored) {
            write_if_changed(&dir.join(LEGACY_FILE), &text);
        }
    }
    Some(cfg)
}

// ── Бэкапы ──

/// Сама программа бэкапов `accounts.json` не делает, но рядом бывают ручные
/// (`accounts.json.bak-before-relogin` и т.п.) — копии с паролями открытым
/// текстом, которые переживут миграцию. Секреты из них вычищаются на месте:
/// в keyring они не переносятся (там уже живые), остальное содержимое цело.
/// Нераспознанный файл не трогается.
fn scrub_backups(dir: &Path) {
    let Ok(rd) = std::fs::read_dir(dir) else { return };
    for ent in rd.flatten() {
        let name = ent.file_name().to_string_lossy().into_owned();
        let is_backup = (name.starts_with("accounts.json.") || name.starts_with("account.json."))
            && !name.ends_with(".tmp")
            && name.contains("bak");
        if !is_backup {
            continue;
        }
        let path = ent.path();
        let Ok(data) = std::fs::read_to_string(&path) else { continue };
        let Ok(mut v) = serde_json::from_str::<serde_json::Value>(&data) else { continue };
        if strip_secret_keys(&mut v) {
            if let Ok(text) = serde_json::to_string_pretty(&v) {
                match write_atomic(&path, text.as_bytes()) {
                    Ok(()) => log::info!("{name}: секреты вычищены"),
                    Err(e) => log::warn!("{name}: не удалось вычистить секреты: {e}"),
                }
            }
        }
    }
}

/// Убрать ключи секретов из объекта или массива объектов. `true` — что-то убрано.
fn strip_secret_keys(v: &mut serde_json::Value) -> bool {
    match v {
        serde_json::Value::Array(arr) => {
            arr.iter_mut().fold(false, |acc, x| strip_secret_keys(x) || acc)
        }
        serde_json::Value::Object(map) => {
            let mut any = false;
            for k in SecretKind::ALL.iter().map(|k| k.name()).chain(["plaintext_secrets"]) {
                any |= map.remove(k).is_some();
            }
            any
        }
        _ => false,
    }
}

#[cfg(test)]
pub(crate) mod tests {
    use super::*;

    /// In-memory хранилище; `broken` — имитация headless Linux без Secret Service.
    #[derive(Default)]
    pub struct MemStore {
        pub map: Mutex<HashMap<String, String>>,
        pub broken: bool,
    }

    impl MemStore {
        fn down() -> StoreError {
            StoreError { msg: "нет хранилища".into() }
        }
    }

    impl SecretStore for MemStore {
        fn get(&self, id: &str) -> Result<Option<String>, StoreError> {
            if self.broken {
                return Err(Self::down());
            }
            Ok(self.map.lock().unwrap().get(id).cloned())
        }
        fn set(&self, id: &str, value: &str) -> Result<(), StoreError> {
            if self.broken {
                return Err(Self::down());
            }
            self.map.lock().unwrap().insert(id.into(), value.into());
            Ok(())
        }
        fn delete(&self, id: &str) -> Result<(), StoreError> {
            if self.broken {
                return Err(Self::down());
            }
            self.map.lock().unwrap().remove(id);
            Ok(())
        }
    }

    const SECRETS: [&str; 3] = ["S3cretPass", "jwt.TOKEN.sig", "1//refresh-tok"];

    fn imap_cfg() -> AccountConfig {
        AccountConfig {
            host: "imap.example.org".into(),
            port: 993,
            username: "user".into(),
            password: SECRETS[0].into(),
            use_tls: true,
            email: "user@example.org".into(),
            smtp_host: "smtp.example.org".into(),
            smtp_port: 465,
            native_url: None,
            native_token: None,
            carddav_url: Some("https://dav.example.org/c/".into()),
            caldav_url: None,
            oauth_refresh_token: None,
        }
    }

    fn native_cfg() -> AccountConfig {
        AccountConfig {
            host: "mail.example.net".into(),
            username: "boss".into(),
            password: String::new(),
            email: "boss@example.net".into(),
            smtp_host: "mail.example.net".into(),
            native_url: Some("https://mail.example.net".into()),
            native_token: Some(SECRETS[1].into()),
            carddav_url: None,
            ..imap_cfg()
        }
    }

    fn oauth_cfg() -> AccountConfig {
        AccountConfig {
            host: "imap.gmail.com".into(),
            username: "someone@gmail.com".into(),
            password: String::new(),
            email: "someone@gmail.com".into(),
            smtp_host: "smtp.gmail.com".into(),
            oauth_refresh_token: Some(SECRETS[2].into()),
            carddav_url: None,
            ..imap_cfg()
        }
    }

    fn all_cfgs() -> Vec<AccountConfig> {
        vec![imap_cfg(), native_cfg(), oauth_cfg()]
    }

    /// Старый формат: ровно то, что писал прежний `to_json`.
    fn legacy_json(cfg: &AccountConfig) -> serde_json::Value {
        serde_json::json!({
            "host": cfg.host, "port": cfg.port, "use_tls": cfg.use_tls,
            "username": cfg.username, "password": cfg.password, "email": cfg.email,
            "smtp_host": cfg.smtp_host, "smtp_port": cfg.smtp_port,
            "native_url": cfg.native_url, "native_token": cfg.native_token,
            "carddav_url": cfg.carddav_url, "caldav_url": cfg.caldav_url,
            "oauth_refresh_token": cfg.oauth_refresh_token,
        })
    }

    fn assert_no_secrets(text: &str) {
        for s in SECRETS {
            assert!(!text.contains(s), "секрет {s} на диске: {text}");
        }
    }

    fn same(a: &AccountConfig, b: &AccountConfig) {
        assert_eq!(a.account_key(), b.account_key());
        assert_eq!(a.password, b.password);
        assert_eq!(a.native_token, b.native_token);
        assert_eq!(a.oauth_refresh_token, b.oauth_refresh_token);
        assert_eq!(a.native_url, b.native_url);
        assert_eq!(a.carddav_url, b.carddav_url);
        assert_eq!(a.email, b.email);
        assert_eq!(a.smtp_host, b.smtp_host);
    }

    #[test]
    fn serialization_never_writes_secret_fields() {
        // Даже запись, у которой секретные поля заполнены, их не выдаёт.
        let mut rec = StoredAccount::from_config(&imap_cfg());
        rec.password = Some(SECRETS[0].into());
        rec.native_token = Some(SECRETS[1].into());
        rec.oauth_refresh_token = Some(SECRETS[2].into());
        let text = serde_json::to_string(&rec).unwrap();
        assert_no_secrets(&text);
        let v: serde_json::Value = serde_json::from_str(&text).unwrap();
        for k in ["password", "native_token", "oauth_refresh_token", "plaintext_secrets"] {
            assert!(v.get(k).is_none(), "поле {k} на диске: {text}");
        }
    }

    #[test]
    fn save_puts_secrets_in_store_not_file() {
        let dir = tempfile::tempdir().unwrap();
        let store = MemStore::default();
        save_all_in(dir.path(), &all_cfgs(), &store);
        let text = std::fs::read_to_string(dir.path().join(ACCOUNTS_FILE)).unwrap();
        assert_no_secrets(&text);
        assert!(!text.contains("plaintext_secrets"));
        assert_eq!(store.map.lock().unwrap().len(), 3);
        let back = load_all_in(dir.path(), &store);
        assert_eq!(back.len(), 3);
        for (a, b) in back.iter().zip(all_cfgs().iter()) {
            same(a, b);
        }
    }

    #[test]
    fn legacy_plaintext_file_is_migrated() {
        let dir = tempfile::tempdir().unwrap();
        let arr = serde_json::Value::Array(all_cfgs().iter().map(legacy_json).collect());
        std::fs::write(dir.path().join(ACCOUNTS_FILE), arr.to_string()).unwrap();
        std::fs::write(dir.path().join("accounts.json.bak-before-relogin"), arr.to_string())
            .unwrap();

        let store = MemStore::default();
        let got = load_all_in(dir.path(), &store);
        assert_eq!(got.len(), 3);
        for (a, b) in got.iter().zip(all_cfgs().iter()) {
            same(a, b);
        }
        // Файл переписан без секретов, бэкап вычищен, tmp не осталось.
        let text = std::fs::read_to_string(dir.path().join(ACCOUNTS_FILE)).unwrap();
        assert_no_secrets(&text);
        let bak =
            std::fs::read_to_string(dir.path().join("accounts.json.bak-before-relogin")).unwrap();
        assert_no_secrets(&bak);
        assert!(bak.contains("imap.example.org"));
        let names: Vec<String> = std::fs::read_dir(dir.path())
            .unwrap()
            .map(|e| e.unwrap().file_name().to_string_lossy().into_owned())
            .collect();
        assert!(names.iter().all(|n| !n.contains(".tmp-")), "{names:?}");

        // Секреты — в хранилище под своими именами.
        let m = store.map.lock().unwrap();
        assert_eq!(
            m.get(&secret_id("user@imap.example.org", SecretKind::Password)).map(String::as_str),
            Some(SECRETS[0])
        );
        drop(m);

        // Повторная загрузка из «чистого» файла даёт то же самое.
        let again = load_all_in(dir.path(), &store);
        for (a, b) in again.iter().zip(all_cfgs().iter()) {
            same(a, b);
        }
    }

    #[test]
    fn broken_store_keeps_secrets_in_file() {
        let dir = tempfile::tempdir().unwrap();
        let arr = serde_json::Value::Array(all_cfgs().iter().map(legacy_json).collect());
        std::fs::write(dir.path().join(ACCOUNTS_FILE), arr.to_string()).unwrap();

        let store = MemStore { broken: true, ..Default::default() };
        let got = load_all_in(dir.path(), &store);
        assert_eq!(got.len(), 3, "учётки не теряются");
        for (a, b) in got.iter().zip(all_cfgs().iter()) {
            same(a, b);
        }
        // Секреты остались, но только в явном блоке деградации.
        let text = std::fs::read_to_string(dir.path().join(ACCOUNTS_FILE)).unwrap();
        let v: serde_json::Value = serde_json::from_str(&text).unwrap();
        assert_eq!(v[0]["plaintext_secrets"]["password"], SECRETS[0]);
        assert!(v[0].get("password").is_none());

        // Хранилище ожило — следующая загрузка доводит миграцию.
        let ok = MemStore::default();
        let got = load_all_in(dir.path(), &ok);
        for (a, b) in got.iter().zip(all_cfgs().iter()) {
            same(a, b);
        }
        let text = std::fs::read_to_string(dir.path().join(ACCOUNTS_FILE)).unwrap();
        assert_no_secrets(&text);
        assert!(!text.contains("plaintext_secrets"));
    }

    #[test]
    fn missing_secret_keeps_account_and_auth_kind() {
        let dir = tempfile::tempdir().unwrap();
        let store = MemStore::default();
        save_all_in(dir.path(), &all_cfgs(), &store);
        store.map.lock().unwrap().clear();
        let got = load_all_in(dir.path(), &store);
        assert_eq!(got.len(), 3);
        assert_eq!(got[0].password, "");
        // Нативная учётка остаётся нативной — провайдер поймёт 401 как «войти заново».
        assert_eq!(got[1].native_token.as_deref(), Some(""));
        assert_eq!(got[2].oauth_refresh_token.as_deref(), Some(""));
    }

    #[test]
    fn remove_forgets_secrets() {
        let dir = tempfile::tempdir().unwrap();
        let store = MemStore::default();
        save_all_in(dir.path(), &all_cfgs(), &store);
        remove_in(dir.path(), &native_cfg().account_key(), &store);
        let got = load_all_in(dir.path(), &store);
        assert_eq!(got.len(), 2);
        let m = store.map.lock().unwrap();
        assert!(!m.keys().any(|k| k.starts_with(&native_cfg().account_key())));
        assert_eq!(m.len(), 2);
    }

    #[test]
    fn forget_all_clears_store() {
        let dir = tempfile::tempdir().unwrap();
        let store = MemStore::default();
        save_all_in(dir.path(), &all_cfgs(), &store);
        forget_all_in(dir.path(), &store);
        assert!(store.map.lock().unwrap().is_empty());
    }

    #[test]
    fn legacy_single_account_file_is_scrubbed() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::write(dir.path().join(LEGACY_FILE), legacy_json(&imap_cfg()).to_string()).unwrap();
        let store = MemStore::default();
        let cfg = load_legacy_in(dir.path(), &store).unwrap();
        same(&cfg, &imap_cfg());
        let text = std::fs::read_to_string(dir.path().join(LEGACY_FILE)).unwrap();
        assert_no_secrets(&text);
        // И читается обратно уже из хранилища.
        same(&load_legacy_in(dir.path(), &store).unwrap(), &imap_cfg());
    }

    #[test]
    fn legacy_record_without_any_secret_is_skipped_as_before() {
        let dir = tempfile::tempdir().unwrap();
        let v = serde_json::json!([{ "host": "h", "username": "u" }]);
        std::fs::write(dir.path().join(ACCOUNTS_FILE), v.to_string()).unwrap();
        assert!(load_all_in(dir.path(), &MemStore::default()).is_empty());
    }

    #[test]
    fn cached_store_asks_backend_once() {
        struct Counting(std::sync::atomic::AtomicUsize);
        impl SecretStore for Counting {
            fn get(&self, _: &str) -> Result<Option<String>, StoreError> {
                self.0.fetch_add(1, Ordering::SeqCst);
                Ok(Some("v".into()))
            }
            fn set(&self, _: &str, _: &str) -> Result<(), StoreError> {
                self.0.fetch_add(1, Ordering::SeqCst);
                Ok(())
            }
            fn delete(&self, _: &str) -> Result<(), StoreError> {
                Ok(())
            }
        }
        let c = CachedStore::new(Counting(Default::default()));
        assert_eq!(c.get("a").unwrap().as_deref(), Some("v"));
        assert_eq!(c.get("a").unwrap().as_deref(), Some("v"));
        c.set("a", "v").unwrap();
        assert_eq!(c.inner.0.load(Ordering::SeqCst), 1);
    }

    /// Настоящий keyring станции: `cargo test -- --ignored os_keyring`.
    /// Пишет и стирает только служебную запись `selftest:*`.
    #[test]
    #[ignore]
    fn os_keyring_roundtrip() {
        let k = KeyringStore::new();
        let id = format!("selftest:{}", std::process::id());
        let long = "x".repeat(2000);
        k.set(&id, &long).unwrap();
        assert_eq!(k.get(&id).unwrap().as_deref(), Some(long.as_str()));
        k.delete(&id).unwrap();
        assert_eq!(k.get(&id).unwrap(), None);
        k.delete(&id).unwrap();
    }

    #[test]
    fn atomic_write_replaces_existing() {
        let dir = tempfile::tempdir().unwrap();
        let p = dir.path().join("x.json");
        write_atomic(&p, b"one").unwrap();
        write_atomic(&p, b"two").unwrap();
        assert_eq!(std::fs::read_to_string(&p).unwrap(), "two");
        assert_eq!(std::fs::read_dir(dir.path()).unwrap().count(), 1);
    }

    #[test]
    fn concurrent_adds_lose_nothing() {
        let dir = tempfile::tempdir().unwrap();
        let store = std::sync::Arc::new(MemStore::default());
        let handles: Vec<_> = (0..8)
            .map(|i| {
                let d = dir.path().to_path_buf();
                let s = store.clone();
                std::thread::spawn(move || {
                    let _g = lock(&d);
                    let mut all = load_all_in(&d, s.as_ref());
                    all.push(AccountConfig { username: format!("u{i}"), ..imap_cfg() });
                    save_all_in(&d, &all, s.as_ref());
                })
            })
            .collect();
        for h in handles {
            h.join().unwrap();
        }
        assert_eq!(load_all_in(dir.path(), store.as_ref()).len(), 8);
    }
}
