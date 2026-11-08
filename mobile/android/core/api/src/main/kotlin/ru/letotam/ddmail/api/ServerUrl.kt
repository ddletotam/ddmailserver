package ru.letotam.ddmail.api

import java.net.IDN
import java.net.URI

/**
 * Адрес сервера из ввода пользователя → `https://хост[:порт]`.
 *
 * Только https (контракт §3): по http пароль ушёл бы открытым текстом.
 * Схему можно не писать; путь, слэш в конце и пробелы отбрасываются;
 * кириллический домен переводится в punycode.
 */
fun normalizeServerUrl(input: String): Result<String> {
    val raw = input.trim().trimEnd('/')
    if (raw.isEmpty()) return Result.failure(IllegalArgumentException("Введите адрес сервера."))
    val lower = raw.lowercase()
    if (lower.startsWith("http://")) {
        return Result.failure(IllegalArgumentException("Нужен https:// — по http:// пароль ушёл бы открытым текстом."))
    }
    val withScheme = if (lower.startsWith("https://")) raw else "https://$raw"
    val uri = try {
        URI(withScheme)
    } catch (_: Exception) {
        return Result.failure(IllegalArgumentException("Не похоже на адрес сервера."))
    }
    val host = uri.host ?: uri.authority?.substringBefore(':')
    if (host.isNullOrBlank() || !host.contains('.')) {
        return Result.failure(IllegalArgumentException("Не похоже на адрес сервера."))
    }
    val ascii = try {
        IDN.toASCII(host).lowercase()
    } catch (_: Exception) {
        return Result.failure(IllegalArgumentException("Не похоже на адрес сервера."))
    }
    val port = if (uri.port > 0 && uri.port != 443) ":${uri.port}" else ""
    return Result.success("https://$ascii$port")
}

/** Домен адреса почты в ASCII (IDN → punycode); `null` — это не адрес. */
fun emailDomain(email: String): String? {
    val at = email.trim().lastIndexOf('@')
    if (at <= 0) return null
    val domain = email.trim().substring(at + 1).trim().trimEnd('.')
    if (!domain.contains('.') || domain.startsWith('.')) return null
    return try {
        IDN.toASCII(domain).lowercase()
    } catch (_: Exception) {
        null
    }
}
