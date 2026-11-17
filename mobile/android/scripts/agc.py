import json, os, requests
D=os.path.expanduser('~/.config/ddmail-android')
C=json.load(open(f'{D}/agc-apiclient.json'))
APPID=json.load(open(f'{D}/agconnect-services.json'))['client']['app_id']
B='https://connect-api.cloud.huawei.com/api'
def tok():
    r=requests.post(B+'/oauth2/v1/token',json={'grant_type':'client_credentials','client_id':C['client_id'],'client_secret':C['client_secret']},timeout=20)
    r.raise_for_status(); j=r.json()
    if 'access_token' not in j: raise SystemExit(j)
    return j['access_token']
T=None
def H():
    global T
    T=T or tok()
    return {'Authorization':'Bearer '+T,'client_id':C['client_id']}
def get(p,**q): r=requests.get(B+p,headers=H(),params=q,timeout=30); return r.status_code,r.json()
def put(p,body,**q): r=requests.put(B+p,headers=H(),params=q,json=body,timeout=30); return r.status_code,r.json()
def post(p,body=None,**q): r=requests.post(B+p,headers=H(),params=q,json=body,timeout=60); return r.status_code,r.json()
if __name__=='__main__':
    s,j=get('/publish/v2/app-info',appId=APPID,lang='ru-RU')
    print(s, json.dumps(j,ensure_ascii=False)[:1500])
