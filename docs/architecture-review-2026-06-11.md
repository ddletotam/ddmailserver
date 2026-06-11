# Архитектурное ревью — 11.06.2026

Двухчастное ревью: серверная часть (Go) и десктоп-клиент (Rust/Slint).
Критерии заказчика:

1. **Сервер**: стандартный протокольный контур (IMAP/SMTP/CalDAV/CardDAV/LDAP) и контур
   собственного клиента (HTTP API) не должны перемешиваться; сервер одинаково хорошо
   обслуживает Thunderbird/Apple Mail и наш клиент.
2. **Клиент**: UI и функционал не должны блокировать друг друга; UI — «тонкий» слой
   поверх функционального ядра; всё фоноспособное — в фон, в UI только прогресс.

---

## Часть 1. Сервер (Go)

### Карта архитектуры как есть

```
cmd/mailserver
   ├──> web ──> ВСЁ (db, imap/client, caldav/*, carddav/*, calendar,
   │            avatar, search, oauth, notify, parser, crypto ...)
   ├──> worker ──> db, imap/client, smtp/client, caldav/*, carddav/client, ...
   ├──> imap/server  ──> db, models, parser, search, notify, timeutil
   ├──> imap/client  ──> db, models, parser, oauth, calendar, task
   ├──> smtp/server  ──> db, models            (минимальный)
   ├──> smtp/mx      ──> db, models, parser, notify, calendar
   ├──> caldav/server, carddav/server, ldap ──> db, models
   └──> db ──> crypto, models, timeutil        (низ стека)
```

### Вердикт по главному критерию: контуры разделены чисто ✅

- Ни один протокольный сервер не импортирует `internal/web`. Клиентская специфика
  (JWT, desktop WS, аватары, поисковые дропдауны) живёт в `web` и ходит в данные
  через общий `internal/db`, не в обход.
- Ни одного `if clientIsOurs` в командной логике IMAP/SMTP. Grep по
  avatar/calendar/jwt/desktop дал только легитимные совпадения: iTIP-обработка `.ics`
  (стандартная серверная функция) и XOAUTH2 к внешним провайдерам (не наш JWT).
- Единственный канал клиентской специфики в IMAP — `imap/server/metadata.go`:
  `/shared/vendor/ddmail/identities` через RFC 5464 vendor-namespace. Это корректный
  способ (чужие клиенты игнорируют), но канал требует дисциплины: METADATA держать
  тонким, богатую функциональность — только через HTTP API.
  Мелочь: анонсируется capability `METADATA`, а реализован только GETMETADATA —
  правильнее `METADATA-SERVER` либо корректный NO на SETMETADATA.

### [КРИТИЧНО]

| # | Проблема | Где | Суть | Статус |
|---|----------|-----|------|--------|
| S1 | Гонка генерации UID | `db/messages.go:935` `CopyMessageToFolder` | read-modify-write `uid_next` без транзакции (vs атомарный `GetNextUIDForFolder:629`). Параллельные COPY/MOVE+APPEND в одну папку → одинаковый UID → нарушение RFC 3501 §2.3.1.1, рассинхрон кэшей клиентов вплоть до потери писем. Чинить: `UPDATE ... RETURNING` + транзакция вокруг выдачи UID + INSERT. | ✅ исправлено 11.06 |
| S2 | EXPUNGE/flags не пушатся в другие сессии | `imap/server/mailbox.go:818`, `backend.go:71` | Backend умеет слать только EXISTS (новые письма). Удаление/смена флагов одним клиентом не доезжает до другого открытого сеанса (Apple Mail в IDLE видит «призраков»). Ломает именно стандартные клиенты. Чинить: слать `backend.ExpungeUpdate` (seqnum по убыванию) и `backend.MessageUpdate`. | ✅ исправлено 11.06 |
| S3 | flag_sync без retry/backoff/DLQ | `worker/flag_sync_task.go:100-106`, `db/flag_sync.go` | При лежащем внешнем IMAP очередь молотит connect+LOGIN на каждом тике, бесконечно. Upsert «latest wins» (`flag_sync.go:13`) может затереть queued-delete → письмо на источнике не удалится. Чинить по образцу календаря (миграция 035: retry_count/last_error/next_attempt_at + бэкофф). | ✅ исправлено 11.06 (**миграция 037 — применить до деплоя!**); queued-delete теперь sticky |
| S4 | Нет дедупликации in-flight задач | `worker/scheduler.go:512,:214`, `pool.go:188` | Медленный внешний сервер → два параллельных FlagSyncTask/SyncTask на один аккаунт → двойные STORE, гонки с S3. Чинить: singleflight per accountID. | ⬜ |

### [ВАЖНО]

| # | Проблема | Где | Суть | Статус |
|---|----------|-----|------|--------|
| S5 | O(folder) + потолок 10000 на IMAP-операциях | `mailbox.go:138,238,282,450,629,670,723,770` | Каждый FETCH/STORE/COPY/MOVE грузит всю папку (LIMIT 10000) для seqnum-маппинга. При >10k писем маппинг неполный → UID-операции молча теряют письма. Чинить: UID-операции резолвить адресно в SQL. | ⬜ |
| S6 | MOVE без транзакции | `mailbox.go:664,759`, `db/messages.go` | COPY всех, потом DELETE циклом; паника между фазами = дубликаты в обеих папках. | ⬜ |
| S7 | CORS через `strings.Contains` | `web/middleware.go:168` | Пропустит `mail.letotam.ru.evil.com`; рядом `Allow-Credentials: true` (cookie-контур). Чинить: точное сравнение хоста / allowlist. | ✅ исправлено 11.06 |
| S8 | Фрагментированный конфиг IMAP-сервера | `imap/server/server.go:26,80,109,161` | AutoLogout и расширения (UIDPLUS/IDLE/METADATA) только в `NewWithTLSAndHub`; все конструкторы ставят `AllowInsecureAuth=true`. Чинить: единая фабрика опций. | ⬜ |
| S9 | Нет `local_modified`-аналога для флагов писем | `imap/client/sync_task.go` | Окно: pull-синк может перетереть ещё-не-отправленное локальное изменение флага (для календаря/контактов паттерн есть). Чинить: при обновлении строки не трогать флаги, если по message_id есть запись в flag_sync_queue. | ⬜ |

### [ЗАМЕТКА]

- CONDSTORE/QRESYNC нет — допустимо, но в связке с S2 многосессионный синк флагов запаздывает. Направление развития.
- Протокольные логи болтливы (адреса/UID на INFO) — с учётом 152-ФЗ понизить до debug, не логировать идентичности.
- `bodyCache` по msg.ID — корректно (тела immutable), кэш общий на backend.
- Удачно: разделение `direct_send_task` vs `send_task` (`scheduler.go:291`); `err != nil` соблюдается; глобальных мутабельных переменных нет.

---

## Часть 2. Десктоп-клиент (Rust/Slint)

### Карта потоков как есть

1. **UI-поток** (Slint loop): все `ui.on_*`, `handle_engine_result`, 2 таймера. Владеет `Rc<Shared>` (RefCell-поля).
2. **Render-поток**: один, `Job` через unbounded mpsc (`main.rs:1152`); WebView2/WebKit → RGBA; `body_cache` по ключу `(folder, uid, width, policy_gen)`.
3. **Engine-поток**: один, `EngineCmd` через unbounded mpsc (`engine.rs:350`); на каждую команду `rt.block_on` — Tokio используется как однопоточный исполнитель.

Источник правды двойной: доменное состояние в `Shared`, презентационное в Slint-свойствах; синхронизация ручная (4 скопированных места `active-*`/`selected`).

### Вердикт: диагноз заказчика подтверждён

Ресайз-кейс (`main.rs:1446` → полный `open_conversation`, включая сетевой `FetchMessages`) —
симптом отсутствия границы между «сменить диалог» и «переразложить пиксели».

### [КРИТИЧНО]

| # | Проблема | Где | Суть | Статус |
|---|----------|-----|------|--------|
| C1 | Ресайз = полная перезагрузка диалога | `main.rs:1446-1453` → `550-588` | (а) синхронный SQLite в UI-колбэке, (б) 100% промах body_cache при новой ширине, (в) безусловный сетевой FetchMessages. Порог 24px не спасает при drag. Чинить: тела в памяти (`current_bodies`), ресайз шлёт только relayout с дебаунсом, сеть — только при смене диалога. | ✅ исправлено 11.06 |
| C2 | Нет gen-токена у фетча тел | `main.rs:2262-2277` | Открыл A → переключился на B → запоздавший ответ A затирает экран B. Для поиска guard есть (`search_query_inflight:2358`), для тел/календаря — нет. Чинить: `open_generation`, дроп устаревших результатов и Job'ов. | ✅ исправлено 11.06 |
| C3 | Синхронный SQLite в UI-колбэках системно | `main.rs:1621, 1833, 1859, 1877, 1338-1346` | Каждый Enter в композере грузит ВСЕ тела диалога ради subject; reply/forward/toggle — по одному телу; стартовый цикл сканирует диалоги до первого непустого до первого кадра. Чинить: брать из `current_bodies[row]`. | ✅ почти весь (Enter, reply/forward, toggle-media/scripts — из `current_bodies`; toggle больше не делает сетевой рефетч). Остался стартовый цикл — ⬜ |
| C4 | Один engine-поток сериализует всё | `engine.rs:363-520` | Долгий FetchMessages/Send блокирует SearchDropdown, SetFlags, календарь. Чинить: быстрый/тяжёлый каналы либо `rt.spawn` с лимитом конкурентности по типу. | ⬜ |

### [ВАЖНО]

| # | Проблема | Где | Суть | Статус |
|---|----------|-----|------|--------|
| C5 | Unbounded-каналы без коалесцирования | `main.rs:1152`, `engine.rs:350` | Drag-ресайз ставит N полных перерендеров; отменить нельзя; «залипший» прогресс-бар. Чинить: latest-wins / gen-дроп на render-потоке. | ✅ латest-wins через `render_seq` 11.06: устаревшие job'ы скипаются целиком + abort между телами mid-render. Engine-канал — ⬜ (это C4) |
| C6 | body_cache без лимита | `main.rs:1167,1234` | Рост по (письмо × ширина × policy_gen), bitmap'ы до 6000px высотой — неограниченный RAM. Чинить: LRU по байтам + квантование ширины + сброс старых policy_gen. | ⬜ |
| C7 | `main.rs` 2444 строки, ~7 ответственностей | весь файл | composer, календарная раскладка (apply_calendar_view — 175 строк), search, render-цикл, hit-test. Чинить: декомпозиция (см. целевая архитектура). | ⬜ |
| C8 | RefCell-borrow держится на дисциплине | `main.rs:1533, 1420, 769-773` | Вложенные borrow в колбэках, спасает `.clone()` по соглашению. Мина при росте кода. | ⬜ |
| C9 | Слепой рефетч на любой Done | `main.rs:2289-2302` | Пометил письмо прочитанным → полный рефетч 200 диалогов + тел в сериализованный канал. Чинить: точечное обновление состояния. | ⬜ |

### Что уже хорошо (не переделывать)

- Slint-код почти полностью декларативен, логика в Rust.
- Pack-to-Image (memcpy RGBA) на render-потоке, UI — только `Image::from_rgba8`.
- `render_common.rs` — чистая граница WebView2/WebKitGTK; hit-test point-in-rect без живого DOM.
- Stale-guard поиска — образец для C2.

### Целевая архитектура: Intent вниз, StatePatch вверх

UI-колбэк = 1-3 строки: собрать `Intent`, отдать ядру. Ядро владеет состоянием и
воркерами, наружу — только готовые презентационные снимки:

```rust
enum Intent {                        // UI → ядро
    OpenConversation(usize),
    ResizeViewport(u32),             // ядро решает: relayout, НЕ fetch
    Send(SendSpec),
    MsgAction { row, action },
    SearchTyped(String),
    Calendar(CalIntent),
}
enum StatePatch {                    // ядро → UI, уже презентационное
    Sidebar(Vec<ConvItem>),
    Messages { rows, generation },
    RenderProgress { done, total },
    Calendar(CalendarView),
}
```

Декомпозиция `main.rs`: `core/state.rs` (единый AppState), `core/conversations.rs`,
`core/compose.rs`, `core/search.rs`, `core/calendar.rs`, `ui/bubble_html.rs`,
`render/worker.rs`, тонкий `app/wiring.rs`.

Порядок миграции: (1) gen-токены → (2) расцепить ресайз → (3) latest-wins + LRU →
(4) SQLite из колбэков → (5) разделить engine → (6-7) экстракция модулей + Intent/StatePatch.
Шаги 1-5 дают весь выигрыш по отзывчивости без переписывания.

---

## Сводный приоритет

| # | Где | Что | Почему |
|---|-----|-----|--------|
| 1 | сервер | S1 UID-гонка | целостность данных |
| 2 | сервер | S2 EXPUNGE/flags push | ломает стандартные клиенты — критерий №1 |
| 3 | клиент | C1+C2 gen-токены + ресайз | главная UX-боль, низкий риск |
| 4 | сервер | S3 DLQ для flag_sync | решение уже написано для календаря |
| 5 | клиент | C4+C5 engine-каналы, latest-wins, LRU | системная отзывчивость |
| 6 | оба | S4 singleflight / C7 декомпозиция | гигиена, постепенно |

**Главный архитектурный вывод**: сервер уже имеет ту структуру, которую нужно получить
на клиенте — общее ядро (`db`/`models`) и независимые «тонкие» потребители поверх него.
Клиенту нужна та же форма: ядро владеет состоянием и I/O, UI — один из потребителей.
