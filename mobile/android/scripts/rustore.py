import os, time, base64, json, requests
from datetime import datetime, timezone
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import padding
E={}
for l in open(os.path.expanduser('~/.config/ddmail-android/rustore-api.env')):
    if '=' in l: k,v=l.rstrip('\n').split('=',1); E[k]=v
B='https://public-api.rustore.ru'
def tok():
    kid=E['RUSTORE_KEY_ID']; ts=datetime.now(timezone.utc).astimezone().isoformat(timespec='milliseconds')
    key=serialization.load_der_private_key(base64.b64decode(E['RUSTORE_PRIVATE_KEY']),None)
    sig=base64.b64encode(key.sign((kid+ts).encode(),padding.PKCS1v15(),hashes.SHA512())).decode()
    r=requests.post(B+'/public/auth/',json={'keyId':kid,'timestamp':ts,'signature':sig},timeout=20)
    j=r.json()
    if r.status_code!=200 or not j.get('body'): raise SystemExit(f'auth {r.status_code} {r.text[:300]}')
    return j['body']['jwe']
T=None
def H():
    global T; T=T or tok(); return {'Public-Token':T}
def get(p,**q): r=requests.get(B+p,headers=H(),params=q,timeout=30); return r.status_code, r.json()
def post(p,body=None,**q): r=requests.post(B+p,headers=H(),params=q,json=body,timeout=60); return r.status_code, r.json()
if __name__=='__main__':
    s,j=get('/public/v1/application')
    print(s, json.dumps(j,ensure_ascii=False,indent=1)[:3000])
