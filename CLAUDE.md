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

## Security TODOs
- [ ] Encrypt IMAP/SMTP passwords in DB (requires passing encryption key to workers)

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
