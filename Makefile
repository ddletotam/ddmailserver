.PHONY: build clean install test run deps

# Build settings
BINARY_NAME=mailserver
BUILD_DIR=build
INSTALL_DIR=/usr/local/bin
CONFIG_DIR=/etc/mailserver
DATA_DIR=/var/lib/mailserver

# Go settings
GOCMD=go
GOBUILD=$(GOCMD) build
GOCLEAN=$(GOCMD) clean
GOTEST=$(GOCMD) test
GOGET=$(GOCMD) get
GOMOD=$(GOCMD) mod

# Build for current platform
build:
	@echo "Building $(BINARY_NAME)..."
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 $(GOBUILD) -o $(BUILD_DIR)/$(BINARY_NAME) -v ./cmd/mailserver
	@echo "Build complete: $(BUILD_DIR)/$(BINARY_NAME)"

# Build for Linux (useful if building from Windows/Mac)
build-linux:
	@echo "Building $(BINARY_NAME) for Linux..."
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GOBUILD) -o $(BUILD_DIR)/$(BINARY_NAME)-linux-amd64 -v ./cmd/mailserver
	@echo "Build complete: $(BUILD_DIR)/$(BINARY_NAME)-linux-amd64"

# Build for all platforms
build-all: build-linux
	@echo "Building for Windows..."
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 $(GOBUILD) -o $(BUILD_DIR)/$(BINARY_NAME)-windows-amd64.exe -v ./cmd/mailserver
	@echo "Building for macOS..."
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 $(GOBUILD) -o $(BUILD_DIR)/$(BINARY_NAME)-darwin-amd64 -v ./cmd/mailserver
	@echo "All builds complete"

# Clean build artifacts
clean:
	@echo "Cleaning..."
	$(GOCLEAN)
	rm -rf $(BUILD_DIR)
	@echo "Clean complete"

# Download dependencies
deps:
	@echo "Downloading dependencies..."
	$(GOMOD) download
	$(GOMOD) tidy
	@echo "Dependencies ready"

# Run tests
test:
	@echo "Running tests..."
	$(GOTEST) -v ./...

# Run locally
run: build
	@echo "Running $(BINARY_NAME)..."
	./$(BUILD_DIR)/$(BINARY_NAME) -config configs/config.yaml

# Install on Linux system (requires root)
install: build
	@echo "Installing $(BINARY_NAME)..."
	@sudo mkdir -p $(INSTALL_DIR)
	@sudo mkdir -p $(CONFIG_DIR)
	@sudo mkdir -p $(DATA_DIR)
	@sudo cp $(BUILD_DIR)/$(BINARY_NAME) $(INSTALL_DIR)/
	@sudo chmod +x $(INSTALL_DIR)/$(BINARY_NAME)
	@sudo cp configs/config.example.yaml $(CONFIG_DIR)/config.yaml
	@sudo cp deployments/mailserver.service /etc/systemd/system/
	@sudo systemctl daemon-reload
	@echo "Installation complete!"
	@echo ""
	@echo "Next steps:"
	@echo "1. Edit config: sudo nano $(CONFIG_DIR)/config.yaml"
	@echo "2. Setup database: psql -U postgres -f migrations/001_initial_schema.sql"
	@echo "3. Start service: sudo systemctl start mailserver"
	@echo "4. Enable on boot: sudo systemctl enable mailserver"

# Uninstall
uninstall:
	@echo "Uninstalling $(BINARY_NAME)..."
	@sudo systemctl stop mailserver || true
	@sudo systemctl disable mailserver || true
	@sudo rm -f $(INSTALL_DIR)/$(BINARY_NAME)
	@sudo rm -f /etc/systemd/system/mailserver.service
	@sudo systemctl daemon-reload
	@echo "Uninstalled. Config and data preserved in $(CONFIG_DIR) and $(DATA_DIR)"

# View logs
logs:
	sudo journalctl -u mailserver -f

# Check status
status:
	sudo systemctl status mailserver

# Development mode (with auto-reload)
dev:
	@which air > /dev/null || (echo "Installing air..." && go install github.com/cosmtrek/air@latest)
	air
