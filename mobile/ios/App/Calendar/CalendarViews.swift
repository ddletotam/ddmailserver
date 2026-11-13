import DDMailAPI
import DDMailCalendar
import SwiftUI

/// Календарь (контракт iOS §11, Android §12): вид — как у «Календаря» Apple.
/// iPhone — лента месяцев, тап по дню — день со шкалой времени; iPad и Мак —
/// День / Неделя / Месяц.
struct CalendarScreen: View {
    @Bindable var model: CalendarModel
    @Environment(\.horizontalSizeClass) private var sizeClass
    @State private var dayShown = false
    /// Месяц ленты iPhone — смещение от текущего (позиция прокрутки).
    @State private var feedPosition: Int? = 0

    var body: some View {
        let formOpen = model.draft != nil
        let compact = sizeClass == .compact
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
                        case .month:
                            ScrollView {
                                MonthGrid(model: model, month: model.selected.month1, large: true) { model.select($0) }
                            }
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
        .overlay(alignment: .bottom) { NoticeBanner(text: $model.notice) }
        .task { await model.start() }
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
        Spacer(minLength: 4)
        if !compact {
            Picker("Вид", selection: $model.mode) {
                ForEach(CalendarModel.ViewMode.allCases) { Text($0.title).tag($0) }
            }
            .pickerStyle(.segmented)
            .labelsHidden()
            .frame(width: 240)
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
        case .month, .week: "\(cal.standaloneMonthSymbols[m.month - 1].capitalized) \(m.year)"
        case .day: d.formatted(.dateTime.weekday(.wide).day().month(.wide))
        }
    }

    private func step(_ n: Int, compact: Bool) {
        let cal = model.cal
        if compact {
            // Лента месяцев: соседний месяц.
            feedPosition = MonthsFeed.clamp((feedPosition ?? 0) + n)
            return
        }
        switch model.mode {
        case .day: model.select(model.selected.adding(n, cal))
        case .week: model.select(model.selected.adding(7 * n, cal))
        case .month:
            let m = model.selected.month1.adding(n)
            model.select(Day(m.year, m.month, min(model.selected.day, m.length(cal))))
        }
    }
}

// MARK: Месяцы

/// iPhone: лента месяцев, вертикальная прокрутка, под числом — точки событий;
/// дни недели закреплены сверху; открывается на текущем месяце.
private struct MonthsFeed: View {
    let model: CalendarModel
    /// Смещение показанного месяца от текущего — общее с первой строкой (‹ ›).
    @Binding var position: Int?
    let onDay: (Day) -> Void
    static let span = 18
    private var span: Int { Self.span }

    static func clamp(_ i: Int) -> Int { min(max(i, -span), span) }

    var body: some View {
        let current = model.today.month1
        VStack(spacing: 0) {
            WeekdayHeader(cal: model.cal).padding(.horizontal, 8).padding(.vertical, 6)
            Divider()
            ScrollView {
                LazyVStack(alignment: .leading, spacing: 24) {
                    ForEach(-span...span, id: \.self) { i in
                        let m = current.adding(i)
                        VStack(alignment: .leading, spacing: 8) {
                            Text(Self.title(m, current: current, cal: model.cal))
                                .font(.title3.bold())
                                .foregroundStyle(m == current ? Color.red : .primary)
                                .padding(.horizontal, 16)
                            MonthGrid(model: model, month: m, large: false, onDay: onDay)
                        }
                        .id(i)
                        .onAppear { model.ensure(m) }
                    }
                }
                .scrollTargetLayout()
                .padding(.vertical, 12)
            }
            .scrollPosition(id: $position, anchor: .top)
        }
    }

    /// «Ноябрь»; у другого года — «Май 2025» (как в «Календаре» Apple, без «г.»).
    static func title(_ m: Month, current: Month, cal: Calendar) -> String {
        let name = cal.standaloneMonthSymbols[m.month - 1].capitalized
        return m.year == current.year ? name : "\(name) \(m.year)"
    }
}

/// Сетка месяца: на iPhone — точки событий, на iPad и Маке — названия.
struct MonthGrid: View {
    let model: CalendarModel
    let month: Month
    let large: Bool
    let onDay: (Day) -> Void

    var body: some View {
        let cal = model.cal
        let weeks = Self.weeks(month, cal)
        let index = model.index[month] ?? [:]
        let today = model.today
        let selected = model.selected
        VStack(spacing: 0) {
            if large { WeekdayHeader(cal: cal).padding(.bottom, 4) }
            ForEach(weeks.indices, id: \.self) { w in
                HStack(spacing: 0) {
                    ForEach(weeks[w], id: \.self) { d in
                        let inMonth = d.month1 == month
                        DayCell(day: d, inMonth: inMonth, today: d == today, selected: large && d == selected,
                                occs: inMonth ? index[d] ?? [] : [], large: large)
                            .contentShape(Rectangle())
                            .onTapGesture { if inMonth { onDay(d) } }
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
    var body: some View {
        let symbols = cal.shortStandaloneWeekdaySymbols
        HStack(spacing: 0) {
            ForEach(0..<7, id: \.self) { i in
                Text(symbols[(i + cal.firstWeekday - 1) % 7].capitalized)
                    .font(.caption.weight(.semibold))
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

    var body: some View {
        VStack(spacing: 3) {
            Text("\(day.day)")
                .font(large ? .callout : .body)
                .fontWeight(today ? .bold : .regular)
                .foregroundStyle(today ? .white : inMonth ? .primary : .clear)
                .frame(width: 30, height: 30)
                .background(Circle().fill(today ? Color.red : selected ? Color.secondary.opacity(0.2) : .clear))
            if large {
                VStack(alignment: .leading, spacing: 2) {
                    ForEach(occs.prefix(3)) { o in
                        Text(o.event.summary)
                            .font(.caption2)
                            .lineLimit(1)
                            .padding(.horizontal, 4)
                            .frame(maxWidth: .infinity, alignment: .leading)
                            .background(RoundedRectangle(cornerRadius: 3).fill(Color(argb: o.color).opacity(0.22)))
                    }
                    if occs.count > 3 {
                        Text("ещё \(occs.count - 3)").font(.caption2).foregroundStyle(.secondary)
                    }
                }
                Spacer(minLength: 0)
            } else {
                HStack(spacing: 2) {
                    ForEach(occs.prefix(3)) { o in Circle().fill(Color(argb: o.color)).frame(width: 5, height: 5) }
                }
                .frame(height: 5)
            }
        }
        .frame(maxWidth: .infinity, minHeight: large ? 96 : 44, alignment: .top)
        .padding(.vertical, 2)
        .accessibilityElement(children: .combine)
        .accessibilityLabel(inMonth ? "\(day.day), событий: \(occs.count)" : "")
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

/// Неделя (iPad, Мак): семь колонок шкалы времени.
private struct WeekTimeline: View {
    let model: CalendarModel

    var body: some View {
        let cal = model.cal
        let sel = model.selected
        let lead = (cal.component(.weekday, from: sel.start(cal)) - cal.firstWeekday + 7) % 7
        let days = (0..<7).map { sel.adding($0 - lead, cal) }
        VStack(spacing: 0) {
            HStack(spacing: 0) {
                Color.clear.frame(width: Timeline.gutter)
                ForEach(days, id: \.self) { d in
                    Text(d.start(cal).formatted(.dateTime.weekday(.abbreviated).day()))
                        .font(.subheadline.weight(d == model.today ? .bold : .regular))
                        .foregroundStyle(d == model.today ? .red : .primary)
                        .frame(maxWidth: .infinity)
                }
            }
            .padding(.vertical, 6)
            Divider()
            Timeline(model: model, days: days)
        }
    }
}

/// Шкала времени дня или недели: «весь день» сверху, события по часам,
/// пересекающиеся — колонками; линия «сейчас» — красная.
struct Timeline: View {
    let model: CalendarModel
    let days: [Day]
    static let hour: CGFloat = 52
    static let gutter: CGFloat = 52

    var body: some View {
        let cal = model.cal
        let allDay = days.map { d in model.occs(d).filter(\.allDay) }
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
                            Color.clear.frame(width: Self.gutter)
                            ForEach(days, id: \.self) { d in
                                DayColumn(model: model, day: d)
                                    .frame(maxWidth: .infinity)
                            }
                        }
                        NowLine(days: days, cal: cal)
                    }
                    .frame(height: Self.hour * 24)
                    .id("grid")
                }
                .onAppear {
                    // Сразу к рабочему дню, а не к полуночи.
                    proxy.scrollTo("grid", anchor: UnitPoint(x: 0, y: 7.5 / 24))
                }
            }
        }
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

/// Колонка дня: события в часовой сетке, пересекающиеся — рядом.
private struct DayColumn: View {
    let model: CalendarModel
    let day: Day

    var body: some View {
        let cal = model.cal
        let dayStart = day.start(cal)
        let timed = model.occs(day).filter { !$0.allDay }
        let laid = Self.layout(timed)
        GeometryReader { geo in
            ForEach(laid, id: \.occ.id) { item in
                let o = item.occ
                let top = max(date(o.start).timeIntervalSince(dayStart), 0) / 3600 * Timeline.hour
                let bottom = min(date(o.end).timeIntervalSince(dayStart), 86_400) / 3600 * Timeline.hour
                let w = geo.size.width / CGFloat(item.columns)
                EventChip(occ: o, compact: false)
                    .frame(width: max(w - 2, 10), height: max(bottom - top - 1, 18), alignment: .topLeading)
                    .offset(x: w * CGFloat(item.column), y: top)
                    .onTapGesture { model.detail = o }
            }
        }
        .contentShape(Rectangle())
        .onLongPressGesture(minimumDuration: 0.5) {
            // Долгое нажатие на пустом месте — новое событие в этот день (Android §12).
            model.newEvent(at: cal.date(bySettingHour: 12, minute: 0, second: 0, of: dayStart))
        }
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

    var body: some View {
        let c = Color(argb: occ.color)
        HStack(spacing: 0) {
            Rectangle().fill(c).frame(width: 3)
            VStack(alignment: .leading, spacing: 1) {
                Text(occ.event.summary.isEmpty ? "(без названия)" : occ.event.summary)
                    .font(.caption.weight(.semibold))
                    .lineLimit(compact ? 1 : 2)
                if !compact && !occ.allDay {
                    Text(date(occ.start).formatted(date: .omitted, time: .shortened))
                        .font(.caption2)
                        .foregroundStyle(.secondary)
                }
            }
            .padding(.horizontal, 4)
            .padding(.vertical, 2)
            Spacer(minLength: 0)
        }
        .background(c.opacity(0.18))
        .clipShape(RoundedRectangle(cornerRadius: 4))
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
