"""Иконка ddmail для всех платформ из одного векторного источника.

Облачко чата с конвертом (зелёный градиент, наклон по часовой). На крупных
размерах (домашний экран, App Store, Mac от 128 pt, лаунчер Android) — кружок
с тремя точками у правого верхнего угла; на мелких (настройки, Spotlight,
уведомления, Mac 16/32) — без него, чтобы не превращался в пятно.

Запуск (вне дерева проекта, зависимости в одноразовом venv):
    uv venv /tmp/iconenv && uv pip install -p /tmp/iconenv/bin/python resvg-py pillow
    /tmp/iconenv/bin/python -I mobile/shared/icon/make_icons.py
"""
import io
import json
import os
from pathlib import Path

import resvg_py
from PIL import Image

ROOT = Path(__file__).resolve().parents[2]  # mobile/
IOS = ROOT / "ios/App/Assets.xcassets/AppIcon.appiconset"
RES = ROOT / "android/app/src/main/res"
STORE = ROOT / "store-assets"

G_LIGHT, G_DEEP, LINE, TINT = "#4fe0b0", "#00a37a", "#00a37a", "#7fe6c4"
X, Y, W, H, R = 200, 215, 624, 540, 190  # облачко в поле 1024
TILT = 4  # наклон облачка по часовой


def bubble_paths():
    x, y, w, h, r = X, Y, W, H, R
    rect = (f"M{x+r},{y} H{x+w-r} A{r},{r} 0 0 1 {x+w},{y+r} V{y+h-r} A{r},{r} 0 0 1 {x+w-r},{y+h} "
            f"H{x+r} A{r},{r} 0 0 1 {x},{y+h-r} V{y+r} A{r},{r} 0 0 1 {x+r},{y} Z")
    # Короткий хвостик поверх нижнего левого угла, остриём вниз-влево.
    tail = (f"M{x},{y+h-170} C{x},{y+h-70} {x+2},{y+h+20} {x+6},{y+h+70} "
            f"C{x+60},{y+h+50} {x+120},{y+h+12} {x+220},{y+h-2} L{x+200},{y+h-160} Z")
    return rect, tail


def envelope():
    cx, cy, w, h = X + W / 2 + 12, Y + H / 2 - 12, 420, 300
    x, y = cx - w / 2, cy - h / 2
    vy = y + h * 0.56
    body = (f'<rect x="{x}" y="{y}" width="{w}" height="{h}" rx="38" fill="url(#env)"/>'
            f'<path d="M{x+20},{y+22} L{cx},{vy} L{x+w-20},{y+22} Z" fill="{TINT}" opacity="0.18"/>'
            f'<path d="M{x+30},{y+32} L{cx},{vy} L{x+w-30},{y+32}" fill="none" stroke="{LINE}" '
            f'stroke-width="20" stroke-linecap="round" stroke-linejoin="round" opacity="0.6"/>')
    return body, cx, cy


def badge():
    # Кружок «…» у правого верхнего угла облачка, частично за его краем.
    cx, cy, r = X + W - 34, Y + 26, 122
    dots = "".join(f'<circle cx="{cx + dx}" cy="{cy}" r="16" fill="{G_DEEP}"/>' for dx in (-50, 0, 50))
    return (f'<g filter="url(#bs)"><circle cx="{cx}" cy="{cy}" r="{r}" fill="url(#badge)"/></g>{dots}')


def art(dots: bool) -> str:
    """Облачко + конверт (+ кружок) в поле 1024, без фона."""
    rect, tail = bubble_paths()
    env, ecx, ecy = envelope()
    return (f'<g transform="rotate({TILT} 512 512)">'
            f'<g filter="url(#s)"><path d="{rect}" fill="url(#g)"/><path d="{tail}" fill="url(#g)"/></g>'
            f'<g transform="rotate(1 {ecx} {ecy})" filter="url(#es)">{env}</g>'
            f'{badge() if dots else ""}</g>')


DEFS = f'''<defs>
<linearGradient id="bg" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="#ffffff"/><stop offset="1" stop-color="#eef7f3"/></linearGradient>
<linearGradient id="g" gradientUnits="userSpaceOnUse" x1="260" y1="200" x2="780" y2="820"><stop offset="0" stop-color="{G_LIGHT}"/><stop offset="1" stop-color="{G_DEEP}"/></linearGradient>
<linearGradient id="env" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="#ffffff"/><stop offset="1" stop-color="#f1faf6"/></linearGradient>
<linearGradient id="badge" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="#ffffff"/><stop offset="1" stop-color="#eef7f3"/></linearGradient>
<filter id="s" x="-20%" y="-20%" width="140%" height="150%"><feDropShadow dx="0" dy="20" stdDeviation="28" flood-color="#00704f" flood-opacity="0.22"/></filter>
<filter id="es" x="-20%" y="-20%" width="140%" height="150%"><feDropShadow dx="0" dy="8" stdDeviation="12" flood-color="#00573d" flood-opacity="0.18"/></filter>
<filter id="bs" x="-40%" y="-40%" width="180%" height="190%"><feDropShadow dx="0" dy="10" stdDeviation="16" flood-color="#00573d" flood-opacity="0.20"/></filter>
</defs>'''


def svg(body: str, background: bool = True) -> str:
    bg = '<rect width="1024" height="1024" fill="url(#bg)"/>' if background else ""
    return (f'<svg xmlns="http://www.w3.org/2000/svg" width="1024" height="1024" viewBox="0 0 1024 1024">'
            f'{DEFS}{bg}{body}</svg>')


def render(s: str, px: int, mode="RGB") -> Image.Image:
    png = bytes(resvg_py.svg_to_bytes(svg_string=s, width=px, height=px))
    return Image.open(io.BytesIO(png)).convert(mode)


def ios_and_mac():
    full, small = svg(art(True)), svg(art(False))
    os.makedirs(IOS, exist_ok=True)
    for f in IOS.glob("*.png"):
        f.unlink()
    images, made = [], {}

    def file_for(px: int, dots: bool) -> str:
        name = f"icon-{px}{'' if dots else '-plain'}.png"
        if name not in made:
            render(full if dots else small, px).save(IOS / name)
            made[name] = True
        return name

    # (idiom, size pt, scales, с точками?) — точки на домашнем экране и выше.
    table = [
        ("iphone", 20, (2, 3), False), ("iphone", 29, (2, 3), False),
        ("iphone", 40, (2, 3), False), ("iphone", 60, (2, 3), True),
        ("ipad", 20, (1, 2), False), ("ipad", 29, (1, 2), False),
        ("ipad", 40, (1, 2), False), ("ipad", 76, (1, 2), True), ("ipad", 83.5, (2,), True),
        ("ios-marketing", 1024, (1,), True),
        ("mac", 16, (1, 2), False), ("mac", 32, (1, 2), False),
        ("mac", 128, (1, 2), True), ("mac", 256, (1, 2), True), ("mac", 512, (1, 2), True),
    ]
    for idiom, pt, scales, dots in table:
        for sc in scales:
            px = round(pt * sc)
            size = f"{pt:g}x{pt:g}"
            images.append({"filename": file_for(px, dots), "idiom": idiom, "scale": f"{sc}x", "size": size})
    json.dump({"images": images, "info": {"author": "xcode", "version": 1}},
              open(IOS / "Contents.json", "w"), indent=2)
    return full


def android():
    # Адаптивная иконка 108dp: видимая область — центральные 72dp, гарантированно
    # не режется круг 66dp. Рисунок вписываем в ~64dp (масштаб 0.62 от поля 1024).
    scale = 0.70
    off = 512 * (1 - scale)
    fg = svg(f'<g transform="translate({off} {off}) scale({scale})">{art(True)}</g>', background=False)
    bgs = svg("")
    densities = {"mdpi": 108, "hdpi": 162, "xhdpi": 216, "xxhdpi": 324, "xxxhdpi": 432}
    for d, px in densities.items():
        out = RES / f"mipmap-{d}"
        os.makedirs(out, exist_ok=True)
        render(fg, px, "RGBA").save(out / "ic_launcher_foreground.png")
        render(bgs, px).save(out / "ic_launcher_background.png")
    # Одноцветная (тематические иконки Android 13+): силуэт облачка с прорезью конверта, без точек.
    rect, tail = bubble_paths()
    env, ecx, ecy = envelope()
    x0, y0 = ecx - 210, ecy - 150
    mono = svg(
        f'<g transform="translate({off} {off}) scale({scale})"><g transform="rotate({TILT} 512 512)">'
        f'<mask id="m"><rect width="1024" height="1024" fill="#fff"/>'
        f'<g transform="rotate(1 {ecx} {ecy})"><rect x="{x0}" y="{y0}" width="420" height="300" rx="38" fill="#000"/></g></mask>'
        f'<g mask="url(#m)"><path d="{rect}" fill="#fff"/><path d="{tail}" fill="#fff"/></g>'
        f'<g transform="rotate(1 {ecx} {ecy})"><path d="M{x0+30},{y0+32} L{ecx},{y0+168} L{x0+390},{y0+32}" '
        f'fill="none" stroke="#fff" stroke-width="26" stroke-linecap="round" stroke-linejoin="round"/></g>'
        f'</g></g>', background=False)
    for d, px in densities.items():
        render(mono, px, "RGBA").save(RES / f"mipmap-{d}" / "ic_launcher_monochrome.png")


def store(full: str):
    os.makedirs(STORE, exist_ok=True)
    render(full, 1024).save(STORE / "icon-1024.png")          # App Store
    render(full, 512).save(STORE / "icon-512.png")            # RuStore
    render(full, 216).save(STORE / "icon-216.png")            # Huawei AppGallery


if __name__ == "__main__":
    full = ios_and_mac()
    android()
    store(full)
    print("ok")
