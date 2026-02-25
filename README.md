# MailServer - Self-Hosted Email Aggregator

A self-hosted email aggregation service that allows you to manage multiple email accounts through a single IMAP/SMTP server.

## Features

- **Email Aggregation**: Add multiple external email accounts (Gmail, Outlook, etc.) and access them all through a single IMAP connection
- **Unified Inbox**: All your emails from different accounts in one place
- **Transparent Sending**: Send emails through your original SMTP servers while using a single client configuration
- **Self-Hosted**: Full control over your data and infrastructure
- **Web UI**: Easy configuration and management through a web interface
- **Flexible Anti-Spam**: Custom spam filtering rules (coming soon)

## Architecture

```
Email Client (Thunderbird, etc.)
         ↓
    IMAP/SMTP (Your MailServer)
         ↓
    PostgreSQL Database
         ↓
External IMAP/SMTP Servers (Gmail, Outlook, etc.)
```

## Requirements

- Go 1.22+
- PostgreSQL 14+
- Docker & Docker Compose (optional)

## Installation

### Using Docker Compose (Recommended)

```bash
docker-compose up -d
```

### Manual Installation

1. Install PostgreSQL
2. Create database:
   ```sql
   CREATE DATABASE mailserver;
   ```

3. Build and run:
   ```bash
   go build -o mailserver ./cmd/mailserver
   ./mailserver
   ```

## Configuration

Edit `configs/config.yaml`:

```yaml
server:
  imap_port: 1143
  smtp_port: 1587
  web_port: 8080

database:
  host: localhost
  port: 5432
  user: mailserver
  password: changeme
  dbname: mailserver
```

## Usage

1. Open web interface at `http://localhost:8080`
2. Add your external email accounts
3. Configure your email client with:
   - IMAP: `localhost:1143`
   - SMTP: `localhost:1587`
   - Username: your web UI username
   - Password: your web UI password

## Development Status

This project is in active development. Current focus:
- [x] Project structure
- [ ] Database schema
- [ ] IMAP server implementation
- [ ] SMTP server implementation
- [ ] IMAP/SMTP clients for external servers
- [ ] Web UI and REST API
- [ ] Anti-spam engine (future)

## License

MIT

## Contributing

Contributions are welcome! Please open an issue or submit a pull request.
