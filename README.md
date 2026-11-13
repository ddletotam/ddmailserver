# DDMail — self-hosted агрегатор почты, календаря и контактов

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.24-blue.svg)](https://go.dev/)
[![PostgreSQL](https://img.shields.io/badge/PostgreSQL-14+-green.svg)](https://www.postgresql.org/)

Один Go-бинарь зеркалит несколько внешних почтовых ящиков и календарей
в PostgreSQL и отдаёт их заново через один вход: IMAP/SMTP, CalDAV, CardDAV
и нативный API для собственных клиентов (десктоп, Android, iOS/macOS).
Может быть и полноценным почтовым сервером для своих доменов: принимает
почту по MX и подписывает исходящую DKIM.

Рабочий экземпляр: https://mail.letotam.ru/

## Что умеет

**Сервер** (`cmd/mailserver`)
- Внешние учётки IMAP/SMTP (пароль или OAuth Google/Microsoft): синк с IDLE,
  дедупликация по Message-ID, обратная синхронизация флагов и удалений.
- Отправка через исходный SMTP учётки — клиент настраивается один раз.
- Свои домены: приём на порт 25 (MX), DKIM-подпись, ящики на домене.
- Календарь и контакты: CalDAV/CardDAV-сервер и синк внешних источников
  (Yandex, Apple, Google), обработка приглашений iTIP.
- Спам: встроенный анализатор, скрытая папка «Спам», «Не спам и доверять
  отправителю», объяснение вердикта.
- Полнотекстовый поиск через Meilisearch.
- Push о новой почте: регистрация токенов устройств, доставка через APNs
  (в уведомлении только «Новые письма» — без отправителя и темы).
- `.mobileconfig`-профили для iOS/macOS (обход сломанного автообнаружения CalDAV).
- Веб-интерфейс: регистрация, учётки, домены, календарь, контакты, спам; RU/EN.
- Защита входа: общий лимит неудачных попыток для web, API, IMAP, SMTP, DAV.

**Клиенты**
| Клиент | Каталог | Стек | Контракт поведения |
|---|---|---|---|
| Десктоп (Windows/Linux) | `desktop/` | Rust + Slint, кэш SQLite | [`docs/desktop-behavior-contract.md`](docs/desktop-behavior-contract.md) |
| Android | `mobile/android` | Kotlin (сборки HMS/GMS) | [`docs/android-behavior-contract.md`](docs/android-behavior-contract.md) |
| iOS / macOS | `mobile/ios` | Swift, XcodeGen | [`docs/ios-behavior-contract.md`](docs/ios-behavior-contract.md) |

Клиенты работают через нативный API (`/api/desktop/v1`, HTTP + WebSocket),
умеют подключать и внешние учётки напрямую (IMAP/SMTP, CalDAV/CardDAV).
Сторонние клиенты (Thunderbird, iOS Mail) подключаются по IMAP/SMTP/CalDAV/CardDAV.

## Архитектура

```
 Десктоп / Android / iOS        Thunderbird, iOS Mail       Внешние MTA
          │ нативный API (WS+HTTP)     │ IMAP/SMTP/DAV          │ SMTP :25
          └──────────────┬─────────────┴───────────────────────┘
                         ▼
          ┌──────────────────────────────────────┐
          │ mailserver (один процесс)            │
          │ web/API · IMAP · SMTP · MX · DAV     │──► Meilisearch
          │ воркеры синка · NotifyHub · push     │──► APNs
          └──────────────────┬───────────────────┘
                             ▼
                        PostgreSQL
                             ▲
          синк ◄─────────────┘
 Внешние IMAP/SMTP, CalDAV/CardDAV (Gmail, Yandex, Mail.ru, …)
```

Подробно (C4, уровни 1–3): [`docs/architecture-c4.md`](docs/architecture-c4.md).

## Структура репозитория

| Путь | Что там |
|---|---|
| `cmd/mailserver` | сервер |
| `cmd/msgid-audit` | утилита: сколько писем теряется на совпадении Message-ID |
| `internal/` | пакеты сервера: `imap`, `smtp`, `caldav`, `carddav`, `web`, `worker`, `db`, `push`, `search`, … |
| `migrations/` | SQL-миграции, встроены в бинарь |
| `configs/config.example.yaml` | пример конфигурации со всеми секциями |
| `deployments/` | systemd-юнит, скрипты установки и обновления |
| `desktop/`, `mobile/` | клиенты |
| `docs/` | архитектура, контракты клиентов, бэклог |

## Сборка и запуск

Требования: Go 1.24+, PostgreSQL 14+; Meilisearch — для поиска.

```bash
cp configs/config.example.yaml configs/config.yaml   # поправить БД, секреты, порты
go build ./cmd/mailserver
./mailserver -config configs/config.yaml
```

Схема БД создаётся и обновляется при старте (`-migrate=auto`).
`-migrate=plan` — показать, что будет применено; `-migrate=off` — не трогать схему.

Docker Compose (PostgreSQL + сервер): `docker-compose up -d`.

Порты по умолчанию: IMAP 1143 / 1993 (TLS), SMTP 1587 / 1465 (TLS),
MX 25, веб и API 8080. В проде стандартные порты перенаправляются на них,
веб — за nginx.

## Тесты

```bash
go test ./...
npm test
```

Клиенты: `./gradlew assembleHmsDebug` в `mobile/android`,
`scripts/mac.sh test` в `mobile/ios`, `cargo test` в `desktop/native`.

## Лицензия

MIT — см. [LICENSE](LICENSE).
