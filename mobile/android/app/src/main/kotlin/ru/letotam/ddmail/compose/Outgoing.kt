package ru.letotam.ddmail.compose

import ru.letotam.ddmail.api.Identity
import ru.letotam.ddmail.api.MessageBody
import ru.letotam.ddmail.api.OutgoingAttachment
import ru.letotam.ddmail.api.SendRequest
import ru.letotam.ddmail.sync.Conversation

/** Файл, приложенный к письму в композере. */
class PendingAttachment(val name: String, val mime: String, val bytes: ByteArray) {
    val size: Int get() = bytes.size
}

/**
 * Что набрано в композере. Поля «Кому», «Копия», «Тема» — переопределения:
 * пусто значит «как выведет ветка отправки» (десктопный контракт §1).
 */
data class Draft(
    val text: String = "",
    val to: String = "",
    val cc: String = "",
    val subject: String = "",
    /** Адрес «От кого»; пусто — выбрать по беседе. */
    val from: String = "",
    val attachments: List<PendingAttachment> = emptyList(),
) {
    /** «Есть что отправлять»: текст или хотя бы одно вложение (§1). */
    val canSend: Boolean get() = text.isNotBlank() || attachments.isNotEmpty()
}

/** Ветка отправки (§1 десктопа), в порядке проверки. */
sealed interface Branch {
    /** Пересылка: адресат — только явное «Кому». */
    data class Forward(val original: MessageBody) : Branch

    /** Новое письмо (из списка). */
    data object New : Branch

    /** Явный ответ на конкретное письмо; в группе — всем, кроме своих. */
    data class Reply(val original: MessageBody, val isGroup: Boolean) : Branch

    /** Неявный ответ в открытой беседе — её собеседникам. */
    data class Implicit(val conversation: Conversation, val lastIncoming: MessageBody?, val last: MessageBody?) : Branch
}

/** Результат сборки: запрос или причина, почему отправлять нечего/некому. */
sealed interface Built {
    data class Ok(val request: SendRequest, val to: List<String>) : Built
    data class Refused(val reason: String) : Built
}

fun splitAddresses(s: String): List<String> =
    s.split(',', ';', ' ', '\n').map { it.trim() }.filter { it.contains('@') }

/** `Имя <адрес>` → адрес в нижнем регистре; голый адрес — он же. */
fun bareAddress(raw: String): String {
    val lt = raw.indexOf('<')
    val gt = raw.indexOf('>', lt + 1)
    return (if (lt >= 0 && gt > lt) raw.substring(lt + 1, gt) else raw).trim().lowercase()
}

private fun re(s: String) = when {
    s.isBlank() -> s
    s.trim().lowercase().startsWith("re:") -> s
    else -> "Re: $s"
}

private fun escapeHtml(s: String) = s
    .replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;").replace("\"", "&quot;")

/** Набранный текст в HTML: переносы строк — `<br>`, ничего больше не выдумываем. */
fun textToHtml(text: String): String = "<div>" + escapeHtml(text).replace("\n", "<br>") + "</div>"

/**
 * «От кого»: явный выбор; иначе адрес, на который пришла беседа (если он
 * свой); иначе адрес по умолчанию.
 */
fun pickFrom(draft: Draft, identities: List<Identity>, receivedBy: String?): Identity? {
    fun find(a: String) = identities.firstOrNull { it.email.equals(a, ignoreCase = true) }
    return draft.from.takeIf { it.isNotBlank() }?.let(::find)
        ?: receivedBy?.let(::find)
        ?: identities.firstOrNull { it.isDefault }
        ?: identities.firstOrNull()
}

fun fromHeader(id: Identity): String =
    if (id.name.isBlank() || id.name.equals(id.email, ignoreCase = true)) id.email
    else "\"" + id.name.replace("\"", "'") + "\" <${id.email}>"

private fun threading(b: MessageBody?): Pair<String, String> {
    if (b == null) return "" to ""
    val refs = b.references + listOfNotNull(b.messageId.takeIf { it.isNotEmpty() })
    return b.messageId to refs.joinToString(" ")
}

/**
 * Собрать письмо по ветке (§1). Непустое «Кому» побеждает любую
 * авто-деривацию, непустая тема — авто-«Re:»/«Fwd:»; копия уходит всегда.
 */
fun build(
    branch: Branch,
    draft: Draft,
    identities: List<Identity>,
    forwardHtml: String = "",
    forwardText: String = "",
    extraAttachments: List<PendingAttachment> = emptyList(),
): Built {
    if (!draft.canSend && branch !is Branch.Forward) return Built.Refused("Нечего отправлять.")
    val own = identities.map { it.email.lowercase() }.toSet()
    val toOverride = splitAddresses(draft.to)
    val cc = splitAddresses(draft.cc)

    val receivedBy = (branch as? Branch.Implicit)?.conversation?.receivedBy
    val from = pickFrom(draft, identities, receivedBy) ?: return Built.Refused("Нет ни одного своего адреса для отправки.")

    val (to, subject, irt, refs) = when (branch) {
        is Branch.Forward -> {
            if (toOverride.isEmpty()) return Built.Refused("Укажите, кому переслать.")
            val subj = draft.subject.ifBlank { "Fwd: ${branch.original.subject}" }
            Quad(toOverride, subj, "", "")
        }
        Branch.New -> {
            if (toOverride.isEmpty()) return Built.Refused("Укажите адресата.")
            Quad(toOverride, draft.subject, "", "")
        }
        is Branch.Reply -> {
            val o = branch.original
            val derived = buildList {
                add(o.fromAddr.lowercase())
                if (branch.isGroup) (o.to + o.cc).forEach { add(bareAddress(it)) }
            }.filter { it.isNotEmpty() && it !in own }.distinct()
            val t = toOverride.ifEmpty { derived }
            if (t.isEmpty()) return Built.Refused("Не нашлось, кому ответить.")
            val (i, r) = threading(o)
            Quad(t, draft.subject.ifBlank { re(o.subject) }, i, r)
        }
        is Branch.Implicit -> {
            val c = branch.conversation
            // Беседа «все адреса мои» (письмо себе) собеседников не имеет —
            // отвечаем отправителю последнего входящего (§4 «Роли» десктопа).
            val t = toOverride.ifEmpty {
                c.counterparts.map { it.addr }.filter { it.isNotBlank() }.ifEmpty {
                    listOfNotNull((branch.lastIncoming?.fromAddr ?: c.receivedBy).takeIf { it.isNotBlank() })
                }
            }
            if (t.isEmpty()) return Built.Refused("У беседы нет адресата.")
            // Тема и цепочка — от последнего входящего: на него и отвечают,
            // даже если наше письмо было позже (§1 десктопа).
            val base = branch.lastIncoming?.subject ?: c.lastSubject
            val (i, r) = threading(branch.lastIncoming ?: branch.last)
            Quad(t, draft.subject.ifBlank { re(base) }, i, r)
        }
    }
    val html = textToHtml(draft.text) + forwardHtml
    val text = draft.text + forwardText
    val atts = (draft.attachments + extraAttachments).map { OutgoingAttachment(it.name, it.mime, it.bytes) }
    return Built.Ok(
        SendRequest(
            from = fromHeader(from),
            to = to,
            cc = cc,
            subject = subject,
            html = html,
            text = text,
            inReplyTo = irt,
            references = refs,
            attachments = atts,
        ),
        to,
    )
}

private data class Quad(val to: List<String>, val subject: String, val irt: String, val refs: String)

/** Блок пересылаемого письма: заголовки и само письмо. */
fun forwardBlock(o: MessageBody): Pair<String, String> {
    val date = java.time.Instant.ofEpochSecond(o.dateTs).atZone(java.time.ZoneId.systemDefault())
        .format(java.time.format.DateTimeFormatter.ofPattern("dd.MM.yyyy HH:mm"))
    val head = listOf(
        "---------- Пересылаемое сообщение ----------",
        "От: ${o.from.ifBlank { o.fromAddr }}" + if (o.from.isNotBlank() && o.fromAddr.isNotBlank()) " <${o.fromAddr}>" else "",
        "Дата: $date",
        "Тема: ${o.subject}",
        "Кому: ${o.to.joinToString(", ")}",
    )
    val text = "\n\n" + head.joinToString("\n") + "\n\n" + (o.text ?: "")
    val html = "<br><div>" + head.joinToString("<br>") { escapeHtml(it) } + "</div><br>" +
        (o.html ?: textToHtml(o.text ?: ""))
    return text to html
}
