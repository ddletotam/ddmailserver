package ru.letotam.ddmail

import android.app.Application
import okhttp3.OkHttpClient
import ru.letotam.ddmail.api.DdmailApi
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
}
