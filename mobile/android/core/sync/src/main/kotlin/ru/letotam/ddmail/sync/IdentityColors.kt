package ru.letotam.ddmail.sync

import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.contentOrNull
import kotlinx.serialization.json.jsonPrimitive

/**
 * Цвет строки беседы по группе адреса, на который пришла почта (контракт §5,
 * десктоп §1в). Документ групп — `/identity-groups`, общий с десктопом:
 * у группы свой `color`, пустой — пастель палитры по индексу группы
 * (`identities::group_color`). `user+tag@` без своей записи берёт цвет
 * `user@`; адрес вне групп — серый [UNKNOWN].
 *
 * Цвета — ARGB, чтобы модуль не зависел от Compose.
 */
class IdentityColors(private val byAddr: Map<String, Long> = emptyMap()) {

    fun colorOf(addr: String): Long {
        val lc = addr.trim().lowercase()
        return byAddr[lc] ?: baseOfTagged(lc)?.let { byAddr[it] } ?: UNKNOWN
    }

    companion object {
        /** `IDENT_PASTEL` десктопа (sidebar.rs) — тот же порядок. */
        val PASTEL: List<Long> = listOf<Long>(
            0xFFE4E1, 0xE8F5E9, 0xE3F2FD, 0xFFF9C4, 0xF3E5F5, 0xE0F7FA, 0xFBE9E7, 0xF1F8E9,
            0xEDE7F6, 0xE8EAF6, 0xFCE4EC, 0xE0F2F1, 0xFFF3E0, 0xF9FBE7, 0xEFEBE9, 0xECEFF1,
            0xFFF8E1, 0xE1F5FE, 0xDCEDC8, 0xFFCDD2, 0xF8BBD0, 0xD1C4E9, 0xB2DFDB, 0xB3E5FC,
        ).map { 0xFF000000 or it }

        /** «Некрасивый серый» десктопа (`IDENT_UNKNOWN`). */
        const val UNKNOWN: Long = 0xFFD5D5D0

        fun baseOfTagged(email: String): String? {
            val at = email.indexOf('@').takeIf { it > 0 } ?: return null
            val plus = email.indexOf('+').takeIf { it in 1 until at } ?: return null
            return email.substring(0, plus) + email.substring(at)
        }

        /** `#RGB` / `#RRGGBB` / `#AARRGGBB`; прочее — `null`. */
        fun parseHex(s: String): Long? {
            val h = s.trim().removePrefix("#")
            val full = when (h.length) {
                3 -> h.map { "$it$it" }.joinToString("")
                6, 8 -> h
                else -> return null
            }
            val v = full.toLongOrNull(16) ?: return null
            return if (full.length == 6) 0xFF000000 or v else v
        }

        fun parse(doc: JsonElement?): IdentityColors {
            val groups = ((doc as? JsonObject)?.get("groups") as? JsonArray) ?: return IdentityColors()
            val m = HashMap<String, Long>()
            groups.forEachIndexed { gi, g ->
                val obj = g as? JsonObject ?: return@forEachIndexed
                val own = (obj["color"])?.let { runCatching { it.jsonPrimitive.contentOrNull }.getOrNull() }
                val color = own?.takeIf { it.isNotBlank() }?.let(::parseHex) ?: PASTEL[gi % PASTEL.size]
                (obj["addresses"] as? JsonArray)?.forEach { a ->
                    val email = ((a as? JsonObject)?.get("email"))
                        ?.let { runCatching { it.jsonPrimitive.contentOrNull }.getOrNull() }
                        ?.trim()?.lowercase()
                    if (!email.isNullOrEmpty()) m.putIfAbsent(email, color)
                }
            }
            return IdentityColors(m)
        }
    }
}
