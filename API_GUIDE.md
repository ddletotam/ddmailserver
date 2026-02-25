# REST API Guide

## Quick Start

### 1. Setup Configuration

Copy the example config:
```bash
cp configs/config.example.yaml configs/config.yaml
```

Edit `configs/config.yaml` and update:
- Database connection settings
- JWT secret (change from default!)
- Ports if needed

### 2. Setup Database

Create PostgreSQL database:
```sql
CREATE DATABASE mailserver;
```

Run migrations:
```bash
psql -U mailserver -d mailserver -f migrations/001_initial_schema.sql
psql -U mailserver -d mailserver -f migrations/002_outbox.sql
```

### 3. Start Server

```bash
./mailserver
```

The server will start:
- IMAP Server: `localhost:1143`
- SMTP Server: `localhost:1587`
- Web API: `http://localhost:8080`

## API Endpoints

### Public Endpoints

#### Register User
```bash
POST /api/register
Content-Type: application/json

{
  "username": "john",
  "password": "secret123",
  "email": "john@example.com"
}
```

Response:
```json
{
  "user": {
    "id": 1,
    "username": "john",
    "email": "john@example.com",
    "created_at": "2024-01-01T00:00:00Z"
  },
  "token": "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9..."
}
```

#### Login
```bash
POST /api/login
Content-Type: application/json

{
  "username": "john",
  "password": "secret123"
}
```

Response: Same as register

### Protected Endpoints

All protected endpoints require `Authorization: Bearer <token>` header.

#### List Accounts
```bash
GET /api/accounts
Authorization: Bearer <token>
```

Response:
```json
[
  {
    "id": 1,
    "user_id": 1,
    "name": "My Gmail",
    "email": "user@gmail.com",
    "imap_host": "imap.gmail.com",
    "imap_port": 993,
    "imap_username": "user@gmail.com",
    "imap_tls": true,
    "smtp_host": "smtp.gmail.com",
    "smtp_port": 587,
    "smtp_username": "user@gmail.com",
    "smtp_tls": true,
    "enabled": true,
    "last_sync": "2024-01-01T00:00:00Z",
    "created_at": "2024-01-01T00:00:00Z"
  }
]
```

#### Create Account
```bash
POST /api/accounts
Authorization: Bearer <token>
Content-Type: application/json

{
  "name": "My Gmail",
  "email": "user@gmail.com",
  "imap_host": "imap.gmail.com",
  "imap_port": 993,
  "imap_username": "user@gmail.com",
  "imap_password": "app-password-here",
  "imap_tls": true,
  "smtp_host": "smtp.gmail.com",
  "smtp_port": 587,
  "smtp_username": "user@gmail.com",
  "smtp_password": "app-password-here",
  "smtp_tls": true
}
```

**Note for Gmail**: You need to use an [App Password](https://support.google.com/accounts/answer/185833), not your regular password.

#### Get Account
```bash
GET /api/accounts/{id}
Authorization: Bearer <token>
```

#### Update Account
```bash
PUT /api/accounts/{id}
Authorization: Bearer <token>
Content-Type: application/json

{
  "name": "Updated Name",
  "enabled": true,
  ...
}
```

#### Delete Account
```bash
DELETE /api/accounts/{id}
Authorization: Bearer <token>
```

## Testing with curl

### Complete workflow example:

```bash
# 1. Register
curl -X POST http://localhost:8080/api/register \
  -H "Content-Type: application/json" \
  -d '{"username":"test","password":"test123","email":"test@example.com"}'

# Save the token from response
TOKEN="<token-from-response>"

# 2. Add Gmail account
curl -X POST http://localhost:8080/api/accounts \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "My Gmail",
    "email": "your-email@gmail.com",
    "imap_host": "imap.gmail.com",
    "imap_port": 993,
    "imap_username": "your-email@gmail.com",
    "imap_password": "your-app-password",
    "imap_tls": true,
    "smtp_host": "smtp.gmail.com",
    "smtp_port": 587,
    "smtp_username": "your-email@gmail.com",
    "smtp_password": "your-app-password",
    "smtp_tls": true
  }'

# 3. List accounts
curl -X GET http://localhost:8080/api/accounts \
  -H "Authorization: Bearer $TOKEN"
```

## Using with Email Client

After adding accounts via API:

### Configure Thunderbird/Apple Mail/etc:

**IMAP Settings:**
- Server: `localhost`
- Port: `1143`
- Username: your registered username
- Password: your registered password
- Security: None (for development)

**SMTP Settings:**
- Server: `localhost`
- Port: `1587`
- Username: your registered username
- Password: your registered password
- Security: None (for development)

The mailserver will:
1. Sync emails from your configured accounts every 60 seconds
2. Show all emails in a unified inbox
3. Send emails through the appropriate account based on the from address

## Common IMAP/SMTP Server Settings

### Gmail
- IMAP: `imap.gmail.com:993` (TLS)
- SMTP: `smtp.gmail.com:587` (STARTTLS)
- Note: Requires App Password

### Outlook/Hotmail
- IMAP: `outlook.office365.com:993` (TLS)
- SMTP: `smtp.office365.com:587` (STARTTLS)

### Yahoo
- IMAP: `imap.mail.yahoo.com:993` (TLS)
- SMTP: `smtp.mail.yahoo.com:587` (STARTTLS)

## Security Notes

⚠️ **IMPORTANT**: The current implementation has security issues:

1. **Passwords are stored in plain text** - Need to implement proper hashing
2. **No TLS on IMAP/SMTP servers** - Only for development
3. **Insecure auth allowed** - Only for development

Before production use:
- Implement bcrypt password hashing
- Add TLS certificates
- Enable secure authentication only
- Add rate limiting
- Implement proper input validation
