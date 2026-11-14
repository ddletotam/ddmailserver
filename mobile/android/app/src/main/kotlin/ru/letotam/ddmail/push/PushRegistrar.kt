package ru.letotam.ddmail.push

import android.content.Context
import android.util.Log
import androidx.core.app.NotificationManagerCompat
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.flow.distinctUntilChanged
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.launch
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlinx.coroutines.withTimeoutOrNull
import ru.letotam.ddmail.BuildConfig
import ru.letotam.ddmail.DdmailApp
import ru.letotam.ddmail.api.SessionApi
import ru.letotam.ddmail.api.TokenSource
import ru.letotam.ddmail.auth.Session
import ru.letotam.ddmail.background.SyncWorker

/**
 * Push в приложении (контракт §7): токен у службы сборки, регистрация на
 * сервере ddmail-учётки, снятие при выходе и выключенных уведомлениях, синк
 * по пришедшему push. Внешние IMAP-учётки push не имеют — их почту приносит
 * фоновый синк (`SyncWorker`).
 *
 * В push ничего о письме нет (152-ФЗ): он только будит приложение, текст
 * уведомления (отправитель, тема) собирается из кэша после синка — тем же
 * [ru.letotam.ddmail.background.MailNotifier], что и в фоновом проходе.
 */
class PushRegistrar(private val app: DdmailApp, private val provider: PushProvider) {
    private val prefs = app.getSharedPreferences("push", Context.MODE_PRIVATE)
    private val lock = Mutex()

    val kind: String get() = provider.kind

    /** Из `Application.onCreate`: SDK, токен, слежение за тумблером уведомлений. */
    fun start() {
        if (!provider.configured) {
            Log.i(TAG, "push: ${provider.kind} not configured in this build — background sync only")
            return
        }
        try {
            provider.init(app)
        } catch (e: Exception) {
            Log.w(TAG, "push: ${provider.kind} init failed", e)
            return
        }
        // Первое значение приходит сразу — это и есть синк при запуске;
        // дальше — переключение «Уведомлений» в настройках.
        app.appScope.launch {
            app.settings.notifications.distinctUntilChanged().collect { sync() }
        }
        requestToken()
    }

    /** Токен может смениться — служба советует спрашивать при каждом запуске. */
    fun requestToken() {
        if (!provider.configured) return
        try {
            provider.requestToken(app)
        } catch (e: Exception) {
            Log.w(TAG, "push: ${provider.kind} token request failed", e)
        }
    }

    /** Служба выдала (новый) токен. */
    fun onNewToken(token: String?) {
        val t = token?.trim().orEmpty()
        if (t.isEmpty()) return
        if (prefs.getString(KEY_TOKEN, null) != t) {
            Log.i(TAG, "push: ${provider.kind} token ${t.take(8)}…")
            prefs.edit().putString(KEY_TOKEN, t).apply()
        }
        syncLater()
    }

    /** Пришёл push «есть новые письма» (или служба сообщила о потерянных). */
    fun onMessage() {
        SyncWorker.runNow(app)
    }

    /** Вход, выход, разрешение на уведомления — привести регистрацию к нужному виду. */
    fun syncLater() {
        if (!provider.configured) return
        app.appScope.launch { sync() }
    }

    /**
     * Регистрация — к нужному виду: у ddmail-учётки — текущий токен (или
     * никакой, если уведомления выключены или запрещены). Ошибка сети — не
     * беда: следующий запуск, вход или смена токена повторят.
     */
    suspend fun sync() = lock.withLock {
        if (!provider.configured) return@withLock
        app.ensureAccounts()
        val session = app.ddmailSession()
        val want = PushTarget.desired(session, provider.kind, prefs.getString(KEY_TOKEN, null), notificationsAllowed())
        val have = PushTarget.decode(prefs.getString(KEY_REGISTERED, null))
        if (have != null && have != want) {
            // Снять прежнюю можно только сессией той же учётки; другой учётки
            // уже нет (её выход снял регистрацию сам — [forget]).
            val removed = session == null || !have.belongsTo(session) ||
                attempt("unregister") { app.sessionApi(session).deletePushToken(have.kind, have.token) }
            if (removed || want != null) clearRegistered()
        }
        val current = PushTarget.decode(prefs.getString(KEY_REGISTERED, null))
        if (session != null && PushTarget.needsRegister(want, current, prefs.getLong(KEY_REGISTERED_AT, 0), System.currentTimeMillis())) {
            val w = want ?: return@withLock
            var delivery = false
            val ok = attempt("register") {
                delivery = app.sessionApi(session).registerPushToken(w.kind, w.token, BuildConfig.APPLICATION_ID)
            }
            if (ok) {
                prefs.edit()
                    .putString(KEY_REGISTERED, w.encode())
                    .putLong(KEY_REGISTERED_AT, System.currentTimeMillis())
                    .apply()
                Log.i(TAG, "push: registered ${w.kind} for ${session.accountKey}, server delivery=$delivery")
            }
        }
    }

    /**
     * Учётку ddmail удаляют: снять регистрацию её сессией, пока токен есть.
     * Запрос — мимо хранилища сессии: обмен токена не должен вернуть на диск
     * стёртую сессию. Не дошёл — сервер отправит push в никуда, служба
     * назовёт токен недействительным позже или телефон его проигнорирует.
     */
    fun forget(session: Session) {
        val have = PushTarget.decode(prefs.getString(KEY_REGISTERED, null)) ?: return
        if (!have.belongsTo(session)) return
        clearRegistered()
        val api = SessionApi(session.serverUrl, app.http, EphemeralTokens(session.token))
        app.appScope.launch {
            withTimeoutOrNull(FORGET_TIMEOUT_MS) {
                attempt("unregister on logout") { api.deletePushToken(have.kind, have.token) }
            }
        }
    }

    private fun clearRegistered() {
        prefs.edit().remove(KEY_REGISTERED).remove(KEY_REGISTERED_AT).apply()
    }

    /**
     * Уведомления включены в настройках приложения и разрешены системой
     * (Android 13+ — POST_NOTIFICATIONS; раньше — общий выключатель).
     * Без этого будить телефон незачем: показать всё равно нечего.
     */
    private suspend fun notificationsAllowed(): Boolean =
        app.settings.notifications.first() && NotificationManagerCompat.from(app).areNotificationsEnabled()

    private suspend fun attempt(what: String, block: suspend () -> Unit): Boolean = try {
        block()
        true
    } catch (e: CancellationException) {
        throw e
    } catch (e: Exception) {
        Log.w(TAG, "push: $what failed: ${e.message}")
        false
    }

    /** Токен без записи на диск — для снятия регистрации ушедшей учётки. */
    private class EphemeralTokens(@Volatile private var token: String) : TokenSource {
        override suspend fun current() = token
        override suspend fun update(token: String) {
            this.token = token
        }
    }

    private companion object {
        const val TAG = "ddmail"
        const val KEY_TOKEN = "device_token"
        const val KEY_REGISTERED = "registered"
        const val KEY_REGISTERED_AT = "registered_at"
        const val FORGET_TIMEOUT_MS = 15_000L
    }
}
