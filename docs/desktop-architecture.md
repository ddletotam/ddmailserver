# DDMail Desktop — архитектура клиента (C2/C3 + data flow)

Дата: 2026-07-21. Код: `desktop/native` (Slint UI) + `desktop/core` (движок данных).

## Почему «однопоточный» движок

Клиент — **не** однопоточный: в процессе живёт полдюжины потоков. Однопоточным
(точнее — *последовательным*) сделан только **командный цикл движка**
(`engine.rs`): один `std::thread` крутит `mpsc`-очередь и выполняет команды
строго по одной (`rt.block_on` на каждую сетевую операцию). Это осознанный
выбор:

1. **SQLite-кэш — один писатель.** `cache.db` открыт одним соединением под
   `Mutex`. Последовательный цикл гарантирует, что запись диалогов, тел,
   watermark-ов и reminder-строк не дерётся за блокировки и не ловит
   `SQLITE_BUSY`.
2. **Стейтфул-протоколы.** IMAP-сессия имеет состояние (`SELECT`-нутая папка);
   команды к одному аккаунту обязаны идти по очереди. Параллелить = держать
   пул соединений на аккаунт — цена без выгоды при наших объёмах.
3. **Порядок результатов = корректность.** Delta-синк опирается на
   watermark-ы (`conv_since`, `journal_seq`): fetch → apply → save нового
   watermark должны быть атомарной последовательностью. Параллельные fetch-и
   могли бы записать watermark поверх более раннего незавершённого.
4. **Простота отказов.** Один цикл — одно место для обработки паник, ретраев
   и rebuild-а движка при смене конфигурации аккаунтов.

Тяжёлые и независимые вещи из цикла **вынесены**:

- **Рендер писем** — отдельный поток с WebKitGTK/WebView2 (тулкиты сами
  требуют один выделенный поток: GTK-цикл на Linux, COM STA на Windows).
- **WebSocket-вотчеры** аккаунтов — tokio-задачи на воркерах рантайма движка,
  пуши приходят независимо от очереди команд.
- **UI** никогда не ждёт движок: команды уходят в очередь, результаты
  возвращаются через `slint::invoke_from_event_loop`.

Цена последовательности проявилась 2026-07-21: ~150 аватарных HTTP-фетчей
вставали в очередь ПЕРЕД интерактивными командами календаря — «безумно
долгая загрузка». Лечение — не параллелизм, а **приоритет**: `FetchAvatar`
паркуется в `avatar_backlog` и обслуживается только при пустом канале.
Если когда-нибудь упрёмся снова — следующий шаг не «сделать всё параллельным»,
а выделить второй низкоприоритетный цикл для bulk-операций.

## Потоки процесса

| Поток | Кто создаёт | Задача |
|---|---|---|
| UI (main) | Slint | event loop, состояние `Shared`, таймеры (reminders scan, geometry saver), тост-окна |
| Engine | `engine.rs:608` | последовательный командный цикл + tokio Runtime |
| tokio workers | Runtime движка | WS-вотчеры аккаунтов (push) |
| Render worker | `main.rs` | WebKitGTK / WebView2 offscreen-рендер тел писем |
| ksni tray | `tray.rs` | D-Bus StatusNotifierItem (Linux) |
| One-shot login | по кнопке | login/OAuth без блокировки UI |

## C2 — контейнеры

```mermaid
flowchart TB
    user((Пользователь))

    subgraph app["DDMail Desktop (процесс ddmail-native)"]
        ui["UI-поток<br/>Slint event loop, Shared,<br/>тосты, трей, reminders-таймер"]
        engine["Движок<br/>последовательный командный цикл,<br/>avatar-backlog, tokio Runtime"]
        render["Render worker<br/>WebKitGTK (Linux) / WebView2 (Win)<br/>offscreen HTML → RGBA"]
        cache[("SQLite cache.db<br/>диалоги, тела, контакты,<br/>reminders2, watermarks")]
        texcache[("Texture cache<br/>RAM + PNG на диске")]
        cfg[/"accounts.json, calendar.json,<br/>window.json, policy"/]
    end

    server["DDMail Server<br/>(mail.letotam.ru)"]
    ext_dav["Внешние CalDAV/CardDAV<br/>(Yandex, Google, iCloud)"]
    ext_imap["Внешние IMAP/SMTP<br/>(standalone-аккаунты)"]
    goauth["Google OAuth2<br/>(loopback flow)"]

    user -->|клики, ввод| ui
    ui -->|"EngineCmd (mpsc)"| engine
    engine -->|"EngineResult → invoke_from_event_loop"| ui
    ui -->|"render jobs (mpsc, seq/abort)"| render
    render -->|"SharedPixelBuffer + link/text rects"| ui
    engine <--> cache
    render <--> texcache
    ui <--> cfg

    engine -->|"HTTP API /api/desktop/v1/*"| server
    engine -.->|"WebSocket push:<br/>new_message, flags_changed,<br/>calendar_updated"| server
    engine -->|CalDAV/CardDAV| ext_dav
    engine -->|IMAP/SMTP| ext_imap
    engine -->|"token refresh"| goauth
```

## C3 — компоненты клиента

```mermaid
flowchart LR
    subgraph uithread["UI-поток (main.rs)"]
        shared["Shared<br/>(thread-local состояние)"]
        her["handle_engine_result<br/>(диспетчер результатов)"]
        calview["apply_calendar_view<br/>compute_h/v, pending_cal_scroll"]
        remsched["reminders scan-таймер<br/>+ handle_reminder_action"]
        toasts["toast_window<br/>(frameless Slint-окна)"]
        tray["tray (ksni / tray-icon)"]
        winstate["window_state, policy,<br/>calendar_settings"]
    end

    subgraph enginethread["Поток движка (engine.rs)"]
        loop_["Командный цикл<br/>(mpsc, avatar_backlog)"]
        conns["AccountConn × N<br/>(key → provider)"]
    end

    subgraph core["ddmail-core (провайдеры)"]
        native["NativeProvider<br/>HTTP + WS-вотчер,<br/>auto-refresh JWT"]
        imapp["ImapProvider<br/>IMAP/SMTP + XOAUTH2"]
        caldav["caldav_client /<br/>carddav_client (RFC 6764)"]
        oauth["oauth (Google loopback)"]
        cachemod["cache.rs (SQLite):<br/>conversations, bodies,<br/>reminders2, meta"]
    end

    subgraph renderthread["Render worker"]
        rengine["render_webkit /<br/>render_webview2"]
        rcommon["render_common<br/>(link rects, text runs,<br/>hide scrollbars)"]
        texc["texture_cache<br/>RAM LRU + disk PNG"]
        sanitize["sanitize + policy<br/>(media/scripts per sender)"]
    end

    shared -->|EngineCmd| loop_
    loop_ --> conns
    conns --> native & imapp
    imapp --> caldav & oauth
    loop_ <--> cachemod
    loop_ -->|EngineResult| her
    native -.->|EngineEvent push| her
    her --> calview & shared
    remsched --> toasts
    her -->|"CalendarEvents → seed/prune"| remsched
    shared -->|render job| rengine
    rengine --> rcommon & sanitize
    rengine <--> texc
    rengine -->|битмапы + слои| shared
```

## Data flow

### Открытие диалога (рендер тел)

```mermaid
sequenceDiagram
    participant U as Пользователь
    participant UI as UI-поток
    participant E as Движок
    participant S as Сервер (HTTP)
    participant R as Render worker

    U->>UI: клик по диалогу
    UI->>E: FetchMessages(conversation)
    E->>S: GET /messages (тела, cid→data)
    S-->>E: тела писем
    E-->>UI: Bodies → invoke_from_event_loop
    UI->>R: render job (bodies, width, scale, seq)
    Note over R: cache-иерархия:<br/>RAM → disk PNG → WebKit render
    R->>R: sanitize → HTML → bitmap +<br/>link rects + text runs
    R-->>UI: SharedPixelBuffer на каждое письмо
    UI->>UI: Image + слои кликов/выделения
```

### Входящее письмо (push → delta)

```mermaid
sequenceDiagram
    participant MX as Сервер (MX/синк)
    participant WS as WS-вотчер (tokio)
    participant UI as UI-поток
    participant E as Движок
    participant S as Сервер (HTTP)

    MX->>WS: push new_message / flags_changed
    WS->>UI: EngineEvent → invoke_from_event_loop
    UI->>E: FetchConversations (delta)
    E->>S: GET /changes?since=journal_seq (tombstones)
    E->>S: GET /conversations?since=watermark
    S-->>E: изменённые диалоги + server_now_ms
    E->>E: upsert в SQLite, новые watermarks
    E-->>UI: Conversations (merged, все аккаунты)
    UI->>UI: сайдбар, unread-бейджи, тост о письме
```

### Календарь и напоминания (включая stale-id и удаления)

```mermaid
sequenceDiagram
    participant S as Сервер
    participant WS as WS-вотчер
    participant UI as UI-поток
    participant E as Движок
    participant C as SQLite (reminders2)
    participant T as Тост-окно

    S->>WS: push calendar_updated
    WS->>UI: EngineEvent (дебаунс ≤ 1 раз / 2 мин)
    UI->>E: FetchCalendarEvents(week)
    E->>S: GET /calendar-events?from&to (× аккаунты)
    E-->>UI: CalendarEvents{events, from, to, complete}
    UI->>C: seed(events) — посев каскадов алармов
    UI->>C: prune_orphan_reminders (только complete):<br/>событие исчезло → строки долой
    UI->>T: close_for_event(осиротевшие)
    Note over UI: apply_calendar_view →<br/>сетка + pending-скролл к рабочим часам

    loop scan-таймер
        UI->>C: reminders::scan(now)
        C-->>UI: due-строки
        UI->>T: показать «скоро»/«наступило»
    end

    T->>UI: клик по телу тоста
    UI->>UI: raise + view=календарь + pending_open_event
    UI->>E: FetchCalendarEvents(week события)
    E-->>UI: CalendarEvents
    alt id из тоста жив
        UI->>UI: карточка события по id
    else id протух (ре-синк сменил id)
        UI->>UI: fallback по (summary, occurrence) → карточка
    end
```
