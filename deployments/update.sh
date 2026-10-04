#!/bin/bash
# Update script for DDMailServer on remote server

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
SERVICE_NAME="mailserver"
SERVICE_USER="${SERVICE_USER:-mailserver}"
CONFIG_FILE="${CONFIG_FILE:-/etc/mailserver/config.yaml}"

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

# Build
echo ""
echo "🔨 Building application..."
make build

# Schema migrations are embedded in the binary and applied by the server on
# start (each in a transaction, under an advisory lock). Show what the new
# binary is going to do; this only reads the database.
echo ""
echo "🔍 Schema migrations the new binary will apply on start:"
if ! sudo -u "$SERVICE_USER" build/mailserver -config "$CONFIG_FILE" -migrate=plan; then
    echo "❌ The new binary cannot work with this database (see above). Nothing was changed."
    exit 1
fi

# Stop service before replacing binary
echo ""
echo "⏸️  Stopping $SERVICE_NAME service..."
sudo systemctl stop $SERVICE_NAME

# Install new binary
echo "📦 Installing new binary..."
sudo cp build/mailserver /usr/local/bin/mailserver
sudo chmod +x /usr/local/bin/mailserver
echo "   Binary updated: /usr/local/bin/mailserver"

# Start service
echo ""
echo "▶️  Starting $SERVICE_NAME service..."
sudo systemctl start $SERVICE_NAME

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
echo "  Migrations:   sudo journalctl -u $SERVICE_NAME | grep migrations:"
echo "  Check status: sudo systemctl status $SERVICE_NAME"
echo "  Stop:         sudo systemctl stop $SERVICE_NAME"
echo "  Start:        sudo systemctl start $SERVICE_NAME"
