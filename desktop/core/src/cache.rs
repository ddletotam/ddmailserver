use rusqlite::{Connection, params};
use std::path::PathBuf;
use std::sync::Mutex;

use crate::types::*;

/// Parse a "Name <email>" header value into (name, addr). Returns ("", "") if no addr present.
fn parse_addr_pair(value: &str) -> (String, String) {
    let v = value.trim();
    if let (Some(start), Some(end)) = (v.rfind('<'), v.rfind('>')) {
        if start < end {
            let addr = v[start + 1..end].trim().to_string();
            let name = v[..start].trim().trim_matches('"').trim().to_string();
            return (name, addr);
        }
    }
    if v.contains('@') {
        return (String::new(), v.to_string());
    }
    (String::new(), String::new())
}

/// Pull (name, addr) entries from a MessageBody's From/To/Cc fields.
fn collect_address_entries(body: &MessageBody) -> Vec<(String, String)> {
    let mut out: Vec<(String, String)> = Vec::new();
    let (fname, faddr) = parse_addr_pair(&body.from);
    let final_addr = if !faddr.is_empty() { faddr } else { body.from_addr.clone() };
    out.push((fname, final_addr));
    for h in body.to.iter().chain(body.cc.iter()) {
        let (n, a) = parse_addr_pair(h);
        if !a.is_empty() {
            out.push((n, a));
        }
    }
    out
}

/// Версия разбора тела письма, которой помечается каждая строка
/// `message_bodies` (колонка `parser_version`).
///
/// В `message_bodies` лежит уже РАЗОБРАННОЕ письмо: html, текст, список
/// вложений, адреса. Тела по (folder, uid) не перезапрашиваются, поэтому
/// исправление разбора без этой метки до закэшированных писем не доходит
/// никогда — та же ловушка, что была с кэшем текстур.
///
/// **Поднимать на единицу**, когда меняется то, ЧТО попадает в строку тела:
/// MIME-разбор в core (`imap.rs`/`imap_provider.rs`: выбор html/text,
/// декодирование, inline/attachment, адреса, references), cid:-подстановка
/// в движке клиента, или серверная выдача тел (`POST /conversations/messages`)
/// — когда её починка должна дойти до уже скачанных писем. Не поднимать ради
/// изменений рендера (emlrender, обвязка пузыря): тело от них не меняется.
///
/// Что происходит после подъёма: строка с другой версией на пути ОТКРЫТИЯ
/// диалога считается промахом и перезапрашивается; пока ответа нет (или
/// сети нет вовсе) показывается старое тело. Фоновая догрузка устаревшие
/// строки не трогает — иначе каждый подъём перекачивал бы весь ящик.
/// Подробности — §4а `docs/desktop-behavior-contract.md`.
pub const PARSER_VERSION: i64 = 1;

/// Одна миграция схемы `cache.db`. Номер миграции — её позиция в
/// [`MIGRATIONS`] плюс один; `PRAGMA user_version` = номер последней
/// применённой. Каждая идёт в своей транзакции вместе с повышением версии:
/// упала — откатилась целиком, версия не сдвинулась, следующий запуск
/// попробует её снова.
struct Migration {
    name: &'static str,
    apply: fn(&rusqlite::Transaction) -> rusqlite::Result<()>,
}

/// Упорядоченный список миграций. Только дописывать в конец; уже
/// выпущенную миграцию не править — она отработала у пользователей, и
/// правка до них не дойдёт.
const MIGRATIONS: &[Migration] = &[
    Migration { name: "базовая схема", apply: m001_base_schema },
    Migration { name: "message_bodies.parser_version", apply: m002_parser_version },
];

/// Схема, которая была у кэша до появления `user_version`. Новые таблицы и
/// колонки сюда НЕ дописываются — для них заводится следующая миграция.
const BASE_SCHEMA: &str = "
    CREATE TABLE IF NOT EXISTS conversations (
        id TEXT PRIMARY KEY,
        account_key TEXT NOT NULL,
        label TEXT NOT NULL,
        avatar_hash TEXT NOT NULL DEFAULT '',
        counterpart_name TEXT NOT NULL DEFAULT '',
        counterpart_addr TEXT NOT NULL DEFAULT '',
        counterparts_json TEXT NOT NULL DEFAULT '[]',
        is_group INTEGER NOT NULL DEFAULT 0,
        last_date TEXT NOT NULL DEFAULT '',
        last_date_ts INTEGER NOT NULL DEFAULT 0,
        last_subject TEXT NOT NULL DEFAULT '',
        unread_count INTEGER NOT NULL DEFAULT 0,
        total_count INTEGER NOT NULL DEFAULT 0,
        updated_at INTEGER NOT NULL DEFAULT 0
    );

    CREATE TABLE IF NOT EXISTS identities (
        email TEXT NOT NULL,
        account_key TEXT NOT NULL,
        name TEXT NOT NULL DEFAULT '',
        signature TEXT NOT NULL DEFAULT '',
        is_default INTEGER NOT NULL DEFAULT 0,
        color TEXT NOT NULL DEFAULT '',
        PRIMARY KEY(email, account_key)
    );

    CREATE TABLE IF NOT EXISTS avatar_cache (
        email TEXT PRIMARY KEY,
        png_data BLOB,
        mime TEXT NOT NULL DEFAULT '',
        cached_at INTEGER NOT NULL DEFAULT 0
    );

    CREATE TABLE IF NOT EXISTS contacts (
        account_key TEXT NOT NULL,
        email TEXT NOT NULL,
        name TEXT NOT NULL DEFAULT '',
        source TEXT NOT NULL DEFAULT 'auto',
        last_seen_ts INTEGER NOT NULL DEFAULT 0,
        PRIMARY KEY(account_key, email, source)
    );
    CREATE INDEX IF NOT EXISTS idx_contacts_email ON contacts(account_key, email);
    CREATE INDEX IF NOT EXISTS idx_contacts_name ON contacts(account_key, name);

    CREATE INDEX IF NOT EXISTS idx_conv_account ON conversations(account_key);
    CREATE INDEX IF NOT EXISTS idx_conv_date ON conversations(last_date_ts);

    CREATE TABLE IF NOT EXISTS conversation_messages (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        conversation_id TEXT NOT NULL,
        folder TEXT NOT NULL,
        uid INTEGER NOT NULL,
        UNIQUE(conversation_id, folder, uid)
    );

    CREATE TABLE IF NOT EXISTS message_bodies (
        folder TEXT NOT NULL,
        uid INTEGER NOT NULL,
        account_key TEXT NOT NULL,
        subject TEXT NOT NULL DEFAULT '',
        from_header TEXT NOT NULL DEFAULT '',
        from_addr TEXT NOT NULL DEFAULT '',
        to_header TEXT NOT NULL DEFAULT '',
        cc_header TEXT NOT NULL DEFAULT '',
        date_header TEXT NOT NULL DEFAULT '',
        date_ts INTEGER NOT NULL DEFAULT 0,
        html TEXT,
        text_body TEXT,
        attachments_json TEXT NOT NULL DEFAULT '[]',
        is_outgoing INTEGER NOT NULL DEFAULT 0,
        message_id TEXT NOT NULL DEFAULT '',
        in_reply_to TEXT NOT NULL DEFAULT '',
        references_json TEXT NOT NULL DEFAULT '[]',
        cached_at INTEGER NOT NULL DEFAULT 0,
        PRIMARY KEY(folder, uid, account_key)
    );

    -- Calendar reminders, one row per alarm of an occurrence (spec
    -- 2026-07-11). seq = cascade position: 0 is the primary alarm,
    -- 1.. are the event's secondary alarms (each armed only when the
    -- previous toast dies by timeout), 100 is the single user-chosen
    -- reminder from «напомнить позже» (it replaces the cascade).
    --
    -- status machine:
    --   armed     → shown      (toast on screen)
    --   chained   → armed      (previous toast timed out)
    --   shown     → done       (toast timed out → arm the next)
    --   *         → cancelled  (✕ / user choice / event edited)
    --   *         → expired    (occurrence ended while client off)
    --
    -- signature fingerprints (dtstart, dtend, leads, summary): any
    -- event change → delete + reseed, per spec.
    --
    -- В отличие от остального кэша, решения пользователя здесь (отложить,
    -- ✕) с сервера не восстанавливаются — поэтому кэш никогда не
    -- пересоздаётся целиком, кроме случая битого файла (см. Cache::new).
    CREATE TABLE IF NOT EXISTS reminders2 (
        event_id INTEGER NOT NULL,
        occurrence_start_ms INTEGER NOT NULL,
        occurrence_end_ms INTEGER NOT NULL DEFAULT 0,
        seq INTEGER NOT NULL,
        fire_at_ms INTEGER NOT NULL,
        lead_min INTEGER NOT NULL DEFAULT 0,
        at_start INTEGER NOT NULL DEFAULT 0,
        status TEXT NOT NULL DEFAULT 'armed',
        summary TEXT NOT NULL DEFAULT '',
        signature TEXT NOT NULL DEFAULT '',
        -- Чей это календарь. Нужен ровно для одного: выключение
        -- календаря должно гасить ВСЕ его напоминания, а не только те,
        -- чьи события сейчас лежат в загруженном окне. 0 — строка,
        -- посеянная до появления колонки; ближайший пересев её
        -- проставит.
        calendar_id INTEGER NOT NULL DEFAULT 0,
        PRIMARY KEY (event_id, occurrence_start_ms, seq)
    );
    CREATE INDEX IF NOT EXISTS idx_reminders2_due
        ON reminders2(status, fire_at_ms);

    -- Small key/value store for sync bookkeeping (delta watermarks,
    -- last-full-sync timestamps). One row per key.
    CREATE TABLE IF NOT EXISTS meta (
        key TEXT PRIMARY KEY,
        value TEXT NOT NULL DEFAULT ''
    );
";

/// Колонки, которые до `user_version` доезжали `ALTER TABLE … ADD COLUMN`
/// с заглушённой ошибкой на каждом старте. Существующая база может не иметь
/// любой из них (смотря с какой версии клиента она жила).
const BASE_COLUMNS: &[(&str, &str, &str)] = &[
    ("conversation_messages", "seen", "INTEGER NOT NULL DEFAULT 1"),
    ("conversation_messages", "message_id", "TEXT NOT NULL DEFAULT ''"),
    ("conversations", "avatar_hash", "TEXT NOT NULL DEFAULT ''"),
    ("conversations", "received_by", "TEXT NOT NULL DEFAULT ''"),
    ("conversations", "last_subject", "TEXT NOT NULL DEFAULT ''"),
    ("conversations", "counterparts_json", "TEXT NOT NULL DEFAULT '[]'"),
    ("reminders2", "calendar_id", "INTEGER NOT NULL DEFAULT 0"),
    ("message_bodies", "message_id", "TEXT NOT NULL DEFAULT ''"),
    ("message_bodies", "in_reply_to", "TEXT NOT NULL DEFAULT ''"),
    ("message_bodies", "references_json", "TEXT NOT NULL DEFAULT '[]'"),
    // Empty for rows cached before this column; the viewer falls back to
    // the network for those and they fill in on the next sync.
    ("message_bodies", "raw_headers", "TEXT NOT NULL DEFAULT ''"),
    ("avatar_cache", "mime", "TEXT NOT NULL DEFAULT ''"),
];

fn has_column(conn: &Connection, table: &str, column: &str) -> rusqlite::Result<bool> {
    let mut stmt = conn.prepare(&format!("PRAGMA table_info({table})"))?;
    let names = stmt.query_map([], |r| r.get::<_, String>(1))?;
    for n in names {
        if n? == column {
            return Ok(true);
        }
    }
    Ok(false)
}

fn add_column_if_missing(
    conn: &Connection,
    table: &str,
    column: &str,
    decl: &str,
) -> rusqlite::Result<()> {
    if !has_column(conn, table, column)? {
        conn.execute(&format!("ALTER TABLE {table} ADD COLUMN {column} {decl}"), [])?;
    }
    Ok(())
}

/// Миграция 1: схема, к которой приходил прежний код. Свежая база получает
/// её целиком; существующая (`user_version` = 0, таблицы уже есть) — только
/// недостающие колонки, без потери данных.
fn m001_base_schema(tx: &rusqlite::Transaction) -> rusqlite::Result<()> {
    tx.execute_batch(BASE_SCHEMA)?;
    for (table, column, decl) in BASE_COLUMNS {
        add_column_if_missing(tx, table, column, decl)?;
    }
    // The v1 reminders table had a broken PRIMARY KEY (event_id,
    // occurrence) that made the second alarm row of an occurrence
    // impossible — superseded wholesale by reminders2. Dropping it loses
    // at most one pending snooze, once, at upgrade time.
    tx.execute("DROP TABLE IF EXISTS event_reminders", [])?;
    Ok(())
}

/// Миграция 2: версия разбора у каждой строки тела ([`PARSER_VERSION`]).
/// Уже лежащие строки получают 1: они разобраны тем же кодом, что и сразу
/// после введения версии, и массового перекачивания при обновлении клиента
/// быть не должно.
fn m002_parser_version(tx: &rusqlite::Transaction) -> rusqlite::Result<()> {
    add_column_if_missing(tx, "message_bodies", "parser_version", "INTEGER NOT NULL DEFAULT 1")
}

/// Версия схемы, которую знает этот код.
pub const SCHEMA_VERSION: i64 = MIGRATIONS.len() as i64;

fn user_version(conn: &Connection) -> rusqlite::Result<i64> {
    conn.pragma_query_value(None, "user_version", |r| r.get(0))
}

/// Догнать схему до [`SCHEMA_VERSION`]. Каждая миграция — в транзакции
/// `IMMEDIATE` вместе с повышением `user_version`; версия перечитывается уже
/// под блокировкой, так что два соединения, открывшиеся одновременно (UI и
/// движок), одну миграцию дважды не применят.
fn migrate(conn: &mut Connection) -> rusqlite::Result<()> {
    let start = user_version(conn)?;
    if start > SCHEMA_VERSION {
        // База от более нового клиента. Миграции только добавляют, так что
        // старый код на ней работает; откатывать чужую схему не берёмся.
        eprintln!(
            "cache: схема v{start} новее известной этому клиенту v{SCHEMA_VERSION} — миграции пропущены"
        );
        return Ok(());
    }
    for (i, m) in MIGRATIONS.iter().enumerate() {
        let target = i as i64 + 1;
        if target <= start {
            continue;
        }
        let tx = conn.transaction_with_behavior(rusqlite::TransactionBehavior::Immediate)?;
        if user_version(&tx)? >= target {
            continue; // успело другое соединение
        }
        (m.apply)(&tx)?;
        tx.pragma_update(None, "user_version", target)?;
        tx.commit()?;
        eprintln!("cache: миграция {target} «{}» применена", m.name);
    }
    Ok(())
}

/// Ошибка, после которой файл как база не годится: чинить нечего, можно
/// только отложить его в сторону.
fn is_corrupt(e: &rusqlite::Error) -> bool {
    matches!(
        e.sqlite_error_code(),
        Some(rusqlite::ErrorCode::DatabaseCorrupt | rusqlite::ErrorCode::NotADatabase)
    )
}

/// Сколько ждать чужую запись, прежде чем вернуть SQLITE_BUSY. Соединений у
/// клиента минимум два (UI-поток и поток движка открывают `Cache` каждый
/// себе), и без ожидания запись одного во время транзакции другого падала
/// сразу — а падение у вызывающих чаще всего уходит в `.ok()`.
const BUSY_TIMEOUT: std::time::Duration = std::time::Duration::from_secs(5);

fn open_conn(db_path: &std::path::Path) -> rusqlite::Result<Connection> {
    let conn = Connection::open(db_path)?;
    conn.busy_timeout(BUSY_TIMEOUT)?;
    conn.execute_batch("PRAGMA journal_mode=WAL; PRAGMA synchronous=NORMAL;")?;
    Ok(conn)
}

/// Отложить битую базу в сторону (`cache.db.broken-<unix-время>`, вместе с
/// -wal/-shm), а не удалить: напоминания в ней с сервера не восстановить, и
/// если файл ещё можно спасти руками, он должен остаться.
fn quarantine(db_path: &std::path::Path) -> Result<std::path::PathBuf, String> {
    let ts = chrono::Utc::now().timestamp();
    let mut moved = db_path.as_os_str().to_owned();
    moved.push(format!(".broken-{ts}"));
    let moved = std::path::PathBuf::from(moved);
    std::fs::rename(db_path, &moved).map_err(|e| format!("rename {}: {e}", db_path.display()))?;
    for side in ["-wal", "-shm"] {
        let mut from = db_path.as_os_str().to_owned();
        from.push(side);
        let from = std::path::PathBuf::from(from);
        if from.exists() {
            let mut to = moved.as_os_str().to_owned();
            to.push(side);
            if let Err(e) = std::fs::rename(&from, std::path::PathBuf::from(to)) {
                eprintln!("cache: не удалось отложить {}: {e}", from.display());
            }
        }
    }
    Ok(moved)
}

/// Уборка на каждом старте — не схема, а данные; ошибки только в лог.
fn startup_housekeeping(conn: &Connection) {
    // Existing rows pre-MIME stored Gravatar PNG bytes — purge so the next
    // lookup uses the new chain (and labels the result with a MIME). Заодно
    // (и это поведение, на которое уже полагаются) рестарт сбрасывает
    // отрицательный кэш аватарок: у пустой записи mime тоже пустой.
    if let Err(e) = conn.execute("DELETE FROM avatar_cache WHERE mime = ''", []) {
        eprintln!("cache: чистка avatar_cache: {e}");
    }

    // One-shot wipe of reminders2 when the reminder-data version lags.
    // Bump REMINDERS_DATA_VERSION whenever a fixed bug left polluted rows
    // that would keep firing until they naturally reseed. v2: the server
    // used to emit every override VEVENT's VALARM on every occurrence, so
    // occurrences carried dozens of duplicate alarms → an endless toast
    // cascade. Clearing lets seed() rebuild from the corrected data.
    // v3: dedup of the same meeting carried under two event_ids landed;
    // clear the transitional mix (incl. any orphaned user-snooze row) so
    // the cascade reseeds clean.
    // v4: строки, посеянные до колонки `calendar_id`, лежат с нулём —
    // такую ни выключение календаря не гасит (нечего сопоставить), ни
    // проверка видимости в момент выстрела. Пока они живы (горизонт посева
    // — 30 дней вперёд), скрытый календарь продолжает звонить. Стираем
    // один раз: после этого каждая строка знает свой календарь.
    const REMINDERS_DATA_VERSION: &str = "4";
    let wipe = || -> rusqlite::Result<()> {
        let rv: String = conn
            .query_row("SELECT value FROM meta WHERE key = 'reminders_data_version'", [], |r| {
                r.get(0)
            })
            .or_else(|e| match e {
                rusqlite::Error::QueryReturnedNoRows => Ok(String::new()),
                e => Err(e),
            })?;
        if rv != REMINDERS_DATA_VERSION {
            let tx = conn.unchecked_transaction()?;
            tx.execute("DELETE FROM reminders2", [])?;
            tx.execute(
                "INSERT OR REPLACE INTO meta (key, value) VALUES ('reminders_data_version', ?1)",
                params![REMINDERS_DATA_VERSION],
            )?;
            tx.commit()?;
        }
        Ok(())
    };
    if let Err(e) = wipe() {
        eprintln!("cache: reminders_data_version: {e}");
    }
}

pub struct Cache {
    conn: Mutex<Connection>,
}

impl Cache {
    /// Открыть `cache.db` в `app_dir` и догнать схему.
    ///
    /// Стратегия при сбое:
    /// * файл не база / битый (`SQLITE_CORRUPT`, `SQLITE_NOTADB`) — файл
    ///   откладывается в сторону ([`quarantine`]) и создаётся пустой кэш.
    ///   Почта, диалоги, контакты, аватарки и identities приедут с сервера;
    ///   теряются только напоминания, но из битого файла их и так не прочесть;
    /// * любая другая ошибка миграции (занято, диск, права) — транзакция
    ///   откатилась, в лог пишется, на какой версии осталась схема, и кэш
    ///   открывается как есть: отдельные запросы к недостающим колонкам будут
    ///   падать со своими ошибками, а следующий старт повторит миграцию.
    ///   Удалять из-за этого базу нельзя — в ней пользовательские решения по
    ///   напоминаниям, которых нет на сервере.
    pub fn new(app_dir: PathBuf) -> Result<Self, String> {
        std::fs::create_dir_all(&app_dir).map_err(|e| format!("mkdir: {e}"))?;
        let db_path = app_dir.join("cache.db");

        let open_fresh = |why: &rusqlite::Error| -> Result<Connection, String> {
            let moved = quarantine(&db_path)?;
            eprintln!(
                "cache: {} повреждён ({why}) — отложен в {}, создаётся новый кэш",
                db_path.display(),
                moved.display()
            );
            let mut conn = open_conn(&db_path).map_err(|e| format!("SQLite open: {e}"))?;
            migrate(&mut conn).map_err(|e| format!("SQLite migrate: {e}"))?;
            Ok(conn)
        };

        let conn = match open_conn(&db_path) {
            Ok(mut conn) => match migrate(&mut conn) {
                Ok(()) => conn,
                Err(e) if is_corrupt(&e) => {
                    drop(conn);
                    open_fresh(&e)?
                }
                Err(e) => {
                    let v = user_version(&conn).unwrap_or(-1);
                    eprintln!(
                        "cache: миграция схемы не прошла ({e}); схема осталась v{v} из \
                         v{SCHEMA_VERSION}, кэш открыт как есть, повтор — при следующем открытии"
                    );
                    conn
                }
            },
            Err(e) if is_corrupt(&e) => open_fresh(&e)?,
            Err(e) => return Err(format!("SQLite open: {e}")),
        };

        startup_housekeeping(&conn);
        Ok(Self { conn: Mutex::new(conn) })
    }

    /// Save conversations to cache (replaces all for this account).
    pub fn save_conversations(
        &self,
        account_key: &str,
        conversations: &[Conversation],
    ) -> Result<(), String> {
        let conn = self.conn.lock().map_err(|e| format!("lock: {e}"))?;
        let now = chrono::Utc::now().timestamp();

        let tx = conn.unchecked_transaction().map_err(|e| format!("tx: {e}"))?;

        // Delete old conversations for this account
        tx.execute(
            "DELETE FROM conversation_messages WHERE conversation_id IN \
            (SELECT id FROM conversations WHERE account_key = ?1)",
            params![account_key],
        )
        .map_err(|e| format!("del msgs: {e}"))?;
        tx.execute("DELETE FROM conversations WHERE account_key = ?1", params![account_key])
            .map_err(|e| format!("del convs: {e}"))?;

        for conv in conversations {
            let cp_name = conv.counterparts.first().map(|c| c.name.as_str()).unwrap_or("");
            let cp_addr = conv.counterparts.first().map(|c| c.addr.as_str()).unwrap_or("");
            let cps_json = serde_json::to_string(&conv.counterparts)
                .map_err(|e| format!("serialize counterparts: {e}"))?;

            // OR REPLACE, а не голый INSERT: два диалога с одинаковым id в
            // одной выдаче — ошибка на той стороне, но платить за неё полным
            // откатом транзакции нельзя. Так уже было: правка ключа заставила
            // сервер отдать дубль, `INSERT` упал на PRIMARY KEY, ошибка ушла
            // в `.ok()` у вызывающего, и кэш диалогов перестал обновляться
            // молча — DELETE выше откатился вместе со всем остальным. Ссылки
            // на письма идут через OR IGNORE, так что при дубле диалог
            // соберёт письма обеих групп, а не потеряет их.
            tx.execute(
                "INSERT OR REPLACE INTO conversations (id, account_key, label, avatar_hash, received_by, counterpart_name, counterpart_addr, \
                 counterparts_json, is_group, last_date, last_date_ts, last_subject, unread_count, total_count, updated_at) \
                 VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12, ?13, ?14, ?15)",
                params![
                    conv.id, account_key, conv.label, conv.avatar_hash, conv.received_by, cp_name, cp_addr,
                    cps_json, conv.is_group as i32, conv.last_date, conv.last_date_ts,
                    conv.last_subject, conv.unread_count, conv.total_count, now
                ],
            ).map_err(|e| format!("ins conv: {e}"))?;

            for mr in &conv.messages {
                tx.execute(
                    "INSERT OR IGNORE INTO conversation_messages (conversation_id, folder, uid, seen, message_id) VALUES (?1, ?2, ?3, ?4, ?5)",
                    params![conv.id, mr.folder, mr.uid, mr.seen as i32, mr.message_id],
                ).map_err(|e| format!("ins msg ref: {e}"))?;
            }

            // Auto-record the counterpart as a contact.
            if !cp_addr.is_empty() {
                let lc = cp_addr.to_lowercase();
                tx.execute(
                    "INSERT INTO contacts (account_key, email, name, source, last_seen_ts) \
                     VALUES (?1, ?2, ?3, 'auto', ?4) \
                     ON CONFLICT(account_key, email, source) DO UPDATE SET \
                       name = CASE WHEN excluded.name != '' THEN excluded.name ELSE name END, \
                       last_seen_ts = excluded.last_seen_ts",
                    params![account_key, lc, cp_name, conv.last_date_ts],
                )
                .map_err(|e| format!("auto-contact: {e}"))?;
            }
        }

        tx.commit().map_err(|e| format!("commit: {e}"))?;
        Ok(())
    }

    /// Upsert a partial set of conversations (delta sync): each conversation
    /// is replaced/inserted by id, its message refs rewritten; everything
    /// else stays untouched. Use save_conversations for a full replace.
    pub fn upsert_conversations(
        &self,
        account_key: &str,
        conversations: &[Conversation],
    ) -> Result<(), String> {
        let conn = self.conn.lock().map_err(|e| format!("lock: {e}"))?;
        let now = chrono::Utc::now().timestamp();
        let tx = conn.unchecked_transaction().map_err(|e| format!("tx: {e}"))?;

        for conv in conversations {
            let cp_name = conv.counterparts.first().map(|c| c.name.as_str()).unwrap_or("");
            let cp_addr = conv.counterparts.first().map(|c| c.addr.as_str()).unwrap_or("");
            let cps_json = serde_json::to_string(&conv.counterparts)
                .map_err(|e| format!("serialize counterparts: {e}"))?;

            tx.execute(
                "INSERT OR REPLACE INTO conversations (id, account_key, label, avatar_hash, received_by, counterpart_name, \
                 counterpart_addr, counterparts_json, is_group, last_date, last_date_ts, last_subject, unread_count, \
                 total_count, updated_at) \
                 VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12, ?13, ?14, ?15)",
                params![
                    conv.id, account_key, conv.label, conv.avatar_hash, conv.received_by, cp_name, cp_addr,
                    cps_json, conv.is_group as i32, conv.last_date, conv.last_date_ts,
                    conv.last_subject, conv.unread_count, conv.total_count, now
                ],
            ).map_err(|e| format!("upsert conv: {e}"))?;

            tx.execute(
                "DELETE FROM conversation_messages WHERE conversation_id = ?1",
                params![conv.id],
            )
            .map_err(|e| format!("del msg refs: {e}"))?;
            for mr in &conv.messages {
                tx.execute(
                    "INSERT OR IGNORE INTO conversation_messages (conversation_id, folder, uid, seen, message_id) VALUES (?1, ?2, ?3, ?4, ?5)",
                    params![conv.id, mr.folder, mr.uid, mr.seen as i32, mr.message_id],
                ).map_err(|e| format!("ins msg ref: {e}"))?;
            }
        }

        tx.commit().map_err(|e| format!("commit: {e}"))
    }

    /// Remove a conversation and everything that belongs to it: the row,
    /// its message refs, and the cached bodies of those refs. Used by the
    /// desktop "delete conversation" action so a restart can't resurrect
    /// the deleted thread from cache.
    pub fn delete_conversation(
        &self,
        account_key: &str,
        conversation_id: &str,
    ) -> Result<(), String> {
        let conn = self.conn.lock().map_err(|e| format!("lock: {e}"))?;
        let tx = conn.unchecked_transaction().map_err(|e| format!("tx: {e}"))?;
        tx.execute(
            "DELETE FROM message_bodies WHERE account_key = ?1 AND (folder, uid) IN \
             (SELECT folder, uid FROM conversation_messages WHERE conversation_id = ?2)",
            params![account_key, conversation_id],
        )
        .map_err(|e| format!("del bodies: {e}"))?;
        tx.execute(
            "DELETE FROM conversation_messages WHERE conversation_id = ?1",
            params![conversation_id],
        )
        .map_err(|e| format!("del refs: {e}"))?;
        tx.execute(
            "DELETE FROM conversations WHERE account_key = ?1 AND id = ?2",
            params![account_key, conversation_id],
        )
        .map_err(|e| format!("del conv: {e}"))?;
        tx.commit().map_err(|e| format!("commit: {e}"))
    }

    /// Apply DELETE tombstones from the change journal: drop each message_id's
    /// ref and cached body for this account, then remove any conversation left
    /// with no messages. Returns how many message refs were removed. Message
    /// refs written before the message_id column existed (empty message_id)
    /// won't match — a full resync repopulates them.
    pub fn apply_deletions(
        &self,
        account_key: &str,
        message_ids: &[String],
    ) -> Result<usize, String> {
        if message_ids.is_empty() {
            return Ok(0);
        }
        let conn = self.conn.lock().map_err(|e| format!("lock: {e}"))?;
        let tx = conn.unchecked_transaction().map_err(|e| format!("tx: {e}"))?;
        let mut removed = 0usize;
        for mid in message_ids {
            if mid.is_empty() {
                continue;
            }
            removed += tx
                .execute(
                    "DELETE FROM conversation_messages WHERE message_id = ?1 \
                 AND conversation_id IN (SELECT id FROM conversations WHERE account_key = ?2)",
                    params![mid, account_key],
                )
                .map_err(|e| format!("del conv msg: {e}"))?;
            tx.execute(
                "DELETE FROM message_bodies WHERE message_id = ?1 AND account_key = ?2",
                params![mid, account_key],
            )
            .ok();
        }
        // Conversations whose every message is now gone disappear too.
        tx.execute(
            "DELETE FROM conversations WHERE account_key = ?1 \
             AND id NOT IN (SELECT DISTINCT conversation_id FROM conversation_messages)",
            params![account_key],
        )
        .map_err(|e| format!("del empty conv: {e}"))?;
        tx.commit().map_err(|e| format!("commit: {e}"))?;
        Ok(removed)
    }

    /// Read a sync-bookkeeping value (see the `meta` table).
    pub fn get_meta(&self, key: &str) -> Option<String> {
        let conn = self.conn.lock().ok()?;
        conn.query_row("SELECT value FROM meta WHERE key = ?1", params![key], |r| {
            r.get::<_, String>(0)
        })
        .ok()
    }

    /// Write a sync-bookkeeping value.
    pub fn set_meta(&self, key: &str, value: &str) -> Result<(), String> {
        let conn = self.conn.lock().map_err(|e| format!("lock: {e}"))?;
        conn.execute(
            "INSERT OR REPLACE INTO meta (key, value) VALUES (?1, ?2)",
            params![key, value],
        )
        .map_err(|e| format!("set meta: {e}"))?;
        Ok(())
    }

    /// Which of `refs` already have a cached body. Used by the engine's
    /// missing-only fetch: bodies are immutable, so a cached (folder, uid)
    /// never needs refetching.
    ///
    /// Проверка идёт ровно по первичному ключу (folder, uid, account_key),
    /// поэтому её не жалко звать на весь список бесед: фоновая догрузка
    /// (`EngineCmd::PrefetchBodies`) так и выясняет, чего в кэше не хватает,
    /// не вытаскивая при этом html и текст всего ящика.
    ///
    /// Пустая строка (`body_is_blank`) здесь считается присутствующей. Лечит
    /// такие путь открытия диалога (§4а контракта); фон их не трогает, иначе
    /// по-настоящему пустое письмо перекачивалось бы на каждом цикле синка.
    /// Так же и строка устаревшей версии разбора ([`Self::stale_body_refs`]):
    /// фон её не перекачивает, иначе каждый подъём [`PARSER_VERSION`]
    /// означал бы перекачку всего ящика.
    pub fn cached_body_refs(
        &self,
        account_key: &str,
        refs: &[MessageRef],
    ) -> Result<Vec<MessageRef>, String> {
        let conn = self.conn.lock().map_err(|e| format!("lock: {e}"))?;
        let mut stmt = conn
            .prepare(
                "SELECT 1 FROM message_bodies WHERE folder = ?1 AND uid = ?2 AND account_key = ?3",
            )
            .map_err(|e| format!("prepare: {e}"))?;
        let mut out = Vec::new();
        for mr in refs {
            let hit: Result<i32, _> =
                stmt.query_row(params![mr.folder, mr.uid, account_key], |r| r.get(0));
            if hit.is_ok() {
                out.push(mr.clone());
            }
        }
        Ok(out)
    }

    /// Какие из `refs` лежат в кэше разобранными НЕ текущей версией
    /// ([`PARSER_VERSION`]). Путь открытия диалога считает их промахом и
    /// перезапрашивает; `load_message_bodies` их по-прежнему отдаёт — старое
    /// тело лучше пустого пузыря, пока сервер не ответил (или его нет).
    pub fn stale_body_refs(
        &self,
        account_key: &str,
        refs: &[MessageRef],
    ) -> Result<Vec<MessageRef>, String> {
        let conn = self.conn.lock().map_err(|e| format!("lock: {e}"))?;
        let mut stmt = conn
            .prepare(
                "SELECT parser_version FROM message_bodies \
                 WHERE folder = ?1 AND uid = ?2 AND account_key = ?3",
            )
            .map_err(|e| format!("prepare: {e}"))?;
        let mut out = Vec::new();
        for mr in refs {
            match stmt.query_row(params![mr.folder, mr.uid, account_key], |r| r.get::<_, i64>(0)) {
                Ok(v) if v != PARSER_VERSION => out.push(mr.clone()),
                Ok(_) | Err(rusqlite::Error::QueryReturnedNoRows) => {}
                Err(e) => return Err(format!("parser_version {}/{}: {e}", mr.folder, mr.uid)),
            }
        }
        Ok(out)
    }

    /// Темы всех закэшированных писем: `(account_key, folder, uid, subject)`.
    /// Для локального поиска по темам (выпадашка поиска, секция «Диалоги»).
    /// Фильтр — на вызывающем, а не в SQL: `LOWER`/`LIKE` в SQLite
    /// регистр складывают только для ASCII, кириллица мимо.
    pub fn body_subjects(&self) -> Result<Vec<(String, String, u32, String)>, String> {
        let conn = self.conn.lock().map_err(|e| format!("lock: {e}"))?;
        let mut stmt = conn
            .prepare(
                "SELECT account_key, folder, uid, subject FROM message_bodies WHERE subject <> ''",
            )
            .map_err(|e| format!("prepare: {e}"))?;
        let rows = stmt
            .query_map([], |r| Ok((r.get(0)?, r.get(1)?, r.get(2)?, r.get(3)?)))
            .map_err(|e| format!("query: {e}"))?;
        Ok(rows.filter_map(|r| r.ok()).collect())
    }

    /// Distinct account keys present in the conversations table.
    pub fn account_keys(&self) -> Result<Vec<String>, String> {
        let conn = self.conn.lock().map_err(|e| format!("lock: {e}"))?;
        let mut stmt = conn
            .prepare("SELECT DISTINCT account_key FROM conversations")
            .map_err(|e| format!("prepare: {e}"))?;
        let rows =
            stmt.query_map([], |r| r.get::<_, String>(0)).map_err(|e| format!("query: {e}"))?;
        Ok(rows.filter_map(|r| r.ok()).collect())
    }

    /// Load cached conversations for an account.
    pub fn load_conversations(&self, account_key: &str) -> Result<Vec<Conversation>, String> {
        let conn = self.conn.lock().map_err(|e| format!("lock: {e}"))?;

        let mut stmt = conn.prepare(
            "SELECT id, label, avatar_hash, received_by, counterpart_name, counterpart_addr, counterparts_json, is_group, \
             last_date, last_date_ts, last_subject, unread_count, total_count \
             FROM conversations WHERE account_key = ?1 ORDER BY last_date_ts DESC"
        ).map_err(|e| format!("prepare: {e}"))?;

        let rows = stmt
            .query_map(params![account_key], |row| {
                Ok((
                    row.get::<_, String>(0)?,  // id
                    row.get::<_, String>(1)?,  // label
                    row.get::<_, String>(2)?,  // avatar_hash
                    row.get::<_, String>(3)?,  // received_by
                    row.get::<_, String>(4)?,  // cp_name (legacy)
                    row.get::<_, String>(5)?,  // cp_addr (legacy)
                    row.get::<_, String>(6)?,  // counterparts_json
                    row.get::<_, bool>(7)?,    // is_group
                    row.get::<_, String>(8)?,  // last_date
                    row.get::<_, i64>(9)?,     // last_date_ts
                    row.get::<_, String>(10)?, // last_subject
                    row.get::<_, u32>(11)?,    // unread_count
                    row.get::<_, u32>(12)?,    // total_count
                ))
            })
            .map_err(|e| format!("query: {e}"))?;

        let mut conversations = Vec::new();
        for row in rows {
            let (
                id,
                label,
                avatar_hash,
                received_by,
                cp_name,
                cp_addr,
                cps_json,
                is_group,
                last_date,
                last_date_ts,
                last_subject,
                unread_count,
                total_count,
            ) = row.map_err(|e| format!("row: {e}"))?;

            // Prefer the JSON column. Rows written before that migration will
            // store an empty array there — fall back to the legacy single-pair
            // columns for those.
            let counterparts: Vec<ContactInfo> = serde_json::from_str(&cps_json)
                .ok()
                .filter(|v: &Vec<ContactInfo>| !v.is_empty())
                .unwrap_or_else(|| vec![ContactInfo { name: cp_name, addr: cp_addr }]);

            // Load message refs
            let mut msg_stmt = conn.prepare(
                "SELECT folder, uid, COALESCE(seen, 1), COALESCE(message_id, '') FROM conversation_messages WHERE conversation_id = ?1"
            ).map_err(|e| format!("prepare msgs: {e}"))?;
            let messages: Vec<MessageRef> = msg_stmt
                .query_map(params![id], |r| {
                    Ok(MessageRef {
                        folder: r.get(0)?,
                        uid: r.get(1)?,
                        seen: r.get::<_, i32>(2)? != 0,
                        message_id: r.get(3)?,
                    })
                })
                .map_err(|e| format!("query msgs: {e}"))?
                .filter_map(|r| r.ok())
                .collect();

            conversations.push(Conversation {
                id,
                label,
                avatar_hash,
                received_by,
                counterparts,
                is_group,
                last_date,
                last_date_ts,
                last_subject,
                unread_count,
                total_count,
                messages,
                draft: None,                // Drafts not cached
                account_key: String::new(), // stamped by the engine on merge
                merged: false,
            });
        }

        Ok(conversations)
    }

    /// Save freshly parsed message bodies to cache, stamped with the current
    /// [`PARSER_VERSION`].
    pub fn save_message_bodies(
        &self,
        account_key: &str,
        bodies: &[MessageBody],
    ) -> Result<(), String> {
        self.store_bodies(account_key, bodies, Some(PARSER_VERSION))
    }

    /// Пересохранить тела, прочитанные из кэша и подправленные на месте
    /// (cid:-подстановка движка), НЕ трогая их `parser_version`: разобраны
    /// они прежним кодом, и пометка текущей версией спрятала бы их от
    /// перезапроса навсегда. Строка, которой ещё нет, получает текущую.
    pub fn resave_message_bodies(
        &self,
        account_key: &str,
        bodies: &[MessageBody],
    ) -> Result<(), String> {
        self.store_bodies(account_key, bodies, None)
    }

    /// `version`: `Some` — проставить её, `None` — оставить ту, что у строки.
    fn store_bodies(
        &self,
        account_key: &str,
        bodies: &[MessageBody],
        version: Option<i64>,
    ) -> Result<(), String> {
        let conn = self.conn.lock().map_err(|e| format!("lock: {e}"))?;
        let now = chrono::Utc::now().timestamp();

        // Auto-record contacts from From/To/Cc of each message.
        for body in bodies {
            let entries = collect_address_entries(body);
            for (name, addr) in entries {
                if addr.is_empty() {
                    continue;
                }
                let lc = addr.to_lowercase();
                conn.execute(
                    "INSERT INTO contacts (account_key, email, name, source, last_seen_ts) \
                     VALUES (?1, ?2, ?3, 'auto', ?4) \
                     ON CONFLICT(account_key, email, source) DO UPDATE SET \
                       name = CASE WHEN excluded.name != '' THEN excluded.name ELSE name END, \
                       last_seen_ts = excluded.last_seen_ts",
                    params![account_key, lc, name, body.date_ts],
                )
                .map_err(|e| format!("auto-contact: {e}"))?;
            }
        }

        for body in bodies {
            let att_json = serde_json::to_string(&body.attachments).unwrap_or_else(|_| "[]".into());
            let refs_json = serde_json::to_string(&body.references).unwrap_or_else(|_| "[]".into());
            conn.execute(
                "INSERT OR REPLACE INTO message_bodies \
                 (folder, uid, account_key, subject, from_header, from_addr, to_header, cc_header, \
                  date_header, date_ts, html, text_body, attachments_json, is_outgoing, \
                  message_id, in_reply_to, references_json, raw_headers, cached_at, \
                  parser_version) \
                 VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12, ?13, ?14, ?15, \
                         ?16, ?17, ?18, ?19, \
                         COALESCE(?20, (SELECT parser_version FROM message_bodies \
                                        WHERE folder = ?1 AND uid = ?2 AND account_key = ?3), \
                                  ?21))",
                params![
                    body.folder,
                    body.uid,
                    account_key,
                    body.subject,
                    body.from,
                    body.from_addr,
                    body.to.join(", "),
                    body.cc.join(", "),
                    body.date,
                    body.date_ts,
                    body.html,
                    body.text,
                    att_json,
                    body.is_outgoing as i32,
                    body.message_id,
                    body.in_reply_to,
                    refs_json,
                    body.raw_headers,
                    now,
                    version,
                    PARSER_VERSION
                ],
            )
            .map_err(|e| format!("ins body: {e}"))?;
        }
        Ok(())
    }

    /// Load cached message bodies.
    pub fn load_message_bodies(
        &self,
        account_key: &str,
        refs: &[MessageRef],
    ) -> Result<Vec<MessageBody>, String> {
        let conn = self.conn.lock().map_err(|e| format!("lock: {e}"))?;

        let mut bodies = Vec::new();
        let mut stmt = conn
            .prepare(
                "SELECT folder, uid, subject, from_header, from_addr, to_header, cc_header, \
             date_header, date_ts, html, text_body, attachments_json, is_outgoing, \
             message_id, in_reply_to, references_json, raw_headers \
             FROM message_bodies WHERE folder = ?1 AND uid = ?2 AND account_key = ?3",
            )
            .map_err(|e| format!("prepare: {e}"))?;

        for mr in refs {
            let result = stmt.query_row(params![mr.folder, mr.uid, account_key], |row| {
                let to_str: String = row.get(5)?;
                let cc_str: String = row.get(6)?;
                let att_json: String = row.get(11)?;
                let refs_json: String = row.get(15)?;

                Ok(MessageBody {
                    folder: row.get(0)?,
                    uid: row.get(1)?,
                    subject: row.get(2)?,
                    from: row.get(3)?,
                    from_addr: row.get(4)?,
                    to: to_str
                        .split(", ")
                        .filter(|s| !s.is_empty())
                        .map(|s| s.to_string())
                        .collect(),
                    cc: cc_str
                        .split(", ")
                        .filter(|s| !s.is_empty())
                        .map(|s| s.to_string())
                        .collect(),
                    date: row.get(7)?,
                    date_ts: row.get(8)?,
                    html: row.get(9)?,
                    text: row.get(10)?,
                    attachments: serde_json::from_str(&att_json).unwrap_or_default(),
                    is_outgoing: row.get::<_, i32>(12)? != 0,
                    message_id: row.get(13)?,
                    in_reply_to: row.get(14)?,
                    references: serde_json::from_str(&refs_json).unwrap_or_default(),
                    raw_headers: row.get(16)?,
                })
            });

            match result {
                Ok(body) => bodies.push(body),
                // `QueryReturnedNoRows` is the ordinary "not cached yet" case;
                // anything else means the query itself is wrong, and silence
                // there reads as an empty cache — which is exactly how a
                // column-index mistake turned into "nothing loads".
                Err(rusqlite::Error::QueryReturnedNoRows) => {}
                Err(e) => eprintln!("cache: load body {}/{} failed: {e}", mr.folder, mr.uid),
            }
        }

        bodies.sort_by_key(|b| b.date_ts);
        Ok(bodies)
    }

    /// Save identities to cache.
    pub fn save_identities(
        &self,
        account_key: &str,
        identities: &[Identity],
    ) -> Result<(), String> {
        let conn = self.conn.lock().map_err(|e| format!("lock: {e}"))?;
        // Clear old identities for this account
        conn.execute("DELETE FROM identities WHERE account_key = ?1", params![account_key])
            .map_err(|e| format!("del identities: {e}"))?;
        for id in identities {
            conn.execute(
                "INSERT INTO identities (email, account_key, name, signature, is_default, color) \
                 VALUES (?1, ?2, ?3, ?4, ?5, ?6)",
                params![
                    id.email,
                    account_key,
                    id.name,
                    id.signature,
                    id.is_default as i32,
                    id.color
                ],
            )
            .map_err(|e| format!("ins identity: {e}"))?;
        }
        Ok(())
    }

    /// Load cached identities.
    pub fn load_identities(&self, account_key: &str) -> Result<Vec<Identity>, String> {
        let conn = self.conn.lock().map_err(|e| format!("lock: {e}"))?;
        let mut stmt = conn.prepare(
            "SELECT email, name, signature, is_default, color FROM identities WHERE account_key = ?1 ORDER BY is_default DESC"
        ).map_err(|e| format!("prepare: {e}"))?;

        let rows = stmt
            .query_map(params![account_key], |row| {
                Ok(Identity {
                    email: row.get(0)?,
                    name: row.get(1)?,
                    signature: row.get(2)?,
                    is_default: row.get::<_, i32>(3)? != 0,
                    color: row.get(4)?,
                    // Capabilities aren't cached — they come from the live
                    // /identities fetch; default false until that lands.
                    can_create_events: false,
                    can_create_contacts: false,
                })
            })
            .map_err(|e| format!("query: {e}"))?;

        let mut identities = Vec::new();
        for row in rows {
            if let Ok(id) = row {
                identities.push(id);
            }
        }
        Ok(identities)
    }

    /// Insert/update contacts in batch. Existing rows keep their non-empty names if a new
    /// row arrives with empty name.
    pub fn record_contacts(&self, account_key: &str, contacts: &[Contact]) -> Result<(), String> {
        let conn = self.conn.lock().map_err(|e| format!("lock: {e}"))?;
        let now = chrono::Utc::now().timestamp();
        let tx = conn.unchecked_transaction().map_err(|e| format!("tx: {e}"))?;
        for c in contacts {
            if c.email.is_empty() {
                continue;
            }
            let lc = c.email.to_lowercase();
            tx.execute(
                "INSERT INTO contacts (account_key, email, name, source, last_seen_ts) \
                 VALUES (?1, ?2, ?3, ?4, ?5) \
                 ON CONFLICT(account_key, email, source) DO UPDATE SET \
                   name = CASE WHEN excluded.name != '' THEN excluded.name ELSE name END, \
                   last_seen_ts = excluded.last_seen_ts",
                params![account_key, lc, c.name, c.source, now],
            )
            .map_err(|e| format!("upsert contact: {e}"))?;
        }
        tx.commit().map_err(|e| format!("commit: {e}"))
    }

    /// Search contacts by query (matches against email or name, case-insensitive).
    /// Deduped by email; carddav source wins over auto, then most recent.
    pub fn search_contacts(
        &self,
        account_key: &str,
        query: &str,
        limit: u32,
    ) -> Result<Vec<Contact>, String> {
        let conn = self.conn.lock().map_err(|e| format!("lock: {e}"))?;
        let pattern = format!("%{}%", query.to_lowercase());
        let mut stmt = conn
            .prepare(
                "SELECT email, name, source, last_seen_ts FROM contacts \
             WHERE account_key = ?1 \
             AND (LOWER(email) LIKE ?2 OR LOWER(name) LIKE ?2) \
             ORDER BY \
               CASE WHEN source='carddav' THEN 0 ELSE 1 END, \
               last_seen_ts DESC, \
               name",
            )
            .map_err(|e| format!("prepare: {e}"))?;
        let rows = stmt
            .query_map(params![account_key, pattern], |row| {
                Ok(Contact {
                    email: row.get::<_, String>(0)?,
                    name: row.get::<_, String>(1)?,
                    source: row.get::<_, String>(2)?,
                })
            })
            .map_err(|e| format!("query: {e}"))?;

        let mut seen_emails: std::collections::HashSet<String> = std::collections::HashSet::new();
        let mut out: Vec<Contact> = Vec::new();
        for row in rows {
            if let Ok(c) = row {
                if !seen_emails.insert(c.email.clone()) {
                    continue;
                }
                out.push(c);
                if out.len() as u32 >= limit {
                    break;
                }
            }
        }
        Ok(out)
    }

    /// Get cached avatar bytes + MIME if fresh enough (7d positive / 1d negative).
    /// Returns None when the cache miss should trigger a refetch.
    pub fn get_avatar(&self, email: &str) -> Option<(Vec<u8>, String)> {
        let conn = self.conn.lock().ok()?;
        let now = chrono::Utc::now().timestamp();
        let week_ago = now - 7 * 86400;
        let day_ago = now - 86400;
        // Empty payload = negative cache; expire after 1 day so transient
        // failures (DNS hiccup, server restart) get re-tried sooner.
        let row = conn
            .query_row(
                "SELECT png_data, mime, cached_at FROM avatar_cache WHERE email = ?1",
                params![email],
                |row| {
                    Ok((row.get::<_, Vec<u8>>(0)?, row.get::<_, String>(1)?, row.get::<_, i64>(2)?))
                },
            )
            .ok()?;
        let (data, mime, cached_at) = row;
        let ttl_floor = if data.is_empty() { day_ago } else { week_ago };
        if cached_at <= ttl_floor {
            return None;
        }
        Some((data, mime))
    }

    /// Save avatar bytes + MIME to cache. Empty data = negative cache row.
    pub fn save_avatar(&self, email: &str, data: &[u8], mime: &str) -> Result<(), String> {
        let conn = self.conn.lock().map_err(|e| format!("lock: {e}"))?;
        let now = chrono::Utc::now().timestamp();
        conn.execute(
            "INSERT OR REPLACE INTO avatar_cache (email, png_data, mime, cached_at) VALUES (?1, ?2, ?3, ?4)",
            params![email, data, mime, now],
        ).map_err(|e| format!("ins avatar: {e}"))?;
        Ok(())
    }

    // ── Calendar reminders ──

    /// Seed (or refresh) the alarm cascade for one future occurrence.
    ///
    /// `leads` come from the event's VALARMs in document order (element 0 =
    /// primary, the rest = the secondary cascade; 0 minutes = "at start").
    /// The signature fingerprints everything reminder-relevant about the
    /// event: matching → the occurrence is left untouched, so fired and
    /// cancelled states survive routine refetches; differing → every row is
    /// dropped and the cascade rebuilds as if the event just appeared (per
    /// spec, any event change resets its notifications).
    pub fn seed_occurrence(
        &self,
        event_id: i64,
        calendar_id: i64,
        occ_start_ms: i64,
        occ_end_ms: i64,
        summary: &str,
        leads: &[i32],
        signature: &str,
    ) -> Result<(), String> {
        let conn = self.conn.lock().map_err(|e| format!("lock: {e}"))?;

        // Проставить календарь строкам, посеянным до появления колонки (или
        // переехавшим между календарями). Идёт до всех ранних возвратов:
        // неизменившееся событие дальше не идёт, а привязка нужна и ему —
        // иначе выключение календаря его не погасит.
        conn.execute(
            "UPDATE reminders2 SET calendar_id = ?1 \
             WHERE event_id = ?2 AND occurrence_start_ms = ?3 AND calendar_id <> ?1",
            params![calendar_id, event_id, occ_start_ms],
        )
        .map_err(|e| format!("backfill calendar_id: {e}"))?;

        // A user decision for this LOGICAL occurrence (occurrence_start +
        // summary) must survive event-id churn. The server re-creates the same
        // meeting under a fresh event_id whenever the feed's identity is
        // ambiguous; without this guard a new id would re-arm the whole -N
        // cascade, resurrecting the pre-alarm the user just snoozed to «в
        // момент начала» (or dismissed with ✕).
        //
        // The key is deliberately the same pair `dedup_occurrence` uses, and
        // the same identity the server-side feed matcher keys on: three
        // mechanisms disagreeing about what "the same occurrence" means would
        // be worse than the residual risk here — two genuinely different
        // meetings sharing a title AND a start time also share the decision.
        //
        // We look across ALL event_ids: a manual reminder is seq=100, a
        // dismissal is status='cancelled'. seq=100 wins when both are present
        // (user_choice_reminder leaves cancelled rows alongside its seq=100).
        let decision: Option<(i64, i64, i64, i32)> = conn
            .query_row(
                "SELECT seq, fire_at_ms, occurrence_end_ms, at_start FROM reminders2 \
                 WHERE occurrence_start_ms = ?1 AND summary = ?2 \
                   AND (seq = 100 OR status = 'cancelled') \
                   AND status NOT IN ('done', 'expired') \
                 ORDER BY (seq = 100) DESC, fire_at_ms DESC LIMIT 1",
                params![occ_start_ms, summary],
                |r| Ok((r.get(0)?, r.get(1)?, r.get(2)?, r.get(3)?)),
            )
            .ok();
        if let Some((seq, fire_at, dec_end, at_start)) = decision {
            // Carry the decision onto THIS event_id if it doesn't already hold
            // it, so it outlives the old id being pruned. Never arm the cascade.
            let have: i64 = conn
                .query_row(
                    "SELECT COUNT(*) FROM reminders2 \
                     WHERE event_id = ?1 AND occurrence_start_ms = ?2 \
                       AND (seq = 100 OR status = 'cancelled') \
                       AND status NOT IN ('done', 'expired')",
                    params![event_id, occ_start_ms],
                    |r| r.get(0),
                )
                .unwrap_or(0);
            if have == 0 {
                let manual = seq == 100;
                let end = if dec_end > 0 { dec_end } else { occ_end_ms };
                if manual {
                    conn.execute(
                        "INSERT OR REPLACE INTO reminders2 \
                         (event_id, occurrence_start_ms, occurrence_end_ms, seq, fire_at_ms, \
                          lead_min, at_start, status, summary, signature, calendar_id) \
                         VALUES (?1, ?2, ?3, 100, ?4, ?5, ?6, 'armed', ?7, ?8, ?9)",
                        params![
                            event_id,
                            occ_start_ms,
                            end,
                            fire_at,
                            ((occ_start_ms - fire_at) / 60_000).max(0),
                            at_start,
                            summary,
                            signature,
                            calendar_id
                        ],
                    )
                    .map_err(|e| format!("carry manual: {e}"))?;
                } else {
                    // Dismissal marker: seq=100 + cancelled → scan never arms it,
                    // and this same guard keeps re-seeds quiet forever.
                    conn.execute(
                        "INSERT OR REPLACE INTO reminders2 \
                         (event_id, occurrence_start_ms, occurrence_end_ms, seq, fire_at_ms, \
                          lead_min, at_start, status, summary, signature, calendar_id) \
                         VALUES (?1, ?2, ?3, 100, ?4, 0, 0, 'cancelled', ?5, ?6, ?7)",
                        params![
                            event_id,
                            occ_start_ms,
                            end,
                            occ_start_ms,
                            summary,
                            signature,
                            calendar_id
                        ],
                    )
                    .map_err(|e| format!("carry dismissal: {e}"))?;
                }
            }
            return Ok(());
        }

        let existing: Option<String> = conn
            .query_row(
                "SELECT signature FROM reminders2 \
                 WHERE event_id = ?1 AND occurrence_start_ms = ?2 LIMIT 1",
                params![event_id, occ_start_ms],
                |r| r.get(0),
            )
            .ok();
        match existing {
            Some(sig) if sig == signature => return Ok(()),
            Some(_) => {
                conn.execute(
                    "DELETE FROM reminders2 WHERE event_id = ?1 AND occurrence_start_ms = ?2",
                    params![event_id, occ_start_ms],
                )
                .map_err(|e| format!("reseed clear: {e}"))?;
            }
            None => {}
        }
        for (i, lead) in leads.iter().enumerate() {
            let status = if i == 0 { "armed" } else { "chained" };
            conn.execute(
                "INSERT OR IGNORE INTO reminders2 \
                 (event_id, occurrence_start_ms, occurrence_end_ms, seq, fire_at_ms, \
                  lead_min, at_start, status, summary, signature, calendar_id) \
                 VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11)",
                params![
                    event_id,
                    occ_start_ms,
                    occ_end_ms,
                    i as i64,
                    occ_start_ms - (*lead as i64) * 60_000,
                    lead,
                    (*lead == 0) as i32,
                    status,
                    summary,
                    signature,
                    calendar_id
                ],
            )
            .map_err(|e| format!("seed row: {e}"))?;
        }
        Ok(())
    }

    /// Retire the alarm rows of any OTHER event that shares this logical
    /// occurrence (same start + summary). The same meeting can exist under
    /// several event_ids — a recurring master plus a stored override, or the
    /// same event mirrored across two calendars — and each would otherwise
    /// fire its own identical toast. We keep `keep_event_id`'s cascade and
    /// permanently silence the duplicates.
    pub fn dedup_occurrence(
        &self,
        keep_event_id: i64,
        occ_start_ms: i64,
        summary: &str,
    ) -> Result<(), String> {
        let conn = self.conn.lock().map_err(|e| format!("lock: {e}"))?;
        conn.execute(
            "UPDATE reminders2 SET status = 'done' \
             WHERE occurrence_start_ms = ?1 AND summary = ?2 AND event_id <> ?3 \
               AND status IN ('armed', 'chained', 'shown')",
            params![occ_start_ms, summary, keep_event_id],
        )
        .map_err(|e| format!("dedup occ: {e}"))?;
        Ok(())
    }

    /// The toast for this row is on screen — stop the scanner returning it.
    pub fn mark_reminder_shown(
        &self,
        event_id: i64,
        occ_start_ms: i64,
        seq: i64,
    ) -> Result<(), String> {
        let conn = self.conn.lock().map_err(|e| format!("lock: {e}"))?;
        conn.execute(
            "UPDATE reminders2 SET status = 'shown' \
             WHERE event_id = ?1 AND occurrence_start_ms = ?2 AND seq = ?3",
            params![event_id, occ_start_ms, seq],
        )
        .map_err(|e| format!("mark shown: {e}"))?;
        Ok(())
    }

    /// The toast died by TIMEOUT (no user choice): retire this row and arm
    /// the next link of the cascade, if any — per spec a secondary
    /// event-defined alarm fires only when the previous toast expired
    /// untouched.
    pub fn reminder_timeout(
        &self,
        event_id: i64,
        occ_start_ms: i64,
        seq: i64,
    ) -> Result<(), String> {
        let conn = self.conn.lock().map_err(|e| format!("lock: {e}"))?;
        conn.execute(
            "UPDATE reminders2 SET status = 'done' \
             WHERE event_id = ?1 AND occurrence_start_ms = ?2 AND seq = ?3",
            params![event_id, occ_start_ms, seq],
        )
        .map_err(|e| format!("timeout done: {e}"))?;
        conn.execute(
            "UPDATE reminders2 SET status = 'armed' \
             WHERE event_id = ?1 AND occurrence_start_ms = ?2 AND status = 'chained' \
               AND seq = (SELECT MIN(seq) FROM reminders2 \
                          WHERE event_id = ?1 AND occurrence_start_ms = ?2 AND status = 'chained')",
            params![event_id, occ_start_ms],
        )
        .map_err(|e| format!("arm next: {e}"))?;
        Ok(())
    }

    /// «✕» on a reminder toast (or a user snooze replacing the cascade):
    /// kill every remaining notification of the occurrence, irreversibly.
    /// Rows stay (status = cancelled) so the signature keeps routine
    /// reseeds from resurrecting them.
    pub fn cancel_occurrence_reminders(
        &self,
        event_id: i64,
        occ_start_ms: i64,
    ) -> Result<(), String> {
        let conn = self.conn.lock().map_err(|e| format!("lock: {e}"))?;
        conn.execute(
            "UPDATE reminders2 SET status = 'cancelled' \
             WHERE event_id = ?1 AND occurrence_start_ms = ?2 \
               AND status IN ('armed', 'chained', 'shown')",
            params![event_id, occ_start_ms],
        )
        .map_err(|e| format!("cancel occ: {e}"))?;
        Ok(())
    }

    /// «Напомнить позже» commit: replace the whole cascade with ONE
    /// user-chosen reminder (seq 100).
    pub fn user_choice_reminder(
        &self,
        event_id: i64,
        occ_start_ms: i64,
        occ_end_ms: i64,
        fire_at_ms: i64,
        at_start: bool,
        summary: &str,
    ) -> Result<(), String> {
        let sig: String = {
            let conn = self.conn.lock().map_err(|e| format!("lock: {e}"))?;
            conn.query_row(
                "SELECT signature FROM reminders2 \
                 WHERE event_id = ?1 AND occurrence_start_ms = ?2 LIMIT 1",
                params![event_id, occ_start_ms],
                |r| r.get(0),
            )
            .unwrap_or_default()
        };
        self.cancel_occurrence_reminders(event_id, occ_start_ms)?;
        let conn = self.conn.lock().map_err(|e| format!("lock: {e}"))?;
        conn.execute(
            "INSERT OR REPLACE INTO reminders2 \
             (event_id, occurrence_start_ms, occurrence_end_ms, seq, fire_at_ms, \
              lead_min, at_start, status, summary, signature) \
             VALUES (?1, ?2, ?3, 100, ?4, ?5, ?6, 'armed', ?7, ?8)",
            params![
                event_id,
                occ_start_ms,
                occ_end_ms,
                fire_at_ms,
                ((occ_start_ms - fire_at_ms) / 60_000).max(0),
                at_start as i32,
                summary,
                sig
            ],
        )
        .map_err(|e| format!("user choice ins: {e}"))?;
        Ok(())
    }

    /// The occurrence ended while nobody was looking — retire every row
    /// silently (startup corner case: "прошло — тихо удаляем").
    pub fn expire_occurrence_reminders(
        &self,
        event_id: i64,
        occ_start_ms: i64,
    ) -> Result<(), String> {
        let conn = self.conn.lock().map_err(|e| format!("lock: {e}"))?;
        conn.execute(
            "UPDATE reminders2 SET status = 'expired' \
             WHERE event_id = ?1 AND occurrence_start_ms = ?2 \
               AND status IN ('armed', 'chained', 'shown')",
            params![event_id, occ_start_ms],
        )
        .map_err(|e| format!("expire occ: {e}"))?;
        Ok(())
    }

    /// Delete all reminders for an event (every occurrence). Event edited or
    /// deleted → everything regenerates from the event's current settings.
    pub fn purge_event_reminders(&self, event_id: i64) -> Result<(), String> {
        let conn = self.conn.lock().map_err(|e| format!("lock: {e}"))?;
        conn.execute("DELETE FROM reminders2 WHERE event_id = ?1", params![event_id])
            .map_err(|e| format!("purge event: {e}"))?;
        Ok(())
    }

    /// Снять все напоминания календаря — целиком, а не только по событиям из
    /// загруженного окна. Возвращает event_id, у которых что-то удалилось:
    /// вызывающая сторона гасит по ним уже висящие тосты.
    ///
    /// Строки с calendar_id = 0 (посеяны до появления колонки и с тех пор не
    /// пересевались) сюда не попадают — их проставит ближайший seed_occurrence.
    pub fn purge_calendar_reminders(&self, calendar_id: i64) -> Result<Vec<i64>, String> {
        let conn = self.conn.lock().map_err(|e| format!("lock: {e}"))?;
        let mut stmt = conn
            .prepare("SELECT DISTINCT event_id FROM reminders2 WHERE calendar_id = ?1")
            .map_err(|e| format!("prepare purge calendar: {e}"))?;
        let ids: Vec<i64> = stmt
            .query_map(params![calendar_id], |r| r.get(0))
            .map_err(|e| format!("query purge calendar: {e}"))?
            .filter_map(|r| r.ok())
            .collect();
        drop(stmt);
        conn.execute("DELETE FROM reminders2 WHERE calendar_id = ?1", params![calendar_id])
            .map_err(|e| format!("purge calendar: {e}"))?;
        Ok(ids)
    }

    /// Обнулить привязку к календарю у строк события. Существует ради тестов
    /// в соседнем крейте: воспроизводит строку, посеянную до появления
    /// колонки `calendar_id`, чтобы проверить, что пересев её чинит.
    pub fn debug_clear_reminder_calendar(&self, event_id: i64) -> Result<(), String> {
        let conn = self.conn.lock().map_err(|e| format!("lock: {e}"))?;
        conn.execute(
            "UPDATE reminders2 SET calendar_id = 0 WHERE event_id = ?1",
            params![event_id],
        )
        .map_err(|e| format!("clear calendar_id: {e}"))?;
        Ok(())
    }

    /// Armed rows whose fire time has passed — raw material for the scan
    /// side, which decides shown / expired / already-running per occurrence.
    pub fn due_reminders(&self, now_ms: i64) -> Result<Vec<ReminderRow>, String> {
        let conn = self.conn.lock().map_err(|e| format!("lock: {e}"))?;
        let mut stmt = conn
            .prepare(
                "SELECT event_id, occurrence_start_ms, occurrence_end_ms, seq, \
                    fire_at_ms, lead_min, at_start, summary, calendar_id \
             FROM reminders2 \
             WHERE status = 'armed' AND fire_at_ms <= ?1 \
             ORDER BY fire_at_ms ASC",
            )
            .map_err(|e| format!("prep: {e}"))?;
        let rows = stmt
            .query_map(params![now_ms], |r| {
                Ok(ReminderRow {
                    event_id: r.get(0)?,
                    occurrence_start_ms: r.get(1)?,
                    occurrence_end_ms: r.get(2)?,
                    seq: r.get(3)?,
                    fire_at_ms: r.get(4)?,
                    lead_min: r.get(5)?,
                    at_start: r.get::<_, i32>(6)? != 0,
                    summary: r.get(7)?,
                    calendar_id: r.get(8)?,
                })
            })
            .map_err(|e| format!("query: {e}"))?;
        let mut out = Vec::new();
        for r in rows {
            out.push(r.map_err(|e| format!("row: {e}"))?);
        }
        Ok(out)
    }

    /// Bound the table: drop rows whose occurrence is well past.
    pub fn prune_old_reminders(&self, cutoff_ms: i64) -> Result<(), String> {
        let conn = self.conn.lock().map_err(|e| format!("lock: {e}"))?;
        conn.execute("DELETE FROM reminders2 WHERE occurrence_start_ms < ?1", params![cutoff_ms])
            .map_err(|e| format!("prune: {e}"))?;
        Ok(())
    }

    /// Cull reminders whose event vanished from the server: rows with an
    /// occurrence inside [from_ms, to_ms) whose event_id is not in `keep`
    /// (the ids of a fresh, COMPLETE fetch of that window) lose the whole
    /// event's cascade — a deleted meeting must not keep toasting. Returns
    /// the culled event ids so the caller can close any open toasts.
    pub fn prune_orphan_reminders(
        &self,
        from_ms: i64,
        to_ms: i64,
        keep: &std::collections::HashSet<i64>,
    ) -> Result<Vec<i64>, String> {
        let conn = self.conn.lock().map_err(|e| format!("lock: {e}"))?;
        let mut stmt = conn
            .prepare(
                "SELECT DISTINCT event_id FROM reminders2 \
                 WHERE occurrence_start_ms >= ?1 AND occurrence_start_ms < ?2",
            )
            .map_err(|e| format!("prepare: {e}"))?;
        let rows = stmt
            .query_map(params![from_ms, to_ms], |r| r.get::<_, i64>(0))
            .map_err(|e| format!("query: {e}"))?;
        let mut orphans = Vec::new();
        for r in rows {
            let id = r.map_err(|e| format!("row: {e}"))?;
            if !keep.contains(&id) {
                orphans.push(id);
            }
        }
        for id in &orphans {
            conn.execute("DELETE FROM reminders2 WHERE event_id = ?1", params![id])
                .map_err(|e| format!("delete: {e}"))?;
        }
        Ok(orphans)
    }

    /// Cull reminder rows whose occurrence no longer exists: the event is
    /// still present (same id) but was MOVED — rows keyed by the old
    /// occurrence_start stay armed and fire at the old time otherwise.
    /// `valid` maps event_id → the occurrence starts the current fetch
    /// actually derives inside [from_ms, to_ms); rows of events absent from
    /// `valid` are `prune_orphan_reminders`' job. Returns affected event ids
    /// so the UI can close their stale toasts.
    pub fn prune_moved_reminders(
        &self,
        from_ms: i64,
        to_ms: i64,
        valid: &std::collections::HashMap<i64, std::collections::HashSet<i64>>,
    ) -> Result<Vec<i64>, String> {
        let conn = self.conn.lock().map_err(|e| format!("lock: {e}"))?;
        let mut stmt = conn
            .prepare(
                "SELECT DISTINCT event_id, occurrence_start_ms FROM reminders2 \
                 WHERE occurrence_start_ms >= ?1 AND occurrence_start_ms < ?2",
            )
            .map_err(|e| format!("prepare: {e}"))?;
        let rows = stmt
            .query_map(params![from_ms, to_ms], |r| Ok((r.get::<_, i64>(0)?, r.get::<_, i64>(1)?)))
            .map_err(|e| format!("query: {e}"))?;
        let mut stale: Vec<(i64, i64)> = Vec::new();
        for r in rows {
            let (id, occ) = r.map_err(|e| format!("row: {e}"))?;
            if let Some(starts) = valid.get(&id) {
                if !starts.contains(&occ) {
                    stale.push((id, occ));
                }
            }
        }
        let mut affected: Vec<i64> = Vec::new();
        for (id, occ) in &stale {
            conn.execute(
                "DELETE FROM reminders2 WHERE event_id = ?1 AND occurrence_start_ms = ?2",
                params![id, occ],
            )
            .map_err(|e| format!("delete: {e}"))?;
            if !affected.contains(id) {
                affected.push(*id);
            }
        }
        Ok(affected)
    }
}

/// One alarm row of an occurrence's cascade, denormalised enough that the
/// notifier renders the toast without touching any other table.
#[derive(Debug, Clone, serde::Serialize)]
pub struct ReminderRow {
    pub event_id: i64,
    pub occurrence_start_ms: i64,
    /// 0 = unknown end (the scanner substitutes start + 30 min).
    pub occurrence_end_ms: i64,
    /// Cascade position: 0 primary, 1.. event-defined secondary, 100 user.
    pub seq: i64,
    pub fire_at_ms: i64,
    pub lead_min: i32,
    /// "At the moment of start" presentation: ✕ + body only, no snooze.
    pub at_start: bool,
    pub summary: String,
    /// Чей календарь — сканер проверяет видимость в момент выстрела, а не
    /// только при посеве. 0 = не атрибутировано (строка старой схемы).
    pub calendar_id: i64,
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Временный каталог без внешних зависимостей; удаляется на Drop.
    struct TmpDir(PathBuf);

    impl TmpDir {
        fn new(tag: &str) -> Self {
            static SEQ: std::sync::atomic::AtomicU32 = std::sync::atomic::AtomicU32::new(0);
            let n = SEQ.fetch_add(1, std::sync::atomic::Ordering::Relaxed);
            let dir = std::env::temp_dir().join(format!(
                "ddmail_cache_{tag}_{}_{n}_{}",
                std::process::id(),
                chrono::Utc::now().timestamp_nanos_opt().unwrap_or(0)
            ));
            std::fs::create_dir_all(&dir).unwrap();
            TmpDir(dir)
        }
        fn db(&self) -> PathBuf {
            self.0.join("cache.db")
        }
    }

    impl Drop for TmpDir {
        fn drop(&mut self) {
            std::fs::remove_dir_all(&self.0).ok();
        }
    }

    fn columns(conn: &Connection, table: &str) -> Vec<String> {
        let mut stmt = conn.prepare(&format!("PRAGMA table_info({table})")).unwrap();
        stmt.query_map([], |r| r.get::<_, String>(1)).unwrap().map(|r| r.unwrap()).collect()
    }

    fn schema_version(conn: &Connection) -> i64 {
        conn.pragma_query_value(None, "schema_version", |r| r.get(0)).unwrap()
    }

    fn mref(folder: &str, uid: u32) -> MessageRef {
        MessageRef { folder: folder.into(), uid, message_id: String::new(), seen: true }
    }

    fn body(folder: &str, uid: u32, text: &str) -> MessageBody {
        MessageBody {
            uid,
            folder: folder.into(),
            subject: "s".into(),
            from: "a@test".into(),
            from_addr: "a@test".into(),
            to: Vec::new(),
            cc: Vec::new(),
            date: String::new(),
            date_ts: 0,
            html: None,
            text: Some(text.into()),
            attachments: Vec::new(),
            is_outgoing: false,
            message_id: format!("<{uid}@test>"),
            in_reply_to: String::new(),
            references: Vec::new(),
            raw_headers: String::new(),
        }
    }

    /// База клиента до появления `user_version`, причём старого: без всех
    /// колонок, что доезжали `ALTER TABLE`, и с давно упразднённой таблицей
    /// напоминаний v1.
    fn make_legacy_db(path: &std::path::Path) {
        let conn = Connection::open(path).unwrap();
        conn.execute_batch(
            "
            PRAGMA journal_mode=WAL;
            CREATE TABLE conversations (
                id TEXT PRIMARY KEY, account_key TEXT NOT NULL, label TEXT NOT NULL,
                counterpart_name TEXT NOT NULL DEFAULT '', counterpart_addr TEXT NOT NULL DEFAULT '',
                is_group INTEGER NOT NULL DEFAULT 0, last_date TEXT NOT NULL DEFAULT '',
                last_date_ts INTEGER NOT NULL DEFAULT 0, unread_count INTEGER NOT NULL DEFAULT 0,
                total_count INTEGER NOT NULL DEFAULT 0, updated_at INTEGER NOT NULL DEFAULT 0);
            CREATE TABLE identities (
                email TEXT NOT NULL, account_key TEXT NOT NULL, name TEXT NOT NULL DEFAULT '',
                signature TEXT NOT NULL DEFAULT '', is_default INTEGER NOT NULL DEFAULT 0,
                color TEXT NOT NULL DEFAULT '', PRIMARY KEY(email, account_key));
            CREATE TABLE avatar_cache (
                email TEXT PRIMARY KEY, png_data BLOB, cached_at INTEGER NOT NULL DEFAULT 0);
            CREATE TABLE conversation_messages (
                id INTEGER PRIMARY KEY AUTOINCREMENT, conversation_id TEXT NOT NULL,
                folder TEXT NOT NULL, uid INTEGER NOT NULL, UNIQUE(conversation_id, folder, uid));
            CREATE TABLE message_bodies (
                folder TEXT NOT NULL, uid INTEGER NOT NULL, account_key TEXT NOT NULL,
                subject TEXT NOT NULL DEFAULT '', from_header TEXT NOT NULL DEFAULT '',
                from_addr TEXT NOT NULL DEFAULT '', to_header TEXT NOT NULL DEFAULT '',
                cc_header TEXT NOT NULL DEFAULT '', date_header TEXT NOT NULL DEFAULT '',
                date_ts INTEGER NOT NULL DEFAULT 0, html TEXT, text_body TEXT,
                attachments_json TEXT NOT NULL DEFAULT '[]', is_outgoing INTEGER NOT NULL DEFAULT 0,
                cached_at INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(folder, uid, account_key));
            CREATE TABLE reminders2 (
                event_id INTEGER NOT NULL, occurrence_start_ms INTEGER NOT NULL,
                occurrence_end_ms INTEGER NOT NULL DEFAULT 0, seq INTEGER NOT NULL,
                fire_at_ms INTEGER NOT NULL, lead_min INTEGER NOT NULL DEFAULT 0,
                at_start INTEGER NOT NULL DEFAULT 0, status TEXT NOT NULL DEFAULT 'armed',
                summary TEXT NOT NULL DEFAULT '', signature TEXT NOT NULL DEFAULT '',
                PRIMARY KEY (event_id, occurrence_start_ms, seq));
            CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL DEFAULT '');
            CREATE TABLE event_reminders (event_id INTEGER, occurrence INTEGER,
                PRIMARY KEY(event_id, occurrence));

            INSERT INTO conversations (id, account_key, label, last_date_ts)
                VALUES ('c1', 'acc', 'Иван', 100);
            INSERT INTO conversation_messages (conversation_id, folder, uid)
                VALUES ('c1', 'INBOX', 7);
            INSERT INTO message_bodies (folder, uid, account_key, subject, text_body)
                VALUES ('INBOX', 7, 'acc', 'тема', 'тело письма');
            INSERT INTO identities (email, account_key, name) VALUES ('me@test', 'acc', 'Я');
            INSERT INTO meta (key, value) VALUES ('reminders_data_version', '4');
            INSERT INTO meta (key, value) VALUES ('conv_full_ts:acc', '12345');
            INSERT INTO reminders2 (event_id, occurrence_start_ms, seq, fire_at_ms, status, summary)
                VALUES (1, 1000, 100, 900, 'armed', 'отложенное пользователем');
            ",
        )
        .unwrap();
    }

    #[test]
    fn legacy_db_migrates_without_data_loss() {
        let dir = TmpDir::new("legacy");
        make_legacy_db(&dir.db());

        let cache = Cache::new(dir.0.clone()).expect("cache");

        let conn = Connection::open(dir.db()).unwrap();
        assert_eq!(user_version(&conn).unwrap(), SCHEMA_VERSION);
        for (table, column, _) in BASE_COLUMNS {
            assert!(columns(&conn, table).iter().any(|c| c == column), "{table}.{column}");
        }
        assert!(columns(&conn, "message_bodies").iter().any(|c| c == "parser_version"));
        let old_table: i64 = conn
            .query_row(
                "SELECT COUNT(*) FROM sqlite_master WHERE name = 'event_reminders'",
                [],
                |r| r.get(0),
            )
            .unwrap();
        assert_eq!(old_table, 0);

        // Данные на месте и читаются штатным кодом.
        let convs = cache.load_conversations("acc").unwrap();
        assert_eq!(convs.len(), 1);
        assert_eq!(convs[0].label, "Иван");
        assert_eq!(convs[0].messages.len(), 1);
        assert!(convs[0].messages[0].seen, "seen по умолчанию 1");
        let bodies = cache.load_message_bodies("acc", &[mref("INBOX", 7)]).unwrap();
        assert_eq!(bodies.len(), 1);
        assert_eq!(bodies[0].text.as_deref(), Some("тело письма"));
        assert_eq!(cache.load_identities("acc").unwrap().len(), 1);
        assert_eq!(cache.get_meta("conv_full_ts:acc").as_deref(), Some("12345"));
        // Напоминание (не восстановимое с сервера) пережило миграцию.
        let due = cache.due_reminders(i64::MAX).unwrap();
        assert_eq!(due.len(), 1);
        assert_eq!(due[0].summary, "отложенное пользователем");
        assert_eq!(due[0].calendar_id, 0);
        // Строки, лежавшие до версии разбора, перезапрашивать не нужно.
        assert!(cache.stale_body_refs("acc", &[mref("INBOX", 7)]).unwrap().is_empty());
    }

    /// База, где часть колонок уже добавлена прежним кодом (`ALTER … .ok()`),
    /// а часть — нет: миграция 1 обязана пройти без «duplicate column».
    #[test]
    fn partially_altered_legacy_db_migrates() {
        let dir = TmpDir::new("partial");
        make_legacy_db(&dir.db());
        {
            let conn = Connection::open(dir.db()).unwrap();
            conn.execute_batch(
                "ALTER TABLE conversations ADD COLUMN avatar_hash TEXT NOT NULL DEFAULT 'h';
                 ALTER TABLE avatar_cache ADD COLUMN mime TEXT NOT NULL DEFAULT '';",
            )
            .unwrap();
        }
        let cache = Cache::new(dir.0.clone()).expect("cache");
        let convs = cache.load_conversations("acc").unwrap();
        assert_eq!(convs[0].avatar_hash, "h");
        let conn = Connection::open(dir.db()).unwrap();
        assert_eq!(user_version(&conn).unwrap(), SCHEMA_VERSION);
    }

    #[test]
    fn fresh_db_gets_full_schema() {
        let dir = TmpDir::new("fresh");
        let cache = Cache::new(dir.0.clone()).expect("cache");
        cache.save_message_bodies("acc", &[body("INBOX", 1, "t")]).unwrap();
        let conn = Connection::open(dir.db()).unwrap();
        assert_eq!(user_version(&conn).unwrap(), SCHEMA_VERSION);
        for (table, column, _) in BASE_COLUMNS {
            assert!(columns(&conn, table).iter().any(|c| c == column), "{table}.{column}");
        }
        assert!(columns(&conn, "message_bodies").iter().any(|c| c == "parser_version"));
    }

    /// Повторный старт схему не трогает: `schema_version` SQLite растёт от
    /// любого DDL, в том числе от «пустого» ALTER.
    #[test]
    fn reopen_is_noop() {
        let dir = TmpDir::new("reopen");
        drop(Cache::new(dir.0.clone()).expect("cache"));
        let before = schema_version(&Connection::open(dir.db()).unwrap());
        drop(Cache::new(dir.0.clone()).expect("cache"));
        drop(Cache::new(dir.0.clone()).expect("cache"));
        let conn = Connection::open(dir.db()).unwrap();
        assert_eq!(schema_version(&conn), before);
        assert_eq!(user_version(&conn).unwrap(), SCHEMA_VERSION);
    }

    /// База от более нового клиента открывается и не «откатывается».
    #[test]
    fn newer_schema_is_left_alone() {
        let dir = TmpDir::new("newer");
        drop(Cache::new(dir.0.clone()).expect("cache"));
        Connection::open(dir.db())
            .unwrap()
            .pragma_update(None, "user_version", SCHEMA_VERSION + 5)
            .unwrap();
        let cache = Cache::new(dir.0.clone()).expect("cache");
        cache.set_meta("k", "v").unwrap();
        let conn = Connection::open(dir.db()).unwrap();
        assert_eq!(user_version(&conn).unwrap(), SCHEMA_VERSION + 5);
    }

    /// Миграция, упавшая на середине, не оставляет полусделанной схемы и не
    /// двигает версию.
    #[test]
    fn failed_migration_rolls_back() {
        let dir = TmpDir::new("rollback");
        make_legacy_db(&dir.db());
        let mut conn = Connection::open(dir.db()).unwrap();
        let tx = conn.transaction().unwrap();
        let r = (|| -> rusqlite::Result<()> {
            m001_base_schema(&tx)?;
            tx.execute("SELECT * FROM no_such_table", [])?;
            Ok(())
        })();
        assert!(r.is_err());
        drop(tx); // rollback
        assert_eq!(user_version(&conn).unwrap(), 0);
        assert!(!columns(&conn, "conversations").iter().any(|c| c == "received_by"));
    }

    #[test]
    fn corrupt_file_is_quarantined_not_deleted() {
        let dir = TmpDir::new("corrupt");
        std::fs::write(dir.db(), vec![0x5au8; 8192]).unwrap();
        let cache = Cache::new(dir.0.clone()).expect("cache over a garbage file");
        cache.set_meta("k", "v").unwrap();
        let broken: Vec<_> = std::fs::read_dir(&dir.0)
            .unwrap()
            .filter_map(|e| e.ok())
            .filter(|e| e.file_name().to_string_lossy().starts_with("cache.db.broken-"))
            .collect();
        assert_eq!(broken.len(), 1);
        assert_eq!(std::fs::read(broken[0].path()).unwrap(), vec![0x5au8; 8192]);
    }

    #[test]
    fn parser_version_marks_stale_rows() {
        let dir = TmpDir::new("parser");
        let cache = Cache::new(dir.0.clone()).expect("cache");
        let refs = [mref("INBOX", 1), mref("INBOX", 2), mref("INBOX", 3)];
        cache.save_message_bodies("acc", &[body("INBOX", 1, "a"), body("INBOX", 2, "b")]).unwrap();
        assert!(cache.stale_body_refs("acc", &refs).unwrap().is_empty(), "свежие и отсутствующие");

        // Строка, разобранная прежней версией.
        Connection::open(dir.db())
            .unwrap()
            .execute(
                "UPDATE message_bodies SET parser_version = ?1 WHERE uid = 2",
                [PARSER_VERSION - 1],
            )
            .unwrap();
        let stale = cache.stale_body_refs("acc", &refs).unwrap();
        assert_eq!(stale.iter().map(|m| m.uid).collect::<Vec<_>>(), vec![2]);
        // Устаревшее тело по-прежнему читается — офлайн показывать есть что.
        assert_eq!(cache.load_message_bodies("acc", &refs).unwrap().len(), 2);

        // Пересохранение после cid:-подстановки версию не поднимает…
        cache.resave_message_bodies("acc", &[body("INBOX", 2, "b2")]).unwrap();
        assert_eq!(cache.stale_body_refs("acc", &refs).unwrap().len(), 1);
        // …а новая строка через него получает текущую.
        cache.resave_message_bodies("acc", &[body("INBOX", 3, "c")]).unwrap();
        assert_eq!(cache.stale_body_refs("acc", &refs).unwrap().len(), 1);
        // Перезапрос с сервера — поднимает.
        cache.save_message_bodies("acc", &[body("INBOX", 2, "b3")]).unwrap();
        assert!(cache.stale_body_refs("acc", &refs).unwrap().is_empty());
    }
}
