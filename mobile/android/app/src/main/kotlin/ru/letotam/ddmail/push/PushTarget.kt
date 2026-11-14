package ru.letotam.ddmail.push

import ru.letotam.ddmail.auth.Session

/**
 * Регистрация устройства на сервере: чья (сервер и логин ddmail-учётки) и
 * какой токен какой службы. Хранится, чтобы снять её при выходе из учётки
 * и не повторять без нужды.
 */
data class PushTarget(val serverUrl: String, val username: String, val kind: String, val token: String) {
    /** Регистрация сделана этой сессией (той же учёткой того же сервера). */
    fun belongsTo(s: Session): Boolean = s.serverUrl == serverUrl && s.username == username

    fun encode(): String = listOf(serverUrl, username, kind, token).joinToString(SEP)

    companion object {
        private const val SEP = "\n"

        fun decode(s: String?): PushTarget? {
            val p = s?.split(SEP) ?: return null
            if (p.size != 4 || p.any { it.isBlank() }) return null
            return PushTarget(p[0], p[1], p[2], p[3])
        }

        /**
         * Какой должна быть регистрация сейчас: у ddmail-учётки — текущий
         * токен, если уведомления разрешены; иначе — никакой. Внешние
         * IMAP-учётки push не имеют (их почту приносит фоновый синк).
         */
        fun desired(session: Session?, kind: String?, token: String?, notificationsAllowed: Boolean): PushTarget? {
            if (session == null || kind.isNullOrBlank() || token.isNullOrBlank() || !notificationsAllowed) return null
            return PushTarget(session.serverUrl, session.username, kind, token)
        }

        /** Повторять ли регистрацию: изменилась — сразу, та же — раз в сутки. */
        fun needsRegister(want: PushTarget?, have: PushTarget?, lastAtMs: Long, nowMs: Long): Boolean =
            want != null && (want != have || nowMs - lastAtMs >= REFRESH_MS || nowMs < lastAtMs)

        /** Сервер вытесняет давно не обновлявшиеся устройства — подтверждаем раз в сутки. */
        const val REFRESH_MS = 24L * 60 * 60 * 1000
    }
}
