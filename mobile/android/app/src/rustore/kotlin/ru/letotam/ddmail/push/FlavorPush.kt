package ru.letotam.ddmail.push

import android.app.Application
import android.util.Log
import ru.letotam.ddmail.BuildConfig
import ru.letotam.ddmail.DdmailApp
import ru.rustore.sdk.pushclient.RuStorePushClient
import ru.rustore.sdk.pushclient.messaging.exception.RuStorePushClientException
import ru.rustore.sdk.pushclient.messaging.model.RemoteMessage
import ru.rustore.sdk.pushclient.messaging.service.RuStoreMessagingService

/**
 * Сборка `rustore` — RuStore Push (контракт §1, §7). Project id — из
 * `rustore.properties` станции (Gradle кладёт его в `BuildConfig`); нет —
 * сборка без push. Доставляет приложение RuStore (или другое приложение VK,
 * взявшее его роль); нет ни одного — токена нет, работает фоновый синк.
 */
object FlavorPush : PushProvider {
    override val kind = "rustore"

    override val configured: Boolean get() = BuildConfig.RUSTORE_PROJECT_ID.isNotBlank()

    override fun init(app: Application) {
        RuStorePushClient.init(application = app, projectId = BuildConfig.RUSTORE_PROJECT_ID)
    }

    override fun requestToken(app: Application) {
        val ddmail = app as DdmailApp
        RuStorePushClient.getToken()
            .addOnSuccessListener { token -> ddmail.push.onNewToken(token) }
            .addOnFailureListener { e -> Log.w("ddmail", "push: rustore getToken failed: ${e.message}") }
    }
}

/** Служба сообщений RuStore: новый токен и push «есть новые письма». */
class RuStorePushService : RuStoreMessagingService() {
    override fun onNewToken(token: String) {
        (application as DdmailApp).push.onNewToken(token)
    }

    /**
     * В push ничего о письме нет (только data) — он только будит: синк, и
     * уведомление с отправителем и темой собирается из кэша.
     */
    override fun onMessageReceived(message: RemoteMessage) {
        (application as DdmailApp).push.onMessage()
    }

    /** Служба выбросила недоставленные push — синк заберёт пропущенное. */
    override fun onDeletedMessages() {
        (application as DdmailApp).push.onMessage()
    }

    override fun onError(errors: List<RuStorePushClientException>) {
        errors.forEach { Log.w("ddmail", "push: rustore error: ${it.message}") }
    }
}
