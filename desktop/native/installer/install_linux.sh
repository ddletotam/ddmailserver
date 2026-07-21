#!/usr/bin/env bash
# ddmail — Linux per-user install (the analogue of ddmail.iss on Windows).
# Build:  cargo build --release (done here unless --no-build)
# Input:  target/release/ddmail-native
# Output: ~/.local/bin/ddmail-native + desktop entry + hicolor icons
set -euo pipefail

cd "$(dirname "$0")/.."

if [[ "${1:-}" != "--no-build" ]]; then
    cargo build --release
fi

BIN=target/release/ddmail-native
[[ -x "$BIN" ]] || { echo "error: $BIN not found — build first" >&2; exit 1; }

DATA="${XDG_DATA_HOME:-$HOME/.local/share}"

install -Dm755 "$BIN" "$HOME/.local/bin/ddmail-native"
install -Dm644 assets/ddmail.svg "$DATA/icons/hicolor/scalable/apps/ddmail.svg"
install -Dm644 assets/ddmail_icon.png "$DATA/icons/hicolor/256x256/apps/ddmail.png"
install -Dm644 assets/ddmail.desktop "$DATA/applications/ddmail.desktop"

# Refresh caches so the launcher picks the icon up right away (best-effort —
# most desktops rescan on their own).
update-desktop-database "$DATA/applications" 2>/dev/null || true
gtk-update-icon-cache -f "$DATA/icons/hicolor" 2>/dev/null || true

echo "installed: $HOME/.local/bin/ddmail-native"
echo "desktop entry: $DATA/applications/ddmail.desktop"
