# Account configuration (ddmail-native)

The native client has no login screen yet. It resolves one account from, in
order:

1. **Environment variables** (handy for dev):
   - `DDMAIL_IMAP_HOST`, `DDMAIL_IMAP_PORT` (default 993), `DDMAIL_IMAP_USER`,
     `DDMAIL_IMAP_PASS`, `DDMAIL_IMAP_TLS` (1/0, default 1), `DDMAIL_EMAIL`
     (default = user)
   - SMTP: `DDMAIL_SMTP_HOST` (default = IMAP host), `DDMAIL_SMTP_PORT`
     (default 465)
   - Optional native mode: `DDMAIL_NATIVE_URL`, `DDMAIL_NATIVE_TOKEN`

2. **Config file** `%APPDATA%/ru.letotam.ddmail/account.json` (Windows) or
   `$HOME/ru.letotam.ddmail/account.json`:

   ```json
   {
     "host": "mail.letotam.ru",
     "port": 993,
     "username": "lucky",
     "password": "…",
     "use_tls": true,
     "email": "lucky@mail.letotam.ru",
     "smtp_host": "mail.letotam.ru",
     "smtp_port": 465
   }
   ```

   Native mode (server token instead of IMAP): add `"native_url"` and
   `"native_token"`.

> Plaintext for now — same trust level as the env vars. A real login screen
> with OS keyring storage is planned.

If neither source is present the client runs cache-only (no live fetch).
