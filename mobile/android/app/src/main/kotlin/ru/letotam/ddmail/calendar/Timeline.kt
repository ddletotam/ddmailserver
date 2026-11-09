package ru.letotam.ddmail.calendar

import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.clickable
import androidx.compose.foundation.gestures.detectDragGesturesAfterLongPress
import androidx.compose.foundation.gestures.detectTapGestures
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.BoxWithConstraints
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxHeight
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.offset
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.produceState
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.alpha
import androidx.compose.ui.draw.clip
import androidx.compose.ui.draw.clipToBounds
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.input.pointer.pointerInput
import androidx.compose.ui.platform.LocalDensity
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.Dp
import androidx.compose.ui.unit.IntOffset
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import kotlinx.coroutines.delay
import java.time.LocalDate
import java.time.LocalDateTime
import java.time.ZoneId
import java.time.format.DateTimeFormatter
import kotlin.math.roundToInt

private val HHMM = DateTimeFormatter.ofPattern("HH:mm")
private const val SNAP_MIN = 15
private const val DAY_MIN = 24 * 60

/** Красная линия «сейчас» и сегодняшнее число — как в iOS. */
val NowRed = Color(0xFFE5484D)

/** Блок на шкале: колонка внутри дня при пересечениях. */
private data class Placed(val occ: Occ, val day: Int, val startMin: Int, val endMin: Int, val col: Int, val cols: Int)

/**
 * Раскладка пересекающихся: кластер цепочки пересечений делится на колонки,
 * каждое событие встаёт в первую свободную.
 */
private fun layoutDay(day: Int, date: LocalDate, occs: List<Occ>, zone: ZoneId): List<Placed> {
    val items = occs.filter { !it.allDay }.map { o ->
        val dayStart = date.atStartOfDay(zone).toInstant().toEpochMilli()
        val s = ((o.start - dayStart) / 60_000).toInt().coerceIn(0, DAY_MIN)
        val e = ((o.end - dayStart) / 60_000).toInt().coerceIn(s + 15, DAY_MIN).coerceAtLeast(s + 15)
        Triple(o, s, e)
    }.sortedWith(compareBy({ it.second }, { -it.third }))
    val out = mutableListOf<Placed>()
    var cluster = mutableListOf<Triple<Occ, Int, Int>>()
    var clusterEnd = -1
    fun flush() {
        if (cluster.isEmpty()) return
        val colEnds = mutableListOf<Int>()
        val assigned = cluster.map { t ->
            val c = colEnds.indexOfFirst { it <= t.second }.let { if (it < 0) { colEnds += t.third; colEnds.size - 1 } else { colEnds[it] = t.third; it } }
            t to c
        }
        assigned.forEach { (t, c) -> out += Placed(t.first, day, t.second, t.third, c, colEnds.size) }
        cluster = mutableListOf()
    }
    for (t in items) {
        if (t.second >= clusterEnd) flush()
        cluster += t
        clusterEnd = maxOf(clusterEnd, t.third)
    }
    flush()
    return out
}

/**
 * Шкала времени для одного или нескольких дней (День / Неделя). Тап по
 * событию — карточка; долгое нажатие на событии записываемого календаря —
 * перенос (шаг 15 мин, в неделе — и между днями), за нижнюю кромку —
 * растяжение; долгое нажатие на пустом месте — новое событие там.
 */
@Composable
fun Timeline(
    days: List<LocalDate>,
    occsOf: (LocalDate) -> List<Occ>,
    onOpen: (Occ) -> Unit,
    onMove: (Occ, Long, Long) -> Unit,
    onCreate: (LocalDateTime) -> Unit,
    modifier: Modifier = Modifier,
    hourHeight: Dp = 56.dp,
    showDayHeader: Boolean = false,
) {
    val zone = ZoneId.systemDefault()
    val density = LocalDensity.current
    val hourPx = with(density) { hourHeight.toPx() }
    val minPx = hourPx / 60f
    val scroll = rememberScrollState()
    // Открылись — к рабочему утру или к «сейчас», если сегодня на экране.
    LaunchedEffect(days.firstOrNull()) {
        val today = LocalDate.now()
        val target = if (today in days) (LocalDateTime.now().hour - 1).coerceAtLeast(0) else 8
        scroll.scrollTo((target * hourPx).roundToInt())
    }
    val now by produceState(System.currentTimeMillis()) {
        while (true) {
            delay(30_000)
            value = System.currentTimeMillis()
        }
    }
    val gutter = 44.dp
    Column(modifier) {
        if (showDayHeader) DayHeaderRow(days, gutter)
        AllDayRow(days, occsOf, onOpen, gutter)
        HorizontalDivider()
        // Обрезка: в листаемом пейджере линия «сейчас» соседнего дня торчала бы на край.
        BoxWithConstraints(Modifier.fillMaxSize().clipToBounds().verticalScroll(scroll)) {
            val totalH = hourHeight * 24
            val colW = (maxWidth - gutter) / days.size
            val colWpx = with(density) { colW.toPx() }
            Box(Modifier.fillMaxWidth().height(totalH)) {
                // Сетка часов.
                for (h in 0..23) {
                    Box(Modifier.offset(y = hourHeight * h).fillMaxWidth().height(hourHeight)) {
                        if (h > 0) Text(
                            "%02d:00".format(h), fontSize = 11.sp, color = MaterialTheme.colorScheme.outline,
                            modifier = Modifier.width(gutter).offset(y = (-7).dp).padding(end = 6.dp),
                            textAlign = androidx.compose.ui.text.style.TextAlign.End,
                        )
                        HorizontalDivider(Modifier.padding(start = gutter), color = MaterialTheme.colorScheme.outlineVariant.copy(alpha = 0.6f))
                    }
                }
                // Пустое место: долгое нажатие — новое событие на этом получасе.
                Box(
                    Modifier.padding(start = gutter).fillMaxSize().pointerInput(days) {
                        detectTapGestures(onLongPress = { p ->
                            val di = (p.x / colWpx).toInt().coerceIn(0, days.size - 1)
                            val m = ((p.y / minPx).toInt() / 30) * 30
                            onCreate(days[di].atStartOfDay().plusMinutes(m.toLong()))
                        })
                    },
                )
                // Вертикальные разделители дней недели.
                if (days.size > 1) for (i in 1 until days.size) {
                    Box(Modifier.offset(x = gutter + colW * i).width(0.5.dp).fillMaxHeight().background(MaterialTheme.colorScheme.outlineVariant))
                }
                days.forEachIndexed { di, date ->
                    for (p in layoutDay(di, date, occsOf(date), zone)) {
                        EventBlock(p, gutter, colW, hourHeight, minPx, colWpx, days, onOpen, onMove)
                    }
                    if (date == LocalDate.now()) {
                        val m = java.time.Instant.ofEpochMilli(now).atZone(zone).let { it.hour * 60 + it.minute }
                        Box(Modifier.offset(x = gutter + colW * di - 4.dp, y = hourHeight * (m / 60f) - 4.dp).size(8.dp).clip(CircleShape).background(NowRed))
                        Box(Modifier.offset(x = gutter + colW * di, y = hourHeight * (m / 60f) - 1.dp).width(colW).height(2.dp).background(NowRed))
                    }
                }
            }
        }
    }
}

@Composable
private fun EventBlock(
    p: Placed,
    gutter: Dp,
    colW: Dp,
    hourHeight: Dp,
    minPx: Float,
    colWpx: Float,
    days: List<LocalDate>,
    onOpen: (Occ) -> Unit,
    onMove: (Occ, Long, Long) -> Unit,
) {
    val o = p.occ
    val movable = o.ev.editable && !o.recurring
    var dy by remember(o.key) { mutableStateOf(0f) }
    var dx by remember(o.key) { mutableStateOf(0f) }
    var resize by remember(o.key) { mutableStateOf(false) }
    var dragging by remember(o.key) { mutableStateOf(false) }
    val w = colW / p.cols
    val top = hourHeight * (p.startMin / 60f)
    val baseH = hourHeight * ((p.endMin - p.startMin) / 60f)
    val snap = { px: Float -> ((px / minPx) / SNAP_MIN).roundToInt() * SNAP_MIN }
    val dMin = snap(dy)
    val dDay = if (days.size > 1) (dx / colWpx).roundToInt() else 0
    val h = if (resize) (baseH + hourHeight * (dMin / 60f)).coerceAtLeast(hourHeight / 4) else baseH
    val y = if (resize) top else top + hourHeight * (dMin / 60f)
    val x = gutter + colW * (p.day + if (resize) 0 else dDay) + w * p.col
    val density = LocalDensity.current
    val blockHpx = with(density) { baseH.toPx() }
    Box(
        Modifier
            .offset { IntOffset(x.roundToPx(), y.roundToPx()) }
            .width(w)
            .height(h)
            .padding(1.dp)
            .clip(RoundedCornerShape(5.dp))
            .background(o.color.copy(alpha = if (dragging) 0.45f else 0.22f))
            .then(if (dragging) Modifier.border(1.5.dp, o.color, RoundedCornerShape(5.dp)) else Modifier)
            .clickable { onOpen(o) }
            .then(
                if (!movable) Modifier else Modifier.pointerInput(o.key, days.size) {
                    detectDragGesturesAfterLongPress(
                        onDragStart = { pos ->
                            dragging = true
                            // Нижняя кромка (12 dp) тянет конец, остальное — переносит целиком.
                            resize = pos.y > blockHpx - 12.dp.toPx() && blockHpx > 24.dp.toPx()
                        },
                        onDragEnd = {
                            val m = snap(dy)
                            val dd = if (days.size > 1 && !resize) (dx / colWpx).roundToInt() else 0
                            dragging = false
                            if (m != 0 || dd != 0) {
                                val shift = (m.toLong() + dd * DAY_MIN.toLong()) * 60_000L
                                if (resize) onMove(o, o.start, (o.end + m * 60_000L).coerceAtLeast(o.start + SNAP_MIN * 60_000L))
                                else onMove(o, o.start + shift, o.end + shift)
                            }
                            dx = 0f; dy = 0f; resize = false
                        },
                        onDragCancel = { dragging = false; dx = 0f; dy = 0f; resize = false },
                        onDrag = { change, amount ->
                            change.consume()
                            dy += amount.y
                            dx += amount.x
                        },
                    )
                },
            ),
    ) {
        Box(Modifier.width(3.dp).fillMaxHeight().background(o.color))
        Column(Modifier.padding(start = 6.dp, end = 3.dp, top = 2.dp)) {
            Text(
                o.ev.summary.ifBlank { "(без названия)" }, fontSize = 12.sp, fontWeight = FontWeight.SemiBold,
                maxLines = if (h > 40.dp) 2 else 1, overflow = TextOverflow.Ellipsis, lineHeight = 14.sp,
            )
            if (h > 34.dp) {
                val zone = ZoneId.systemDefault()
                val shown = if (dragging) {
                    val shift = if (resize) 0L else (dMin.toLong() + dDay * DAY_MIN) * 60_000L
                    val endShift = if (resize) dMin * 60_000L else shift
                    "${java.time.Instant.ofEpochMilli(o.start + shift).atZone(zone).format(HHMM)}–${java.time.Instant.ofEpochMilli(o.end + endShift).atZone(zone).format(HHMM)}"
                } else "${o.startTime(zone).format(HHMM)}–${o.endTime(zone).format(HHMM)}"
                Text(shown, fontSize = 11.sp, color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 1)
            }
            if (h > 56.dp && o.ev.location.isNotBlank()) {
                Text(o.ev.location, fontSize = 11.sp, color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 1, overflow = TextOverflow.Ellipsis)
            }
        }
        if (movable && dragging) Box(
            Modifier.align(Alignment.BottomCenter).padding(bottom = 2.dp).width(24.dp).height(3.dp).clip(CircleShape).background(o.color).alpha(0.8f),
        )
    }
}

private val WEEKDAY_SHORT = listOf("пн", "вт", "ср", "чт", "пт", "сб", "вс")

@Composable
private fun DayHeaderRow(days: List<LocalDate>, gutter: Dp) {
    val today = LocalDate.now()
    Row(Modifier.fillMaxWidth().padding(start = gutter, top = 4.dp, bottom = 4.dp)) {
        days.forEach { d ->
            Row(Modifier.weight(1f), horizontalArrangement = androidx.compose.foundation.layout.Arrangement.Center, verticalAlignment = Alignment.CenterVertically) {
                Text(WEEKDAY_SHORT[d.dayOfWeek.value - 1] + " ", fontSize = 13.sp, color = MaterialTheme.colorScheme.onSurfaceVariant)
                Box(
                    Modifier.size(26.dp).clip(CircleShape).background(if (d == today) NowRed else Color.Transparent),
                    contentAlignment = Alignment.Center,
                ) {
                    Text("${d.dayOfMonth}", fontSize = 14.sp, fontWeight = FontWeight.SemiBold, color = if (d == today) Color.White else MaterialTheme.colorScheme.onSurface)
                }
            }
        }
    }
}

@Composable
private fun AllDayRow(days: List<LocalDate>, occsOf: (LocalDate) -> List<Occ>, onOpen: (Occ) -> Unit, gutter: Dp) {
    val perDay = days.map { d -> occsOf(d).filter { it.allDay } }
    if (perDay.all { it.isEmpty() }) return
    Row(Modifier.fillMaxWidth().padding(vertical = 2.dp)) {
        Text("весь\nдень", fontSize = 10.sp, lineHeight = 11.sp, color = MaterialTheme.colorScheme.outline, modifier = Modifier.width(gutter).padding(end = 6.dp), textAlign = androidx.compose.ui.text.style.TextAlign.End)
        perDay.forEach { list ->
            Column(Modifier.weight(1f).padding(horizontal = 1.dp)) {
                list.take(3).forEach { o -> AllDayChip(o, onOpen) }
                if (list.size > 3) Text("+${list.size - 3}", fontSize = 11.sp, color = MaterialTheme.colorScheme.onSurfaceVariant)
            }
        }
    }
}

@Composable
fun AllDayChip(o: Occ, onOpen: (Occ) -> Unit, modifier: Modifier = Modifier) {
    Text(
        o.ev.summary.ifBlank { "(без названия)" },
        fontSize = 12.sp, maxLines = 1, overflow = TextOverflow.Ellipsis,
        modifier = modifier.fillMaxWidth().padding(vertical = 1.dp).clip(RoundedCornerShape(4.dp))
            .background(o.color.copy(alpha = 0.25f)).clickable { onOpen(o) }.padding(horizontal = 4.dp, vertical = 1.dp),
    )
}
