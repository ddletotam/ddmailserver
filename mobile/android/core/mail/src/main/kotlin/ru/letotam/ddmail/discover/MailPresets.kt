package ru.letotam.ddmail.discover

/**
 * Серверы известных служб по домену адреса — порт `mail_presets.rs`
 * десктопа. Таблица короткая намеренно: только службы, у которых серверы не
 * выводятся из домена (`imap.mail.ru` обслуживает и `bk.ru`, и `inbox.ru`).
 * Outlook.com нет: Microsoft не пускает IMAP по паролю.
 */
object MailPresets {
    data class Servers(
        val imapHost: String,
        val imapPort: Int,
        val smtpHost: String,
        val smtpPort: Int,
        /** Название службы из таблицы; `null` — серверы угаданы. */
        val provider: String?,
        /** Что сказать про пароль (многие пускают только по паролю приложения). */
        val passwordHint: String,
    )

    private class Preset(val domains: List<String>, val name: String, val imap: String, val smtp: String, val hint: String)

    private val PRESETS = listOf(
        Preset(
            listOf("mail.ru", "inbox.ru", "list.ru", "bk.ru", "internet.ru", "xmail.ru"), "Mail.ru", "imap.mail.ru", "smtp.mail.ru",
            "Mail.ru пускает внешние программы только по паролю приложения: Настройки → Безопасность → Пароли для внешних приложений.",
        ),
        Preset(
            listOf("yandex.ru", "ya.ru", "yandex.com", "yandex.by", "yandex.kz", "narod.ru"), "Яндекс Почты", "imap.yandex.ru", "smtp.yandex.ru",
            "Нужен пароль приложения (id.yandex.ru → Безопасность → Пароли приложений) и включённый IMAP в настройках почты.",
        ),
        Preset(
            listOf("gmail.com", "googlemail.com"), "Gmail", "imap.gmail.com", "smtp.gmail.com",
            "Gmail принимает только пароль приложения — он появляется после включения двухэтапной аутентификации в аккаунте Google.",
        ),
        Preset(
            listOf("gmx.com"), "GMX", "imap.gmx.com", "mail.gmx.com",
            "Доступ по IMAP в GMX выключен по умолчанию: включите его в настройках ящика (POP3 & IMAP).",
        ),
        Preset(
            listOf("gmx.net", "gmx.de", "gmx.at", "gmx.ch"), "GMX", "imap.gmx.net", "mail.gmx.net",
            "Доступ по IMAP в GMX выключен по умолчанию: включите его в настройках ящика (POP3 & IMAP).",
        ),
        Preset(
            listOf("rambler.ru", "lenta.ru", "autorambler.ru", "myrambler.ru", "ro.ru"), "Рамблер Почты", "imap.rambler.ru", "smtp.rambler.ru",
            "Включите доступ почтовым программам в настройках Рамблер Почты.",
        ),
        Preset(
            listOf("icloud.com", "me.com", "mac.com"), "iCloud", "imap.mail.me.com", "smtp.mail.me.com",
            "iCloud пускает только по паролю приложения: account.apple.com → Вход и безопасность → Пароли приложений.",
        ),
    )

    /** `null` — адрес ещё не похож на адрес (нет домена с точкой). */
    fun guess(email: String): Servers? {
        val domain = email.trim().substringAfterLast('@', "").trim().lowercase()
        if (!email.contains('@')) return null
        val dot = domain.lastIndexOf('.')
        if (dot <= 0 || domain.length - dot - 1 < 2) return null
        PRESETS.firstOrNull { domain in it.domains }?.let { p ->
            // iCloud: SMTP только STARTTLS на 587.
            val smtpPort = if (p.smtp == "smtp.mail.me.com") 587 else 465
            return Servers(p.imap, 993, p.smtp, smtpPort, p.name, p.hint)
        }
        return Servers("imap.$domain", 993, "smtp.$domain", 465, null, "")
    }
}
