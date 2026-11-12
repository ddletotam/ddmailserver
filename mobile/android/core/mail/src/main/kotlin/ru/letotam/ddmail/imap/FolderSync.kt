package ru.letotam.ddmail.imap

/** Что клиент помнит о папке между проходами (кэш, по папке). */
data class FolderState(
    val uidValidity: Long,
    val uidNext: Long,
    /** 0 — сервер без CONDSTORE. */
    val highestModSeq: Long,
    /** Самый большой UID в кэше. */
    val lastUid: Long,
)

/** Итог прохода по папке — применяется к кэшу одной транзакцией. */
data class FolderDelta(
    /** Сменилась UIDVALIDITY (или папку видим впервые): кэш папки — выбросить целиком. */
    val reset: Boolean,
    val added: List<MessageHeader>,
    /** Новые флаги писем, которые уже в кэше. */
    val flags: List<FlagsItem>,
    val removed: Set<Long>,
    val state: FolderState,
)

/**
 * Инкрементальная синхронизация папки (контракт mobile-external-accounts §3) —
 * по состоянию `UIDVALIDITY`/`UIDNEXT`/`HIGHESTMODSEQ`, без полного пересинка:
 *
 * - UIDVALIDITY сменилась (или кэша нет) — окно заново: `UID SEARCH ALL`,
 *   последние [window] UID, их заголовки;
 * - новые письма — `UID FETCH <последний+1>:*`, только если вырос UIDNEXT;
 * - флаги — QRESYNC (`SELECT … (QRESYNC …)` сам присылает изменения и
 *   `VANISHED`), иначе CONDSTORE (`CHANGEDSINCE`, и только если
 *   HIGHESTMODSEQ сдвинулся), иначе `UID FETCH <окно> (FLAGS)`;
 * - удалённые — `VANISHED` (QRESYNC), иначе `UID SEARCH UID <окно>` и разница.
 *
 * Окно — по UID, не по номерам. Лишнее сверх окна (старое) уходит из кэша.
 */
object FolderSync {
    fun sync(client: ImapClient, folder: String, window: Int, prev: FolderState?, cached: Collection<Long>): FolderDelta {
        val qr = if (prev != null && prev.highestModSeq > 0 && client.qresync) Qresync(prev.uidValidity, prev.highestModSeq) else null
        val info = client.select(folder, qr)

        if (prev == null || info.uidValidity != prev.uidValidity || info.uidValidity == 0L) {
            val all = if (info.exists == 0L) emptyList() else client.search("ALL").sorted()
            val take = all.takeLast(window)
            val headers = if (take.isEmpty()) emptyList() else client.fetchHeaders(take)
            return FolderDelta(
                reset = true,
                added = headers,
                flags = emptyList(),
                removed = cached.toSet(),
                state = FolderState(info.uidValidity, info.uidNext, info.highestModSeq, headers.maxOfOrNull { it.uid } ?: 0),
            )
        }

        val known = cached.toSortedSet()
        val removed = HashSet<Long>()
        val flags = mutableListOf<FlagsItem>()
        if (known.isNotEmpty()) {
            val range = "${known.first()}:${maxOf(known.last(), prev.lastUid)}"
            when {
                qr != null -> {
                    removed += known.filter { UidSet.contains(info.vanished, it) }
                    flags += info.changed
                }
                client.condstore && prev.highestModSeq > 0 && info.highestModSeq > 0 -> {
                    if (info.highestModSeq != prev.highestModSeq) flags += client.fetchFlags(range, prev.highestModSeq).first
                    // Удаление без QRESYNC не обязано двигать HIGHESTMODSEQ — сверяем всегда.
                    val present = client.search("UID $range").toHashSet()
                    removed += known.filter { it !in present }
                }
                else -> {
                    flags += client.fetchFlags(range).first
                    val present = client.search("UID $range").toHashSet()
                    removed += known.filter { it !in present }
                }
            }
        }

        val grew = info.uidNext == 0L || prev.uidNext == 0L || info.uidNext > prev.uidNext
        val added = if (grew && info.exists > 0) client.fetchHeadersFrom(prev.lastUid + 1).filter { it.uid !in known } else emptyList()

        // Окно: сверх него — самые старые по UID уходят из кэша.
        val after = (known - removed) + added.map { it.uid }
        val over = after.size - window
        if (over > 0) removed += after.sorted().take(over)

        val lastUid = maxOf(prev.lastUid, added.maxOfOrNull { it.uid } ?: 0)
        return FolderDelta(
            reset = false,
            added = added.filter { it.uid !in removed },
            flags = flags.filter { it.uid in known && it.uid !in removed },
            removed = removed.filter { it in known }.toSet(),
            state = FolderState(info.uidValidity, info.uidNext, info.highestModSeq, lastUid),
        )
    }
}
