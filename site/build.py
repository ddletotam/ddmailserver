#!/usr/bin/env python3
"""Сборка сайта почтового сервиса lunlun.email (лендинг, инструкции, FAQ).

Сервис работает на технологиях ddmail; приложения DDMail — его рекомендованный
клиент (самостоятельный продукт, в магазинах под своим именем).

Только стандартная библиотека Python, никаких внешних ресурсов на страницах:
шрифты системные, картинки и стили — свои.

    python3 site/build.py            # → ~/.cache/builds/ddmail-site/
    python3 site/build.py --out DIR

Страница — фрагмент HTML в pages/ с первой строкой-метаданными:
    <!--meta {"title": "…", "description": "…", "nav": "help"} -->
Подстановки в страницах и шаблоне: {{root}} — относительный путь до корня
сайта (сайт открывается и с диска, и из подкаталога), {{cfg.путь.к.ключу}} —
значение из config.json, {{snippet:имя}} — готовые блоки (кнопки магазинов,
призыв зарегистрироваться). После сборки проверяются все локальные ссылки.
"""
import argparse
import html
import json
import os
import re
import shutil
import sys
from datetime import date
from pathlib import Path

HERE = Path(__file__).resolve().parent
REPO = HERE.parent
DEFAULT_OUT = Path(os.path.expanduser("~/.cache/builds/lunlun-email-site"))

NAV = [
    ("home", "index.html", "Главная"),
    ("apps", "apps.html", "Приложение"),
    ("help", "help/index.html", "Инструкции"),
    ("faq", "faq.html", "Вопросы"),
]

# Картинки берём из репозитория (те же, что в магазинах), не копируем в site/.
ASSETS = {
    "img/icon-512.png": REPO / "mobile/store-assets/icon-512.png",
    "img/icon-216.png": REPO / "mobile/store-assets/icon-216.png",
    "img/screen-conversations.png": REPO / "mobile/store-assets/android/01-conversations.png",
    "img/screen-conversation.png": REPO / "mobile/store-assets/android/02-conversation.png",
    "img/screen-compose.png": REPO / "mobile/store-assets/android/03-search-compose.png",
    "img/screen-calendar.png": REPO / "mobile/store-assets/android/04-calendar.png",
    "img/screen-contacts.png": REPO / "mobile/store-assets/android/05-contacts.png",
    "img/screen-tasks.png": REPO / "mobile/store-assets/android/06-tasks.png",
    "img/screen-group.png": REPO / "mobile/store-assets/android/07-group-conversation.png",
}

META_RE = re.compile(r"^<!--meta (\{.*?\}) -->\n", re.S)
VAR_RE = re.compile(r"\{\{\s*([a-zA-Z0-9_.:-]+)\s*\}\}")


def lookup(cfg, dotted):
    cur = cfg
    for part in dotted.split("."):
        if not isinstance(cur, dict) or part not in cur:
            raise KeyError(dotted)
        cur = cur[part]
    return cur


def store_button(label, sub, store):
    if store.get("live"):
        return (f'<a class="store" href="{html.escape(store["url"])}" rel="noopener">'
                f'<span class="store-sub">{sub}</span><span class="store-name">{label}</span></a>')
    return (f'<span class="store store-soon" aria-disabled="true">'
            f'<span class="store-sub">скоро</span><span class="store-name">{label}</span></span>')


def snippets(cfg):
    s, d = cfg["stores"], cfg["desktop"]
    mobile = "".join([
        store_button("App Store", "iPhone и iPad", s["appstore"]),
        store_button("AppGallery", "Huawei", s["appgallery"]),
        store_button("RuStore", "Android", s["rustore"]),
    ])
    desktop = "".join([
        store_button("macOS", "Mac App Store", d["macos"]),
        store_button("Windows", d["windows"].get("sub", "компьютер"), d["windows"]),
        store_button("Linux", d["linux"].get("sub", "компьютер"), d["linux"]),
    ])
    sup = cfg["support_email"]
    # Регистрация — только через Telegram-бота (решение 2026-11-28).
    bot = cfg["signup"]["bot"]
    signup_href = cfg["signup"]["url"]
    signup_label = "Получить адрес"
    signup_note = (f"Адрес выдаёт наш Telegram-бот @{bot}: выберите логин — и почта готова. "
                   "Бесплатно, один адрес на аккаунт Telegram.")
    fr = cfg["free"]
    rows = "".join(
        f'<tr><td><b>{html.escape(p["title"])}</b><br><span class="muted">{html.escape(p["text"])}</span></td>'
        f'<td class="nowrap">{p["price"]} ⭐ / мес</td></tr>'
        for p in cfg["paid"])
    pricing = (
        '<div class="grid">'
        f'<div class="card"><h3>Бесплатно</h3><p>Адрес на {html.escape(cfg["domain"])}, ящик {fr["storage_gb"]} ГБ, '
        f'до {fr["send_per_day"]} писем и {fr["recipients_per_day"]} разных получателей в сутки. '
        'Приложение, календарь, задачи, контакты и мгновенные уведомления — тоже бесплатно.</p></div>'
        '<div class="card"><h3>Что хотите — то и подключаете</h3><p>Тарифов нет: каждая возможность подключается '
        'отдельно и оплачивается помесячно звёздами Telegram в боте. Отменить можно в любой момент.</p></div>'
        '</div>'
        f'<div class="table-wrap" style="margin-top:24px"><table><tr><th>Возможность</th><th>Цена</th></tr>{rows}</table></div>')
    return {
        "stores-mobile": f'<div class="stores">{mobile}</div>',
        "stores-desktop": f'<div class="stores">{desktop}</div>',
        "signup-href": html.escape(signup_href),
        "signup-label": signup_label,
        "signup-note": signup_note,
        "pricing": pricing,
        "year": str(date.today().year),
    }


def render(text, cfg, snip, root):
    def sub(m):
        key = m.group(1)
        if key == "root":
            return root
        if key.startswith("snippet:"):
            return snip[key[len("snippet:"):]]
        if key.startswith("cfg."):
            return html.escape(str(lookup(cfg, key[4:])))
        raise KeyError(f"неизвестная подстановка {{{{{key}}}}}")
    # Дважды: сниппеты сами содержат {{root}} не бывает, но страница может.
    return VAR_RE.sub(sub, text)


def privacy_body():
    """Текст действующей политики (deployments/privacy.html) без её обвязки:
    одна редакция на сайт и приложение, правится только там."""
    src = (REPO / "deployments/privacy.html").read_text(encoding="utf-8")
    m = re.search(r"<main>(.*?)</main>", src, re.S)
    if not m:
        sys.exit("privacy.html: не найден <main>")
    return m.group(1).strip()


def build(out):
    cfg = json.loads((HERE / "config.json").read_text(encoding="utf-8"))
    snip = snippets(cfg)
    base = (HERE / "templates/base.html").read_text(encoding="utf-8")
    if out.exists():
        shutil.rmtree(out)
    out.mkdir(parents=True)

    pages = sorted(p for p in (HERE / "pages").rglob("*.html"))
    built = []
    for p in pages:
        rel = p.relative_to(HERE / "pages").as_posix()
        raw = p.read_text(encoding="utf-8")
        m = META_RE.match(raw)
        if not m:
            sys.exit(f"{rel}: нет строки <!--meta {{...}} -->")
        meta = json.loads(m.group(1))
        body = raw[m.end():]
        if "{{privacy}}" in body:
            body = body.replace("{{privacy}}", privacy_body())
        depth = rel.count("/")
        root = "../" * depth if depth else "./"
        if rel == "404.html":
            root = "/"  # отдаётся сервером на любом пути — только абсолютные ссылки
        current = ' aria-current="page"'
        nav = "".join(
            f'<a href="{root}{href}"{current if key == meta.get("nav") else ""}>{label}</a>'
            for key, href, label in NAV
        )
        page = base.replace("{{body}}", body).replace("{{nav}}", nav)
        page = page.replace("{{page_title}}", html.escape(meta["title"]))
        page = page.replace("{{page_description}}", html.escape(meta["description"]))
        page = page.replace("{{canonical}}", cfg["site_url"].rstrip("/") + "/" + ("" if rel == "index.html" else rel))
        page = render(page, cfg, snip, root)
        dst = out / rel
        dst.parent.mkdir(parents=True, exist_ok=True)
        dst.write_text(page, encoding="utf-8")
        built.append(rel)

    shutil.copytree(HERE / "static", out / "static")
    for dst, src in ASSETS.items():
        (out / dst).parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(src, out / dst)

    site = cfg["site_url"].rstrip("/")
    urls = "".join(
        f"<url><loc>{site}/{'' if r == 'index.html' else r}</loc></url>" for r in built if r != "404.html"
    )
    (out / "sitemap.xml").write_text(
        f'<?xml version="1.0" encoding="UTF-8"?>\n<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">{urls}</urlset>\n',
        encoding="utf-8")
    # Файлы подтверждения владения доменом (почтовые постмастеры и т. п.):
    # site/root/* кладутся в корень сайта как есть.
    if (HERE / "root").is_dir():
        for f in (HERE / "root").iterdir():
            if f.is_file():
                shutil.copyfile(f, out / f.name)
    (out / "robots.txt").write_text(f"User-agent: *\nAllow: /\nSitemap: {site}/sitemap.xml\n", encoding="utf-8")
    return built


def check_links(out):
    """Каждая локальная ссылка и картинка ведёт на существующий файл/якорь;
    внешних скриптов, стилей и шрифтов нет."""
    bad = []
    ids = {}
    for f in out.rglob("*.html"):
        ids[f] = set(re.findall(r'\sid="([^"]+)"', f.read_text(encoding="utf-8")))
    for f in out.rglob("*.html"):
        text = f.read_text(encoding="utf-8")
        for tag in re.findall(r'<(script|img|iframe|source)\b[^>]*?\s(?:src|href)="(?:https?:)?//', text):
            bad.append(f"{f.relative_to(out)}: внешний ресурс в <{tag}>")
        for link in re.findall(r'<link\b[^>]*>', text):
            if 'rel="canonical"' not in link and re.search(r'href="(?:https?:)?//', link):
                bad.append(f"{f.relative_to(out)}: внешний ресурс {link}")
        for attr, url in re.findall(r'\s(href|src)="([^"]+)"', text):
            if re.match(r"^(https?:|mailto:|tel:|data:)", url):
                continue
            if url.startswith("/"):
                url = os.path.relpath(out / url.lstrip("/"), f.parent) + ("/" if url.endswith("/") else "")
            path, _, frag = url.partition("#")
            target = (f.parent / path).resolve() if path else f
            if target.is_dir():
                target = target / "index.html"
            if not target.exists():
                bad.append(f"{f.relative_to(out)}: нет файла для {url}")
            elif frag and target.suffix == ".html" and frag not in ids.get(target, set()):
                bad.append(f"{f.relative_to(out)}: нет якоря #{frag} в {target.relative_to(out)}")
    return bad


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", type=Path, default=DEFAULT_OUT)
    a = ap.parse_args()
    built = build(a.out)
    bad = check_links(a.out)
    for b in bad:
        print("ОШИБКА:", b, file=sys.stderr)
    print(f"собрано страниц: {len(built)} → {a.out}")
    sys.exit(1 if bad else 0)


if __name__ == "__main__":
    main()
