package ru.letotam.ddmail.account

import kotlinx.serialization.Serializable

/**
 * Внешняя учётка IMAP + SMTP (mobile-external-accounts §1). Здесь — только
 * несекретное; пароль хранится отдельно, зашифрованным (`AccountStore`).
 */
@Serializable
data class ImapAccount(
    /** Адрес почты учётки — свой адрес и From по умолчанию. */
    val email: String,
    /** Имя отправителя в From. */
    val name: String = "",
    val imapHost: String,
    val imapPort: Int = 993,
    /** Логин IMAP (и SMTP — те же учётные данные). */
    val login: String,
    val smtpHost: String,
    val smtpPort: Int = 465,
    /** `false` — «SMTP без авторизации» (внутренний релей). */
    val smtpAuth: Boolean = true,
    /** «Другие мои адреса» — айдентики учётки, добавленные пользователем. */
    val extraAddresses: List<String> = emptyList(),
    /** Адреса CalDAV/CardDAV (база или коллекция); пусто — нет. */
    val caldavUrl: String = "",
    val carddavUrl: String = "",
) {
    /** `account_key` десктопа: `логин@хост` IMAP. */
    val key: String get() = accountKey(imapHost, login)

    /**
     * Свои адреса учётки (контракт §3): её email, логин, если он адрес, и
     * добавленные пользователем. В нижнем регистре, без повторов.
     */
    val ownAddresses: List<String>
        get() = (listOf(email) + listOfNotNull(login.takeIf { it.contains('@') }) + extraAddresses)
            .map { it.trim().lowercase() }.filter { it.contains('@') }.distinct()

    companion object {
        fun accountKey(host: String, username: String) = "$username@$host"
    }
}
