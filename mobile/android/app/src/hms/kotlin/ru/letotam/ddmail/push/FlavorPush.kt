package ru.letotam.ddmail.push

import android.app.Application
import android.os.Bundle
import android.util.Log
import com.huawei.hms.aaid.HmsInstanceId
import com.huawei.hms.push.HmsMessageService
import com.huawei.hms.push.HmsMessaging
import com.huawei.hms.push.RemoteMessage
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import ru.letotam.ddmail.BuildConfig
import ru.letotam.ddmail.DdmailApp

/**
 * Сборка `hms` — Huawei Push Kit (контракт §1, §7). App ID — из
 * `agconnect-services.json` станции (Gradle кладёт его в `BuildConfig` и в
 * assets); нет файла — сборка без push.
 */
object FlavorPush : PushProvider {
    override val kind = "hms"

    override val configured: Boolean get() = BuildConfig.HMS_APP_ID.isNotBlank()

    /** Push Kit поднимается сам; токен — только по [requestToken] (автоинициализация выключена в манифесте). */
    override fun init(app: Application) {}

    /**
     * Токен — блокирующим вызовом, не на главном потоке. На EMUI 10+ он
     * возвращается сразу; на старых — пустой, и токен приходит в
     * [HmsPushService.onNewToken].
     */
    override fun requestToken(app: Application) {
        val ddmail = app as DdmailApp
        ddmail.appScope.launch(Dispatchers.IO) {
            try {
                val token = HmsInstanceId.getInstance(app).getToken(BuildConfig.HMS_APP_ID, HmsMessaging.DEFAULT_TOKEN_SCOPE)
                if (!token.isNullOrBlank()) ddmail.push.onNewToken(token)
            } catch (e: Exception) {
                Log.w("ddmail", "push: hms getToken failed: ${e.message}")
            }
        }
    }
}

/** Служба сообщений HMS: новый токен и push «есть новые письма». */
class HmsPushService : HmsMessageService() {
    override fun onNewToken(token: String?, bundle: Bundle?) {
        (application as DdmailApp).push.onNewToken(token)
    }

    override fun onNewToken(token: String?) {
        (application as DdmailApp).push.onNewToken(token)
    }

    /**
     * В push ничего о письме нет — он только будит: синк, и уведомление с
     * отправителем и темой собирается из кэша.
     */
    override fun onMessageReceived(message: RemoteMessage?) {
        (application as DdmailApp).push.onMessage()
    }

    override fun onTokenError(e: Exception?) {
        Log.w("ddmail", "push: hms token error: ${e?.message}")
    }
}
