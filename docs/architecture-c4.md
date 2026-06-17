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
        native["ddmail-native<br/>UI на Slint + рендер тела WebKitGTK"]
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
- NotifyHub разводит push-события на **два потребителя**: desktop-WebSocket (`webapi`) и IMAP IDLE (`imaps`). ⚠️ см. оценку, п. 2.

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

    dbl["слой БД<br/>(messages, folders, accounts, calendar, contacts)"]
    parser["MIME-парсер + санитайзер"]
    cal["календарь / обработчик входящих iTIP"]
    spam["спам-анализатор"]
    idx["поисковый индексатор (Meili)"]
    hub["NotifyHub"]

    router --> desk & caldav & carddav & oauthh & webui
    desk --> dbl
    desk --> idx
    desk --> hub
    caldav --> dbl
    carddav --> dbl
    desk --> parser
    dbl --> pg[("Postgres")]
    cal --> dbl
```

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
    rworker["рендер-воркер<br/>WebKitGTK → bitmap + text-runs/link-rects"]
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

## Оценка архитектуры — где «скрипит»

Тревога обоснована; трение сосредоточено в нескольких несущих решениях
(все баги этой сессии сводились к одному из них):

1. **Волатильный db-`id` как идентичность письма для клиента.** desktop-API
   отдаёт `messages.id` как `uid` письма и тянет тело через `GetMessageByID`.
   Но этот id **нестабилен**: зеркало-синк апстрима, поглощение iTIP (hard-delete)
   и потоки vault/spam удаляют+пере-вставляют строки, так что только что
   отданный id может исчезнуть → висячие ссылки, пустые тела (баг Mariya/Sergey).
   **Направление фикса:** контракт клиент↔сервер должен опираться на
   **стабильный id** — RFC `Message-ID` или `(account_id, remote_folder, remote_uid)`.

2. **Два параллельных пути пуша.** Уведомления расходятся к IMAP-IDLE-клиентам
   (канал go-imap, `notifyExpunge`) *и* к десктопу (NotifyHub → WS) — но проведены
   независимо, поэтому события реализуют для одного и забывают для другого
   (удаления доходили до IMAP, но не до десктопа — чинили сегодня).
   **Направление фикса:** один источник истины событий (NotifyHub), который
   потребляют *оба* — и IMAP-сервер, и WS; IMAP-путь должен публиковать в hub,
   а не обходить его.

3. **У дельты нет семантики удалений.** Дельта диалогов сообщает *изменённые*
   треды, никогда *удалённые*; сверка полагается на полный ресинк раз в 24ч
   (или новый, триггеримый по expunge). **Направление фикса:** tombstone'ы в
   дельте, либо трактовать любое структурное изменение как «ресинк».

4. **Корень — «зеркалить всё в Postgres и пере-id'ить».** Сервер пере-сохраняет
   почту апстрима как новые строки PG со свежими id/uid. Именно это порождает
   чурн id (п.1), вопросы дубль-идентичности и дуальный контракт desktop-vs-IMAP.
   Это самое спорное место: правильна ли модель полного пере-store, или агрегатор
   должен держать апстрим-стабильные идентификаторы сквозняком?

5. **Дуальный режим клиента (native vs обычный IMAP).** Клиент может быть
   клиентом нативного API ЛИБО обычным IMAP-клиентом. Включение native-режима
   (ради календаря) обнажило дыры native-пути, которые IMAP-режим прятал.
   Работа по мультиаккаунту (P1) здорова, но стоит на шатком контракте п.1.

**Вывод:** топология для агрегатора разумная; риск — в **контракте идентичности
письма** (п.1/п.4) и **раздробленном веере уведомлений** (п.2). Оба чинятся без
ре-архитектуры — это вопросы контракта/проводки, а не структуры. Я бы сделал
стабильный message-id раньше, чем наращивать фичи на нативном API.
