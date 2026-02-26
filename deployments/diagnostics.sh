#!/bin/bash

# DDMailServer Diagnostics Script
# Run this to collect info for troubleshooting

echo "╔══════════════════════════════════════════╗"
echo "║  DDMailServer Diagnostics Report        ║"
echo "╚══════════════════════════════════════════╝"
echo ""

echo "=== System Information ==="
echo "Hostname: $(hostname)"
echo "OS: $(cat /etc/os-release | grep PRETTY_NAME | cut -d '"' -f 2)"
echo "Kernel: $(uname -r)"
echo "Uptime: $(uptime -p)"
echo ""

echo "=== Service Status ==="
systemctl is-active mailserver && echo "Service: ACTIVE ✓" || echo "Service: INACTIVE ✗"
systemctl is-enabled mailserver && echo "Auto-start: ENABLED ✓" || echo "Auto-start: DISABLED ✗"
echo ""

echo "=== Listening Ports ==="
sudo netstat -tlnp | grep -E ':(1143|1587|8080)' || echo "No mailserver ports listening"
echo ""

echo "=== Process Information ==="
ps aux | grep -E '[m]ailserver' || echo "No mailserver process running"
echo ""

echo "=== Database Connection ==="
sudo -u postgres psql -d mailserver -c "SELECT version();" 2>/dev/null && echo "Database: CONNECTED ✓" || echo "Database: ERROR ✗"
sudo -u postgres psql -d mailserver -c "SELECT count(*) FROM users;" 2>/dev/null && echo "Users table: OK ✓" || echo "Users table: ERROR ✗"
echo ""

echo "=== Configuration ==="
[ -f /etc/mailserver/config.yaml ] && echo "Config file: EXISTS ✓" || echo "Config file: MISSING ✗"
[ -d /var/lib/mailserver ] && echo "Data dir: EXISTS ✓" || echo "Data dir: MISSING ✗"
[ -x /usr/local/bin/mailserver ] && echo "Binary: EXISTS ✓" || echo "Binary: MISSING ✗"
echo ""

echo "=== Recent Logs (last 20 lines) ==="
sudo journalctl -u mailserver -n 20 --no-pager
echo ""

echo "=== Disk Space ==="
df -h / | tail -1
echo ""

echo "=== Memory Usage ==="
free -h
echo ""

echo "=== Firewall Status ==="
if command -v ufw &> /dev/null; then
    sudo ufw status | grep -E '(1143|1587|8080)' || echo "No mailserver rules found"
elif command -v firewall-cmd &> /dev/null; then
    sudo firewall-cmd --list-ports | grep -E '(1143|1587|8080)' || echo "No mailserver rules found"
else
    echo "No firewall detected"
fi
echo ""

echo "=== API Health Check ==="
curl -s http://localhost:8080/health && echo "" && echo "API: RESPONDING ✓" || echo "API: NOT RESPONDING ✗"
echo ""

echo "=== End of Report ==="
echo ""
echo "To view full logs: sudo journalctl -u mailserver -f"
echo "To restart service: sudo systemctl restart mailserver"
