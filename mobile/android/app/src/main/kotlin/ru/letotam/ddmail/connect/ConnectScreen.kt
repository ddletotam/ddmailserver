package ru.letotam.ddmail.connect

import androidx.activity.compose.BackHandler
import androidx.compose.animation.core.animateFloatAsState
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.text.KeyboardActions
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.Checkbox
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.remember
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.focus.FocusRequester
import androidx.compose.ui.focus.focusRequester
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.input.ImeAction
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.input.PasswordVisualTransformation
import androidx.compose.ui.unit.dp
import ru.letotam.ddmail.R
import ru.letotam.ddmail.ui.Msg

/**
 * Подключение (контракт §3; mobile-external-accounts §1): первый запуск и
 * «Добавить учётку» — одно окно. Колонка ограничена по ширине: на разложенном
 * Mate XT (до 1107 dp) поля во всю ширину читались бы плохо. [onCancel] —
 * учётки уже есть, можно вернуться к почте.
 */
@Composable
fun ConnectScreen(vm: ConnectViewModel, modifier: Modifier = Modifier, onCancel: (() -> Unit)? = null) {
    val s by vm.state.collectAsState()
    BackHandler(enabled = !s.busy && (s.step != ConnectStep.Email || onCancel != null)) {
        if (!vm.back()) onCancel?.invoke()
    }

    Box(modifier.fillMaxSize().imePadding(), contentAlignment = Alignment.TopCenter) {
        Column(
            Modifier
                .widthIn(max = 460.dp)
                .fillMaxWidth()
                .verticalScroll(rememberScrollState())
                .padding(horizontal = 24.dp, vertical = 32.dp),
            verticalArrangement = Arrangement.spacedBy(14.dp),
        ) {
            Text(
                when (s.step) {
                    ConnectStep.Email -> stringResource(R.string.connect_title)
                    ConnectStep.Searching -> stringResource(R.string.searching_title)
                    ConnectStep.Found -> stringResource(R.string.found_title)
                    ConnectStep.Manual -> stringResource(R.string.manual_title)
                    ConnectStep.Imap -> stringResource(if (s.editingKey != null) R.string.imap_edit_title else R.string.imap_title)
                },
                style = MaterialTheme.typography.headlineSmall,
            )
            when (s.step) {
                ConnectStep.Email -> EmailStep(s, vm)
                ConnectStep.Searching -> SearchingStep(s)
                ConnectStep.Found, ConnectStep.Manual -> LoginStep(s, vm)
                ConnectStep.Imap -> ImapStep(s, vm)
            }
            s.notice?.let { Notice(it) }
            s.error?.let {
                Text(it.text(), color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodyMedium)
            }
            Buttons(s, vm, onCancel)
        }
    }
}

@Composable
private fun EmailStep(s: ConnectState, vm: ConnectViewModel) {
    val focus = remember { FocusRequester() }
    LaunchedEffect(Unit) { focus.requestFocus() }
    Text(stringResource(R.string.connect_email_hint), style = MaterialTheme.typography.bodyMedium)
    OutlinedTextField(
        value = s.email,
        onValueChange = vm::setEmail,
        label = { Text(stringResource(R.string.field_email)) },
        singleLine = true,
        keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Email, imeAction = ImeAction.Search),
        keyboardActions = KeyboardActions(onSearch = { vm.findServer() }),
        modifier = Modifier.fillMaxWidth().focusRequester(focus),
    )
}

@Composable
private fun SearchingStep(s: ConnectState) {
    val p = s.progress
    val fraction by animateFloatAsState(
        if (p == null || p.total == 0) 0f else p.done.toFloat() / p.total,
        label = "discovery",
    )
    Text(p?.stage?.ifEmpty { null } ?: s.email, style = MaterialTheme.typography.bodyMedium)
    LinearProgressIndicator(progress = { fraction }, modifier = Modifier.fillMaxWidth())
    if (p != null) {
        Text(
            stringResource(R.string.searching_count, p.done, p.total),
            style = MaterialTheme.typography.bodySmall,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
    }
}

@Composable
private fun LoginStep(s: ConnectState, vm: ConnectViewModel) {
    val focus = remember { FocusRequester() }
    // Нашли сервер и подставили логин — осталось ввести пароль.
    LaunchedEffect(s.step) { focus.requestFocus() }
    OutlinedTextField(
        value = s.server,
        onValueChange = vm::setServer,
        label = { Text(stringResource(R.string.field_server)) },
        singleLine = true,
        enabled = !s.busy,
        readOnly = s.step == ConnectStep.Found,
        placeholder = { Text("mail.example.ru") },
        keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Uri, imeAction = ImeAction.Next),
        modifier = Modifier.fillMaxWidth(),
    )
    OutlinedTextField(
        value = s.username,
        onValueChange = vm::setUsername,
        label = { Text(stringResource(R.string.field_login)) },
        singleLine = true,
        enabled = !s.busy,
        keyboardOptions = KeyboardOptions(imeAction = ImeAction.Next),
        modifier = Modifier.fillMaxWidth(),
    )
    PasswordField(s, vm, focus)
    Text(
        stringResource(R.string.login_note),
        style = MaterialTheme.typography.bodySmall,
        color = MaterialTheme.colorScheme.onSurfaceVariant,
    )
}

@Composable
private fun PasswordField(s: ConnectState, vm: ConnectViewModel, focus: FocusRequester) {
    OutlinedTextField(
        value = s.password,
        onValueChange = vm::setPassword,
        label = { Text(stringResource(R.string.field_password)) },
        singleLine = true,
        enabled = !s.busy,
        visualTransformation = PasswordVisualTransformation(),
        keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Password, imeAction = ImeAction.Done),
        keyboardActions = KeyboardActions(onDone = { vm.login() }),
        modifier = Modifier.fillMaxWidth().focusRequester(focus),
    )
}

/** IMAP/SMTP: найденное и пароль; «Изменить настройки» или ручной режим — все поля. */
@Composable
private fun ImapStep(s: ConnectState, vm: ConnectViewModel) {
    val focus = remember { FocusRequester() }
    LaunchedEffect(s.step) { if (!s.showServers) runCatching { focus.requestFocus() } }
    @Composable
    fun field(value: String, label: Int, kb: KeyboardType = KeyboardType.Text, mod: Modifier = Modifier.fillMaxWidth(), set: (ConnectState, String) -> ConnectState) =
        OutlinedTextField(
            value = value,
            onValueChange = { v -> vm.edit { set(it, v) } },
            label = { Text(stringResource(label)) },
            singleLine = true,
            enabled = !s.busy,
            keyboardOptions = KeyboardOptions(keyboardType = kb, imeAction = ImeAction.Next),
            modifier = mod,
        )
    field(s.email, R.string.field_email, KeyboardType.Email) { st, v -> st.copy(email = v) }
    if (s.showServers) {
        field(s.name, R.string.field_name) { st, v -> st.copy(name = v) }
        Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            field(s.imapHost, R.string.field_imap_host, KeyboardType.Uri, Modifier.weight(1f)) { st, v -> st.copy(imapHost = v) }
            field(s.imapPort, R.string.field_port, KeyboardType.Number, Modifier.width(96.dp)) { st, v -> st.copy(imapPort = v.filter { it.isDigit() }) }
        }
        Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            field(s.smtpHost, R.string.field_smtp_host, KeyboardType.Uri, Modifier.weight(1f)) { st, v -> st.copy(smtpHost = v) }
            field(s.smtpPort, R.string.field_port, KeyboardType.Number, Modifier.width(96.dp)) { st, v -> st.copy(smtpPort = v.filter { it.isDigit() }) }
        }
        Row(verticalAlignment = Alignment.CenterVertically) {
            Checkbox(checked = !s.smtpAuth, onCheckedChange = { v -> vm.edit { it.copy(smtpAuth = !v) } }, enabled = !s.busy)
            Text(stringResource(R.string.smtp_no_auth), style = MaterialTheme.typography.bodyMedium)
        }
        field(s.imapLogin, R.string.field_imap_login, KeyboardType.Email) { st, v -> st.copy(imapLogin = v) }
        if (s.editingKey != null) {
            field(s.extraAddresses, R.string.field_extra_addresses, KeyboardType.Email) { st, v -> st.copy(extraAddresses = v) }
            field(s.caldavUrl, R.string.field_caldav, KeyboardType.Uri) { st, v -> st.copy(caldavUrl = v) }
            field(s.carddavUrl, R.string.field_carddav, KeyboardType.Uri) { st, v -> st.copy(carddavUrl = v) }
        }
        Text(stringResource(R.string.imap_note), style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
    } else {
        Card(Modifier.fillMaxWidth()) {
            Column(Modifier.padding(12.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
                Text("IMAP: ${s.foundImap}", style = MaterialTheme.typography.bodyMedium)
                Text("SMTP: ${s.foundSmtp}" + if (!s.smtpAuth) " · без авторизации" else "", style = MaterialTheme.typography.bodyMedium)
                Text("Логин: ${s.imapLogin}", style = MaterialTheme.typography.bodySmall)
            }
        }
        TextButton(onClick = { vm.edit { it.copy(showServers = true) } }, enabled = !s.busy) { Text(stringResource(R.string.action_change_servers)) }
    }
    PasswordField(s, vm, focus)
    if (s.passwordHint.isNotEmpty()) {
        Text(s.passwordHint, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
    }
}

@Composable
private fun Notice(msg: Msg) {
    Card(
        colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.primaryContainer),
        modifier = Modifier.fillMaxWidth(),
    ) {
        Text(
            msg.text(),
            color = MaterialTheme.colorScheme.onPrimaryContainer,
            style = MaterialTheme.typography.bodyMedium,
            modifier = Modifier.padding(12.dp),
        )
    }
}

@Composable
private fun Buttons(s: ConnectState, vm: ConnectViewModel, onCancel: (() -> Unit)?) {
    Row(Modifier.fillMaxWidth(), verticalAlignment = Alignment.CenterVertically) {
        when {
            s.step == ConnectStep.Email -> Column {
                TextButton(onClick = vm::manual) { Text(stringResource(R.string.action_manual_imap)) }
                TextButton(onClick = vm::manualDdmail) { Text(stringResource(R.string.action_ddmail_server)) }
                if (onCancel != null) TextButton(onClick = onCancel) { Text(stringResource(R.string.action_cancel)) }
            }
            s.step == ConnectStep.Searching -> TextButton(onClick = { vm.back() }) { Text(stringResource(R.string.action_cancel)) }
            s.editingKey != null -> TextButton(onClick = { onCancel?.invoke() }, enabled = !s.busy) { Text(stringResource(R.string.action_cancel)) }
            else -> TextButton(onClick = { vm.back() }, enabled = !s.busy) { Text(stringResource(R.string.action_back)) }
        }
        Spacer(Modifier.weight(1f))
        when (s.step) {
            ConnectStep.Email -> Button(onClick = vm::findServer) { Text(stringResource(R.string.action_find)) }
            ConnectStep.Searching -> {}
            ConnectStep.Imap -> Button(onClick = vm::login, enabled = !s.busy) {
                Text(stringResource(if (s.busy) R.string.busy_check else R.string.action_save))
            }
            else -> Button(onClick = vm::login, enabled = !s.busy) {
                Text(stringResource(if (s.busy) R.string.busy_login else R.string.action_login))
            }
        }
    }
}
