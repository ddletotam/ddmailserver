package ru.letotam.ddmail.sync

import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.booleanOrNull
import kotlinx.serialization.json.contentOrNull

/**
 * Папки диалогов — порт `desktop/native/src/folders.rs` (десктопный
 * контракт §4 «Папки диалогов»). Обязан совпадать с ним по смыслу: документ
 * общий, а толкует его только клиент.
 *
 * - диалог лежит строго в одной папке или ни в одной (инбокс);
 * - ручное `include` побеждает правила; среди правил — первая подходящая
 *   папка в порядке табов;
 * - между категориями правил «И», внутри списка «ИЛИ»; папка без правил
 *   набирается только руками.
 *
 * Неизвестные поля документа и папок переживают чтение и запись: каждый
 * объект держит исходный JSON и при записи меняет в нём только своё.
 */
enum class DialogKind { Any, Direct, Group }

data class FolderRules(
    val identities: List<String> = emptyList(),
    val domains: List<String> = emptyList(),
    val kind: DialogKind = DialogKind.Any,
) {
    val isEmpty get() = identities.isEmpty() && domains.isEmpty() && kind == DialogKind.Any

    fun matches(c: Conversation): Boolean {
        if (isEmpty) return false
        if (identities.isNotEmpty() && identities.none { it.equals(c.receivedBy, ignoreCase = true) }) return false
        if (domains.isNotEmpty() && c.counterparts.none { cp -> domainMatches(cp.addr) }) return false
        return when (kind) {
            DialogKind.Any -> true
            DialogKind.Direct -> !c.isGroup
            DialogKind.Group -> c.isGroup
        }
    }

    /** `acme.com` ловит и `mail.acme.com`. */
    private fun domainMatches(addr: String): Boolean {
        val host = addr.lowercase().substringAfterLast('@', "").ifEmpty { return false }
        return domains.any { raw ->
            val d = raw.trim().trimStart('@').lowercase()
            d.isNotEmpty() && (host == d || host.endsWith(".$d"))
        }
    }
}

data class DialogFolder(
    val id: String,
    val name: String,
    val icon: String,
    val rules: FolderRules,
    val include: List<DialogKey>,
    val exclude: List<DialogKey>,
    /** «Не показывать в списке»: таба нет, диалоги не видны нигде. */
    val hidden: Boolean,
    /** «Письма сразу прочитаны» — исполняет десктоп (§4). */
    val autoRead: Boolean,
    internal val raw: JsonObject = JsonObject(emptyMap()),
)

data class FoldersDoc(
    val folders: List<DialogFolder> = emptyList(),
    internal val raw: JsonObject = JsonObject(emptyMap()),
) {
    /** Индекс папки диалога или `null` — инбокс. */
    fun folderOf(c: Conversation): Int? {
        val k = c.key
        folders.indexOfFirst { k in it.include }.takeIf { it >= 0 }?.let { return it }
        return folders.indexOfFirst { k !in it.exclude && it.rules.matches(c) }.takeIf { it >= 0 }
    }

    /**
     * Положить диалоги в папку [target] руками (`null` — в инбокс). Ручной
     * выбор побеждает всё: ключ снимается из чужих `include`, а каждая
     * папка, чьи правила его ловят, получает его в `exclude`. Одна правка
     * документа на весь набор — для множественного выбора (§5).
     */
    fun assign(convs: Collection<Conversation>, target: Int?): FoldersDoc {
        var out = folders
        for (c in convs) {
            val k = c.key
            out = out.mapIndexed { i, f ->
                val inc = f.include.filter { it != k }.toMutableList()
                val exc = f.exclude.filter { it != k }.toMutableList()
                if (i == target) inc += k else if (f.rules.matches(c)) exc += k
                f.copy(include = inc, exclude = exc)
            }
        }
        return copy(folders = out)
    }

    /** Видимая часть списка для папки [active] (`null` — инбокс); порядок исходный. */
    fun filter(convs: List<Conversation>, active: Int?): List<Conversation> =
        convs.filter { folderOf(it) == active }

    /** Диалоги с непрочитанным — по папкам и в инбоксе (как у Telegram: диалоги, не письма). */
    fun unreadCounts(convs: List<Conversation>): Pair<List<Int>, Int> {
        val out = IntArray(folders.size)
        var inbox = 0
        for (c in convs) {
            if (c.unreadCount <= 0) continue
            when (val i = folderOf(c)) {
                null -> inbox++
                else -> out[i]++
            }
        }
        return out.toList() to inbox
    }

    fun toJson(): JsonObject {
        val list = JsonArray(folders.map { it.toJson() })
        return JsonObject(raw + ("folders" to list))
    }

    companion object {
        fun parse(e: JsonElement?): FoldersDoc {
            val obj = e as? JsonObject ?: return FoldersDoc()
            val folders = (obj["folders"] as? JsonArray)?.mapNotNull { (it as? JsonObject)?.let(::parseFolder) }
            return FoldersDoc(folders.orEmpty(), obj)
        }

        private fun parseFolder(o: JsonObject): DialogFolder {
            val rules = o["rules"] as? JsonObject ?: JsonObject(emptyMap())
            return DialogFolder(
                id = o.str("id"),
                name = o.str("name"),
                icon = o.str("icon"),
                rules = FolderRules(
                    identities = rules.strList("identities"),
                    domains = rules.strList("domains"),
                    kind = when (rules.str("kind")) {
                        "direct" -> DialogKind.Direct
                        "group" -> DialogKind.Group
                        else -> DialogKind.Any
                    },
                ),
                include = o.keys("include"),
                exclude = o.keys("exclude"),
                hidden = o.bool("hidden"),
                autoRead = o.bool("auto_read"),
                raw = o,
            )
        }
    }
}

private fun DialogFolder.toJson(): JsonObject {
    fun keys(l: List<DialogKey>) = JsonArray(
        l.map { JsonObject(mapOf("account" to JsonPrimitive(it.account), "id" to JsonPrimitive(it.id))) },
    )
    return JsonObject(
        raw + mapOf(
            "include" to keys(include),
            "exclude" to keys(exclude),
        ),
    )
}

private fun JsonObject.str(k: String) = (this[k] as? JsonPrimitive)?.contentOrNull.orEmpty()
private fun JsonObject.bool(k: String) = (this[k] as? JsonPrimitive)?.booleanOrNull ?: false
private fun JsonObject.strList(k: String) =
    (this[k] as? JsonArray)?.mapNotNull { (it as? JsonPrimitive)?.contentOrNull }.orEmpty()
private fun JsonObject.keys(k: String) = (this[k] as? JsonArray)?.mapNotNull { el ->
    val o = el as? JsonObject ?: return@mapNotNull null
    DialogKey(o.str("account"), o.str("id")).takeIf { it.id.isNotEmpty() }
}.orEmpty()

