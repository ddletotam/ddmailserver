#!/bin/bash

set -e

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Configuration
BINARY_NAME="mailserver"
INSTALL_DIR="/usr/local/bin"
CONFIG_DIR="/etc/mailserver"
DATA_DIR="/var/lib/mailserver"
SERVICE_USER="mailserver"
SERVICE_FILE="/etc/systemd/system/mailserver.service"

echo -e "${GREEN}╔══════════════════════════════════════════╗${NC}"
echo -e "${GREEN}║  DDMailServer Installation Script       ║${NC}"
echo -e "${GREEN}╚══════════════════════════════════════════╝${NC}"
echo ""

# Check if running as root
if [ "$EUID" -ne 0 ]; then
    echo -e "${RED}Error: Please run as root (sudo)${NC}"
    exit 1
fi

# Check if PostgreSQL is installed
if ! command -v psql &> /dev/null; then
    echo -e "${YELLOW}Warning: PostgreSQL not found. You'll need to install it.${NC}"
    read -p "Continue anyway? (y/n) " -n 1 -r
    echo
    if [[ ! $REPLY =~ ^[Yy]$ ]]; then
        exit 1
    fi
fi

# Check if Go is installed (for building)
if ! command -v go &> /dev/null; then
    echo -e "${YELLOW}Warning: Go not found. Will try to use pre-built binary.${NC}"
    BUILD=false
else
    BUILD=true
fi

# Create system user
echo -e "${GREEN}[1/8]${NC} Creating system user..."
if ! id -u $SERVICE_USER > /dev/null 2>&1; then
    useradd --system --no-create-home --shell /bin/false $SERVICE_USER
    echo "  ✓ User $SERVICE_USER created"
else
    echo "  ✓ User $SERVICE_USER already exists"
fi

# Create directories
echo -e "${GREEN}[2/8]${NC} Creating directories..."
mkdir -p $INSTALL_DIR
mkdir -p $CONFIG_DIR
mkdir -p $DATA_DIR
chown -R $SERVICE_USER:$SERVICE_USER $DATA_DIR
echo "  ✓ Directories created"

# Build or copy binary
echo -e "${GREEN}[3/8]${NC} Installing binary..."
if [ "$BUILD" = true ] && [ -f "go.mod" ]; then
    echo "  Building from source..."
    CGO_ENABLED=0 go build -o $INSTALL_DIR/$BINARY_NAME ./cmd/mailserver
    echo "  ✓ Built and installed"
elif [ -f "build/$BINARY_NAME" ]; then
    cp build/$BINARY_NAME $INSTALL_DIR/
    echo "  ✓ Copied pre-built binary"
elif [ -f "build/$BINARY_NAME-linux-amd64" ]; then
    cp build/$BINARY_NAME-linux-amd64 $INSTALL_DIR/$BINARY_NAME
    echo "  ✓ Copied pre-built binary"
else
    echo -e "${RED}  Error: No binary found. Please build first with 'make build-linux'${NC}"
    exit 1
fi

chmod +x $INSTALL_DIR/$BINARY_NAME
echo "  ✓ Binary installed at $INSTALL_DIR/$BINARY_NAME"

# Install config
echo -e "${GREEN}[4/8]${NC} Installing configuration..."
if [ ! -f "$CONFIG_DIR/config.yaml" ]; then
    if [ -f "configs/config.example.yaml" ]; then
        cp configs/config.example.yaml $CONFIG_DIR/config.yaml
        echo "  ✓ Config installed at $CONFIG_DIR/config.yaml"
    else
        echo -e "${RED}  Error: config.example.yaml not found${NC}"
        exit 1
    fi
else
    echo "  ! Config already exists, skipping"
fi

# Install systemd service
echo -e "${GREEN}[5/8]${NC} Installing systemd service..."
if [ -f "deployments/mailserver.service" ]; then
    cp deployments/mailserver.service $SERVICE_FILE
    systemctl daemon-reload
    echo "  ✓ Service installed"
else
    echo -e "${RED}  Error: mailserver.service not found${NC}"
    exit 1
fi

# Setup database
echo -e "${GREEN}[6/8]${NC} Database setup..."
echo "  Would you like to setup the database now? (requires PostgreSQL running)"
read -p "  Setup database? (y/n) " -n 1 -r
echo
if [[ $REPLY =~ ^[Yy]$ ]]; then
    echo "  Enter PostgreSQL superuser (default: postgres):"
    read -r PG_USER
    PG_USER=${PG_USER:-postgres}

    echo "  Creating database and user..."
    sudo -u postgres psql << EOF
CREATE USER mailserver WITH PASSWORD 'changeme';
CREATE DATABASE mailserver OWNER mailserver;
\c mailserver
\i migrations/001_initial_schema.sql
\i migrations/002_outbox.sql
EOF
    echo "  ✓ Database setup complete"
    echo -e "${YELLOW}  ! Remember to change the password in config.yaml${NC}"
else
    echo "  Skipped. Run migrations manually later."
fi

# Configure firewall (if ufw is installed)
echo -e "${GREEN}[7/8]${NC} Firewall configuration..."
if command -v ufw &> /dev/null; then
    read -p "  Open ports 1143 (IMAP) and 1587 (SMTP)? (y/n) " -n 1 -r
    echo
    if [[ $REPLY =~ ^[Yy]$ ]]; then
        ufw allow 1143/tcp comment 'DDMailServer IMAP'
        ufw allow 1587/tcp comment 'DDMailServer SMTP'
        ufw allow 8080/tcp comment 'DDMailServer API'
        echo "  ✓ Firewall rules added"
    fi
else
    echo "  UFW not found, skipping"
fi

# Final steps
echo -e "${GREEN}[8/8]${NC} Final steps..."
echo "  ✓ Installation complete!"
echo ""
echo -e "${GREEN}╔══════════════════════════════════════════╗${NC}"
echo -e "${GREEN}║           Next Steps                     ║${NC}"
echo -e "${GREEN}╚══════════════════════════════════════════╝${NC}"
echo ""
echo -e "${YELLOW}1. Edit configuration:${NC}"
echo "   sudo nano $CONFIG_DIR/config.yaml"
echo ""
echo -e "${YELLOW}2. Update database password in config${NC}"
echo ""
echo -e "${YELLOW}3. Start the service:${NC}"
echo "   sudo systemctl start mailserver"
echo ""
echo -e "${YELLOW}4. Check status:${NC}"
echo "   sudo systemctl status mailserver"
echo ""
echo -e "${YELLOW}5. Enable auto-start on boot:${NC}"
echo "   sudo systemctl enable mailserver"
echo ""
echo -e "${YELLOW}6. View logs:${NC}"
echo "   sudo journalctl -u mailserver -f"
echo ""
echo -e "${YELLOW}7. Test API:${NC}"
echo "   curl http://localhost:8080/health"
echo ""
echo -e "${GREEN}Installation directory:${NC} $INSTALL_DIR"
echo -e "${GREEN}Configuration:${NC} $CONFIG_DIR/config.yaml"
echo -e "${GREEN}Data directory:${NC} $DATA_DIR"
echo -e "${GREEN}Service user:${NC} $SERVICE_USER"
echo ""
