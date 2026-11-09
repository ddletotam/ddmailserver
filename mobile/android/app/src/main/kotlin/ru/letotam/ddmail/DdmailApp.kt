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

    val settings: ru.letotam.ddmail.settings.AppSettings by lazy { ru.letotam.ddmail.settings.AppSettings(this) }
    val notifier: ru.letotam.ddmail.background.MailNotifier by lazy { ru.letotam.ddmail.background.MailNotifier(this, settings) }

    /** Приложение на экране (MainActivity между onStart и onStop). */
    @Volatile var foreground = false

    /** Открытая сейчас беседа — о ней не уведомляем. */
    @Volatile var openConversation: String? = null

    /** Беседа, которую попросили открыть из уведомления. */
    val openRequests = kotlinx.coroutines.flow.MutableStateFlow<String?>(null)

    private var apiFor: Pair<String, SessionApi>? = null

    /**
     * API сессии — один на сессию: у почты, календаря и фоновых задач общий
     * токен, и обменивает его кто-то один.
     */
    @Synchronized
    fun sessionApi(session: Session): SessionApi {
        val key = "${session.serverUrl}|${session.username}"
        apiFor?.let { (k, api) -> if (k == key) return api }
        val tokens = object : TokenSource {
            @Volatile private var token = session.token
            override suspend fun current() = token
            override suspend fun update(token: String) {
                this.token = token
                sessions.updateToken(token)
            }
        }
        return SessionApi(session.serverUrl, http, tokens).also { apiFor = key to it }
    }

    /** Сброс при выходе: новый вход — новый токен. */
    @Synchronized
    fun forgetSessionApi() {
        apiFor = null
    }

    /** Синхронизация сессии; обменянный токен сразу ложится в хранилище. */
    fun engine(session: Session): SyncEngine {
        val host = session.serverUrl.removePrefix("https://").substringBefore(':').substringBefore('/')
        // Ключ учётки — как `account_key` десктопа (`логин@хост`): по нему
        // документ папок хранит ручные назначения, ключи обязаны совпадать.
        return SyncEngine("${session.username}@$host", sessionApi(session), db.dao())
    }

    val calendarPrefs: ru.letotam.ddmail.calendar.CalendarPrefs by lazy { ru.letotam.ddmail.calendar.CalendarPrefs(this) }
    val reminders: ru.letotam.ddmail.reminders.Reminders by lazy { ru.letotam.ddmail.reminders.Reminders(this) }

    /** Сервер сообщил об изменениях в календаре (WS `calendar_updated`). */
    val calendarChanged = kotlinx.coroutines.flow.MutableSharedFlow<Unit>(extraBufferCapacity = 1)

    /** Событие, которое попросили открыть из напоминания: (id события, начало вхождения). */
    val eventRequests = kotlinx.coroutines.flow.MutableStateFlow<Pair<Long, Long>?>(null)

    /** «Напомнить позже» из уведомления: (id события, начало вхождения). */
    val snoozeRequests = kotlinx.coroutines.flow.MutableStateFlow<Pair<Long, Long>?>(null)
}
