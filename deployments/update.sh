#!/bin/bash
# Update script for DDMailServer on remote server

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
SERVICE_NAME="mailserver"

echo "=== DDMailServer Update Script ==="
echo ""

# Check if running as correct user
if [ "$EUID" -eq 0 ]; then
    echo "❌ Do not run this script as root. Run as the mailserver user."
    exit 1
fi

cd "$PROJECT_DIR"

# Pull latest changes
echo "📥 Pulling latest changes from git..."
git pull origin main

# Check for new migrations
echo ""
echo "🔍 Checking for new migrations..."
MIGRATION_FILES=$(ls -1 migrations/*.sql 2>/dev/null | wc -l)
if [ "$MIGRATION_FILES" -gt 0 ]; then
    echo "Found $MIGRATION_FILES migration file(s)"
    echo "⚠️  Please run migrations manually if needed:"
    echo "    psql -h <HOST> -U ddmail -d ddmail -f migrations/003_recovery_key.sql"
    echo ""
    read -p "Have you run all necessary migrations? (y/n) " -n 1 -r
    echo ""
    if [[ ! $REPLY =~ ^[Yy]$ ]]; then
        echo "❌ Aborted. Please run migrations first."
        exit 1
    fi
fi

# Build
echo ""
echo "🔨 Building application..."
make build

# Restart service
echo ""
echo "🔄 Restarting $SERVICE_NAME service..."
sudo systemctl restart $SERVICE_NAME

# Wait a bit for service to start
sleep 2

# Check status
echo ""
echo "📊 Service status:"
sudo systemctl status $SERVICE_NAME --no-pager -l

echo ""
echo "✅ Update complete!"
echo ""
echo "Useful commands:"
echo "  View logs:    sudo journalctl -u $SERVICE_NAME -f"
echo "  Check status: sudo systemctl status $SERVICE_NAME"
echo "  Stop:         sudo systemctl stop $SERVICE_NAME"
echo "  Start:        sudo systemctl start $SERVICE_NAME"
