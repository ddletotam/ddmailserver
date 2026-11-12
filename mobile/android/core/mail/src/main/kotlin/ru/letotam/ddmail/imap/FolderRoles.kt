package ru.letotam.ddmail.imap

/**
 * Роли синхронизируемых папок. [code] — постоянный номер роли: UID письма в
 * кэше упаковывается с ним (`code shl 32 or uid`), чтобы письма разных папок
 * не сталкивались в ключах кэша тел.
 */
enum class FolderRole(val code: Int, val window: Int) {
    // Окна — лимиты десктопа: 5000 INBOX, половина Sent, четверть Drafts; Junk — 500.
    Inbox(1, 5000),
    Sent(2, 2500),
    Drafts(3, 1250),
    Junk(4, 500),
    Trash(5, 0),
    ;

    fun pack(uid: Long): Long = (code.toLong() shl 32) or (uid and 0xffffffffL)

    companion object {
        fun ofCode(c: Int) = entries.firstOrNull { it.code == c }
        fun unpackRole(packed: Long) = ofCode((packed ushr 32).toInt())
        fun unpackUid(packed: Long) = packed and 0xffffffffL
    }
}

/**
 * Папки по ролям (контракт §3): SPECIAL-USE (`\Sent`, `\Drafts`, `\Junk`,
 * `\Trash`), иначе по именам — как `find_sent_folder` десктопа, и такие же
 * списки для остальных (имена сравниваются раскодированными из modified UTF-7).
 */
object FolderRoles {
    private val NAMES = mapOf(
        FolderRole.Sent to listOf("Sent", "INBOX.Sent", "Sent Messages", "Sent Items", "Отправленные", "INBOX.Отправленные"),
        FolderRole.Drafts to listOf("Drafts", "INBOX.Drafts", "Draft", "Черновики", "INBOX.Черновики"),
        FolderRole.Junk to listOf("Junk", "Spam", "INBOX.Junk", "INBOX.Spam", "Спам", "INBOX.Спам", "Junk E-mail", "Bulk Mail"),
        FolderRole.Trash to listOf("Trash", "INBOX.Trash", "Deleted Items", "Deleted Messages", "Корзина", "INBOX.Корзина"),
    )
    private val ATTR = mapOf(
        FolderRole.Sent to "\\SENT",
        FolderRole.Drafts to "\\DRAFTS",
        FolderRole.Junk to "\\JUNK",
        FolderRole.Trash to "\\TRASH",
    )
    private val CONTAINS = mapOf(FolderRole.Sent to "sent", FolderRole.Junk to "spam")

    fun detect(boxes: List<Mailbox>): Map<FolderRole, String> {
        val usable = boxes.filter { it.selectable }
        val out = LinkedHashMap<FolderRole, String>()
        out[FolderRole.Inbox] = usable.firstOrNull { it.name.equals("INBOX", ignoreCase = true) }?.name ?: "INBOX"
        for (role in listOf(FolderRole.Sent, FolderRole.Drafts, FolderRole.Junk, FolderRole.Trash)) {
            val name = usable.firstOrNull { ATTR[role]!! in it.attrs }?.name
                ?: NAMES[role]!!.firstNotNullOfOrNull { n -> usable.firstOrNull { it.display.equals(n, ignoreCase = true) }?.name }
                ?: CONTAINS[role]?.let { c -> usable.firstOrNull { it.display.lowercase().contains(c) && !it.name.equals("INBOX", true) }?.name }
            if (name != null && name !in out.values) out[role] = name
        }
        return out
    }
}
