package ru.letotam.ddmail

import android.app.Application
import okhttp3.OkHttpClient
import ru.letotam.ddmail.api.DdmailApi
import ru.letotam.ddmail.api.SessionApi
import ru.letotam.ddmail.api.TokenSource
import ru.letotam.ddmail.auth.Session
import ru.letotam.ddmail.db.CacheDb
import ru.letotam.ddmail.sync.SyncEngine
import ru.letotam.ddmail.auth.DnsMx
import ru.letotam.ddmail.auth.ServerDiscovery
import ru.letotam.ddmail.auth.SessionStore
import java.util.concurrent.TimeUnit

/** Корень зависимостей приложения — вручную, без DI-фреймворка, пока их мало. */
class DdmailApp : Application() {
    val http: OkHttpClient by lazy {
        OkHttpClient.Builder()
            .connectTimeout(15, TimeUnit.SECONDS)
            .readTimeout(30, TimeUnit.SECONDS)
            .build()
    }

    /** Для поиска сервера: закрытый файрволом порт молчит до таймаута — ждём его коротко. */
    private val probeHttp: OkHttpClient by lazy {
        http.newBuilder()
            .connectTimeout(3, TimeUnit.SECONDS)
            .callTimeout(8, TimeUnit.SECONDS)
            .build()
    }

    val sessions: SessionStore by lazy { SessionStore(this) }

    val discovery: ServerDiscovery by lazy {
        ServerDiscovery(
            probe = { host -> DdmailApi.isDdmail(probeHttp, host) },
            mx = { domain -> DnsMx.lookup(domain) },
        )
    }

    fun api(serverUrl: String) = DdmailApi(serverUrl, http)

    val db: CacheDb by lazy { CacheDb.open(this) }

    /** Синхронизация сессии; обменянный токен сразу ложится в хранилище. */
    fun engine(session: Session): SyncEngine {
        val tokens = object : TokenSource {
            @Volatile private var token = session.token
            override suspend fun current() = token
            override suspend fun update(token: String) {
                this.token = token
                sessions.updateToken(token)
            }
        }
        val host = session.serverUrl.removePrefix("https://").substringBefore(':').substringBefore('/')
        // Ключ учётки — как `account_key` десктопа (`логин@хост`): по нему
        // документ папок хранит ручные назначения, ключи обязаны совпадать.
        return SyncEngine("${session.username}@$host", SessionApi(session.serverUrl, http, tokens), db.dao())
    }
}
