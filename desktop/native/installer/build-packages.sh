#!/usr/bin/env bash
# Сборка Linux-клиента и пакетов .deb/.rpm.
#   installer/build-packages.sh            — собрать бинарь и пакеты
#   installer/build-packages.sh --no-build — только пакеты из готового бинаря
# Бинарь — под glibc 2.31 (Debian 11): на более новых системах он работает,
# на более старых — нет; обычный `cargo build` привязал бы его к glibc станции.
# Нужны: cargo-zigbuild + zig (`pip install ziglang`), nfpm
# (`go install github.com/goreleaser/nfpm/v2/cmd/nfpm@v2.41.1`).
# Результат: ~/.cache/builds/ddmail-packages/ddmail_<v>_amd64.deb, ddmail-<v>-1.x86_64.rpm
set -euo pipefail
cd "$(dirname "$0")/.."

TARGET=x86_64-unknown-linux-gnu
GLIBC=2.31
VERSION="$(sed -n 's/^version = "\(.*\)"/\1/p' Cargo.toml | head -1)"
TARGET_DIR="${CARGO_TARGET_DIR:-target}"
BIN="$TARGET_DIR/$TARGET/release/ddmail-native"
OUT="${HOME}/.cache/builds/ddmail-packages"
NFPM="$(command -v nfpm || echo "$(go env GOPATH 2>/dev/null)/bin/nfpm")"

if [[ "${1:-}" != "--no-build" ]]; then
    cargo zigbuild --release --target "$TARGET.$GLIBC"
fi
[[ -x "$BIN" ]] || { echo "error: $BIN not found — build first" >&2; exit 1; }

# Страховка: ни одного символа glibc новее заявленной.
newest="$(objdump -T "$BIN" | grep -o 'GLIBC_[0-9.]*' | sort -Vu | tail -1)"
if [[ "$(printf '%s\n' "GLIBC_$GLIBC" "$newest" | sort -V | tail -1)" != "GLIBC_$GLIBC" ]]; then
    echo "error: binary needs $newest, expected <= GLIBC_$GLIBC" >&2; exit 1
fi

STAGE="$(mktemp -d)"; trap 'rm -rf "$STAGE"' EXIT
install -m755 "$BIN" "$STAGE/ddmail-native"
install -m644 assets/ddmail.desktop "$STAGE/ddmail.desktop"
install -m644 assets/ddmail.svg "$STAGE/ddmail.svg"
install -m644 assets/ddmail_icon.png "$STAGE/ddmail.png"
cat > "$STAGE/refresh-caches.sh" <<'SH'
#!/bin/sh
# Меню и иконки подхватываются сразу; ошибки не критичны.
update-desktop-database -q /usr/share/applications 2>/dev/null || true
gtk-update-icon-cache -q -f /usr/share/icons/hicolor 2>/dev/null || true
exit 0
SH

# nfpm подставляет переменные не во все поля (в contents.src — нет), поэтому
# конфиг разворачиваем сами.
sed -e "s|\${VERSION}|$VERSION|g" -e "s|\${STAGE}|$STAGE|g" installer/nfpm.yaml > "$STAGE/nfpm.yaml"
mkdir -p "$OUT"
for fmt in deb rpm; do
    "$NFPM" package -f "$STAGE/nfpm.yaml" -p "$fmt" -t "$OUT/"
done
ls -l "$OUT"
