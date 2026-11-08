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
import ru.letotam.ddmail.mail.MailScreen
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
                            MailScreen(
                                vm = vm,
                                session = s.session,
                                status = s.status,
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

    /** Открыть беседу из уведомления. */
    private fun handleOpen(intent: android.content.Intent?) {
        intent?.getStringExtra(EXTRA_OPEN)?.let { app.openRequests.value = it }
    }

    companion object {
        const val EXTRA_OPEN = "open_conversation"
    }
}
