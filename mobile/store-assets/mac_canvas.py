"""Снимок окна Mac-приложения → скриншот App Store 2880×1800: окно по центру
на светло-зелёном градиенте с мягкой тенью. Запуск: python -I mac_canvas.py окно.png out.png
"""
import sys
from PIL import Image
src,dst=sys.argv[1],sys.argv[2]
W,H=2880,1800
c=Image.new('RGB',(W,H))
top,bot=(0xee,0xf7,0xf3),(0xcf,0xec,0xe0)
for y in range(H):
    t=y/(H-1); c.paste(tuple(int(a+(b-a)*t) for a,b in zip(top,bot)),(0,y,W,y+1))
im=Image.open(src).convert('RGBA')
s=min((W-240)/im.width,(H-200)/im.height)
im=im.resize((round(im.width*s),round(im.height*s)),Image.LANCZOS)
sh=Image.new('RGBA',im.size,(0,60,40,70))
from PIL import ImageFilter
pad=60; shadow=Image.new('RGBA',(im.width+2*pad,im.height+2*pad),(0,0,0,0))
shadow.paste(sh,(pad,pad),im.split()[3]); shadow=shadow.filter(ImageFilter.GaussianBlur(28))
x,y=(W-im.width)//2,(H-im.height)//2
c.paste(shadow,(x-pad,y-pad+18),shadow); c.paste(im,(x,y),im)
c.save(dst)
