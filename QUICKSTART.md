# Quick Start Guide

## Минимальный запуск для тестирования

### 1. Установить PostgreSQL

Скачать и установить PostgreSQL: https://www.postgresql.org/download/

### 2. Создать базу данных

```bash
# Подключиться к PostgreSQL
psql -U postgres

# Создать пользователя и базу
CREATE USER mailserver WITH PASSWORD 'changeme';
CREATE DATABASE mailserver OWNER mailserver;
\q
```

### 3. Применить миграции

```bash
cd c:\work\mail
psql -U mailserver -d mailserver -f migrations/001_initial_schema.sql
psql -U mailserver -d mailserver -f migrations/002_outbox.sql
```

### 4. Создать конфигурацию

```bash
cp configs/config.example.yaml configs/config.yaml
```

Отредактировать `configs/config.yaml` если нужно изменить порты или настройки БД.

### 5. Запустить сервер

```bash
./mailserver.exe -config configs/config.yaml
```

Вы должны увидеть:
```
╔══════════════════════════════════════════╗
║     MailServer - Email Aggregator        ║
║     Self-hosted IMAP/SMTP Proxy          ║
╚══════════════════════════════════════════╝

[timestamp] Loading configuration from configs/config.yaml
[timestamp] Configuration loaded successfully
[timestamp] Connecting to database at localhost:5432
[timestamp] Database connection established
[timestamp] Worker pool initialized: 8 CPUs, 50% limit = 4 total workers (2 IMAP, 2 SMTP)
[timestamp] Worker pool started
[timestamp] Scheduler started with interval 1m0s
[timestamp] IMAP server created, will listen on 0.0.0.0:1143
[timestamp] Starting IMAP server on 0.0.0.0:1143
[timestamp] SMTP server created, will listen on 0.0.0.0:1587
[timestamp] Starting SMTP server on 0.0.0.0:1587
[timestamp] Web server created, will listen on 0.0.0.0:8080
[timestamp] Routes configured
[timestamp] Starting web server on 0.0.0.0:8080
[timestamp] Web interface available at http://0.0.0.0:8080
[timestamp] ✓ MailServer started successfully
[timestamp] Press Ctrl+C to stop
```

### 6. Зарегистрировать пользователя

```bash
curl -X POST http://localhost:8080/api/register \
  -H "Content-Type: application/json" \
  -d "{\"username\":\"admin\",\"password\":\"admin123\",\"email\":\"admin@localhost\"}"
```

Сохраните токен из ответа.

### 7. Добавить email аккаунт

```bash
# Замените YOUR_TOKEN на токен из предыдущего шага
curl -X POST http://localhost:8080/api/accounts \
  -H "Authorization: Bearer YOUR_TOKEN" \
  -H "Content-Type: application/json" \
  -d "{
    \"name\": \"My Gmail\",
    \"email\": \"your-email@gmail.com\",
    \"imap_host\": \"imap.gmail.com\",
    \"imap_port\": 993,
    \"imap_username\": \"your-email@gmail.com\",
    \"imap_password\": \"your-app-password\",
    \"imap_tls\": true,
    \"smtp_host\": \"smtp.gmail.com\",
    \"smtp_port\": 587,
    \"smtp_username\": \"your-email@gmail.com\",
    \"smtp_password\": \"your-app-password\",
    \"smtp_tls\": true
  }"
```

**Для Gmail**: Используйте [App Password](https://support.google.com/accounts/answer/185833), не обычный пароль.

### 8. Настроить email клиент

Откройте Thunderbird, Apple Mail или другой email клиент:

**IMAP:**
- Сервер: `localhost`
- Порт: `1143`
- Имя пользователя: `admin` (из шага 6)
- Пароль: `admin123` (из шага 6)
- Безопасность: Нет

**SMTP:**
- Сервер: `localhost`
- Порт: `1587`
- Имя пользователя: `admin`
- Пароль: `admin123`
- Безопасность: Нет

### 9. Проверить работу

1. **Входящая почта**: Подождите ~60 секунд (интервал синхронизации), проверьте inbox в клиенте
2. **Исходящая почта**: Отправьте письмо через клиент, оно должно уйти через Gmail

## Мониторинг

Логи показывают:
- Подключения к IMAP/SMTP серверам
- Синхронизацию писем
- Отправку писем
- Ошибки

Пример:
```
[timestamp] Scheduling sync and send tasks
[timestamp] Found 1 enabled accounts to sync
[timestamp] Submitted sync task for your-email@gmail.com
[timestamp] IMAP worker 0 executing: IMAP sync for your-email@gmail.com (account 1)
[timestamp] Starting sync for account your-email@gmail.com
[timestamp] Connecting to IMAP server imap.gmail.com:993 with TLS
[timestamp] Successfully connected to your-email@gmail.com
[timestamp] Found 3 folders for your-email@gmail.com
[timestamp] Syncing folder INBOX for your-email@gmail.com
[timestamp] Synced 10 messages from folder INBOX
[timestamp] Completed sync for account your-email@gmail.com
```

## Проблемы

### "failed to connect to IMAP server"
- Проверьте настройки хоста и порта
- Для Gmail убедитесь, что используете App Password
- Проверьте, что IMAP включён в настройках Gmail

### "invalid credentials"
- Для Gmail используйте App Password, не обычный пароль
- Проверьте правильность username/password

### "Database connection failed"
- Проверьте, что PostgreSQL запущен
- Проверьте настройки в `configs/config.yaml`

## Следующие шаги

См. [API_GUIDE.md](API_GUIDE.md) для полного описания REST API.

См. [README.md](README.md) для архитектуры и деталей реализации.
