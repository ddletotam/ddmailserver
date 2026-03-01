# Mail Project

## Commands
- `npm test` - run JS tests
- `npm run build` - build frontend
- `go test ./...` - run Go tests
- `go build ./cmd/mailserver` - build server

## Verification
- Run tests after code changes (`go test ./...` и `npm test`)
- Check build passes (`go build ./cmd/mailserver`)
- Run `gofmt -w .` before committing

## Conventions
- Go backend, JS/HTMX/Alpine.js frontend
- Database: SQLite, migrations in /migrations
- Always check `err != nil` for error handling
- No global variables
- Use conventional commits (feat:, fix:, refactor:, etc.)

## Project Structure
- `/cmd/mailserver` - main entry point
- `/internal/web` - web handlers, templates, static files
- `/internal/db` - database layer (users, accounts, messages, folders)
- `/internal/imap/client` - IMAP client for syncing
- `/internal/imap/server` - IMAP server
- `/internal/smtp/client` - SMTP client for sending
- `/internal/smtp/server` - SMTP server
- `/internal/worker` - background task scheduler
- `/migrations` - SQL migrations

## i18n
- Locales in `/internal/web/locales/` (en.json, ru.json)
- Use i18n functions for all user-facing text

## Deployment
- SSH: `lucky@jwebhelp.ru`
- Project: `/opt/ddmailserver`
- Database: `PGPASSWORD=mailserver psql -h 10.0.0.2 -U mailserver -d mailserver`
- Deploy commands:
```bash
cd /opt/ddmailserver
sudo git pull
/usr/local/go/bin/go build -o build/mailserver ./cmd/mailserver
sudo systemctl stop mailserver
sudo cp build/mailserver /usr/local/bin/mailserver
sudo systemctl start mailserver
sudo journalctl -u mailserver -f  # view logs
```

## Server Info
- Production URL: https://ddm.logdoc.ru/ (proxied via nginx)
- IMAP: mail.letotam.ru:993 (TLS) - redirected 993→10993
- SMTP: mail.letotam.ru:465 (TLS) - redirected 465→10465
- MX: port 25 (redirected 25→2525)
- TLS certs: /etc/mailserver/certs/ (Let's Encrypt for mail.letotam.ru)
