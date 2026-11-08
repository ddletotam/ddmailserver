package ru.letotam.ddmail

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.widthIn
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalConfiguration
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.unit.dp
import ru.letotam.ddmail.auth.Session

/** Заглушка после входа: кто, где и жива ли сессия. Список бесед — следующий шаг. */
@Composable
fun ConnectedScreen(
    session: Session,
    status: SessionStatus,
    onRelogin: () -> Unit,
    onLogout: () -> Unit,
    modifier: Modifier = Modifier,
) {
    val cfg = LocalConfiguration.current
    Box(modifier.fillMaxSize(), contentAlignment = Alignment.TopCenter) {
        Column(
            Modifier.widthIn(max = 460.dp).fillMaxWidth().padding(24.dp),
            verticalArrangement = Arrangement.spacedBy(14.dp),
        ) {
            Text(stringResource(R.string.connected_title), style = MaterialTheme.typography.headlineSmall)
            Text(stringResource(R.string.connected_as, session.username, session.serverUrl.removePrefix("https://")))
            val (text, bad) = when (status) {
                SessionStatus.Checking -> stringResource(R.string.session_checking) to false
                SessionStatus.Ok -> stringResource(R.string.session_ok) to false
                is SessionStatus.Offline -> stringResource(R.string.session_offline, status.reason) to true
                SessionStatus.Dead -> stringResource(R.string.session_dead) to true
            }
            Card(
                colors = CardDefaults.cardColors(
                    containerColor = if (bad) MaterialTheme.colorScheme.errorContainer
                    else MaterialTheme.colorScheme.primaryContainer,
                ),
                modifier = Modifier.fillMaxWidth(),
            ) {
                Text(text, Modifier.padding(12.dp))
            }
            Row(horizontalArrangement = Arrangement.spacedBy(12.dp)) {
                if (status == SessionStatus.Dead) {
                    Button(onClick = onRelogin) { Text(stringResource(R.string.action_relogin)) }
                }
                OutlinedButton(onClick = onLogout) { Text(stringResource(R.string.action_logout)) }
            }
            Text(
                "${BuildConfig.VERSION_NAME} · окно ${cfg.screenWidthDp} × ${cfg.screenHeightDp} dp",
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
    }
}
