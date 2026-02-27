#!/bin/bash
# View logs for DDMailServer

SERVICE_NAME="mailserver"

echo "📋 Viewing logs for $SERVICE_NAME (Ctrl+C to exit)..."
echo ""

sudo journalctl -u $SERVICE_NAME -f
