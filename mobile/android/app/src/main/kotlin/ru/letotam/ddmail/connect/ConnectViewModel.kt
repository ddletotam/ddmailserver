package ru.letotam.ddmail.connect

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import ru.letotam.ddmail.DdmailApp
import ru.letotam.ddmail.R
import ru.letotam.ddmail.account.ImapAccount
import ru.letotam.ddmail.api.ApiException
import ru.letotam.ddmail.api.emailDomain
import ru.letotam.ddmail.api.normalizeServerUrl
import ru.letotam.ddmail.auth.Session
import ru.letotam.ddmail.discover.Found
import ru.letotam.ddmail.discover.Hint
import ru.letotam.ddmail.discover.MailPresets
import ru.letotam.ddmail.discover.Progress
import ru.letotam.ddmail.imap.MailException
import ru.letotam.ddmail.imap.Security
import ru.letotam.ddmail.imap.imapSecurity
import ru.letotam.ddmail.imap.smtpSecurity
import ru.letotam.ddmail.sync.ImapEngine
import ru.letotam.ddmail.ui.Msg

/**
 * Шаги подключения (контракт §3; mobile-external-accounts §1): адрес →
 * поиск → вход на сервер ddmail ([Found]/[Manual]) или IMAP-форма ([Imap]).
 */
enum class ConnectStep { Email, Searching, Found, Manual, Imap }

data class ConnectState(
    val step: ConnectStep = ConnectStep.Email,
    val email: String = "",
    val server: String = "",
    val username: String = "",
    val password: String = "",
    val progress: Progress? = null,
    /** Нейтральное сообщение (не ошибка): «сервер не найден, введите вручную». */
    val notice: Msg? = null,
    val error: Msg? = null,
    val busy: Boolean = false,
    // ── IMAP-форма ──
    val name: String = "",
    val imapHost: String = "",
    val imapPort: String = "993",
    val smtpHost: String = "",
    val smtpPort: String = "465",
    val smtpAuth: Boolean = true,
    /** Логин IMAP/SMTP; пусто — адрес почты. */
    val imapLogin: String = "",
    /** Показать все поля серверов (ручной режим или «Изменить»). */
    val showServers: Boolean = false,
    /** Что нашлось: «imap.x.ru:993 · SSL/TLS» — для страницы найденного. */
    val foundImap: String = "",
    val foundSmtp: String = "",
    /** Подсказка про пароль приложения для известных служб. */
    val passwordHint: String = "",
    /** Правка существующей учётки: её прежний ключ. */
    val editingKey: String? = null,
    /** «Другие мои адреса» через запятую (айдентики учётки, §3). */
    val extraAddresses: String = "",
    val caldavUrl: String = "",
    val carddavUrl: String = "",
)

/**
 * Подключение: адрес почты → автонастройка (порт `discover.rs`) → вход в
 * ddmail или IMAP/SMTP с паролем; «Я знаю все настройки» — ручная форма.
 * Перед сохранением IMAP-учётки — проверка входа на IMAP и SMTP. Пароль при
 * поиске никуда не уходит; токен ddmail и пароль IMAP хранятся зашифрованными.
 */
class ConnectViewModel(
    private val app: DdmailApp,
    initial: ConnectState = ConnectState(),
    private val onConnected: (Session) -> Unit,
    private val onImapSaved: (String) -> Unit = {},
) : ViewModel() {
    private val _state = MutableStateFlow(initial)
    val state: StateFlow<ConnectState> = _state

    private var search: Job? = null

    fun setEmail(v: String) = _state.update { it.copy(email = v, error = null) }
    fun setServer(v: String) = _state.update { it.copy(server = v, error = null) }
    fun setUsername(v: String) = _state.update { it.copy(username = v, error = null) }
    fun setPassword(v: String) = _state.update { it.copy(password = v, error = null) }
    fun edit(f: (ConnectState) -> ConnectState) = _state.update { f(it).copy(error = null) }

    fun findServer() {
        val email = _state.value.email.trim()
        val domain = emailDomain(email)
        if (domain == null) {
            _state.update { it.copy(error = Msg(R.string.err_email)) }
            return
        }
        _state.update {
            it.copy(step = ConnectStep.Searching, email = email, progress = null, error = null, notice = null)
        }
        search?.cancel()
        search = viewModelScope.launch {
            // Известная служба из таблицы — первым кандидатом.
            val hint = MailPresets.guess(email)?.takeIf { it.provider != null }?.let { Hint(it.imapHost, it.smtpHost, it.smtpPort) }
            val found = app.mailDiscovery.discover(email, hint) { p -> _state.update { it.copy(progress = p) } }
            apply(email, found)
        }
    }

    private fun apply(email: String, found: Found) {
        val local = email.substringBeforeLast('@')
        val ddmailTaken = app.ddmailSession() != null
        if (found.ddmailUrl != null && !ddmailTaken) {
            _state.update {
                it.copy(
                    step = ConnectStep.Found,
                    server = found.ddmailUrl!!,
                    // Сервер входит по имени пользователя; чаще всего это часть до @.
                    username = it.username.ifEmpty { local },
                )
            }
            return
        }
        val preset = MailPresets.guess(email)
        val ddHost = found.ddmailUrl?.removePrefix("https://")
        _state.update {
            val imap = found.imap
            val smtp = found.smtp
            it.copy(
                step = ConnectStep.Imap,
                passwordHint = preset?.passwordHint.orEmpty(),
                imapLogin = found.login ?: email,
                imapHost = imap?.host ?: ddHost ?: preset?.imapHost.orEmpty(),
                imapPort = (imap?.port ?: 993).toString(),
                smtpHost = smtp?.host ?: ddHost ?: preset?.smtpHost.orEmpty(),
                smtpPort = (smtp?.port ?: preset?.smtpPort ?: 465).toString(),
                smtpAuth = if (smtp != null) found.smtpAuth else true,
                foundImap = imap?.let { e -> label(e.host, e.port, imapSecurity(e.port)) }.orEmpty(),
                foundSmtp = smtp?.let { e -> label(e.host, e.port, smtpSecurity(e.port)) }.orEmpty(),
                showServers = imap == null || smtp == null,
                notice = when {
                    found.ddmailUrl != null -> Msg(R.string.ddmail_taken)
                    imap != null && smtp != null -> Msg(R.string.err_plain, listOf(found.provider?.let { p -> "Настройки службы «$p», порты проверены." } ?: "Серверы отвечают, шифрование проверено."))
                    imap != null -> Msg(R.string.imap_only_found)
                    smtp != null -> Msg(R.string.smtp_only_found)
                    else -> Msg(R.string.nothing_found, listOf(emailDomain(email).orEmpty()))
                },
            )
        }
    }

    private fun label(host: String, port: Int, sec: Security) =
        "$host:$port · " + if (sec == Security.Tls) "SSL/TLS" else "STARTTLS"

    /** «Я знаю все настройки» — ручная IMAP-форма. */
    fun manual() = _state.update { s ->
        val email = s.email.trim()
        val p = MailPresets.guess(email)
        s.copy(
            step = ConnectStep.Imap,
            showServers = true,
            error = null,
            notice = null,
            imapLogin = s.imapLogin.ifEmpty { email },
            imapHost = s.imapHost.ifEmpty { p?.imapHost.orEmpty() },
            smtpHost = s.smtpHost.ifEmpty { p?.smtpHost.orEmpty() },
            smtpPort = if (s.smtpHost.isEmpty()) (p?.smtpPort ?: 465).toString() else s.smtpPort,
            passwordHint = p?.passwordHint.orEmpty(),
        )
    }

    /** Ручной вход на сервер ddmail. */
    fun manualDdmail() = _state.update {
        it.copy(step = ConnectStep.Manual, error = null, notice = null, username = it.username.ifEmpty { it.email.substringBeforeLast('@') })
    }

    /** «Назад»: из поиска — с отменой поиска; с остальных шагов — к адресу почты. */
    fun back(): Boolean {
        val s = _state.value
        if (s.busy || s.step == ConnectStep.Email || s.editingKey != null) return false
        search?.cancel()
        _state.update { it.copy(step = ConnectStep.Email, error = null, notice = null, progress = null) }
        return true
    }

    fun login() {
        val s = _state.value
        if (s.busy) return
        if (s.step == ConnectStep.Imap) return saveImap()
        val username = s.username.trim()
        val server = normalizeServerUrl(s.server)
        if (username.isEmpty() || s.password.isEmpty() || server.isFailure) {
            _state.update {
                it.copy(
                    error = server.exceptionOrNull()?.message
                        ?.takeIf { s.server.isNotBlank() }
                        ?.let { m -> Msg(R.string.err_plain, listOf(m)) }
                        ?: Msg(R.string.err_fill),
                )
            }
            return
        }
        val url = server.getOrThrow()
        _state.update { it.copy(busy = true, error = null, server = url) }
        viewModelScope.launch {
            try {
                val r = app.api(url).login(username, s.password)
                val session = Session(url, r.username, r.userId, r.token)
                app.sessions.save(session)
                _state.update { it.copy(busy = false, password = "") }
                onConnected(session)
            } catch (e: ApiException) {
                _state.update { it.copy(busy = false, error = e.toMsg()) }
            }
        }
    }

    /** IMAP-учётка: проверка входа на IMAP и SMTP, потом — сохранение. */
    private fun saveImap() {
        val s = _state.value
        val email = s.email.trim().lowercase()
        val imapPort = s.imapPort.trim().toIntOrNull()
        val smtpPort = s.smtpPort.trim().toIntOrNull()
        val needPassword = s.editingKey == null || s.password.isNotEmpty()
        if (emailDomain(email) == null) return _state.update { it.copy(error = Msg(R.string.err_email)) }
        if (s.imapHost.isBlank() || s.smtpHost.isBlank() || imapPort == null || smtpPort == null || (needPassword && s.password.isEmpty())) {
            return _state.update { it.copy(error = Msg(R.string.err_fill_imap)) }
        }
        _state.update { it.copy(busy = true, error = null) }
        viewModelScope.launch {
            val old = s.editingKey?.let { k -> app.accounts.list().firstOrNull { it.key == k } }
            val account = ImapAccount(
                email = email,
                name = s.name.trim(),
                imapHost = s.imapHost.trim().lowercase(),
                imapPort = imapPort,
                login = s.imapLogin.trim().ifEmpty { email },
                smtpHost = s.smtpHost.trim().lowercase(),
                smtpPort = smtpPort,
                smtpAuth = s.smtpAuth,
                extraAddresses = s.extraAddresses.split(',', ';', ' ', '\n').map { it.trim().lowercase() }.filter { it.contains('@') }.distinct(),
                caldavUrl = s.caldavUrl.trim().ifEmpty { old?.caldavUrl.orEmpty().takeIf { s.editingKey == null }.orEmpty() },
                carddavUrl = s.carddavUrl.trim().ifEmpty { old?.carddavUrl.orEmpty().takeIf { s.editingKey == null }.orEmpty() },
            )
            try {
                val pw = s.password.ifEmpty { s.editingKey?.let { app.accounts.password(it) }.orEmpty() }
                ImapEngine.check(account, pw)
                // Новая учётка: CalDAV/CardDAV по её домену (контракт §8), правятся в настройках.
                val withDav = if (s.editingKey == null && account.caldavUrl.isEmpty() && account.carddavUrl.isEmpty()) {
                    _state.update { it.copy(notice = Msg(R.string.err_plain, listOf("Ищем календарь и контакты…"))) }
                    val cal = runCatching { ru.letotam.ddmail.sync.PimHub.discover(app.davHttp, account, pw, "caldav") }.getOrNull()
                    val card = runCatching { ru.letotam.ddmail.sync.PimHub.discover(app.davHttp, account, pw, "carddav") }.getOrNull()
                    account.copy(caldavUrl = cal.orEmpty(), carddavUrl = card.orEmpty())
                } else account
                app.accounts.put(withDav, pw, replacingKey = s.editingKey)
                _state.update { it.copy(busy = false, password = "") }
                onImapSaved(account.key)
            } catch (e: MailException) {
                _state.update { it.copy(busy = false, error = Msg(R.string.err_plain, listOf(e.message ?: e.javaClass.simpleName))) }
            }
        }
    }
}

internal fun ApiException.toMsg(): Msg = when (this) {
    is ApiException.Unauthorized -> Msg(R.string.err_credentials)
    is ApiException.Network -> Msg(R.string.err_network, listOf(message ?: ""))
    is ApiException.Server -> Msg(R.string.err_server, listOf(code))
}
