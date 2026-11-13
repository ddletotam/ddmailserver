package ru.letotam.ddmail.sync

/**
 * Письмо целиком (RFC 822) для меню пузыря «Показать → Заголовки / Исходник
 * сообщения» (десктоп, меню пузыря; тот же смысл, что `MessageSource` iOS).
 */
object MessageSource {
    /** Блок заголовков — до первой пустой строки (CRLF или LF); без тела — всё письмо. */
    fun headers(raw: ByteArray): ByteArray {
        var i = 0
        while (i < raw.size) {
            if (raw[i] == LF) {
                val next = raw.getOrNull(i + 1)
                if (next == LF || (next == CR && raw.getOrNull(i + 2) == LF)) {
                    // Конец строки — без своего CR.
                    val end = if (i > 0 && raw[i - 1] == CR) i - 1 else i
                    return raw.copyOfRange(0, end)
                }
            }
            i++
        }
        return raw
    }

    /**
     * Текст для показа: UTF-8, если байты им являются, иначе Latin-1 (байт в
     * символ — ничего не теряется: 8-bit заголовки бывают в любой кодировке).
     */
    fun text(raw: ByteArray): String {
        val dec = Charsets.UTF_8.newDecoder()
            .onMalformedInput(java.nio.charset.CodingErrorAction.REPORT)
            .onUnmappableCharacter(java.nio.charset.CodingErrorAction.REPORT)
        return try {
            dec.decode(java.nio.ByteBuffer.wrap(raw)).toString()
        } catch (_: java.nio.charset.CharacterCodingException) {
            String(raw, Charsets.ISO_8859_1)
        }
    }

    /**
     * Огромное письмо (вложения в base64) текстом тормозит: показывается
     * начало — [limit] символов, «Скопировать» берёт всё.
     */
    fun preview(text: String, limit: Int = 256 * 1024): String =
        if (text.length <= limit) text else text.take(limit) + "\n\n… (показано начало; «Скопировать» берёт всё)"

    private const val LF: Byte = 0x0A
    private const val CR: Byte = 0x0D
}
