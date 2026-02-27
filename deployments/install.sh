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

# ============================================
# STEP 0: Collect configuration parameters
# ============================================
echo -e "${YELLOW}=== Configuration Setup ===${NC}"
echo ""

# Database configuration
echo -e "${GREEN}Database Configuration:${NC}"
read -p "Database host (default: localhost): " DB_HOST
DB_HOST=${DB_HOST:-localhost}

read -p "Database port (default: 5432): " DB_PORT
DB_PORT=${DB_PORT:-5432}

read -p "Database name (default: mailserver): " DB_NAME
DB_NAME=${DB_NAME:-mailserver}

read -p "Database user (default: mailserver): " DB_USER
DB_USER=${DB_USER:-mailserver}

read -sp "Database password (default: changeme): " DB_PASS
echo ""
DB_PASS=${DB_PASS:-changeme}

# JWT Secret
echo ""
read -p "JWT secret (press Enter to auto-generate): " JWT_SECRET
if [ -z "$JWT_SECRET" ]; then
    JWT_SECRET=$(openssl rand -hex 32 2>/dev/null || cat /dev/urandom | tr -dc 'a-zA-Z0-9' | fold -w 64 | head -n 1)
    echo "  Generated JWT secret"
fi

# Database setup decision
echo ""
echo -e "${GREEN}Database Setup:${NC}"
read -p "Would you like to setup the database now? (y/n): " -n 1 -r SETUP_DB
echo ""

echo ""
echo -e "${YELLOW}Configuration collected. Starting installation...${NC}"
echo ""
sleep 2

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

if [ -f "$CONFIG_DIR/config.yaml" ]; then
    echo -e "${YELLOW}  Config already exists at $CONFIG_DIR/config.yaml${NC}"
    read -p "  Overwrite with new configuration? (y/n): " -n 1 -r
    echo ""
    if [[ ! $REPLY =~ ^[Yy]$ ]]; then
        echo "  Keeping existing config"
    else
        rm -f $CONFIG_DIR/config.yaml
    fi
fi

if [ ! -f "$CONFIG_DIR/config.yaml" ]; then
    if [ -f "configs/config.example.yaml" ]; then
        cp configs/config.example.yaml $CONFIG_DIR/config.yaml

        # Update config file with provided values
        sed -i "s/host: \"localhost\"/host: \"$DB_HOST\"/" $CONFIG_DIR/config.yaml
        sed -i "s/port: 5432/port: $DB_PORT/" $CONFIG_DIR/config.yaml
        sed -i "s/dbname: \"mailserver\"/dbname: \"$DB_NAME\"/" $CONFIG_DIR/config.yaml
        sed -i "s/user: \"mailserver\"/user: \"$DB_USER\"/" $CONFIG_DIR/config.yaml
        sed -i "s/password: \"changeme\"/password: \"$DB_PASS\"/" $CONFIG_DIR/config.yaml
        sed -i "s/jwt_secret: \"change-this-secret-key\"/jwt_secret: \"$JWT_SECRET\"/" $CONFIG_DIR/config.yaml

        echo "  ✓ Config installed and configured at $CONFIG_DIR/config.yaml"
    else
        echo -e "${RED}  Error: config.example.yaml not found${NC}"
        exit 1
    fi
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

if [[ $SETUP_DB =~ ^[Yy]$ ]]; then
    if [ "$DB_HOST" = "localhost" ] || [ "$DB_HOST" = "127.0.0.1" ]; then
        # Local database setup
        echo "  Setting up local database..."

        # Detect PostgreSQL superuser
        if id "postgres" &>/dev/null; then
            PG_ADMIN="postgres"
        elif id "pgsql" &>/dev/null; then
            PG_ADMIN="pgsql"
        else
            read -p "  PostgreSQL superuser name: " PG_ADMIN
        fi

        echo "  Creating database and user as $PG_ADMIN..."
        sudo -u $PG_ADMIN psql << EOF
-- Drop existing if exists
DROP DATABASE IF EXISTS $DB_NAME;
DROP USER IF EXISTS $DB_USER;

-- Create new
CREATE USER $DB_USER WITH PASSWORD '$DB_PASS';
CREATE DATABASE $DB_NAME OWNER $DB_USER;
EOF

        if [ $? -eq 0 ]; then
            echo "  ✓ Database and user created"

            # Run migrations
            echo "  Running migrations..."
            sudo -u $PG_ADMIN psql -d $DB_NAME -f migrations/001_initial_schema.sql
            sudo -u $PG_ADMIN psql -d $DB_NAME -f migrations/002_outbox.sql

            if [ $? -eq 0 ]; then
                echo "  ✓ Database setup complete"
            else
                echo -e "${YELLOW}  ! Migrations failed. You may need to run them manually.${NC}"
            fi
        else
            echo -e "${RED}  Error creating database. Please check PostgreSQL is running.${NC}"
        fi
    else
        # Remote database setup
        echo "  Database host is remote: $DB_HOST:$DB_PORT"
        echo ""
        echo -e "${YELLOW}  You need to setup the database on the remote server.${NC}"
        echo ""
        echo "  1. On database server ($DB_HOST), create user and database:"
        echo ""
        echo "     psql -U postgres << EOF"
        echo "     CREATE USER $DB_USER WITH PASSWORD '$DB_PASS';"
        echo "     CREATE DATABASE $DB_NAME OWNER $DB_USER;"
        echo "     EOF"
        echo ""
        echo "  2. From this server, run migrations:"
        echo ""
        echo "     cd $(pwd)"
        echo "     PGPASSWORD='$DB_PASS' psql -h $DB_HOST -p $DB_PORT -U $DB_USER -d $DB_NAME -f migrations/001_initial_schema.sql"
        echo "     PGPASSWORD='$DB_PASS' psql -h $DB_HOST -p $DB_PORT -U $DB_USER -d $DB_NAME -f migrations/002_outbox.sql"
        echo ""
        echo "  Or if you have psql client installed:"
        read -p "  Run migrations now? (y/n): " -n 1 -r
        echo ""
        if [[ $REPLY =~ ^[Yy]$ ]]; then
            if command -v psql &> /dev/null; then
                echo "  Running migrations on remote database..."
                PGPASSWORD="$DB_PASS" psql -h $DB_HOST -p $DB_PORT -U $DB_USER -d $DB_NAME -f migrations/001_initial_schema.sql
                PGPASSWORD="$DB_PASS" psql -h $DB_HOST -p $DB_PORT -U $DB_USER -d $DB_NAME -f migrations/002_outbox.sql

                if [ $? -eq 0 ]; then
                    echo "  ✓ Migrations completed"
                else
                    echo -e "${RED}  Failed to run migrations. Check connection and credentials.${NC}"
                fi
            else
                echo -e "${RED}  psql client not found. Install postgresql-client first.${NC}"
            fi
        else
            echo "  Skipped. Run migrations manually as shown above."
        fi
    fi
else
    echo "  Database setup skipped."
    echo "  You can setup database later manually."
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
if [ ! -z "$DB_HOST" ]; then
    echo -e "${GREEN}Database Configuration:${NC}"
    echo "  Host: $DB_HOST:$DB_PORT"
    echo "  Database: $DB_NAME"
    echo "  User: $DB_USER"
    echo ""
fi
