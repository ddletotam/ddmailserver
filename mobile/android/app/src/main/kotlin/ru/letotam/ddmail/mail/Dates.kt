package ru.letotam.ddmail.mail

import java.time.Instant
import java.time.ZoneId
import java.time.format.DateTimeFormatter

private val TIME = DateTimeFormatter.ofPattern("HH:mm")
private val DAY = DateTimeFormatter.ofPattern("dd.MM")
private val FULL = DateTimeFormatter.ofPattern("dd.MM.yy")

/** Дата строки списка, как у десктопа: сегодня — время, этот год — ДД.ММ, иначе ДД.ММ.ГГ. */
fun formatListDate(epochSeconds: Long, zone: ZoneId = ZoneId.systemDefault(), now: Instant = Instant.now()): String {
    if (epochSeconds <= 0) return ""
    val d = Instant.ofEpochSecond(epochSeconds).atZone(zone)
    val today = now.atZone(zone)
    return when {
        d.toLocalDate() == today.toLocalDate() -> d.format(TIME)
        d.year == today.year -> d.format(DAY)
        else -> d.format(FULL)
    }
}
