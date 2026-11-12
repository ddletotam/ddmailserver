#!/usr/bin/env bash
# Сборка Apple-клиента на Маке по SSH (контракт §1). Код уезжает rsync'ом в
# одну сторону; Мак — только сборочный узел.
#
#   mac.sh test            swift test пакета DDMailKit
#   mac.sh build           сгенерировать проект, собрать под симулятор iOS и под macOS
#   mac.sh run [iphone|ipad]  собрать, поставить в симулятор, запустить, скриншот сюда
#   mac.sh uitest [iphone|ipad|mac] [Класс/тест]  сквозные UI-тесты; скриншоты шагов — сюда
#   mac.sh device [iphone|ipad]  собрать с подписью, поставить на устройство, запустить
#
# Окружение: DDMAIL_MAC (ssh-хост, по умолчанию `mac`),
#            DDMAIL_SHOTS (куда класть скриншоты, по умолчанию ~/.cache/ddmail-ios),
#            DDMAIL_TEST_EMAIL / DDMAIL_TEST_PASSWORD — учётка для uitest (без них
#            сквозные тесты пропускаются), DDMAIL_TEST_MAILRU_EMAIL /
#            DDMAIL_TEST_MAILRU_PASSWORD — внешний ящик для ImapFlowTests,
#            DDMAIL_KEYCHAIN_PASSWORD — для device.
#            По умолчанию — из ~/.config/ddmail-ios/ (см. ниже); в git их нет.
set -euo pipefail

# Секреты станции — вне репозитория (как ~/.config/ddmail-android у Android):
#   test.env           DDMAIL_TEST_EMAIL / DDMAIL_TEST_PASSWORD для uitest
#   keychain-password  пароль входа на Мак: разблокировать связку для подписи
SECRETS="${XDG_CONFIG_HOME:-$HOME/.config}/ddmail-ios"
# shellcheck source=/dev/null
[ -f "$SECRETS/test.env" ] && . "$SECRETS/test.env"
if [ -z "${DDMAIL_KEYCHAIN_PASSWORD:-}" ] && [ -f "$SECRETS/keychain-password" ]; then
    DDMAIL_KEYCHAIN_PASSWORD="$(cat "$SECRETS/keychain-password")"
fi

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
export PATH="$HOME/.local/xcodegen/bin:$PATH"
cd "$HOME/build/ddmail/mobile/ios"
DD="$HOME/build/ddmail/DerivedData"
cmd="$1"; shift

# Зеркала пакетов на Маке (~/src-mirrors): полный клон с GitHub провайдер
# обрывает («unexpected disconnect»), поверхностный проходит. Конфигурация
# зеркал — и для пакета (swift test), и для проекта (xcodebuild); в git её нет.
mirrors() {
    local json="{\"object\":[{\"mirror\":\"file://$HOME/src-mirrors/GRDB.swift\",\"original\":\"https://github.com/groue/GRDB.swift.git\"}],\"version\":1}"
    [ -d "$HOME/src-mirrors/GRDB.swift" ] || return 0
    for d in DDMailKit/.swiftpm/configuration DDMail.xcodeproj/project.xcworkspace/xcshareddata/swiftpm/configuration; do
        mkdir -p "$d" && printf '%s' "$json" > "$d/mirrors.json"
    done
}

gen() { xcodegen generate --quiet; mirrors; }

# Пакеты xcodebuild — в постоянном каталоге, а не в DerivedData: клонируются один раз.
SPM="$HOME/build/ddmail/SourcePackages"

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
        -destination "generic/platform=iOS Simulator" -derivedDataPath "$DD" -clonedSourcePackagesDirPath "$SPM" \
        -quiet build
}

case "$cmd" in
    test)
        # Итоги по наборам и все падения; полный вывод — в test.log на Маке.
        log="$HOME/build/ddmail/test.log"
        rc=0
        mirrors
        (cd DDMailKit && swift test >"$log" 2>&1) || rc=$?
        grep -E "error:|✘|Expectation failed|Suite .* (passed|failed)|Test run with" "$log" || tail -30 "$log"
        exit "$rc"
        ;;
    build)
        gen
        build_sim
        # Мак: права на связку ключей требуют подписи командой; нет учётки
        # Xcode — только компиляция без подписи.
        team="$(defaults read com.apple.dt.Xcode IDEProvisioningTeamByIdentifier 2>/dev/null \
            | awk -F'= ' '/teamID/ {gsub(/[ ;"]/, "", $2); print $2; exit}')"
        if [ -n "${1:-}" ]; then security unlock-keychain -p "$1" login.keychain; fi
        if [ -n "$team" ]; then sign="-allowProvisioningUpdates DEVELOPMENT_TEAM=$team"; else sign="CODE_SIGNING_ALLOWED=NO"; fi
        xcodebuild -project DDMail.xcodeproj -scheme DDMail -configuration Debug \
            -destination "platform=macOS" -derivedDataPath "$DD" -clonedSourcePackagesDirPath "$SPM" \
            $sign -quiet build
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
    uitest)
        gen
        if [ "${3:-iphone}" = mac ]; then
            # Мак: приложение подписано командой (иначе Keychain с защитой данных
            # не пустит), связка разблокирована, назначение — сам Мак.
            team="$(defaults read com.apple.dt.Xcode IDEProvisioningTeamByIdentifier 2>/dev/null \
                | awk -F'= ' '/teamID/ {gsub(/[ ;"]/, "", $2); print $2; exit}')"
            if [ -n "${4:-}" ]; then security unlock-keychain -p "$4" login.keychain; fi
            dest="platform=macOS"
            extra="-allowProvisioningUpdates DEVELOPMENT_TEAM=$team"
        else
            dest="id=$(sim_udid "${3:-iphone}")"
            extra=""
        fi
        rb="$HOME/build/ddmail/ui.xcresult"
        out="$HOME/build/ddmail/ui-shots"
        rm -rf "$rb" "$out"
        rc=0
        only=""
        if [ -n "${7:-}" ]; then only="-only-testing:DDMailUITests/$7"; fi
        TEST_RUNNER_DDMAIL_TEST_EMAIL="${1:-}" TEST_RUNNER_DDMAIL_TEST_PASSWORD="${2:-}" \
        TEST_RUNNER_DDMAIL_TEST_MAILRU_EMAIL="${5:-}" TEST_RUNNER_DDMAIL_TEST_MAILRU_PASSWORD="${6:-}" \
            xcodebuild -project DDMail.xcodeproj -scheme DDMail -configuration Debug \
            -destination "$dest" -derivedDataPath "$DD" -clonedSourcePackagesDirPath "$SPM" -resultBundlePath "$rb" \
            $extra test $only >"$HOME/build/ddmail/uitest.log" 2>&1 || rc=$?
        grep -E "error:|Test Case .*(passed|failed|skipped)|TEST (SUCCEEDED|FAILED)" "$HOME/build/ddmail/uitest.log" | tail -20
        mkdir -p "$out"
        xcrun xcresulttool export attachments --path "$rb" --output-path "$out" >/dev/null 2>&1 || true
        exit "$rc"
        ;;
    device)
        kind="${1:-iphone}"
        gen
        # Команда подписи — из учётки Xcode на Маке, в репозитории её нет.
        team="$(defaults read com.apple.dt.Xcode IDEProvisioningTeamByIdentifier 2>/dev/null \
            | awk -F'= ' '/teamID/ {gsub(/[ ;"]/, "", $2); print $2; exit}')"
        [ -n "$team" ] || { echo "в Xcode не выполнен вход (Settings → Accounts)" >&2; exit 1; }
        xcrun devicectl list devices --json-output "$HOME/build/ddmail/devices.json" >/dev/null
        udid="$(/usr/bin/python3 - "$kind" "$HOME/build/ddmail/devices.json" <<'PY'
import json, sys
kind, path = sys.argv[1], sys.argv[2]
want = "iPad" if kind == "ipad" else "iPhone"
for d in json.load(open(path))["result"]["devices"]:
    hw, cp = d["hardwareProperties"], d["connectionProperties"]
    if hw.get("reality") == "physical" and hw.get("deviceType") == want and cp.get("pairingState") == "paired":
        print(hw["udid"]); sys.exit(0)
sys.exit(f"нет спаренного {want}: подключите кабелем, «Доверять», включите режим разработчика")
PY
)"
        # В SSH-сессии связка ключей заблокирована, и подпись падает с
        # errSecInternalComponent; пароль — из DDMAIL_KEYCHAIN_PASSWORD, если задан.
        if [ -n "${2:-}" ]; then security unlock-keychain -p "$2" login.keychain; fi
        xcodebuild -project DDMail.xcodeproj -scheme DDMail -configuration Debug \
            -destination "id=$udid" -derivedDataPath "$DD" -clonedSourcePackagesDirPath "$SPM" \
            -allowProvisioningUpdates DEVELOPMENT_TEAM="$team" -quiet build
        app="$DD/Build/Products/Debug-iphoneos/DDMail.app"
        xcrun devicectl device install app --device "$udid" "$app" >/dev/null
        # Заблокированный телефон не запустит приложение — это не провал установки.
        if xcrun devicectl device process launch --terminate-existing --device "$udid" ru.letotam.ddmail >/dev/null 2>&1; then
            echo "device: установлено и запущено ($kind $udid)"
        else
            echo "device: установлено ($kind $udid); запуск не удался — телефон заблокирован или сборке ещё не доверяют"
        fi
        ;;
    *)
        echo "неизвестная команда: $cmd" >&2; exit 2 ;;
esac
EOF
}

cmd="${1:-}"
case "$cmd" in
    test)
        sync_code
        remote test
        ;;
    build)
        sync_code
        remote build "${DDMAIL_KEYCHAIN_PASSWORD:-}"
        ;;
    run)
        sync_code
        remote run "${2:-iphone}"
        mkdir -p "$SHOTS"
        out="$SHOTS/${2:-iphone}-$(date +%H%M%S).png"
        scp -q "$HOST:$REMOTE/shot.png" "$out"
        echo "скриншот: $out"
        ;;
    uitest)
        sync_code
        rc=0
        remote uitest "${DDMAIL_TEST_EMAIL:-}" "${DDMAIL_TEST_PASSWORD:-}" "${2:-iphone}" "${DDMAIL_KEYCHAIN_PASSWORD:-}" \
            "${DDMAIL_TEST_MAILRU_EMAIL:-}" "${DDMAIL_TEST_MAILRU_PASSWORD:-}" "${3:-}" || rc=$?
        dir="$SHOTS/uitest-$(date +%H%M%S)"
        mkdir -p "$dir"
        scp -qr "$HOST:$REMOTE/ui-shots/." "$dir/" 2>/dev/null || true
        echo "скриншоты: $dir"
        exit "$rc"
        ;;
    device)
        sync_code
        remote device "${2:-iphone}" "${DDMAIL_KEYCHAIN_PASSWORD:-}"
        ;;
    *)
        sed -n '2,17p' "$0" >&2
        exit 2
        ;;
esac
