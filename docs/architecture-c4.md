# DDMail — Архитектура C4 (уровни 1 / 2 / 3)

Снимок на 2026-06-17. Диаграммы Mermaid (рендерятся в Typora, на GitHub, в любом Mermaid-вьювере).
DDMail — это self-hosted **агрегатор почты + календаря/контактов** с
Telegram-подобным десктоп-клиентом. Один Go-бинарь зеркалит несколько
вышестоящих IMAP/CalDAV/CardDAV-аккаунтов в Postgres и отдаёт их заново
по IMAP/SMTP, нативному desktop-API (WS + HTTP), CalDAV, CardDAV и LDAP.
Десктоп-клиент на Rust/Slint общается по нативному API (с откатом на обычный IMAP).

---

## Уровень 1 — Контекст системы

```mermaid
graph TD
    user["👤 Пользователь"]
    tpc["📧 Сторонние клиенты<br/>(iOS Mail, Thunderbird)"]
    sender["✉️ Внешние отправители (MTA)"]

    ddmail(["DDMail<br/>агрегатор почты и календаря"])

    up["Вышестоящие IMAP/SMTP<br/>(Gmail, Yandex, small.kz, appsec…)"]
    dav["Источники CalDAV / CardDAV<br/>(Yandex, Apple, Google)"]
    oauth["OAuth-провайдеры<br/>(Google, Microsoft)"]
    av["Источники аватаров<br/>(Gravatar / BIMI / favicon)"]

    user -->|"десктоп-клиент (нативный API)"| ddmail
    tpc -->|"IMAP 993 / CalDAV / CardDAV / LDAP"| ddmail
    sender -->|"SMTP :25 (MX)"| ddmail
    ddmail <-->|"синк почты (IMAP IDLE) / отправка (SMTP)"| up
    ddmail <-->|"синк событий/контактов"| dav
    ddmail -->|"обновление токенов"| oauth
    ddmail -->|"загрузка аватаров"| av
```

---

## Уровень 2 — Контейнеры

```mermaid
graph TD
    subgraph client["Десктоп-клиент (Rust)"]
        native["ddmail-native<br/>UI на Slint + рендер тела emlrender"]
        corec["ddmail-core<br/>движок, провайдеры, кэш"]
        lcache[("локальный кэш<br/>SQLite")]
        native --> corec
        corec --> lcache
    end

    subgraph server["Мейлсервер (один Go-процесс)"]
        webapi["Web / нативный API<br/>:8080 — desktop WS+HTTP, CalDAV, CardDAV, OAuth, веб-UI"]
        imaps["IMAP-сервер<br/>:143 / :993 (IDLE)"]
        smtps["SMTP submission<br/>:587 / :465"]
        mx["MX (входящая)<br/>:25"]
        ldap["LDAP<br/>:10389"]
        worker["Воркер / Шедулер<br/>+ IDLE-менеджер"]
        imapc["IMAP-клиент<br/>(синк апстрима)"]
        smtpc["SMTP-клиент<br/>(отправка)"]
        hub["NotifyHub<br/>(pub/sub)"]
    end

    pg[("PostgreSQL")]
    meili[("Meilisearch")]

    up["Вышестоящие IMAP/SMTP"]
    dav["Источники CalDAV/CardDAV"]
    tpc["iOS Mail / Thunderbird"]

    corec -->|"нативный API (WS+HTTP) / либо обычный IMAP"| webapi
    corec -.->|"режим отката"| imaps
    tpc --> imaps
    tpc --> webapi

    webapi --> pg
    imaps --> pg
    smtps --> smtpc
    mx --> pg
    worker --> pg
    worker --> imapc
    worker --> smtpc
    imapc <--> up
    worker <--> dav
    smtpc --> up
    webapi --> meili
    worker --> meili

    worker --> hub
    mx --> hub
    imaps --> hub
    hub --> webapi
    hub --> imaps
```

Примечания:
- «Мейлсервер» — это **один процесс**; боксы — это параллельные слушатели/сервисы, разделяющие БД + NotifyHub, а не отдельные деплои.
- NotifyHub разводит push-события на **два потребителя**: desktop-WebSocket (`webapi`) и IMAP IDLE (`imaps`). См. «Несущие решения», п. 5.

---

## Уровень 3 — Компоненты

### L3a — Мейлсервер: контейнер Web / нативный API

```mermaid
graph TD
    router["HTTP-роутер (mux)"]
    desk["Хендлеры desktop-API<br/>/api/desktop/v1/* (auth, conversations, messages, flags, delete, calendar, search, ws)"]
    caldav["CalDAV-сервер"]
    carddav["CardDAV-сервер"]
    oauthh["OAuth-хендлеры"]
    webui["Хендлеры веб-UI"]

    msgsvc["service/messages<br/>флаги, удаление, purge + flag_sync_queue (одна транзакция)"]
    spamsvc["service/spam<br/>объяснение вердикта"]
    imapsrv["IMAP-сервер (STORE)"]
    dbl["слой БД<br/>(messages, folders, accounts, calendar, contacts)"]
    parser["MIME-парсер + санитайзер"]
    cal["календарь / обработчик входящих iTIP"]
    spam["спам-анализатор (parser.Analyzer) —<br/>единственный движок"]
    idx["поисковый индексатор (Meili)"]
    hub["NotifyHub"]

    router --> desk & caldav & carddav & oauthh & webui
    desk --> msgsvc
    webui --> msgsvc
    imapsrv --> msgsvc
    webui --> spamsvc
    msgsvc --> dbl
    spamsvc --> spam
    spamsvc --> dbl
    desk --> dbl
    desk --> idx
    desk --> hub
    caldav --> dbl
    carddav --> dbl
    desk --> parser
    dbl --> pg[("Postgres")]
    cal --> dbl
```

Изменения писем (флаги, удаление, purge) идут только через `service/messages`:
он решает, когда изменение уходит на исходный сервер внешней учётки, и делает
это в одной транзакции с самим изменением. SQL живёт только в `internal/db`.
Окно «почему это спам» — `service/spam`: сохранённый при доставке вердикт +
повторный прогон того же `parser.Analyzer`, второго движка нет.

### L3b — Мейлсервер: воркер + синк

```mermaid
graph TD
    sched["Шедулер (тикер по интервалу)"]
    idlem["IDLE-менеджер"]
    t1["Задача IMAP-синка<br/>(апстрим → PG, дедуп по Message-ID, поглощение iTIP + hard-delete)"]
    t2["Задача синка флагов<br/>(локальный флаг/удаление → апстрим)"]
    t3["Синк календаря / обратный синк событий"]
    t4["Синк контактов / push"]
    t5["Очистка спама / чистка vault / чистка логов"]
    imapc["IMAP-клиент"]
    hub["NotifyHub"]

    sched --> t1 & t2 & t3 & t4 & t5
    idlem -->|"новая почта → триггер"| t1
    t1 --> imapc
    t1 --> hub
    t1 --> pg[("Postgres")]
    t2 --> imapc
    t3 --> pg
    t4 --> pg
```

### L3c — Десктоп-клиент

```mermaid
graph TD
    ui["UI на Slint (native/main.rs)<br/>диалоги, сетка календаря, композер, просмотр исходника, трей, индикатор"]
    rworker["рендер-воркер<br/>emlrender → bitmap + text-runs/link-rects"]
    engine["движок (оркестратор)<br/>Vec&lt;AccountConn&gt;, командный цикл"]
    np["NativeProvider<br/>HTTP + WS, обновление токена"]
    ip["ImapProvider<br/>IMAP IDLE"]
    cache[("кэш SQLite<br/>conversations, bodies, contacts, identities (account_key)")]

    ui -->|"EngineCmd"| engine
    engine -->|"EngineResult / события"| ui
    ui -->|"Job::Render*"| rworker
    rworker --> ui
    engine --> np
    engine --> ip
    engine --> cache
    np <-->|"/api/desktop/v1 + ws"| webapi["Web/нативный API мейлсервера"]
    ip <--> imaps["IMAP мейлсервера"]
```

---

## Несущие решения и их статус

Тревога была обоснована: всё трение сводилось к идентичности письма. Приняты
твёрдые архитектурные решения и заложен фундамент.

### Решено и реализовано

1. **Идентичность письма = `(user_id, Message-ID)`.** ✅ *Реализовано.*
   Глобальный естественный ключ. Сервер **не переидентифицирует** зеркалируемую
   почту и **не выдумывает** Message-ID (никаких UID/времени в id). Для письма
   апстрима без заголовка id **выводится из содержимого** (см. п.2). Гарантируется на уровне БД
   частичным уникальным индексом `messages_user_message_id_uq` (только для
   непустых message_id — локальные черновики без заголовка не конфликтуют).
   `CreateMessage` → `INSERT … ON CONFLICT (user_id, message_id) DO NOTHING`
   (`ErrDuplicateMessage` = доброкачественный пропуск).
   *Миграции 041 (дедуп + UNIQUE) → 042 (частичный индекс).*

2. **Ингресс без Message-ID.** ✅ *Реализовано.*
   - MX / submission → явная ошибка `5xx`: отправителю, до которого мы можем
     достучаться, сообщаем, что он неправ, а не поощряем такое поведение.
   - IMAP-синк апстрима → **детерминированный id из содержимого**
     (`parser.DeriveMessageID`): `<noid.<sha256[:32]>@ddmail.invalid>` от
     `Date`/`From`/`To`/`Cc`/`Subject` + тела; без `Date` — от всего письма.
     Транспортные заголовки не участвуют → одна идентичность у копий из разных
     аккаунтов, стабильна между синками, MOVE и сменой UIDVALIDITY.
     Апстрим 5xx-нуть нельзя, а пропуск терял реальную почту (билеты, iTIP
     из ELMA и т.п. — ~160 писем/сутки на 2026-09-25).
   - Синтетический id не утекает наружу: `buildRawEmail` вычищает его из
     `In-Reply-To`/`References` (`parser.StripSyntheticMessageIDs`).
   Прежние генераторы (UID+время, `UnixNano`) плодили чурн именно из-за
   нестабильности; вывод из содержимого этой проблемы не имеет.

3. **desktop-контракт опирается на стабильный id.** ✅ *Реализовано.*
   `DesktopMessageRef` несёт `message_id`; тело/флаги/удаление/спам резолвятся
   через `resolveMsgRef` по `(user_id, Message-ID)` с фолбэком на волатильный
   `uid` (= `messages.id`) для старых клиентов. Клиент (Rust) прокидывает
   `message_id` во все refs и кэширует по нему. Это и был корень «пустых тел /
   зависших диалогов» (висячий `messages.id` после delete+reinsert).

### Решено и реализовано (продолжение)

4. **Журнал изменений (Kafka-style) вместо полного ресинка.** ✅ *Реализовано.*
   Таблица `message_changes(seq BIGSERIAL, user_id, message_id, kind, ts)`,
   наполняется **триггером БД** на `messages` (INSERT/UPDATE/DELETE) — единый
   источник истины, ни один путь приложения не может забыть записать событие.
   UPDATE-триггер сужен до видимых клиенту колонок (флаги / папка / видимость),
   чтобы не шуметь на бэкфилле тел. `kind=2` (delete) когда строка уходит из
   видимости (deleted/soft_deleted/is_spam). Клиент хранит глобальный курсор
   `seq` и дочитывает хвост `GET /changes?since=seq` → `{entries, latest_seq,
   low_watermark, reset}`; применяет DELETE-tombstone'ы к кэшу до дельты.
   `reset` (новый клиент / курсор отстал за retention) → полный ресинк +
   принять `latest_seq`. Компакция — раз в сутки, retention 30 дней (как vault).
   *Миграция 043; задеплоено и проверено на проде (триггер пишет, эндпоинт
   отдаёт reset/хвост).* **Это и есть замена «ресинка раз в 24ч» — теперь у
   дельты есть явная семантика удалений.**

### Закрыто журналом

5. **Веер уведомлений стал надёжным.** ✅ Удаления публикуются в NotifyHub, а
   главное — журнал (п.4) делает доставку событий **сверяемой**: клиент не
   доверяет одному пушу, а дочитывает хвост по `seq` при любом триггере
   (коннект / WS-событие / периодика), так что пропущенный пуш ловится на
   следующей сверке. WS-пуш теперь лишь «звонок будильника», истина — в журнале.

### Сознательно принятые рамки (не «скрип», а решения)

- **Зеркалирование в Postgres — оставляем.** Это и есть модель агрегатора;
  чурн id убирается стабильным ключом (п.1), а не отказом от зеркала.
- **Нативный протокол — оставляем.** IMAP «мал» для клиента (календарь,
  диалоги). Дуальный режим (native ⇆ обычный IMAP) — не проблема: каждый
  сервер-источник либо ddmailserver-native, либо IMAP IDLE, третьего нет.

**Вывод:** топология для агрегатора верная; фундамент идентичности **и журнал
изменений** заложены и задеплоены. Все пять точек трения из первоначальной
оценки закрыты решениями (п.1–4 реализованы, п.5 закрыт журналом). Дальше —
фичи на стабильном контракте, а не починка фундамента.
