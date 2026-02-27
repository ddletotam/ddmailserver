#!/bin/bash
# Check status of DDMailServer

SERVICE_NAME="mailserver"

echo "=== DDMailServer Status ==="
echo ""

# Service status
echo "📊 Service status:"
sudo systemctl status $SERVICE_NAME --no-pager

echo ""
echo "🌐 Health check:"
curl -s http://localhost:8080/health | jq . || echo "Health check failed"

echo ""
echo "📝 Recent logs (last 20 lines):"
sudo journalctl -u $SERVICE_NAME -n 20 --no-pager

echo ""
echo "💾 Disk usage:"
df -h /opt/ddmailserver

echo ""
echo "🔧 Process info:"
ps aux | grep mailserver | grep -v grep || echo "Process not found"
