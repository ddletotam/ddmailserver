package ru.letotam.ddmail.sync

import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.booleanOrNull
import kotlinx.serialization.json.contentOrNull

/**
 * Документ групп айдентик — порт `IdentityDoc` десктопа (`identities.rs`,
 * десктоп §1в). Толкование документа на Android — только здесь.
 *
 * Документ общий с десктопом (`/identity-groups` сервера ddmail; без ddmail —
 * локальный, как `identities.json` десктопа), поэтому правка трогает только
 * своё поле, а всё прочее (подписи, неизвестные поля) остаётся как было:
 * источник — [json], модель — его прочтение.
 */
class IdentityGroups private constructor(val json: JsonObject) {

    /** Адрес группы; [email] — в нижнем регистре. */
    data class Address(val email: String, val hidden: Boolean)

    data class Group(
        val id: String,
        val name: String,
        val color: String,
        /** Поле `sender` как есть; адрес-отправитель — [sender]. */
        val senderField: String,
        val addresses: List<Address>,
        val strict: Boolean,
    ) {
        /** Адрес, с которого пишет группа: `sender`, если он в группе, иначе первый адрес. */
        val sender: String get() = if (addresses.any { it.email == senderField }) senderField else addresses.firstOrNull()?.email.orEmpty()

        /** «Имя · адрес» или только адрес — как `group_title` десктопа. */
        val title: String get() = if (name.isBlank()) sender else "${name.trim()} · $sender"
    }

    /** Адрес, найденный в учётке: начальные имя и цвет новой группы. */
    data class Source(val email: String, val name: String = "", val color: String = "")

    val groups: List<Group> = (json["groups"] as? JsonArray).orEmpty().mapNotNull { g ->
        val o = g as? JsonObject ?: return@mapNotNull null
        Group(
            id = o.str("id"),
            name = o.str("name"),
            color = o.str("color"),
            senderField = o.str("sender").trim().lowercase(),
            addresses = (o["addresses"] as? JsonArray).orEmpty().mapNotNull { a ->
                val ao = a as? JsonObject ?: return@mapNotNull null
                val email = ao.str("email").trim().lowercase()
                if (email.isEmpty()) null else Address(email, (ao["hidden"] as? JsonPrimitive)?.booleanOrNull ?: false)
            },
            strict = (o["strict"] as? JsonPrimitive)?.booleanOrNull ?: false,
        )
    }

    /** Поле `default_group`: id группы нового письма. */
    val defaultGroupId: String = json.str("default_group")

    val isEmpty: Boolean get() = groups.isEmpty()

    private fun locateExact(email: String): Pair<Int, Int>? {
        groups.forEachIndexed { gi, g ->
            val ai = g.addresses.indexOfFirst { it.email == email }
            if (ai >= 0) return gi to ai
        }
        return null
    }

    /** (группа, адрес); не добавленный явно `user+tag@` сводится к `user@`. */
    fun locate(email: String): Pair<Int, Int>? {
        val lc = email.trim().lowercase()
        return locateExact(lc) ?: IdentityColors.baseOfTagged(lc)?.let(::locateExact)
    }

    fun groupOf(email: String): Group? = locate(email)?.let { groups[it.first] }

    fun groupById(id: String): Group? = groups.firstOrNull { it.id == id }

    fun isMine(email: String): Boolean = locate(email) != null

    /** Группа по умолчанию (★): `default_group`, нет такой — первая. */
    val defaultGroup: Group? get() = groupById(defaultGroupId) ?: groups.firstOrNull()

    /**
     * С какого адреса отвечать на письмо, пришедшее на [receivedBy]:
     * отправитель его группы. Адрес не мой — отправитель группы по умолчанию,
     * а если она строгая — первой нестрогой. Нет и такой — `null`: адрес
     * выбирают руками (`IdentityDoc::reply_sender`).
     */
    fun replySender(receivedBy: String): String? =
        (groupOf(receivedBy) ?: defaultGroup?.takeIf { !it.strict } ?: groups.firstOrNull { !it.strict })
            ?.sender?.takeIf { it.isNotEmpty() }

    /** Отправитель нового письма — группы по умолчанию (`new_mail_sender`). */
    fun newMailSender(): String? = defaultGroup?.sender?.takeIf { it.isNotEmpty() }

    /** Отправитель по умолчанию для контекста: `null` — новое письмо, иначе адрес, на который пришло письмо. */
    fun autoSender(context: String?): String? = if (context == null) newMailSender() else replySender(context)

    /**
     * Можно ли писать с [from] в контексте письма, пришедшего на [context]
     * (`null` — новое письмо). Нестрогая группа и чужой адрес — всегда;
     * строгая — только новое письмо или письмо, пришедшее на её же адрес.
     */
    fun allowed(from: String, context: String?): Boolean {
        val (gi, _) = locate(from) ?: return true
        if (!groups[gi].strict) return true
        if (context == null) return true
        return locate(context)?.first == gi
    }

    /** Предлагать ли адрес в «От кого»: не скрыт и разрешён в контексте. */
    fun offered(from: String, context: String?): Boolean {
        val hidden = locate(from)?.let { (gi, ai) -> groups[gi].addresses[ai].hidden } ?: false
        return !hidden && allowed(from, context)
    }

    /**
     * Добавить адреса, которых ещё нет: каждый — новой группой из одного
     * (`merge_sources`). Ничего не удаляет и известных адресов не трогает.
     * Без изменений — тот же экземпляр.
     */
    fun merged(sources: List<Source>): IdentityGroups {
        val added = mutableListOf<JsonObject>()
        var cur = this
        for (s in sources) {
            val email = s.email.trim().lowercase()
            if (!looksLikeEmail(email) || cur.locateExact(email) != null) continue
            val color = s.color.trim().ifEmpty { cur.nextColor() }
            val group = JsonObject(
                mapOf(
                    "id" to JsonPrimitive(cur.newId()),
                    "name" to JsonPrimitive(s.name.trim()),
                    "color" to JsonPrimitive(color),
                    "sender" to JsonPrimitive(email),
                    "addresses" to JsonArray(listOf(JsonObject(mapOf("email" to JsonPrimitive(email), "hidden" to JsonPrimitive(false))))),
                    "signature" to JsonArray(emptyList()),
                    "strict" to JsonPrimitive(false),
                ),
            )
            added += group
            cur = IdentityGroups(JsonObject(cur.json + ("groups" to JsonArray(cur.groupsJson() + group))))
        }
        if (added.isEmpty()) return this
        if (cur.groupById(cur.defaultGroupId) == null) {
            cur.groups.firstOrNull()?.let { cur = cur.withDefault(it.id) }
        }
        return cur
    }

    /** Сделать группой по умолчанию (★) группу [id]; поля документа — как были. */
    fun withDefault(id: String): IdentityGroups = IdentityGroups(JsonObject(json + ("default_group" to JsonPrimitive(id))))

    /** Свободный пастельный цвет: первый незанятый, все заняты — по кругу (`next_color`). */
    private fun nextColor(): String {
        val used = groups.map { it.color.uppercase() }.toSet()
        return PASTEL_HEX.firstOrNull { it.uppercase() !in used } ?: PASTEL_HEX[groups.size % PASTEL_HEX.size]
    }

    private fun newId(): String {
        var n = groups.size + 1
        while (groupById("g$n") != null) n++
        return "g$n"
    }

    private fun groupsJson(): List<JsonElement> = (json["groups"] as? JsonArray).orEmpty()

    /** Цвета строк по этому документу. */
    fun colors(): IdentityColors = IdentityColors.parse(json)

    override fun equals(other: Any?): Boolean = other is IdentityGroups && other.json == json
    override fun hashCode(): Int = json.hashCode()

    companion object {
        val EMPTY = IdentityGroups(JsonObject(emptyMap()))

        fun parse(doc: JsonElement?): IdentityGroups = (doc as? JsonObject)?.let(::IdentityGroups) ?: EMPTY

        /** `IDENT_PASTEL` десктопа строками — так их пишет `merge_sources`. */
        private val PASTEL_HEX: List<String> = IdentityColors.PASTEL.map { "#%06X".format(it and 0xFFFFFF) }

        /** Похоже на email: одна `@`, непустые части, точка в домене, без пробелов (`looks_like_email`). */
        fun looksLikeEmail(s: String): Boolean {
            val t = s.trim()
            val at = t.indexOf('@')
            if (at <= 0) return false
            val domain = t.substring(at + 1)
            return domain.isNotEmpty() && '@' !in domain && '.' in domain &&
                !domain.startsWith('.') && !domain.endsWith('.') && t.none { it.isWhitespace() }
        }

        private fun JsonObject.str(key: String): String =
            (this[key] as? JsonPrimitive)?.takeIf { it.isString }?.contentOrNull.orEmpty()
    }
}

/** Документ групп три раза подряд менялся под нами (другая станция) — правка не записана. */
class IdentityGroupsBusy : Exception("Айдентики сейчас меняет другая станция — попробуйте ещё раз")

/** Адреса выбранной группы больше нет в документе (его поменяли на другой станции). */
class IdentityGroupGone : Exception("Группы больше нет — айдентики изменены на другой станции")
