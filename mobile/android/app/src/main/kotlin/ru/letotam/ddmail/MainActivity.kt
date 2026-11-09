package ru.letotam.ddmail

import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.Scaffold
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.ui.Modifier
import androidx.lifecycle.viewmodel.compose.viewModel
import ru.letotam.ddmail.connect.ConnectScreen
import ru.letotam.ddmail.connect.ConnectViewModel
import ru.letotam.ddmail.mail.MailViewModel
import ru.letotam.ddmail.ui.DdmailTheme

class MainActivity : ComponentActivity() {
    private val app get() = application as DdmailApp

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        enableEdgeToEdge()
        handleOpen(intent)
        // Android 13+: уведомления — только с разрешения пользователя.
        if (android.os.Build.VERSION.SDK_INT >= 33 &&
            checkSelfPermission(android.Manifest.permission.POST_NOTIFICATIONS) != android.content.pm.PackageManager.PERMISSION_GRANTED
        ) {
            requestPermissions(arrayOf(android.Manifest.permission.POST_NOTIFICATIONS), 1)
        }
        val app = app
        setContent {
            DdmailTheme {
                val appVm = viewModel { AppViewModel(app) }
                val state by appVm.state.collectAsState()
                Scaffold { inner ->
                    val m = Modifier.padding(inner)
                    when (val s = state) {
                        AppState.Loading -> {}
                        is AppState.Connect -> {
                            // Ключ — поколение: повторный вход начинает форму заново.
                            val vm = viewModel(key = "connect-${s.generation}") {
                                ConnectViewModel(app, s.prefill, appVm::connected)
                            }
                            ConnectScreen(vm, m)
                        }
                        is AppState.Connected -> {
                            // Своя модель на каждую сессию: новый вход — новый движок.
                            val vm = viewModel(key = "mail-${s.session.serverUrl}-${s.session.token.hashCode()}") {
                                MailViewModel(app, app.engine(s.session), appVm::status)
                            }
                            androidx.compose.runtime.LaunchedEffect(Unit) {
                                ru.letotam.ddmail.background.SyncWorker.schedule(app)
                            }
                            AppShell(
                                app = app,
                                session = s.session,
                                status = s.status,
                                mailVm = vm,
                                onDead = { appVm.status(SessionStatus.Dead) },
                                onRelogin = { appVm.relogin(s.session) },
                                onLogout = appVm::logout,
                                modifier = m,
                            )
                        }
                    }
                }
            }
        }
    }

    override fun onNewIntent(intent: android.content.Intent) {
        super.onNewIntent(intent)
        handleOpen(intent)
    }

    override fun onStart() {
        super.onStart()
        app.foreground = true
    }

    override fun onStop() {
        super.onStop()
        app.foreground = false
    }

    /** Открыть беседу или событие из уведомления, «напомнить позже». */
    private fun handleOpen(intent: android.content.Intent?) {
        intent ?: return
        intent.getStringExtra(EXTRA_OPEN)?.let { app.openRequests.value = it }
        val occ = intent.getLongExtra(EXTRA_OCC_START, 0)
        intent.getLongExtra(EXTRA_EVENT_ID, 0).takeIf { it != 0L }?.let { app.eventRequests.value = it to occ }
        intent.getLongExtra(EXTRA_SNOOZE_EVENT, 0).takeIf { it != 0L }?.let { app.snoozeRequests.value = it to occ }
        // Повторная доставка того же интента (поворот, возврат) не должна снова открывать.
        intent.removeExtra(EXTRA_EVENT_ID)
        intent.removeExtra(EXTRA_SNOOZE_EVENT)
    }

    companion object {
        const val EXTRA_OPEN = "open_conversation"
        const val EXTRA_EVENT_ID = "event_id"
        const val EXTRA_OCC_START = "occ_start"
        const val EXTRA_SNOOZE_EVENT = "snooze_event"
    }
}
