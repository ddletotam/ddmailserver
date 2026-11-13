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
 *
 * Системная папка «Спам» (`system: "spam"`): состав задаёт сервер (беседы с
 * `spam`), а не правила и не руки; всегда скрыта. Android всегда подключён к
 * серверу ddmail, поэтому она есть всегда ([withSpam]).
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
    /** Системная папка (`SYSTEM_SPAM`); пусто — обычная. */
    val system: String = "",
    internal val raw: JsonObject = JsonObject(emptyMap()),
) {
    val isSystem get() = system.isNotEmpty()
}

/** Папки правили на другой станции: своя правка не записана, документ — с сервера. */
class FoldersConflict : Exception("Папки изменены на другой станции — правка не сохранена")

/** `system` папки «Спам». */
const val SYSTEM_SPAM = "spam"

data class FoldersDoc(
    val folders: List<DialogFolder> = emptyList(),
    internal val raw: JsonObject = JsonObject(emptyMap()),
) {
    val spamIndex: Int? get() = folders.indexOfFirst { it.system == SYSTEM_SPAM }.takeIf { it >= 0 }

    /**
     * Индекс папки диалога или `null` — инбокс. Спам — только в «Спам», и
     * ничто другое туда не попадает; спам без папки «Спам» не виден нигде
     * ([filter], [unreadCounts]).
     */
    fun folderOf(c: Conversation): Int? {
        if (c.spam) return spamIndex
        val k = c.key
        folders.indexOfFirst { !it.isSystem && k in it.include }.takeIf { it >= 0 }?.let { return it }
        return folders.indexOfFirst { !it.isSystem && k !in it.exclude && it.rules.matches(c) }.takeIf { it >= 0 }
    }

    /**
     * Папка «Спам» ровно одна, скрыта, без правил и ручных назначений — как
     * `FoldersDoc::ensure_spam` десктопа. Нет — добавляется в конец (в
     * документ на сервере она попадёт с первой правкой).
     */
    fun withSpam(): FoldersDoc {
        var seen = false
        val kept = folders.filter { f -> f.system != SYSTEM_SPAM || !seen.also { seen = true } }
        val i = kept.indexOfFirst { it.system == SYSTEM_SPAM }
        val fixed = if (i >= 0) {
            kept.mapIndexed { j, f ->
                if (j != i) f else {
                    val name = f.name.ifBlank { "Спам" }
                    f.copy(
                        name = name,
                        rules = FolderRules(),
                        include = emptyList(),
                        exclude = emptyList(),
                        hidden = true,
                        autoRead = false,
                        raw = JsonObject(
                            f.raw + mapOf(
                                "name" to JsonPrimitive(name),
                                "rules" to JsonObject(emptyMap()),
                                "hidden" to JsonPrimitive(true),
                                "auto_read" to JsonPrimitive(false),
                            ),
                        ),
                    )
                }
            }
        } else {
            val id = if (kept.none { it.id == SYSTEM_SPAM }) SYSTEM_SPAM else "f${System.currentTimeMillis().toString(16)}0"
            kept + DialogFolder(
                id = id,
                name = "Спам",
                icon = "🚫",
                rules = FolderRules(),
                include = emptyList(),
                exclude = emptyList(),
                hidden = true,
                autoRead = false,
                system = SYSTEM_SPAM,
                raw = JsonObject(
                    mapOf(
                        "id" to JsonPrimitive(id),
                        "name" to JsonPrimitive("Спам"),
                        "icon" to JsonPrimitive("🚫"),
                        "rules" to JsonObject(emptyMap()),
                        "hidden" to JsonPrimitive(true),
                        "auto_read" to JsonPrimitive(false),
                        "system" to JsonPrimitive(SYSTEM_SPAM),
                    ),
                ),
            )
        }
        return copy(folders = fixed)
    }

    /**
     * Положить диалоги в папку [target] руками (`null` — в инбокс). Ручной
     * выбор побеждает всё: ключ снимается из чужих `include`, а каждая
     * папка, чьи правила его ловят, получает его в `exclude`. Одна правка
     * документа на весь набор — для множественного выбора (§5).
     */
    fun assign(convs: Collection<Conversation>, target: Int?): FoldersDoc {
        // Руками в «Спам» не кладут и из него не выносят («Не спам» — сервер).
        if (target != null && folders.getOrNull(target)?.isSystem != false) return this
        var out = folders
        for (c in convs.filter { !it.spam }) {
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
        convs.filter { c -> folderOf(c).let { it == active && (it != null || !c.spam) } }

    /** Диалоги с непрочитанным — по папкам и в инбоксе (как у Telegram: диалоги, не письма). */
    fun unreadCounts(convs: List<Conversation>): Pair<List<Int>, Int> {
        val out = IntArray(folders.size)
        var inbox = 0
        for (c in convs) {
            if (c.unreadCount <= 0) continue
            when (val i = folderOf(c)) {
                null -> if (!c.spam) inbox++
                else -> out[i]++
            }
        }
        return out.toList() to inbox
    }

    /** Индекс папки по id. */
    fun indexOf(id: String): Int? = folders.indexOfFirst { it.id == id }.takeIf { it >= 0 }

    /** Свежий id папки — как `new_id` десктопа: `f<мс hex><n>`. */
    fun newId(now: Long = System.currentTimeMillis()): String {
        val base = java.lang.Long.toHexString(now)
        var n = 0
        while (true) {
            val id = "f$base$n"
            if (indexOf(id) == null) return id
            n++
        }
    }

    /** «Новая папка»: в конец, имя «Папка N», иконка 📁 (как `folder_add` десктопа). */
    fun added(id: String = newId()): FoldersDoc {
        val n = folders.size + 1
        return copy(
            folders = folders + DialogFolder(
                id = id, name = "Папка $n", icon = "📁", rules = FolderRules(),
                include = emptyList(), exclude = emptyList(), hidden = false, autoRead = false,
            ),
        )
    }

    /** Правка обычной папки; системные не настраиваются. */
    fun edited(id: String, f: (DialogFolder) -> DialogFolder): FoldersDoc {
        val i = indexOf(id) ?: return this
        if (folders[i].isSystem) return this
        return copy(folders = folders.toMutableList().also { it[i] = f(folders[i]) })
    }

    /** Удалить папку (системную — нельзя); её диалоги возвращаются во «Входящие». */
    fun removed(id: String): FoldersDoc = copy(folders = folders.filter { it.isSystem || it.id != id })

    /**
     * Переставить папку [id] на место папки [toId] — по id, а не по номерам:
     * номера в полосе табов и в документе расходятся (скрытых в полосе нет).
     * Порядок папок — это и порядок табов, и приоритет правил.
     */
    fun moved(id: String, toId: String): FoldersDoc {
        val from = indexOf(id) ?: return this
        val to = indexOf(toId) ?: return this
        if (from == to) return this
        val list = folders.toMutableList()
        val f = list.removeAt(from)
        list.add(to, f)
        return copy(folders = list)
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
                system = o.str("system"),
                raw = o,
            )
        }
    }
}

/**
 * Запись папки: все известные поля — из модели (их правит редактор), прочее —
 * как было в исходном JSON (`#[serde(flatten)] extra` десктопа). Правила тоже
 * сливаются с исходными: неизвестные ключи `rules` переживают правку.
 */
private fun DialogFolder.toJson(): JsonObject {
    fun keys(l: List<DialogKey>) = JsonArray(
        l.map { JsonObject(mapOf("account" to JsonPrimitive(it.account), "id" to JsonPrimitive(it.id))) },
    )
    val rawRules = raw["rules"] as? JsonObject ?: JsonObject(emptyMap())
    val rulesJson = JsonObject(
        rawRules + mapOf(
            "identities" to JsonArray(rules.identities.map(::JsonPrimitive)),
            "domains" to JsonArray(rules.domains.map(::JsonPrimitive)),
            "kind" to JsonPrimitive(
                when (rules.kind) {
                    DialogKind.Any -> "any"
                    DialogKind.Direct -> "direct"
                    DialogKind.Group -> "group"
                },
            ),
        ),
    )
    val out = raw + mapOf(
        "id" to JsonPrimitive(id),
        "name" to JsonPrimitive(name),
        "icon" to JsonPrimitive(icon),
        "rules" to rulesJson,
        "include" to keys(include),
        "exclude" to keys(exclude),
        "hidden" to JsonPrimitive(hidden),
        "auto_read" to JsonPrimitive(autoRead),
    )
    // `system` у обычной папки не пишется (`skip_serializing_if` десктопа).
    return JsonObject(if (system.isEmpty()) out - "system" else out + ("system" to JsonPrimitive(system)))
}

/** Список правила из поля ввода: через запятую, точку с запятой, пробел; в нижнем регистре. */
fun splitRuleList(text: String): List<String> =
    text.split(',', ';', ' ', '\n').map { it.trim().lowercase() }.filter { it.isNotEmpty() }

private fun JsonObject.str(k: String) = (this[k] as? JsonPrimitive)?.contentOrNull.orEmpty()
private fun JsonObject.bool(k: String) = (this[k] as? JsonPrimitive)?.booleanOrNull ?: false
private fun JsonObject.strList(k: String) =
    (this[k] as? JsonArray)?.mapNotNull { (it as? JsonPrimitive)?.contentOrNull }.orEmpty()
private fun JsonObject.keys(k: String) = (this[k] as? JsonArray)?.mapNotNull { el ->
    val o = el as? JsonObject ?: return@mapNotNull null
    DialogKey(o.str("account"), o.str("id")).takeIf { it.id.isNotEmpty() }
}.orEmpty()

