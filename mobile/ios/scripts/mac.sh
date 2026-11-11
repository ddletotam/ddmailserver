#!/usr/bin/env bash
# Сборка Apple-клиента на Маке по SSH (контракт §1). Код уезжает rsync'ом в
# одну сторону; Мак — только сборочный узел.
#
#   mac.sh test            swift test пакета DDMailKit
#   mac.sh build           сгенерировать проект, собрать под симулятор iOS и под macOS
#   mac.sh run [iphone|ipad]  собрать, поставить в симулятор, запустить, скриншот сюда
#
# Окружение: DDMAIL_MAC (ssh-хост, по умолчанию `mac`),
#            DDMAIL_SHOTS (куда класть скриншоты, по умолчанию ~/.cache/ddmail-ios).
set -euo pipefail

HOST="${DDMAIL_MAC:-mac}"
SHOTS="${DDMAIL_SHOTS:-$HOME/.cache/ddmail-ios}"
ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
REMOTE="build/ddmail"   # относительно домашнего каталога на Маке

sync_code() {
    # mobile/shared — документ ленты беседы (контракт §8), нужен в ресурсах.
    (cd "$ROOT" && rsync -azR --delete \
        --exclude '*.xcodeproj' --exclude '.build/' --exclude '.swiftpm/' --exclude '.DS_Store' \
        mobile/ios mobile/shared "$HOST:$REMOTE/")
}

remote() {
    # Общая часть для команд на Маке: PATH к xcodegen, выход при ошибке.
    ssh "$HOST" "bash -s" -- "$@" <<'EOF'
set -euo pipefail
export PATH="$HOME/.local/bin:$PATH"
cd "$HOME/build/ddmail/mobile/ios"
DD="$HOME/build/ddmail/DerivedData"
cmd="$1"; shift

gen() { xcodegen generate --quiet; }

# Простой симулятор нужного типа на самой свежей iOS: «iPhone … Pro» (не Max) или «iPad Pro 13».
sim_udid() {
    local pattern
    case "$1" in
        ipad) pattern='^iPad Pro 13' ;;
        *) pattern='^iPhone [0-9]+ Pro$' ;;
    esac
    xcrun simctl list devices available -j | /usr/bin/python3 -c '
import json, re, sys
pat = re.compile(sys.argv[1])
devs = json.load(sys.stdin)["devices"]
for rt in sorted((k for k in devs if ".iOS-" in k), reverse=True):
    for d in devs[rt]:
        if pat.search(d["name"]):
            print(d["udid"]); sys.exit(0)
sys.exit("нет симулятора под " + sys.argv[1])
' "$pattern"
}

build_sim() {
    xcodebuild -project DDMail.xcodeproj -scheme DDMail -configuration Debug \
        -destination "generic/platform=iOS Simulator" -derivedDataPath "$DD" \
        -quiet build
}

case "$cmd" in
    test)
        (cd DDMailKit && swift test 2>&1 | tail -40)
        ;;
    build)
        gen
        build_sim
        xcodebuild -project DDMail.xcodeproj -scheme DDMail -configuration Debug \
            -destination "platform=macOS" -derivedDataPath "$DD" \
            CODE_SIGN_IDENTITY=- -quiet build
        echo "build: ok (iOS Simulator, macOS)"
        ;;
    run)
        kind="${1:-iphone}"
        gen
        build_sim
        udid="$(sim_udid "$kind")"
        xcrun simctl boot "$udid" 2>/dev/null || true
        xcrun simctl bootstatus "$udid" -b >/dev/null
        app="$DD/Build/Products/Debug-iphonesimulator/DDMail.app"
        xcrun simctl install "$udid" "$app"
        xcrun simctl terminate "$udid" ru.letotam.ddmail 2>/dev/null || true
        xcrun simctl launch "$udid" ru.letotam.ddmail >/dev/null
        sleep 3
        xcrun simctl io "$udid" screenshot --type=png "$HOME/build/ddmail/shot.png" >/dev/null 2>&1
        echo "run: ok ($kind $udid)"
        ;;
    *)
        echo "неизвестная команда: $cmd" >&2; exit 2 ;;
esac
EOF
}

cmd="${1:-}"
case "$cmd" in
    test|build)
        sync_code
        remote "$cmd"
        ;;
    run)
        sync_code
        remote run "${2:-iphone}"
        mkdir -p "$SHOTS"
        out="$SHOTS/${2:-iphone}-$(date +%H%M%S).png"
        scp -q "$HOST:$REMOTE/shot.png" "$out"
        echo "скриншот: $out"
        ;;
    *)
        sed -n '2,11p' "$0" >&2
        exit 2
        ;;
esac
