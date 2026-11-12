package ru.letotam.ddmail

import android.app.Application
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import okhttp3.OkHttpClient
import ru.letotam.ddmail.account.ImapAccount
import ru.letotam.ddmail.api.DdmailApi
import ru.letotam.ddmail.api.SessionApi
import ru.letotam.ddmail.api.TokenSource
import ru.letotam.ddmail.auth.AccountStore
import ru.letotam.ddmail.auth.DnsMx
import ru.letotam.ddmail.auth.ServerDiscovery
import ru.letotam.ddmail.auth.Session
import ru.letotam.ddmail.auth.SessionStore
import ru.letotam.ddmail.db.CacheDb
import ru.letotam.ddmail.discover.Discovery
import ru.letotam.ddmail.discover.Probes
import ru.letotam.ddmail.sync.ImapEngine
import ru.letotam.ddmail.sync.MailHub
import ru.letotam.ddmail.sync.MailSource
import ru.letotam.ddmail.sync.SyncEngine
import java.io.File
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
            .connectTimeout(Discovery.CONNECT_MS.toLong(), TimeUnit.MILLISECONDS)
            .callTimeout(Discovery.PROBE_MS, TimeUnit.MILLISECONDS)
            .followRedirects(true)
            .build()
    }

    /** Фоновые задачи вне экранов: копия в «Отправленные», закрытие соединений. */
    val appScope = CoroutineScope(SupervisorJob() + Dispatchers.Default)

    val sessions: SessionStore by lazy { SessionStore(this) }

    /** Внешние IMAP-учётки и их пароли (зашифрованы Keystore). */
    val accounts: AccountStore by lazy { AccountStore(this) }

    val discovery: ServerDiscovery by lazy {
        ServerDiscovery(
            probe = { host -> DdmailApi.isDdmail(probeHttp, host) },
            mx = { domain -> DnsMx.lookup(domain) },
        )
    }

    /** Автонастройка внешних учёток (порт `discover.rs`): ddmail, autoconfig, IMAP, SMTP. */
    val mailDiscovery: Discovery by lazy {
        Discovery(
            object : Discovery.Net {
                override suspend fun mx(domain: String) = DnsMx.lookup(domain)
                override suspend fun isDdmail(host: String) = DdmailApi.isDdmail(probeHttp, host)
                override suspend fun get(url: String): String? = ru.letotam.ddmail.api.fetchText(probeHttp, url)
                override suspend fun probeImap(host: String, port: Int) = Probes.imap(host, port)
                override suspend fun probeSmtp(host: String, port: Int) = Probes.smtp(host, port)
            },
        )
    }

    fun api(serverUrl: String) = DdmailApi(serverUrl, http)

    val db: CacheDb by lazy { CacheDb.open(this) }

    val settings: ru.letotam.ddmail.settings.AppSettings by lazy { ru.letotam.ddmail.settings.AppSettings(this) }
    val notifier: ru.letotam.ddmail.background.MailNotifier by lazy { ru.letotam.ddmail.background.MailNotifier(this, settings) }

    /** Все учётки разом: сводный список бесед, правки — источнику беседы. */
    val hub: MailHub by lazy { MailHub(db.dao(), http) }

    /** Приложение на экране (MainActivity между onStart и onStop). */
    @Volatile var foreground = false

    /** Открытая сейчас беседа (`Conversation.ref`) — о ней не уведомляем. */
    @Volatile var openConversation: String? = null

    /** Беседа (`Conversation.ref`), которую попросили открыть из уведомления. */
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

    // ── Учётки ──

    private val accountsLock = Mutex()
    private var loaded = false
    @Volatile private var session: Session? = null

    /** Текущая учётка ddmail (не больше одной). */
    fun ddmailSession(): Session? = session

    /**
     * Источники почты по сохранённым учёткам. Живые источники переиспользуются
     * (у них соединения и замки синка): пересоздаётся только изменившаяся учётка.
     */
    suspend fun applyAccounts(ddmail: Session?, imap: List<ImapAccount>) = accountsLock.withLock {
        session = ddmail
        imapAccounts = imap
        loaded = true
        val old = hub.sources.value
        val list = mutableListOf<MailSource>()
        if (ddmail != null) {
            val reuse = old.filterIsInstance<SyncEngine>().firstOrNull { it.accountKey == ddmail.accountKey && apiFor?.first == "${ddmail.serverUrl}|${ddmail.username}" }
            list += reuse ?: SyncEngine(ddmail.accountKey, sessionApi(ddmail), db.dao(), title = "${ddmail.username} · ${ddmail.serverUrl.removePrefix("https://")}")
        }
        for (a in imap) {
            val reuse = old.filterIsInstance<ImapEngine>().firstOrNull { it.account == a }
            list += reuse ?: ImapEngine(a, { accounts.password(a.key) }, db.dao(), appScope, File(cacheDir, "imap-raw"))
        }
        hub.setSources(list)
    }

    @Volatile private var imapAccounts: List<ImapAccount> = emptyList()
    private var pimFor: Pair<String, ru.letotam.ddmail.sync.PimHub>? = null

    /** CalDAV/CardDAV: ответа ждём дольше, чем пробы автонастройки, но не вечно. */
    val davHttp: OkHttpClient by lazy {
        http.newBuilder().connectTimeout(8, TimeUnit.SECONDS).readTimeout(30, TimeUnit.SECONDS).build()
    }

    /**
     * Календарь и контакты: сервер ddmail вместе с CalDAV/CardDAV внешних
     * учёток (mobile-external-accounts §8). `null` — источников нет вовсе.
     */
    @Synchronized
    fun pim(): ru.letotam.ddmail.api.PimApi? {
        val s = session
        val dav = imapAccounts.filter { it.caldavUrl.isNotBlank() || it.carddavUrl.isNotBlank() }
        if (s == null && dav.isEmpty()) return null
        val key = pimKey()
        pimFor?.let { (k, p) -> if (k == key) return p }
        val p = ru.letotam.ddmail.sync.PimHub(
            ddmail = s?.let { sessionApi(it) },
            accounts = dav,
            password = { k -> accounts.password(k) },
            http = davHttp,
            onAuthFailed = { k -> hub.setStatus(k, ru.letotam.ddmail.sync.AccountStatus.NeedsLogin) },
        )
        pimFor = key to p
        return p
    }

    /** Меняется вместе с набором источников календаря — по нему пересоздаются экраны. */
    fun pimKey(): String = (session?.let { "${it.serverUrl}|${it.username}|${it.token.hashCode()}" } ?: "-") + "|" +
        imapAccounts.filter { it.caldavUrl.isNotBlank() || it.carddavUrl.isNotBlank() }.joinToString { "${it.key}:${it.caldavUrl}:${it.carddavUrl}" }.hashCode()

    /** Для фона: процесс поднялся без экрана — учётки с диска. */
    suspend fun ensureAccounts() {
        if (loaded) return
        applyAccounts(sessions.load(), accounts.list())
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
