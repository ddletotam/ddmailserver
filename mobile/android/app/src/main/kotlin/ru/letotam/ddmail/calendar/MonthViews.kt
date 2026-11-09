package ru.letotam.ddmail.calendar

import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.clickable
import androidx.compose.foundation.combinedClickable
import androidx.compose.foundation.ExperimentalFoundationApi
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.aspectRatio
import androidx.compose.foundation.layout.fillMaxHeight
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.grid.GridCells
import androidx.compose.foundation.lazy.grid.LazyVerticalGrid
import androidx.compose.foundation.lazy.grid.items
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import java.time.DayOfWeek
import java.time.LocalDate
import java.time.YearMonth
import java.time.temporal.TemporalAdjusters

val MONTHS = listOf("Январь", "Февраль", "Март", "Апрель", "Май", "Июнь", "Июль", "Август", "Сентябрь", "Октябрь", "Ноябрь", "Декабрь")
val MONTHS_GEN = listOf("января", "февраля", "марта", "апреля", "мая", "июня", "июля", "августа", "сентября", "октября", "ноября", "декабря")
val WEEKDAYS_1 = listOf("П", "В", "С", "Ч", "П", "С", "В")
val WEEKDAYS_FULL = listOf("понедельник", "вторник", "среда", "четверг", "пятница", "суббота", "воскресенье")

fun monthTitle(ym: YearMonth, withYear: Boolean = true) = MONTHS[ym.monthValue - 1] + if (withYear) " ${ym.year}" else ""

/** Понедельник недели, в которой [d]. */
fun weekStart(d: LocalDate): LocalDate = d.with(TemporalAdjusters.previousOrSame(DayOfWeek.MONDAY))

/** Сетка месяца: 6 недель с понедельника — дни соседних месяцев приглушены. */
fun monthCells(ym: YearMonth): List<LocalDate> {
    val first = weekStart(ym.atDay(1))
    val weeks = ((java.time.temporal.ChronoUnit.DAYS.between(first, ym.atEndOfMonth()) / 7) + 1).toInt()
    return (0 until weeks * 7).map { first.plusDays(it.toLong()) }
}

@Composable
fun WeekdayHeader(modifier: Modifier = Modifier) {
    Row(modifier.fillMaxWidth().padding(vertical = 4.dp)) {
        WEEKDAYS_1.forEachIndexed { i, w ->
            Text(
                w, Modifier.weight(1f), fontSize = 12.sp, textAlign = androidx.compose.ui.text.style.TextAlign.Center,
                color = if (i >= 5) MaterialTheme.colorScheme.outline else MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
    }
}

/**
 * Месяц iPhone: числа и под ними точки событий (по цвету календаря, до трёх).
 * Дни чужого месяца не рисуются — месяцы идут лентой один под другим.
 */
@Composable
fun CompactMonth(ym: YearMonth, vm: CalendarViewModel, index: Map<LocalDate, List<Occ>>?, selected: LocalDate?, onDay: (LocalDate) -> Unit) {
    LaunchedEffect(ym) { vm.ensure(ym) }
    val today = LocalDate.now()
    Column(Modifier.fillMaxWidth().padding(bottom = 12.dp)) {
        val firstCol = ym.atDay(1).dayOfWeek.value - 1
        val title = @Composable { m: Modifier ->
            Text(
                MONTHS[ym.monthValue - 1], fontSize = 20.sp, fontWeight = FontWeight.Bold, maxLines = 1, softWrap = false,
                color = if (YearMonth.from(today) == ym) NowRed else MaterialTheme.colorScheme.onSurface,
                modifier = m.padding(start = 10.dp, end = 10.dp, bottom = 4.dp),
            )
        }
        // Название месяца — над столбцом его первого числа, как в iOS; у месяца,
        // что начинается в сб/вс, справа места на слово нет — прижимаем к краю.
        if (firstCol >= 5) Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.End) { title(Modifier) }
        else Row(Modifier.fillMaxWidth()) {
            Spacer(Modifier.weight(firstCol.toFloat() + 0.0001f))
            title(Modifier.weight(7f - firstCol))
        }
        HorizontalDivider(color = MaterialTheme.colorScheme.outlineVariant.copy(alpha = 0.5f))
        monthCells(ym).chunked(7).forEach { week ->
            Row(Modifier.fillMaxWidth().height(56.dp)) {
                week.forEach { d ->
                    Box(Modifier.weight(1f).fillMaxHeight(), contentAlignment = Alignment.TopCenter) {
                        if (YearMonth.from(d) == ym) DayCell(d, today, selected, index?.get(d).orEmpty(), onDay)
                    }
                }
            }
        }
    }
}

@Composable
private fun DayCell(d: LocalDate, today: LocalDate, selected: LocalDate?, occs: List<Occ>, onDay: (LocalDate) -> Unit) {
    val isToday = d == today
    val isSel = d == selected && !isToday
    Column(
        Modifier.fillMaxSize().clickable { onDay(d) }.padding(top = 4.dp),
        horizontalAlignment = Alignment.CenterHorizontally,
    ) {
        Box(
            Modifier.size(30.dp).clip(CircleShape).background(
                when {
                    isToday -> NowRed
                    isSel -> MaterialTheme.colorScheme.onSurface
                    else -> Color.Transparent
                },
            ),
            contentAlignment = Alignment.Center,
        ) {
            Text(
                "${d.dayOfMonth}", fontSize = 17.sp,
                fontWeight = if (isToday || isSel) FontWeight.SemiBold else FontWeight.Normal,
                color = when {
                    isToday || isSel -> Color.White
                    d.dayOfWeek.value >= 6 -> MaterialTheme.colorScheme.outline
                    else -> MaterialTheme.colorScheme.onSurface
                },
            )
        }
        Row(Modifier.padding(top = 3.dp), horizontalArrangement = Arrangement.spacedBy(2.dp)) {
            occs.map { it.color }.distinct().take(3).forEach { c ->
                Box(Modifier.size(5.dp).clip(CircleShape).background(c))
            }
        }
    }
}

/** Мини-месяц — боковая панель iPad и вид «Год». */
@Composable
fun MiniMonth(ym: YearMonth, selected: LocalDate?, onDay: (LocalDate) -> Unit, modifier: Modifier = Modifier, title: Boolean = true, onTitle: (() -> Unit)? = null, marks: Set<LocalDate> = emptySet()) {
    val today = LocalDate.now()
    Column(modifier) {
        if (title) Text(
            MONTHS[ym.monthValue - 1], fontSize = 15.sp, fontWeight = FontWeight.SemiBold,
            color = if (YearMonth.from(today) == ym) NowRed else MaterialTheme.colorScheme.onSurface,
            modifier = Modifier.padding(start = 4.dp, bottom = 2.dp).then(if (onTitle != null) Modifier.clickable { onTitle() } else Modifier),
        )
        WeekdayHeader()
        monthCells(ym).chunked(7).forEach { week ->
            Row(Modifier.fillMaxWidth()) {
                week.forEach { d ->
                    val inMonth = YearMonth.from(d) == ym
                    val isToday = d == today
                    val isSel = d == selected
                    Box(Modifier.weight(1f).aspectRatio(1f).clickable(enabled = inMonth) { onDay(d) }, contentAlignment = Alignment.Center) {
                        if (inMonth) {
                            Box(
                                Modifier.fillMaxSize(0.82f).clip(CircleShape).background(
                                    when {
                                        isToday -> NowRed
                                        isSel -> MaterialTheme.colorScheme.onSurface.copy(alpha = 0.85f)
                                        else -> Color.Transparent
                                    },
                                ),
                                contentAlignment = Alignment.Center,
                            ) {
                                Text(
                                    "${d.dayOfMonth}", fontSize = 12.sp,
                                    color = if (isToday || isSel) Color.White else MaterialTheme.colorScheme.onSurface,
                                    fontWeight = if (d in marks) FontWeight.Bold else FontWeight.Normal,
                                )
                            }
                        }
                    }
                }
            }
        }
    }
}

/** Месяц iPad: сетка с названиями событий в ячейках. Долгое нажатие на дне — новое событие. */
@OptIn(ExperimentalFoundationApi::class)
@Composable
fun BigMonth(ym: YearMonth, vm: CalendarViewModel, index: Map<LocalDate, List<Occ>>?, selected: LocalDate, onDay: (LocalDate) -> Unit, onOpen: (Occ) -> Unit, onCreate: (LocalDate) -> Unit, modifier: Modifier = Modifier) {
    LaunchedEffect(ym) {
        vm.ensure(ym)
        vm.ensure(ym.minusMonths(1))
        vm.ensure(ym.plusMonths(1))
    }
    val today = LocalDate.now()
    val cells = monthCells(ym)
    val all = vm.index.value
    Column(modifier) {
        WeekdayHeader()
        HorizontalDivider()
        cells.chunked(7).forEach { week ->
            Row(Modifier.fillMaxWidth().weight(1f)) {
                week.forEach { d ->
                    val inMonth = YearMonth.from(d) == ym
                    val occs = (if (inMonth) index else all[YearMonth.from(d)])?.get(d).orEmpty()
                    Column(
                        Modifier.weight(1f).fillMaxHeight()
                            .border(0.25.dp, MaterialTheme.colorScheme.outlineVariant)
                            .background(if (d == selected) MaterialTheme.colorScheme.primaryContainer.copy(alpha = 0.35f) else Color.Transparent)
                            .combinedClickable(onClick = { onDay(d) }, onLongClick = { onCreate(d) })
                            .padding(2.dp),
                    ) {
                        Box(Modifier.fillMaxWidth(), contentAlignment = Alignment.TopEnd) {
                            Box(
                                Modifier.size(24.dp).clip(CircleShape).background(if (d == today) NowRed else Color.Transparent),
                                contentAlignment = Alignment.Center,
                            ) {
                                Text(
                                    "${d.dayOfMonth}", fontSize = 13.sp,
                                    color = when {
                                        d == today -> Color.White
                                        !inMonth -> MaterialTheme.colorScheme.outline.copy(alpha = 0.6f)
                                        else -> MaterialTheme.colorScheme.onSurface
                                    },
                                    fontWeight = if (d == today) FontWeight.Bold else FontWeight.Normal,
                                )
                            }
                        }
                        val max = 4
                        occs.take(max).forEach { o -> MonthChip(o, onOpen) }
                        if (occs.size > max) Text("ещё ${occs.size - max}", fontSize = 10.sp, color = MaterialTheme.colorScheme.onSurfaceVariant, modifier = Modifier.padding(start = 2.dp))
                    }
                }
            }
        }
    }
}

@Composable
private fun MonthChip(o: Occ, onOpen: (Occ) -> Unit) {
    val zone = java.time.ZoneId.systemDefault()
    Row(
        Modifier.fillMaxWidth().padding(vertical = 0.5.dp).clip(RoundedCornerShape(3.dp))
            .background(if (o.allDay) o.color.copy(alpha = 0.25f) else Color.Transparent)
            .clickable { onOpen(o) }.padding(horizontal = 2.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        if (!o.allDay) {
            Box(Modifier.size(6.dp).clip(CircleShape).background(o.color))
            Spacer(Modifier.width(3.dp))
        }
        Text(
            o.ev.summary.ifBlank { "(без названия)" }, fontSize = 11.sp, maxLines = 1, overflow = TextOverflow.Ellipsis,
            modifier = Modifier.weight(1f),
        )
        if (!o.allDay) Text(
            o.startTime(zone).let { "%02d:%02d".format(it.hour, it.minute) }, fontSize = 10.sp,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
    }
}

/** Год iPad: 12 мини-месяцев; тап по названию — месяц, по числу — день. */
@Composable
fun YearView(year: Int, selected: LocalDate, columns: Int, onDay: (LocalDate) -> Unit, onMonth: (YearMonth) -> Unit, modifier: Modifier = Modifier) {
    LazyVerticalGrid(GridCells.Fixed(columns), modifier.fillMaxSize().padding(horizontal = 8.dp), horizontalArrangement = Arrangement.spacedBy(16.dp), verticalArrangement = Arrangement.spacedBy(12.dp)) {
        items((1..12).map { YearMonth.of(year, it) }) { ym ->
            MiniMonth(ym, selected, onDay, onTitle = { onMonth(ym) })
        }
    }
}
