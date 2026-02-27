# Deployment Guide for Linux Server

## Quick Deploy (Recommended)

### 1. On your local machine (Windows)

Build for Linux:
```bash
make build-linux
```

This creates `build/mailserver-linux-amd64`

### 2. Copy to server

```bash
# Using SCP
scp -r build/mailserver-linux-amd64 user@your-server:/tmp/
scp -r configs user@your-server:/tmp/mailserver-configs
scp -r migrations user@your-server:/tmp/mailserver-migrations
scp -r deployments user@your-server:/tmp/mailserver-deployments

# Or clone directly on server
ssh user@your-server
git clone https://github.com/ddletotam/ddmailserver.git
cd ddmailserver
```

### 3. On the server

```bash
cd ddmailserver  # or /tmp if you copied files
chmod +x deployments/install.sh
sudo ./deployments/install.sh
```

The install script will:
- Create system user `mailserver`
- Install binary to `/usr/local/bin/`
- Setup config in `/etc/mailserver/`
- Install systemd service
- Setup PostgreSQL database (optional)
- Configure firewall (optional)

---

## Manual Installation

### Prerequisites

```bash
# Update system
sudo apt update && sudo apt upgrade -y

# Install PostgreSQL
sudo apt install -y postgresql postgresql-contrib

# Install other tools
sudo apt install -y curl git make
```

### Step 1: Setup PostgreSQL

```bash
# Switch to postgres user
sudo -i -u postgres

# Create database and user
psql << EOF
CREATE USER mailserver WITH PASSWORD 'your-secure-password';
CREATE DATABASE mailserver OWNER mailserver;
\q
EOF

exit

# Run migrations
sudo -u postgres psql -d mailserver -f migrations/001_initial_schema.sql
sudo -u postgres psql -d mailserver -f migrations/002_outbox.sql
```

### Step 2: Create System User

```bash
sudo useradd --system --no-create-home --shell /bin/false mailserver
```

### Step 3: Install Binary

```bash
# If built locally, copy the binary
sudo cp build/mailserver-linux-amd64 /usr/local/bin/mailserver
sudo chmod +x /usr/local/bin/mailserver

# Or build on server (requires Go)
sudo apt install -y golang-go
make build
sudo cp build/mailserver /usr/local/bin/
```

### Step 4: Configure

```bash
# Create config directory
sudo mkdir -p /etc/mailserver

# Copy config
sudo cp configs/config.example.yaml /etc/mailserver/config.yaml

# Edit config
sudo nano /etc/mailserver/config.yaml
```

Update these settings:
```yaml
database:
  host: "localhost"
  port: 5432
  user: "mailserver"
  password: "your-secure-password"  # Change this!
  dbname: "mailserver"

security:
  jwt_secret: "change-this-to-random-string"  # Change this!

server:
  web_host: "0.0.0.0"
```

### Step 5: Install Systemd Service

```bash
# Copy service file
sudo cp deployments/mailserver.service /etc/systemd/system/

# Create data directory
sudo mkdir -p /var/lib/mailserver
sudo chown mailserver:mailserver /var/lib/mailserver

# Reload systemd
sudo systemctl daemon-reload

# Start service
sudo systemctl start mailserver

# Check status
sudo systemctl status mailserver

# Enable on boot
sudo systemctl enable mailserver
```

### Step 6: Configure Firewall

```bash
# Using UFW
sudo ufw allow 1143/tcp   # IMAP
sudo ufw allow 1587/tcp   # SMTP
sudo ufw allow 8080/tcp   # API (optional, for external access)
sudo ufw reload

# Using firewalld
sudo firewall-cmd --permanent --add-port=1143/tcp
sudo firewall-cmd --permanent --add-port=1587/tcp
sudo firewall-cmd --permanent --add-port=8080/tcp
sudo firewall-cmd --reload
```

---

## Nginx Reverse Proxy (Optional)

If you want to expose the API via HTTPS:

```nginx
# /etc/nginx/sites-available/mailserver
server {
    listen 80;
    server_name mail.yourdomain.com;

    location / {
        proxy_pass http://localhost:8080;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection 'upgrade';
        proxy_set_header Host $host;
        proxy_cache_bypass $http_upgrade;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

Enable and get SSL:
```bash
sudo ln -s /etc/nginx/sites-available/mailserver /etc/nginx/sites-enabled/
sudo nginx -t
sudo systemctl reload nginx

# Get SSL certificate with Let's Encrypt
sudo apt install -y certbot python3-certbot-nginx
sudo certbot --nginx -d mail.yourdomain.com
```

---

## Testing Deployment

### 1. Check Service Status

```bash
# Service status
sudo systemctl status mailserver

# View logs
sudo journalctl -u mailserver -f

# Check if listening
sudo netstat -tlnp | grep mailserver
# Should show ports 1143, 1587, 8080
```

### 2. Test API

```bash
# Health check
curl http://localhost:8080/health

# Register user
curl -X POST http://localhost:8080/api/register \
  -H "Content-Type: application/json" \
  -d '{"username":"test","password":"test123","email":"test@example.com"}'
```

### 3. Test IMAP/SMTP

Configure email client:
- **IMAP**: `your-server-ip:1143`
- **SMTP**: `your-server-ip:1587`
- **Username/Password**: from API registration

---

## Monitoring

### View Logs

```bash
# Real-time logs
sudo journalctl -u mailserver -f

# Last 100 lines
sudo journalctl -u mailserver -n 100

# Logs since today
sudo journalctl -u mailserver --since today

# Errors only
sudo journalctl -u mailserver -p err
```

### Resource Usage

```bash
# CPU and Memory
sudo systemctl status mailserver

# Detailed stats
ps aux | grep mailserver

# Database connections
sudo -u postgres psql -c "SELECT * FROM pg_stat_activity WHERE datname='mailserver';"
```

---

## Troubleshooting

### Service won't start

```bash
# Check logs
sudo journalctl -u mailserver -n 50

# Check config syntax
/usr/local/bin/mailserver -config /etc/mailserver/config.yaml

# Check permissions
ls -la /usr/local/bin/mailserver
ls -la /etc/mailserver/config.yaml
ls -la /var/lib/mailserver
```

### Database connection issues

```bash
# Test PostgreSQL connection
sudo -u postgres psql -d mailserver -c "SELECT version();"

# Check if PostgreSQL is running
sudo systemctl status postgresql

# Check PostgreSQL logs
sudo journalctl -u postgresql -n 50
```

### Port already in use

```bash
# Find what's using the port
sudo lsof -i :1143
sudo lsof -i :1587
sudo lsof -i :8080

# Kill the process if needed
sudo kill -9 <PID>
```

### Can't connect from external network

```bash
# Check firewall
sudo ufw status
sudo iptables -L -n

# Check if service is listening on all interfaces
sudo netstat -tlnp | grep mailserver
# Should show 0.0.0.0:port, not 127.0.0.1:port
```

---

## Updating

### Quick Update (Recommended)

We provide convenient scripts for updating:

```bash
cd /opt/ddmailserver

# Update and restart (interactive)
./deployments/update.sh

# View logs
./deployments/logs.sh

# Check status
./deployments/status.sh
```

The `update.sh` script will:
- Pull latest changes from git
- Check for new migrations (prompts you to run them)
- Rebuild the application
- Restart the service
- Show service status

### Manual Update

```bash
cd /opt/ddmailserver
git pull origin main

# Run new migrations if any
psql -h <HOST> -U ddmail -d ddmail -f migrations/003_recovery_key.sql

# Rebuild
make build

# Restart service
sudo systemctl restart mailserver

# Check logs
sudo journalctl -u mailserver -f
```

---

## Backup

### Database Backup

```bash
# Create backup
sudo -u postgres pg_dump mailserver > mailserver_backup_$(date +%Y%m%d).sql

# Restore backup
sudo -u postgres psql mailserver < mailserver_backup_20240101.sql
```

### Full Backup Script

```bash
#!/bin/bash
BACKUP_DIR="/var/backups/mailserver"
DATE=$(date +%Y%m%d_%H%M%S)

mkdir -p $BACKUP_DIR

# Backup database
sudo -u postgres pg_dump mailserver | gzip > $BACKUP_DIR/db_$DATE.sql.gz

# Backup config
sudo cp /etc/mailserver/config.yaml $BACKUP_DIR/config_$DATE.yaml

# Keep only last 7 days
find $BACKUP_DIR -type f -mtime +7 -delete

echo "Backup completed: $BACKUP_DIR"
```

Add to crontab:
```bash
sudo crontab -e

# Add this line for daily backup at 2 AM
0 2 * * * /path/to/backup-script.sh
```

---

## Uninstall

```bash
# Stop and disable service
sudo systemctl stop mailserver
sudo systemctl disable mailserver

# Remove service file
sudo rm /etc/systemd/system/mailserver.service
sudo systemctl daemon-reload

# Remove binary
sudo rm /usr/local/bin/mailserver

# Remove config (optional - backup first!)
sudo rm -rf /etc/mailserver

# Remove data (optional - backup first!)
sudo rm -rf /var/lib/mailserver

# Remove database (optional - backup first!)
sudo -u postgres psql -c "DROP DATABASE mailserver;"
sudo -u postgres psql -c "DROP USER mailserver;"

# Remove user
sudo userdel mailserver
```

---

## Production Recommendations

1. **Change default passwords** in config.yaml
2. **Enable TLS** for IMAP/SMTP (add certificates to config)
3. **Setup Nginx** reverse proxy with HTTPS for API
4. **Enable firewall** and allow only necessary ports
5. **Setup automated backups** (database + config)
6. **Monitor logs** regularly
7. **Keep system updated**: `sudo apt update && sudo apt upgrade`
8. **Use strong JWT secret** (generate with `openssl rand -hex 32`)

---

## Getting Help

- **GitHub Issues**: https://github.com/ddletotam/ddmailserver/issues
- **Logs**: Always check `sudo journalctl -u mailserver -f`
- **Database**: Check PostgreSQL logs for connection issues
