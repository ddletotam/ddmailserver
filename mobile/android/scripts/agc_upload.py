import agc, os, requests, json, sys, time
APK=os.path.expanduser('~/.cache/builds/gradle/ddmail-android/app/outputs/apk/hms/release/app-hms-release.apk')
name=f'ddmail-{sys.argv[1]}.apk'; size=os.path.getsize(APK)
s,j=agc.get('/publish/v2/upload-url/for-obs',appId=agc.APPID,fileName=name,contentLength=size)
print('upload-url', s, j.get('ret'))
u=j['urlInfo']
with open(APK,'rb') as f:
    r=requests.request(u['method'],u['url'],headers=u['headers'],data=f,timeout=600)
print('put', r.status_code, r.text[:200])
s,j=agc.put('/publish/v2/app-file-info',{'fileType':5,'files':[{'fileName':name,'fileDestUrl':u['objectId']}]},appId=agc.APPID)
print('file-info', s, json.dumps(j,ensure_ascii=False)[:400])
