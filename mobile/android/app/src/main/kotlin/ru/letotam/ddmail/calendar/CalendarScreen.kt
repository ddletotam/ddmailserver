package ru.letotam.ddmail.calendar

import androidx.compose.ui.text.style.TextOverflow
import ru.letotam.ddmail.ui.BurgerButton
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxHeight
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.compose.foundation.pager.HorizontalPager
import androidx.compose.foundation.pager.rememberPagerState
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.ArrowBack
import androidx.compose.material.icons.automirrored.filled.KeyboardArrowLeft
import androidx.compose.material.icons.automirrored.filled.KeyboardArrowRight
import androidx.compose.material.icons.filled.Add
import androidx.compose.material.icons.filled.CalendarViewDay
import androidx.compose.material.icons.filled.CalendarViewMonth
import androidx.compose.material.icons.filled.CalendarViewWeek
import androidx.compose.material.icons.filled.Close
import androidx.compose.material.icons.filled.GridView
import androidx.compose.material.icons.filled.Layers
import androidx.compose.material.icons.filled.Lock
import androidx.compose.material.icons.filled.Star
import androidx.compose.material.icons.filled.StarBorder
import androidx.compose.material.icons.filled.Today
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Checkbox
import androidx.compose.material3.CheckboxDefaults
import androidx.compose.material3.FloatingActionButton
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.SegmentedButton
import androidx.compose.material3.SegmentedButtonDefaults
import androidx.compose.material3.SingleChoiceSegmentedButtonRow
import androidx.compose.material3.SnackbarHost
import androidx.compose.material3.SnackbarHostState
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.VerticalDivider
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.derivedStateOf
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.runtime.snapshotFlow
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.platform.LocalConfiguration
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.compose.ui.zIndex
import androidx.lifecycle.compose.LifecycleResumeEffect
import kotlinx.coroutines.launch
import java.time.LocalDate
import java.time.LocalDateTime
import java.time.YearMonth
import java.time.temporal.ChronoUnit

private const val CENTER = 5_000

/**
 * Календарь (контракт §12). Сложен (350 dp) — как iPhone: лента месяцев с
 * точками, тап по дню — день со шкалой и полосой недели сверху. Разложен —
 * как iPad: боковая панель (мини-месяц, календари) от 840 dp и переключатель
 * День / Неделя / Месяц / Год.
 */
@Composable
fun CalendarScreen(vm: CalendarViewModel, modifier: Modifier = Modifier) {
    val width = LocalConfiguration.current.screenWidthDp
    val wide = width >= 600
    val snack = remember { SnackbarHostState() }
    val notice by vm.notice.collectAsState()
    LaunchedEffect(notice) {
        notice?.let {
            snack.showSnackbar(it)
            vm.notice.value = null
        }
    }
    LifecycleResumeEffect(Unit) {
        vm.refresh()
        onPauseOrDispose { }
    }
    Box(modifier.fillMaxSize()) {
        // Список календарей — в «Настройках → Календарь», как у десктопа:
        // первая строка раздела — только навигация по датам.
        if (wide) WideCalendar(vm, yearCols = if (width >= 840) 4 else 3)
        else CompactCalendar(vm)
        SnackbarHost(snack, Modifier.align(Alignment.BottomCenter).padding(bottom = 16.dp).zIndex(1f))
    }
    EventDetailSheet(vm)
    EventEditor(vm, wide)
}

/** Список календарей: галочка цвета календаря — видимость (только на этом телефоне). */
@Composable
fun CalendarList(vm: CalendarViewModel) {
    val cals by vm.calendars.collectAsState()
    val hidden by vm.hidden.collectAsState()
    val default by vm.defaultCalendar.collectAsState()
    if (cals.isEmpty()) Text("Календарей нет", color = MaterialTheme.colorScheme.onSurfaceVariant, modifier = Modifier.padding(8.dp))
    cals.forEach { c ->
        val color = CalendarColors.of(c, c.id)
        Row(Modifier.fillMaxWidth().clickable { vm.toggleCalendar(c.id) }.padding(vertical = 2.dp), verticalAlignment = Alignment.CenterVertically) {
            Checkbox(c.id !in hidden, { vm.toggleCalendar(c.id) }, colors = CheckboxDefaults.colors(checkedColor = color, uncheckedColor = color))
            Text(c.name.ifBlank { "Календарь" }, Modifier.weight(1f), maxLines = 1, fontSize = 15.sp)
            if (!c.canWrite) Icon(Icons.Filled.Lock, "Только чтение", Modifier.size(16.dp).padding(end = 0.dp), tint = MaterialTheme.colorScheme.outline)
            // Звезда — куда пишутся новые события; без неё форма заставит выбрать.
            else IconButton(onClick = { vm.setDefault(c.id) }, Modifier.size(36.dp)) {
                val on = default == c.id
                Icon(
                    if (on) Icons.Filled.Star else Icons.Filled.StarBorder,
                    if (on) "Календарь по умолчанию" else "Сделать календарём по умолчанию",
                    tint = if (on) Color(0xFFF59E0B) else MaterialTheme.colorScheme.outline,
                )
            }
        }
    }
}

// ── iPhone ──

@Composable
private fun CompactCalendar(vm: CalendarViewModel) {
    val view by vm.view.collectAsState()
    if (view == CalView.Day || view == CalView.Week) CompactDay(vm) else CompactMonths(vm)
}

@Composable
private fun CompactMonths(vm: CalendarViewModel) {
    val selected by vm.selected.collectAsState()
    val index by vm.index.collectAsState()
    val loading by vm.loading.collectAsState()
    val base = remember { YearMonth.now() }
    val list = rememberLazyListState(initialFirstVisibleItemIndex = CENTER + ChronoUnit.MONTHS.between(base, YearMonth.from(selected)).toInt())
    val visible by remember { derivedStateOf { base.plusMonths((list.firstVisibleItemIndex - CENTER).toLong()) } }
    val scope = rememberCoroutineScope()
    Column(Modifier.fillMaxSize()) {
        // Первая строка — как у десктопа: бургер, ‹ Сегодня ›, период, «Новое событие».
        DateBar(
            title = narrowMonthTitle(visible),
            onPrev = { scope.launch { list.animateScrollToItem((list.firstVisibleItemIndex - 1).coerceAtLeast(0)) } },
            onToday = {
                vm.today()
                scope.launch { list.animateScrollToItem(CENTER) }
            },
            onNext = { scope.launch { list.animateScrollToItem(list.firstVisibleItemIndex + 1) } },
            onNew = { vm.newEvent() },
        )
        WeekdayHeader(Modifier.padding(horizontal = 4.dp))
        HorizontalDivider()
        if (loading) LinearProgressIndicator(Modifier.fillMaxWidth().height(2.dp)) else Spacer(Modifier.height(2.dp))
        LazyColumn(state = list, modifier = Modifier.fillMaxSize().padding(horizontal = 4.dp)) {
            items(CENTER * 2, key = { it }) { i ->
                val ym = base.plusMonths((i - CENTER).toLong())
                CompactMonth(ym, vm, index[ym], selected) { d ->
                    vm.select(d)
                    vm.setView(CalView.Day)
                }
            }
        }
    }
}

@Composable
private fun CompactDay(vm: CalendarViewModel) {
    val selected by vm.selected.collectAsState()
    val index by vm.index.collectAsState()
    val today = remember { LocalDate.now() }
    val days = rememberPagerState(initialPage = CENTER + ChronoUnit.DAYS.between(today, selected).toInt()) { CENTER * 2 }
    val thisWeek = remember { weekStart(today) }
    val weeks = rememberPagerState(initialPage = CENTER + (ChronoUnit.WEEKS.between(thisWeek, weekStart(selected))).toInt()) { CENTER * 2 }
    val scope = rememberCoroutineScope()
    // Свайп дня — выбор дня; выбор дня (тап в полосе недели, «сегодня») — к нему.
    LaunchedEffect(days) {
        snapshotFlow { days.settledPage }.collect { p -> vm.select(today.plusDays((p - CENTER).toLong())) }
    }
    LaunchedEffect(selected) {
        val p = CENTER + ChronoUnit.DAYS.between(today, selected).toInt()
        if (days.currentPage != p) days.animateScrollToPage(p)
        val w = CENTER + ChronoUnit.WEEKS.between(thisWeek, weekStart(selected)).toInt()
        if (weeks.currentPage != w) weeks.animateScrollToPage(w)
        vm.ensure(YearMonth.from(selected))
    }
    androidx.activity.compose.BackHandler { vm.setView(CalView.Month) }
    Column(Modifier.fillMaxSize()) {
        // Заголовок периода — месяц: касание (и «Назад») ведёт к месяцам.
        DateBar(
            title = narrowMonthTitle(YearMonth.from(selected)),
            onTitle = { vm.setView(CalView.Month) },
            onPrev = { vm.select(selected.minusDays(1)) },
            onToday = { vm.today() },
            onNext = { vm.select(selected.plusDays(1)) },
            onNew = { vm.newEvent() },
        )
        WeekdayHeader(Modifier.padding(horizontal = 4.dp))
        HorizontalPager(weeks, Modifier.fillMaxWidth().height(40.dp)) { p ->
            val ws = thisWeek.plusWeeks((p - CENTER).toLong())
            Row(Modifier.fillMaxSize().padding(horizontal = 4.dp)) {
                (0..6).forEach { i ->
                    val d = ws.plusDays(i.toLong())
                    val isToday = d == today
                    val isSel = d == selected
                    Box(Modifier.weight(1f).fillMaxHeight().clickable { vm.select(d) }, contentAlignment = Alignment.Center) {
                        Box(
                            Modifier.size(34.dp).clip(CircleShape).background(
                                when {
                                    isSel && isToday -> NowRed
                                    isSel -> MaterialTheme.colorScheme.onSurface
                                    else -> Color.Transparent
                                },
                            ),
                            contentAlignment = Alignment.Center,
                        ) {
                            Text(
                                "${d.dayOfMonth}", fontSize = 18.sp,
                                fontWeight = if (isSel || isToday) FontWeight.SemiBold else FontWeight.Normal,
                                color = when {
                                    isSel -> Color.White
                                    isToday -> NowRed
                                    else -> MaterialTheme.colorScheme.onSurface
                                },
                            )
                        }
                    }
                }
            }
        }
        Text(longDate(selected), Modifier.fillMaxWidth().padding(vertical = 6.dp), fontSize = 14.sp, textAlign = androidx.compose.ui.text.style.TextAlign.Center)
        HorizontalDivider()
        HorizontalPager(days, Modifier.fillMaxSize(), beyondViewportPageCount = 1) { p ->
            val d = today.plusDays((p - CENTER).toLong())
            LaunchedEffect(d) { vm.ensure(YearMonth.from(d)) }
            Timeline(
                days = listOf(d),
                occsOf = { day -> index[YearMonth.from(day)]?.get(day).orEmpty() },
                onOpen = vm::open, onMove = vm::move, onCreate = { at -> vm.newEvent(at) },
                modifier = Modifier.fillMaxSize(),
            )
        }
    }
}

// ── iPad ──

@Composable
private fun WideCalendar(vm: CalendarViewModel, yearCols: Int) {
    val view by vm.view.collectAsState()
    val selected by vm.selected.collectAsState()
    val index by vm.index.collectAsState()
    val loading by vm.loading.collectAsState()
    fun step(sign: Long) = vm.select(
        when (view) {
            CalView.Day -> selected.plusDays(sign)
            CalView.Week -> selected.plusWeeks(sign)
            CalView.Month -> selected.plusMonths(sign)
            CalView.Year -> selected.plusYears(sign)
        },
    )
    val title = when (view) {
        // День недели уже в шапке колонки («чт 12»): в заголовке только дата, иначе
        // он обрезается переключателем вида.
        CalView.Day -> "${selected.dayOfMonth} ${MONTHS_GEN[selected.monthValue - 1]} ${selected.year}"
        CalView.Week, CalView.Month -> monthTitle(YearMonth.from(selected))
        CalView.Year -> "${selected.year}"
    }
    Column(Modifier.fillMaxSize()) {
        // Первая строка — как у десктопа: бургер, ‹ Сегодня ›, период; справа
        // вид (день/неделя/месяц/год) и «Новое событие».
        Row(Modifier.fillMaxWidth().padding(start = 4.dp, end = 12.dp, top = 4.dp, bottom = 4.dp), verticalAlignment = Alignment.CenterVertically) {
            BurgerButton()
            IconButton(onClick = { step(-1) }) { Icon(Icons.AutoMirrored.Filled.KeyboardArrowLeft, "Назад") }
            TextButton(onClick = { vm.today() }) { Text("Сегодня", fontWeight = FontWeight.Bold) }
            IconButton(onClick = { step(1) }) { Icon(Icons.AutoMirrored.Filled.KeyboardArrowRight, "Вперёд") }
            Spacer(Modifier.width(8.dp))
            Text(title, fontSize = 20.sp, fontWeight = FontWeight.Bold, maxLines = 1, overflow = TextOverflow.Ellipsis, modifier = Modifier.weight(1f))
            // Переключатель вида узкий: на 712 dp заголовку периода нужно место.
            SingleChoiceSegmentedButtonRow(Modifier.width(184.dp)) {
                // По убыванию охвата, как у десктопа (mobile-layout.md): Год / Месяц / Неделя / День.
                val opts = listOf(
                    CalView.Year to Icons.Filled.GridView, CalView.Month to Icons.Filled.CalendarViewMonth,
                    CalView.Week to Icons.Filled.CalendarViewWeek, CalView.Day to Icons.Filled.CalendarViewDay,
                )
                val names = mapOf(CalView.Day to "День", CalView.Week to "Неделя", CalView.Month to "Месяц", CalView.Year to "Год")
                opts.forEachIndexed { i, (v, icon) ->
                    SegmentedButton(view == v, { vm.setView(v) }, SegmentedButtonDefaults.itemShape(i, opts.size), icon = {}) {
                        Icon(icon, names[v])
                    }
                }
            }
            Spacer(Modifier.width(8.dp))
            FloatingActionButton(onClick = { vm.newEvent() }, Modifier.size(44.dp), containerColor = NowRed, contentColor = Color.White) {
                Icon(Icons.Filled.Add, "Новое событие")
            }
        }
        if (loading) LinearProgressIndicator(Modifier.fillMaxWidth().height(2.dp)) else Spacer(Modifier.height(2.dp))
        HorizontalDivider()
        val occsOf = { d: LocalDate -> index[YearMonth.from(d)]?.get(d).orEmpty() }
        when (view) {
            CalView.Day -> {
                LaunchedEffect(selected) { vm.ensure(YearMonth.from(selected)) }
                Timeline(listOf(selected), occsOf, vm::open, vm::move, { vm.newEvent(it) }, Modifier.fillMaxSize(), hourHeight = 52.dp, showDayHeader = true)
            }
            CalView.Week -> {
                val ws = weekStart(selected)
                val wd = (0..6).map { ws.plusDays(it.toLong()) }
                LaunchedEffect(ws) { wd.map { YearMonth.from(it) }.distinct().forEach(vm::ensure) }
                Timeline(wd, occsOf, vm::open, vm::move, { vm.newEvent(it) }, Modifier.fillMaxSize(), hourHeight = 48.dp, showDayHeader = true)
            }
            CalView.Month -> {
                val ym = YearMonth.from(selected)
                BigMonth(
                    ym, vm, index[ym], selected,
                    onDay = { d -> if (d == selected) vm.setView(CalView.Day) else vm.select(d) },
                    onOpen = vm::open,
                    onCreate = { d -> vm.select(d); vm.newEvent(LocalDateTime.of(d, java.time.LocalTime.of(9, 0))) },
                    // «ещё N» — сразу этот день целиком (вид «День»), а не только выбор дня.
                    onMore = { d -> vm.select(d); vm.setView(CalView.Day) },
                    modifier = Modifier.fillMaxSize(),
                )
            }
            CalView.Year -> YearView(
                selected.year, selected, yearCols,
                onDay = { d -> vm.select(d); vm.setView(CalView.Day) },
                onMonth = { ym -> vm.select(ym.atDay(1)); vm.setView(CalView.Month) },
            )
        }
    }
}

/**
 * Первая строка календаря на узком экране: бургер, ‹ Сегодня ›, заголовок
 * периода, «Новое событие» — та же, что у десктопа и разложенного экрана.
 * Кнопки по 40 dp: на 350 dp сложенного Mate XT должно влезть всё.
 */
@Composable
private fun DateBar(title: String, onPrev: () -> Unit, onToday: () -> Unit, onNext: () -> Unit, onNew: () -> Unit, onTitle: (() -> Unit)? = null) {
    Row(Modifier.fillMaxWidth().padding(start = 4.dp, end = 4.dp, top = 4.dp), verticalAlignment = Alignment.CenterVertically) {
        BurgerButton(Modifier.size(40.dp))
        IconButton(onClick = onPrev, modifier = Modifier.size(40.dp)) { Icon(Icons.AutoMirrored.Filled.KeyboardArrowLeft, "Назад") }
        TextButton(onClick = onToday, contentPadding = androidx.compose.foundation.layout.PaddingValues(horizontal = 6.dp)) {
            Text("Сегодня", fontWeight = FontWeight.Bold, color = MaterialTheme.colorScheme.onSurface)
        }
        IconButton(onClick = onNext, modifier = Modifier.size(40.dp)) { Icon(Icons.AutoMirrored.Filled.KeyboardArrowRight, "Вперёд") }
        Text(
            title,
            fontSize = 17.sp,
            fontWeight = FontWeight.Bold,
            color = NowRed,
            maxLines = 1,
            overflow = TextOverflow.Ellipsis,
            modifier = Modifier.weight(1f).padding(start = 4.dp).then(if (onTitle != null) Modifier.clickable(onClick = onTitle) else Modifier),
        )
        IconButton(onClick = onNew, modifier = Modifier.size(40.dp)) { Icon(Icons.Filled.Add, "Новое событие", tint = NowRed) }
    }
}

/**
 * Заголовок месяца на сложенном экране (350 dp): год только если он не текущий —
 * «Ноябрь 2026» рядом с ‹ Сегодня › и «+» обрезался до «Ноябрь 20…».
 */
private fun narrowMonthTitle(ym: YearMonth) = monthTitle(ym, withYear = ym.year != LocalDate.now().year)
