package ru.letotam.ddmail.conversation

import java.util.concurrent.ConcurrentHashMap

/**
 * Решение перехватчика WebView: пускать ли запрос `/remote` (внешняя картинка
 * через прокси приложения) — контракт §5 «Папка «Спам»», §8.
 *
 * Обычная беседа — как было: что документ попросил через `/remote`, то и
 * грузится (документ сам не просит до разрешения картинок).
 *
 * Беседа спама — только картинки письма, которому пользователь в этом показе
 * нажал «Показать картинки» ([allow]), и только запросы, которые браузер
 * делает за картинкой (`Sec-Fetch-Dest: image` или `Accept` с `image/`):
 * шрифт, стиль или что-то ещё через тот же `/remote` не пройдёт, даже если
 * письмо как-то протащило такой URL мимо чистки. Разрешение живёт в памяти
 * до открытия другой беседы ([open]) и на диск не пишется; «Всегда от
 * отправителя» на спам не действует (это решает [ConversationController]).
 *
 * Вызывается с потоков перехватчика WebView и с главного — потокобезопасно.
 */
class RemoteImageGate {
    @Volatile
    var spam: Boolean = false
        private set

    /** uid писем открытой беседы: разрешить можно только их. */
    private val known = ConcurrentHashMap.newKeySet<Long>()
    private val allowed = ConcurrentHashMap.newKeySet<Long>()

    /** Открыта беседа: прежние разрешения забываются. */
    fun open(spam: Boolean, uids: Collection<Long>) {
        // Сначала запрет, потом новый состав: в промежутке ничего не пройдёт лишнего.
        this.spam = true
        allowed.clear()
        known.clear()
        known.addAll(uids)
        this.spam = spam
    }

    /** В открытую беседу пришли новые письма. */
    fun extend(uids: Collection<Long>) {
        known.addAll(uids)
    }

    /** «Показать картинки» у письма [uid] — на этот показ беседы. */
    fun allow(uid: Long) {
        if (uid in known) allowed.add(uid)
    }

    fun permits(uid: Long?, headers: Map<String, String>): Boolean {
        if (!spam) return true
        return uid != null && uid in allowed && looksLikeImage(headers)
    }

    companion object {
        /**
         * Запрос за картинкой: `Sec-Fetch-Dest` (если WebView его передал)
         * решает сам; иначе — `Accept`, который Chromium ставит картинкам
         * (`image/avif,image/webp,…`), а шрифтам и стилям — нет. Нет ни того,
         * ни другого — не картинка.
         */
        fun looksLikeImage(headers: Map<String, String>): Boolean {
            fun header(name: String) = headers.entries.firstOrNull { it.key.equals(name, ignoreCase = true) }?.value?.trim()
            header("Sec-Fetch-Dest")?.takeIf { it.isNotEmpty() }?.let { return it.equals("image", ignoreCase = true) }
            val accept = header("Accept") ?: return false
            return accept.split(',').any { it.trim().startsWith("image/", ignoreCase = true) }
        }
    }
}
