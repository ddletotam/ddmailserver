package ru.letotam.ddmail.imap

/**
 * Ошибки внешней учётки, разделённые по тому, что с ними делать
 * (mobile-external-accounts §7): [Auth] — «нужен вход», ретраи не лечат;
 * [Network] — временное, бэкофф; [Rejected] — сервер отказал в конкретной
 * команде при живом входе (ошибка действия, текст — как есть).
 */
sealed class MailException(message: String, cause: Throwable? = null) : Exception(message, cause) {
    class Auth(message: String) : MailException(message)
    class Network(message: String, cause: Throwable? = null) : MailException(message, cause)
    open class Rejected(message: String) : MailException(message)
}

/** Текст для человека: как предписывает контракт (§1 «Первый запуск»). */
fun MailException.humanText(): String = when (this) {
    is MailException.Auth -> "Неверный логин или пароль"
    is MailException.Network -> message ?: "Сервер не отвечает"
    is MailException.Rejected -> message ?: "Сервер отказал"
}

/** Шифрование — по порту, как у десктопа (`discover::imap_security`). */
enum class Security { Tls, StartTls }

fun imapSecurity(port: Int) = if (port == 143) Security.StartTls else Security.Tls

fun smtpSecurity(port: Int) = if (port == 587 || port == 25 || port == 2525) Security.StartTls else Security.Tls
