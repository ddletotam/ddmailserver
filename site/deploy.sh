#!/usr/bin/env bash
# Сборка и выкладка сайта lunlun.email. Повторять после каждой правки.
#   site/deploy.sh            — собрать и выложить
#   site/deploy.sh --dry-run  — собрать и показать, что изменится на сервере
set -euo pipefail
cd "$(dirname "$0")"
OUT="${HOME}/.cache/builds/lunlun-email-site"
python3 build.py --out "$OUT"
rsync -rlt --delete --chmod=D755,F644 ${1:-} "$OUT/" lucky@jwebhelp.ru:/var/www/lunlun.email/
