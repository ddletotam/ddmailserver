import DDMailAPI
import DDMailCalendar
import SwiftUI

/// Календарь (контракт iOS §11, Android §12): вид — как у «Календаря» Apple.
/// iPhone — лента месяцев, тап по дню — день со шкалой времени; iPad и Мак —
/// Год / Месяц / Неделя / День.
struct CalendarScreen: View {
    @Bindable var model: CalendarModel
    @Environment(\.horizontalSizeClass) private var sizeClass
    @State private var dayShown = false
    /// Месяц ленты iPhone — смещение от текущего (позиция прокрутки).
    @State private var feedPosition: Int? = 0

    var body: some View {
        let formOpen = model.draft != nil
        let compact = sizeClass == .compact
        let seriesMove = model.seriesMove
        return NavigationStack {
            VStack(spacing: 0) {
                // Первая строка — как у десктопа: бургер, ‹ Сегодня ›, период, «+ Событие».
                SectionBar { topBar(compact: compact) }
                Divider()
                Group {
                    if compact {
                        MonthsFeed(model: model, position: $feedPosition) { d in
                            model.select(d)
                            dayShown = true
                        }
                    } else {
                        switch model.mode {
                        case .year:
                            YearView(model: model, year: model.selected.year) { d in
                                model.select(d)
                                model.mode = .day
                            } onMonth: { m in
                                model.select(Day(m.year, m.month, min(model.selected.day, m.length(model.cal))))
                                model.mode = .month
                            }
                        case .month: MonthPage(model: model)
                        case .week: WeekTimeline(model: model)
                        case .day: DayScreen(model: model)
                        }
                    }
                }
                .frame(maxHeight: .infinity)
            }
            .navigationDestination(isPresented: $dayShown) { DayScreen(model: model) }
            .sectionRoot()
        }
        .sheet(item: $model.detail) { o in EventDetailView(model: model, occ: o) }
        .sheet(isPresented: Binding(get: { formOpen }, set: { if !$0 { model.draft = nil } })) {
            EventFormView(model: model)
        }
        // Перенос повторяющегося — охват, как в форме (это и следующие / все).
        .confirmationDialog(
            "Перенести повторяющееся событие",
            isPresented: Binding(get: { seriesMove != nil }, set: { if !$0 { model.seriesMove = nil } }),
            titleVisibility: .visible,
            presenting: seriesMove
        ) { _ in
            Button("Это и следующие") { model.confirmSeriesMove(scope: "future") }
            Button("Все повторы") { model.confirmSeriesMove(scope: "all") }
            Button("Отмена", role: .cancel) { model.seriesMove = nil }
        } message: { m in
            Text("«\(m.occ.event.summary.isEmpty ? "(без названия)" : m.occ.event.summary)» → \(Self.span(m.start, m.end))")
        }
        .overlay(alignment: .bottom) { NoticeBanner(text: $model.notice) }
        .task { await model.start() }
    }

    static func span(_ s: Int64, _ e: Int64) -> String {
        let a = date(s), b = date(e)
        return "\(a.formatted(.dateTime.weekday(.abbreviated).day().month(.abbreviated))), "
            + "\(a.formatted(date: .omitted, time: .shortened))–\(b.formatted(date: .omitted, time: .shortened))"
    }

    @ViewBuilder private func topBar(compact: Bool) -> some View {
        HStack(spacing: 2) {
            Button("Назад", systemImage: "chevron.left") { step(-1, compact: compact) }
                .labelStyle(.iconOnly)
                .frame(width: 32, height: 34)
            Button("Сегодня") {
                model.select(model.today)
                feedPosition = 0
            }
            .fontWeight(.semibold)
            Button("Вперёд", systemImage: "chevron.right") { step(1, compact: compact) }
                .labelStyle(.iconOnly)
                .frame(width: 32, height: 34)
        }
        .buttonStyle(.borderless)
        .foregroundStyle(.primary)
        Text(title(compact: compact))
            .font(.headline)
            .lineLimit(1)
            .minimumScaleFactor(0.7)
            .padding(.leading, 4)
            .accessibilityIdentifier("cal.title")
        Spacer(minLength: 4)
        if !compact {
            Picker("Вид", selection: $model.mode) {
                ForEach(CalendarModel.ViewMode.allCases) { Text($0.title).tag($0) }
            }
            .pickerStyle(.segmented)
            .labelsHidden()
            .frame(width: 300)
        }
        Button("Новое событие", systemImage: "plus") { model.newEvent() }
            .labelStyle(.iconOnly)
            .font(.title3)
            .buttonStyle(.borderless)
            .frame(width: 36, height: 36)
            .disabled(model.calendars.isEmpty)
    }

    private func title(compact: Bool) -> String {
        let cal = model.cal
        if compact {
            let m = model.today.month1.adding(feedPosition ?? 0)
            return "\(cal.standaloneMonthSymbols[m.month - 1].capitalized) \(m.year)"
        }
        let d = model.selected.start(cal)
        let m = model.selected.month1
        return switch model.mode {
        case .year: "\(m.year)"
        case .month, .week: "\(cal.standaloneMonthSymbols[m.month - 1].capitalized) \(m.year)"
        case .day: d.formatted(.dateTime.weekday(.wide).day().month(.wide))
        }
    }

    private func step(_ n: Int, compact: Bool) {
        let cal = model.cal
        if compact {
            // Лента месяцев: соседний месяц (лента дорастает сама, предела нет).
            feedPosition = (feedPosition ?? 0) + n
            return
        }
        switch model.mode {
        case .day: model.select(model.selected.adding(n, cal))
        case .week: model.select(model.selected.adding(7 * n, cal))
        case .month, .year:
            let m = model.selected.month1.adding(model.mode == .year ? 12 * n : n)
            model.select(Day(m.year, m.month, min(model.selected.day, m.length(cal))))
        }
    }
}

// MARK: Месяцы

/// iPhone: лента месяцев, вертикальная прокрутка, под числом — точки событий;
/// дни недели закреплены сверху; открывается на текущем месяце. Предела нет:
/// подошли к краю прокруткой или «‹ ›» — лента дорастает ещё на два года.
private struct MonthsFeed: View {
    let model: CalendarModel
    /// Смещение показанного месяца от текущего — общее с первой строкой (‹ ›).
    @Binding var position: Int?
    let onDay: (Day) -> Void
    @State private var range = -Self.grow...Self.grow
    static let grow = 24
    /// За сколько месяцев до края лента дорастает.
    static let margin = 4

    var body: some View {
        let current = model.today.month1
        let shown = range
        VStack(spacing: 0) {
            WeekdayHeader(cal: model.cal).padding(.horizontal, 8).padding(.vertical, 6)
            Divider()
            ScrollView {
                LazyVStack(alignment: .leading, spacing: 24) {
                    ForEach(Array(shown), id: \.self) { i in
                        let m = current.adding(i)
                        VStack(alignment: .leading, spacing: 8) {
                            Text(Self.title(m, current: current, cal: model.cal))
                                .font(.title3.bold())
                                .foregroundStyle(m == current ? Color.red : .primary)
                                .padding(.horizontal, 16)
                            MonthGrid(model: model, month: m, large: false, onDay: onDay) { d in
                                model.select(d)
                                model.newEvent(at: model.cal.date(bySettingHour: 9, minute: 0, second: 0, of: d.start(model.cal)))
                            }
                        }
                        .id(i)
                        .onAppear { model.ensure(m) }
                    }
                }
                .scrollTargetLayout()
                .padding(.vertical, 12)
            }
            .scrollPosition(id: $position, anchor: .top)
            .accessibilityIdentifier("cal.feed")
        }
        .onChange(of: position) { _, p in extend(around: p ?? 0) }
    }

    /// Дорастить ленту, если показанный месяц близко к краю.
    private func extend(around p: Int) {
        var r = range
        if p - r.lowerBound < Self.margin { r = (min(r.lowerBound, p) - Self.grow)...r.upperBound }
        if r.upperBound - p < Self.margin { r = r.lowerBound...(max(r.upperBound, p) + Self.grow) }
        if r != range { range = r }
    }

    /// «Ноябрь»; у другого года — «Май 2025» (как в «Календаре» Apple, без «г.»).
    static func title(_ m: Month, current: Month, cal: Calendar) -> String {
        let name = cal.standaloneMonthSymbols[m.month - 1].capitalized
        return m.year == current.year ? name : "\(name) \(m.year)"
    }
}

/// iPad и Мак: месяц на всю высоту — строки недель делят её поровну, как у
/// «Календаря» Apple; в тесном окне строка не ниже 72 точек, и сетка
/// прокручивается.
private struct MonthPage: View {
    let model: CalendarModel

    var body: some View {
        let month = model.selected.month1
        let weeks = MonthGrid.weeks(month, model.cal).count
        GeometryReader { geo in
            let row = max((geo.size.height - MonthGrid.headerHeight) / CGFloat(weeks), 72)
            ScrollView {
                MonthGrid(model: model, month: month, large: true, rowHeight: row) { model.select($0) } onNew: { d in
                    model.select(d)
                    model.newEvent(at: model.cal.date(bySettingHour: 9, minute: 0, second: 0, of: d.start(model.cal)))
                }
            }
            .scrollBounceBehavior(.basedOnSize)
        }
    }
}

/// Сетка месяца: на iPhone — точки событий, на iPad и Маке — названия.
/// Долгое нажатие на день — новое событие в этот день (Android §12); на Маке
/// то же — в меню по правому клику.
struct MonthGrid: View {
    let model: CalendarModel
    let month: Month
    let large: Bool
    /// Высота строки недели (iPad, Мак); `nil` — по содержимому.
    var rowHeight: CGFloat?
    let onDay: (Day) -> Void
    var onNew: ((Day) -> Void)?

    static let headerHeight: CGFloat = 24

    var body: some View {
        let cal = model.cal
        let weeks = Self.weeks(month, cal)
        let index = model.index[month] ?? [:]
        let today = model.today
        let selected = model.selected
        let onNew = self.onNew
        VStack(spacing: 0) {
            if large { WeekdayHeader(cal: cal).frame(height: Self.headerHeight - 4).padding(.bottom, 4) }
            ForEach(weeks.indices, id: \.self) { w in
                HStack(spacing: 0) {
                    ForEach(weeks[w], id: \.self) { d in
                        let inMonth = d.month1 == month
                        DayCell(day: d, inMonth: inMonth, today: d == today, selected: large && d == selected,
                                occs: inMonth ? index[d] ?? [] : [], large: large, height: rowHeight.map { $0 - 1 })
                            .contentShape(Rectangle())
                            .onTapGesture { if inMonth { onDay(d) } }
                            .onLongPressGesture(minimumDuration: 0.5) { if inMonth { onNew?(d) } }
                            #if os(macOS)
                            .contextMenu {
                                if inMonth, let onNew { Button("Новое событие", systemImage: "plus") { onNew(d) } }
                            }
                            #endif
                    }
                }
                if large { Divider() }
            }
        }
        .padding(.horizontal, large ? 0 : 8)
        .onAppear { model.ensure(month) }
    }

    static func weeks(_ m: Month, _ cal: Calendar) -> [[Day]] {
        let first = m.first
        let lead = (cal.component(.weekday, from: first.start(cal)) - cal.firstWeekday + 7) % 7
        let start = first.adding(-lead, cal)
        let count = Int(ceil(Double(lead + m.length(cal)) / 7))
        return (0..<count).map { w in (0..<7).map { start.adding(w * 7 + $0, cal) } }
    }
}

private struct WeekdayHeader: View {
    let cal: Calendar
    var font: Font = .caption.weight(.semibold)
    var body: some View {
        let symbols = cal.shortStandaloneWeekdaySymbols
        HStack(spacing: 0) {
            ForEach(0..<7, id: \.self) { i in
                Text(symbols[(i + cal.firstWeekday - 1) % 7].capitalized)
                    .font(font)
                    .foregroundStyle(.secondary)
                    .frame(maxWidth: .infinity)
            }
        }
    }
}

private struct DayCell: View {
    let day: Day
    let inMonth: Bool
    let today: Bool
    let selected: Bool
    let occs: [Occ]
    let large: Bool
    /// Высота ячейки сетки iPad/Мака; `nil` — по содержимому.
    var height: CGFloat?

    static let chipHeight: CGFloat = 16
    static let numberHeight: CGFloat = 33

    var body: some View {
        // Сколько плашек влезает; не влезли все — последняя строка «ещё N».
        let fit = height.map { max(Int(($0 - 4 - Self.numberHeight) / Self.chipHeight), 0) } ?? 3
        let shown = occs.count <= fit ? occs.count : max(fit - 1, 0)
        VStack(spacing: 3) {
            Text("\(day.day)")
                .font(large ? .callout : .body)
                .fontWeight(today ? .bold : .regular)
                .foregroundStyle(today ? .white : inMonth ? .primary : .clear)
                .frame(width: 30, height: 30)
                .background(Circle().fill(today ? Color.red : selected ? Color.secondary.opacity(0.2) : .clear))
            if large {
                VStack(alignment: .leading, spacing: 2) {
                    ForEach(occs.prefix(shown)) { o in
                        Text(o.event.summary)
                            .font(.caption2)
                            .lineLimit(1)
                            .padding(.horizontal, 4)
                            .frame(maxWidth: .infinity, minHeight: Self.chipHeight - 2, alignment: .leading)
                            .background(RoundedRectangle(cornerRadius: 3).fill(Color(argb: o.color).opacity(0.22)))
                    }
                    if occs.count > shown {
                        Text("ещё \(occs.count - shown)").font(.caption2).foregroundStyle(.secondary)
                    }
                }
                // Зазор между плашками соседних дней — иначе они сливаются в полосу.
                .padding(.horizontal, 2)
                Spacer(minLength: 0)
            } else {
                HStack(spacing: 2) {
                    ForEach(occs.prefix(3)) { o in Circle().fill(Color(argb: o.color)).frame(width: 5, height: 5) }
                }
                .frame(height: 5)
            }
        }
        .frame(maxWidth: .infinity, minHeight: height.map { $0 - 4 } ?? (large ? 96 : 44), maxHeight: height.map { $0 - 4 }, alignment: .top)
        .padding(.vertical, 2)
        .accessibilityElement(children: .combine)
        .accessibilityLabel(inMonth ? "\(day.day), событий: \(occs.count)" : "")
    }
}

// MARK: Год

/// Год (iPad, Мак; Android §12): двенадцать мини-месяцев, как у «Календаря»
/// Apple; касание числа — этот день, касание названия — месяц.
private struct YearView: View {
    let model: CalendarModel
    let year: Int
    let onDay: (Day) -> Void
    let onMonth: (Month) -> Void

    var body: some View {
        let today = model.today
        let cal = model.cal
        GeometryReader { geo in
            let cols = geo.size.width >= 900 ? 4 : 3
            ScrollView {
                LazyVGrid(columns: Array(repeating: GridItem(.flexible(), spacing: 28, alignment: .top), count: cols), spacing: 24) {
                    ForEach(1...12, id: \.self) { n in
                        MiniMonth(month: Month(year, n), today: today, cal: cal, onDay: onDay) { onMonth(Month(year, n)) }
                    }
                }
                .padding(20)
            }
            .accessibilityIdentifier("cal.year")
        }
    }
}

private struct MiniMonth: View {
    let month: Month
    let today: Day
    let cal: Calendar
    let onDay: (Day) -> Void
    let onTitle: () -> Void

    var body: some View {
        let weeks = MonthGrid.weeks(month, cal)
        let name = cal.standaloneMonthSymbols[month.month - 1].capitalized
        VStack(alignment: .leading, spacing: 6) {
            Button(action: onTitle) {
                Text(name)
                    .font(.title3.bold())
                    .foregroundStyle(month == today.month1 ? Color.red : .primary)
            }
            .buttonStyle(.plain)
            .accessibilityIdentifier("cal.year.month.\(month.month)")
            WeekdayHeader(cal: cal, font: .caption2)
            VStack(spacing: 2) {
                // Всегда шесть строк: соседние мини-месяцы стоят ровно.
                ForEach(0..<6, id: \.self) { w in
                    HStack(spacing: 0) {
                        ForEach(0..<7, id: \.self) { i in
                            if w < weeks.count, weeks[w][i].month1 == month {
                                let d = weeks[w][i]
                                Text("\(d.day)")
                                    .font(.caption.weight(d == today ? .bold : .regular))
                                    .foregroundStyle(d == today ? Color.white : .primary)
                                    .frame(width: 22, height: 22)
                                    .background(Circle().fill(d == today ? Color.red : .clear))
                                    .frame(maxWidth: .infinity)
                                    .contentShape(Rectangle())
                                    .onTapGesture { onDay(d) }
                                    .accessibilityLabel(d.start(cal).formatted(.dateTime.day().month(.wide)))
                                    .accessibilityAddTraits(.isButton)
                            } else {
                                Color.clear.frame(height: 22).frame(maxWidth: .infinity)
                            }
                        }
                    }
                }
            }
        }
    }
}

// MARK: День и неделя

/// День: полоса недели сверху (свайп — соседняя неделя), шкала времени (свайп —
/// соседний день).
struct DayScreen: View {
    @Bindable var model: CalendarModel

    var body: some View {
        VStack(spacing: 0) {
            WeekStrip(model: model)
            Divider()
            Timeline(model: model, days: [model.selected])
                .gesture(DragGesture(minimumDistance: 40).onEnded { v in
                    guard abs(v.translation.width) > abs(v.translation.height) * 2 else { return }
                    model.select(model.selected.adding(v.translation.width < 0 ? 1 : -1, model.cal))
                })
        }
        .navigationTitle(model.selected.start(model.cal).formatted(.dateTime.weekday(.wide).day().month(.wide)))
        .toolbarTitleDisplayMode(.inline)
    }
}

private struct WeekStrip: View {
    let model: CalendarModel

    var body: some View {
        let cal = model.cal
        let sel = model.selected
        let lead = (cal.component(.weekday, from: sel.start(cal)) - cal.firstWeekday + 7) % 7
        let monday = sel.adding(-lead, cal)
        let symbols = cal.veryShortStandaloneWeekdaySymbols
        HStack(spacing: 0) {
            ForEach(0..<7, id: \.self) { i in
                let d = monday.adding(i, cal)
                let isToday = d == model.today, isSel = d == sel
                VStack(spacing: 4) {
                    Text(symbols[(i + cal.firstWeekday - 1) % 7]).font(.caption2).foregroundStyle(.secondary)
                    Text("\(d.day)")
                        .font(.callout.weight(isSel ? .bold : .regular))
                        .foregroundStyle(isSel ? .white : isToday ? .red : .primary)
                        .frame(width: 32, height: 32)
                        .background(Circle().fill(isSel ? (isToday ? Color.red : Color.primary) : .clear))
                }
                .frame(maxWidth: .infinity)
                .contentShape(Rectangle())
                .onTapGesture { model.select(d) }
            }
        }
        .padding(.vertical, 8)
        .gesture(DragGesture(minimumDistance: 30).onEnded { v in
            guard abs(v.translation.width) > abs(v.translation.height) else { return }
            model.select(sel.adding(v.translation.width < 0 ? 7 : -7, cal))
        })
    }
}

/// Неделя (iPad, Мак): семь колонок шкалы времени; заголовки дней — сразу
/// под первой строкой, шкала — на всю оставшуюся высоту.
private struct WeekTimeline: View {
    let model: CalendarModel

    var body: some View {
        let cal = model.cal
        let sel = model.selected
        let lead = (cal.component(.weekday, from: sel.start(cal)) - cal.firstWeekday + 7) % 7
        let days = (0..<7).map { sel.adding($0 - lead, cal) }
        VStack(spacing: 0) {
            HStack(spacing: 0) {
                // Отступ под шкалу часов. Не `Color.clear` с одной шириной: цвет
                // занимает всю предложенную высоту, и заголовки раздувались на
                // полэкрана, сталкивая шкалу вниз (S-06).
                Spacer().frame(width: Timeline.gutter, height: 1)
                ForEach(days, id: \.self) { d in
                    Text(d.start(cal).formatted(.dateTime.weekday(.abbreviated).day()))
                        .font(.subheadline.weight(d == model.today ? .bold : .regular))
                        .foregroundStyle(d == model.today ? .red : .primary)
                        .frame(maxWidth: .infinity)
                        .contentShape(Rectangle())
                        .onTapGesture {
                            model.select(d)
                            model.mode = .day
                        }
                }
            }
            .padding(.vertical, 6)
            .fixedSize(horizontal: false, vertical: true)
            .accessibilityIdentifier("cal.week.header")
            Divider()
            Timeline(model: model, days: days)
        }
    }
}

/// Событие, которое сейчас тянут на шкале: что именно и насколько.
struct EventDragState: Equatable {
    let occID: String
    var kind: EventDrag.Kind
    var translation: CGSize = .zero
}

/// Шкала времени дня или недели: «весь день» сверху, события по часам,
/// пересекающиеся — колонками; линия «сейчас» — красная. События
/// записываемых календарей переносятся и растягиваются (`EventDrag`).
struct Timeline: View {
    let model: CalendarModel
    let days: [Day]
    @State private var drag: EventDragState?
    static let hour: CGFloat = 52
    static let gutter: CGFloat = 52
    static let space = "timeline"

    var body: some View {
        let cal = model.cal
        let allDay = days.map { d in model.occs(d).filter(\.allDay) }
        let dragging = drag
        // Колонка с тянущимся событием — поверх соседних: его видно и над ними.
        let dragDay = dragging.flatMap { st in days.firstIndex { d in model.occs(d).contains { $0.id == st.occID } } }
        VStack(spacing: 0) {
            if allDay.contains(where: { !$0.isEmpty }) {
                HStack(alignment: .top, spacing: 0) {
                    Text("весь день").font(.caption2).foregroundStyle(.secondary).frame(width: Self.gutter)
                    ForEach(days.indices, id: \.self) { i in
                        VStack(spacing: 2) {
                            ForEach(allDay[i]) { o in
                                EventChip(occ: o, compact: true).onTapGesture { model.detail = o }
                            }
                        }
                        .frame(maxWidth: .infinity)
                    }
                }
                .padding(.vertical, 4)
                Divider()
            }
            ScrollViewReader { proxy in
                ScrollView {
                    ZStack(alignment: .topLeading) {
                        HourGrid()
                        HStack(spacing: 0) {
                            Spacer().frame(width: Self.gutter, height: 1)
                            ForEach(days.indices, id: \.self) { i in
                                DayColumn(model: model, day: days[i], index: i, dayCount: days.count, drag: $drag)
                                    .frame(maxWidth: .infinity)
                                    .zIndex(dragDay == i ? 1 : 0)
                            }
                        }
                        NowLine(days: days, cal: cal)
                    }
                    .frame(height: Self.hour * 24)
                    .coordinateSpace(.named(Self.space))
                    .id("grid")
                }
                // Пока событие тянут, шкала не прокручивается под пальцем.
                .scrollDisabled(dragging != nil)
                .accessibilityIdentifier("cal.timeline")
                .onAppear {
                    // Сразу к рабочему дню, а не к полуночи.
                    proxy.scrollTo("grid", anchor: UnitPoint(x: 0, y: 7.5 / 24))
                }
            }
        }
        .sensoryFeedback(.selection, trigger: dragging?.occID)
    }
}

private struct HourGrid: View {
    var body: some View {
        VStack(spacing: 0) {
            ForEach(0..<24, id: \.self) { h in
                HStack(alignment: .top, spacing: 4) {
                    Text(h == 0 ? "" : String(format: "%02d:00", h))
                        .font(.caption2)
                        .foregroundStyle(.secondary)
                        .frame(width: Timeline.gutter - 6, alignment: .trailing)
                        .offset(y: -7)
                    VStack { Divider() }
                }
                .frame(height: Timeline.hour, alignment: .top)
            }
        }
    }
}

/// Колонка дня: события в часовой сетке, пересекающиеся — рядом. Пустое
/// место — 48 получасов: долгое нажатие (Мак — и правый клик) создаёт событие
/// на этом получасе (Android §12).
private struct DayColumn: View {
    let model: CalendarModel
    let day: Day
    let index: Int
    let dayCount: Int
    @Binding var drag: EventDragState?

    var body: some View {
        let cal = model.cal
        let dayStart = day.start(cal)
        let timed = model.occs(day).filter { !$0.allDay }
        let laid = Self.layout(timed)
        let current = drag
        GeometryReader { geo in
            let colW = geo.size.width
            ZStack(alignment: .topLeading) {
                HalfHourSlots(day: day, cal: cal) { model.newEvent(at: $0) }
                ForEach(laid, id: \.occ.id) { item in
                    chip(item, dayStart: dayStart, colW: colW, state: current?.occID == item.occ.id ? current : nil)
                }
            }
        }
    }

    /// Плашка события; тянется — рисуется на новом месте с новым временем.
    @ViewBuilder private func chip(_ item: Placed, dayStart: Date, colW: CGFloat, state: EventDragState?) -> some View {
        let o = item.occ
        let hour = Timeline.hour
        let top = max(date(o.start).timeIntervalSince(dayStart), 0) / 3600 * hour
        let bottom = min(date(o.end).timeIntervalSince(dayStart), 86_400) / 3600 * hour
        let w = colW / CGFloat(item.columns)
        let baseH = max(bottom - top - 1, 18)
        let movable = o.event.editable && !o.allDay
        let shift = state.map { offsets($0, colW: colW) }
        let minutes = CGFloat(shift?.minutes ?? 0) / 60 * hour
        let kind = state?.kind ?? .move
        let y = kind == .bottom ? top : top + minutes
        let h: CGFloat = switch kind {
        case .move: baseH
        case .top: max(baseH - minutes, hour / 4)
        case .bottom: max(baseH + minutes, hour / 4)
        }
        let times = state.map { st in
            EventDrag.apply(st.kind, start: o.start, end: o.end, minutes: shift?.minutes ?? 0, days: shift?.days ?? 0, calendar: model.cal)
        }
        EventChip(occ: o, compact: false, time: times, lifted: state != nil)
            .frame(width: max(w - 2, 10), height: h, alignment: .topLeading)
            .contentShape(Rectangle())
            .onTapGesture { model.detail = o }
            .gesture(dragGesture(o, top: top, height: baseH, colW: colW), including: movable ? .all : .subviews)
            .accessibilityIdentifier("cal.event")
            .accessibilityActions {
                if movable {
                    Button("На 15 минут раньше") { model.drop(o, .move, minutes: -EventDrag.step, days: 0) }
                    Button("На 15 минут позже") { model.drop(o, .move, minutes: EventDrag.step, days: 0) }
                    Button("Длиннее на 15 минут") { model.drop(o, .bottom, minutes: EventDrag.step, days: 0) }
                    Button("Короче на 15 минут") { model.drop(o, .bottom, minutes: -EventDrag.step, days: 0) }
                }
            }
            .offset(x: w * CGFloat(item.column) + CGFloat(shift?.days ?? 0) * colW, y: y)
            .zIndex(state != nil ? 1 : 0)
    }

    /// Смещение пальца → минуты (шаг 15) и дни (только перенос в неделе;
    /// за пределы показанной недели не уводит).
    private func offsets(_ st: EventDragState, colW: CGFloat) -> (minutes: Int, days: Int) {
        let m = EventDrag.snap(st.translation.height, hourHeight: Timeline.hour)
        guard st.kind == .move, dayCount > 1, colW > 0 else { return (m, 0) }
        let d = Int((st.translation.width / colW).rounded())
        return (m, min(max(d, -index), dayCount - 1 - index))
    }

    /// iPhone и iPad — долгое нажатие и ведение (прокрутке шкалы не мешает);
    /// Мак — сразу ведение мышью, как у десктопа.
    private func dragGesture(_ o: Occ, top: CGFloat, height: CGFloat, colW: CGFloat) -> some Gesture {
        let drag = $drag
        let model = self.model
        let offsets = self.offsets
        @MainActor func update(_ v: DragGesture.Value) {
            if drag.wrappedValue?.occID != o.id {
                // Кромка или середина — по месту первого касания внутри плашки.
                drag.wrappedValue = EventDragState(occID: o.id, kind: EventDrag.kind(at: v.startLocation.y - top, height: height))
            }
            drag.wrappedValue?.translation = v.translation
        }
        @MainActor func finish() {
            if let st = drag.wrappedValue, st.occID == o.id {
                let s = offsets(st, colW)
                model.drop(o, st.kind, minutes: s.minutes, days: s.days)
            }
            drag.wrappedValue = nil
        }
        #if os(macOS)
        return DragGesture(minimumDistance: 3, coordinateSpace: .named(Timeline.space))
            .onChanged { update($0) }
            .onEnded { _ in finish() }
        #else
        return LongPressGesture(minimumDuration: 0.35)
            .sequenced(before: DragGesture(minimumDistance: 0, coordinateSpace: .named(Timeline.space)))
            .onChanged { value in
                if case let .second(true, v?) = value { update(v) }
            }
            .onEnded { _ in finish() }
        #endif
    }

    struct Placed { let occ: Occ; let column: Int; let columns: Int }

    /// Пересекающиеся события — в колонки; ширина делится внутри группы.
    static func layout(_ occs: [Occ]) -> [Placed] {
        let sorted = occs.sorted { ($0.start, $1.end) < ($1.start, $0.end) }
        var out: [Placed] = []
        var group: [(Occ, Int)] = []
        var colEnds: [Int64] = []
        var groupEnd: Int64 = .min
        func flush() {
            let n = max(colEnds.count, 1)
            out += group.map { Placed(occ: $0.0, column: $0.1, columns: n) }
            group = []
            colEnds = []
        }
        for o in sorted {
            if o.start >= groupEnd { flush() }
            let end = max(o.end, o.start + 15 * 60_000)
            if let c = colEnds.firstIndex(where: { $0 <= o.start }) {
                colEnds[c] = end
                group.append((o, c))
            } else {
                colEnds.append(end)
                group.append((o, colEnds.count - 1))
            }
            groupEnd = max(groupEnd, end)
        }
        flush()
        return out
    }
}

/// Пустое место колонки дня по получасам: долгое нажатие — новое событие с
/// этого получаса; на Маке — ещё и пункт меню по правому клику.
private struct HalfHourSlots: View {
    let day: Day
    let cal: Calendar
    let onNew: (Date) -> Void

    var body: some View {
        let start = day.start(cal)
        VStack(spacing: 0) {
            ForEach(0..<48, id: \.self) { i in
                let at = cal.date(byAdding: .minute, value: i * 30, to: start) ?? start
                Color.clear
                    .frame(maxWidth: .infinity)
                    .frame(height: Timeline.hour / 2)
                    .contentShape(Rectangle())
                    .onLongPressGesture(minimumDuration: 0.5) { onNew(at) }
                    #if os(macOS)
                    .contextMenu {
                        Button("Новое событие в \(at.formatted(date: .omitted, time: .shortened))", systemImage: "plus") { onNew(at) }
                    }
                    #endif
            }
        }
    }
}

private struct NowLine: View {
    let days: [Day]
    let cal: Calendar

    var body: some View {
        TimelineView(.periodic(from: .now, by: 60)) { ctx in
            let today = Day(ctx.date, cal)
            if let i = days.firstIndex(of: today) {
                let y = ctx.date.timeIntervalSince(today.start(cal)) / 3600 * Timeline.hour
                GeometryReader { geo in
                    let colW = (geo.size.width - Timeline.gutter) / CGFloat(days.count)
                    HStack(spacing: 0) {
                        Circle().fill(.red).frame(width: 8, height: 8)
                        Rectangle().fill(.red).frame(height: 1.5)
                    }
                    .frame(width: colW + 4)
                    .offset(x: Timeline.gutter + colW * CGFloat(i) - 4, y: y - 4)
                }
                .allowsHitTesting(false)
            }
        }
    }
}

struct EventChip: View {
    let occ: Occ
    let compact: Bool
    /// Новое время, пока событие тянут.
    var time: (start: Int64, end: Int64)?
    var lifted = false

    var body: some View {
        let c = Color(argb: occ.color)
        HStack(spacing: 0) {
            Rectangle().fill(c).frame(width: 3)
            VStack(alignment: .leading, spacing: 1) {
                Text(occ.event.summary.isEmpty ? "(без названия)" : occ.event.summary)
                    .font(.caption.weight(.semibold))
                    .lineLimit(compact ? 1 : 2)
                if !compact && !occ.allDay {
                    if let time {
                        Text("\(date(time.start).formatted(date: .omitted, time: .shortened))–\(date(time.end).formatted(date: .omitted, time: .shortened))")
                            .font(.caption2.weight(.semibold))
                            .monospacedDigit()
                    } else {
                        Text(date(occ.start).formatted(date: .omitted, time: .shortened))
                            .font(.caption2)
                            .foregroundStyle(.secondary)
                    }
                }
            }
            .padding(.horizontal, 4)
            .padding(.vertical, 2)
            Spacer(minLength: 0)
        }
        .background(c.opacity(lifted ? 0.42 : 0.18))
        .background(lifted ? Color.white : .clear)
        .clipShape(RoundedRectangle(cornerRadius: 4))
        .overlay {
            if lifted { RoundedRectangle(cornerRadius: 4).strokeBorder(c, lineWidth: 1.5) }
        }
        .shadow(color: .black.opacity(lifted ? 0.18 : 0), radius: 4, y: 2)
        .accessibilityElement(children: .combine)
    }
}

// MARK: Списки календарей

/// Видимость — только на этом устройстве; звезда — календарь для новых
/// событий. Живёт в «Настройках → Календари», как список календарей десктопа.
struct CalendarsListView: View {
    @Bindable var model: CalendarModel

    var body: some View {
        Group {
            List {
                ForEach(model.calendars) { c in
                    HStack {
                        Button {
                            model.toggleCalendar(c.id)
                        } label: {
                            Image(systemName: model.prefs.hidden.contains(c.id) ? "circle" : "checkmark.circle.fill")
                                .foregroundStyle(Color(argb: CalendarColors.of(c, id: c.id)))
                                .font(.title3)
                        }
                        .buttonStyle(.plain)
                        .accessibilityLabel(model.prefs.hidden.contains(c.id) ? "Показать" : "Скрыть")
                        Text(c.name)
                        Spacer()
                        if c.canWrite {
                            Button {
                                model.setDefault(c.id)
                            } label: {
                                Image(systemName: model.prefs.defaultCalendar == c.id ? "star.fill" : "star")
                                    .foregroundStyle(.yellow)
                            }
                            .buttonStyle(.plain)
                            .accessibilityLabel("Календарь для новых событий")
                        } else {
                            Image(systemName: "lock").foregroundStyle(.secondary).accessibilityLabel("Только чтение")
                        }
                    }
                }
            }
            .navigationTitle("Календари")
            .toolbarTitleDisplayMode(.inline)
        }
        .task { if model.calendars.isEmpty { await model.start() } }
    }
}
