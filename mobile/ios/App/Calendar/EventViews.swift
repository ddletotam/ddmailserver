import DDMailAPI
import DDMailCalendar
import SwiftUI

func date(_ ms: Int64) -> Date { Date(timeIntervalSince1970: TimeInterval(ms) / 1000) }
func ms(_ d: Date) -> Int64 { Int64((d.timeIntervalSince1970 * 1000).rounded()) }

/// Карточка события (Android §12): время, повтор словами, место (ссылкой, если
/// это ссылка), видеовстречи, напоминания, ответ на приглашение, участники.
struct EventDetailView: View {
    let model: CalendarModel
    let occ: Occ
    @Environment(\.dismiss) private var dismiss
    @Environment(\.openURL) private var openURL
    @State private var confirmDelete = false

    var body: some View {
        let ev = occ.event
        let writable = model.calendar(ev.calendarID)?.canWrite == true
        NavigationStack {
            List {
                Section {
                    VStack(alignment: .leading, spacing: 6) {
                        HStack(spacing: 8) {
                            Circle().fill(Color(argb: occ.color)).frame(width: 10, height: 10)
                            Text(model.calendar(ev.calendarID)?.name ?? "").font(.subheadline).foregroundStyle(.secondary)
                        }
                        Text(ev.summary.isEmpty ? "(без названия)" : ev.summary).font(.title2.bold())
                        Text(timeText).foregroundStyle(.secondary)
                        if occ.recurring { Label(Recurrence.describe(ev.rrule), systemImage: "repeat").font(.subheadline) }
                    }
                    .padding(.vertical, 4)
                }
                let links = EventLinks.of(ev)
                if !ev.location.isEmpty || !links.isEmpty {
                    Section {
                        if !ev.location.isEmpty {
                            if let u = EventLinks.target(ev.location).flatMap(URL.init(string:)) {
                                Button { openURL(u) } label: { Label(ev.location, systemImage: "mappin.and.ellipse") }
                            } else {
                                Label(ev.location, systemImage: "mappin.and.ellipse")
                            }
                        }
                        ForEach(links.filter { $0 != EventLinks.target(ev.location) }, id: \.self) { l in
                            if let u = URL(string: l) {
                                Button { openURL(u) } label: { Label(u.host() ?? l, systemImage: "video") }
                            }
                        }
                    }
                }
                if !ev.alarmLeads.isEmpty || ev.alarmLeadMin > 0 {
                    Section("Напоминания") {
                        let leads = ev.alarmLeads.isEmpty ? [ev.alarmLeadMin] : ev.alarmLeads
                        ForEach(leads.sorted(by: >), id: \.self) { m in
                            Label(m == 0 ? "В момент начала" : "За \(Self.lead(m))", systemImage: "bell")
                        }
                    }
                }
                if let mine = model.myPartstat(occ), writable {
                    Section("Ваш ответ") {
                        HStack {
                            rsvp("ACCEPTED", "Принять", "checkmark.circle", mine)
                            rsvp("TENTATIVE", "Под вопросом", "questionmark.circle", mine)
                            rsvp("DECLINED", "Отклонить", "xmark.circle", mine)
                        }
                    }
                }
                if !ev.attendees.isEmpty {
                    Section("Участники") {
                        ForEach(ev.attendees, id: \.email) { a in
                            HStack {
                                Image(systemName: Self.partstatSymbol(a.partstat))
                                    .foregroundStyle(Self.partstatColor(a.partstat))
                                    .accessibilityLabel(Self.partstatText(a.partstat))
                                VStack(alignment: .leading) {
                                    Text(a.name.isEmpty ? a.email : a.name)
                                    if !a.name.isEmpty { Text(a.email).font(.caption).foregroundStyle(.secondary) }
                                }
                            }
                        }
                    }
                }
                if !ev.description.isEmpty {
                    Section("Описание") { Text(ev.description).textSelection(.enabled) }
                }
                if ev.deletable {
                    Section {
                        Button("Удалить событие", systemImage: "trash", role: .destructive) { confirmDelete = true }
                    }
                }
            }
            .navigationTitle("Событие")
            .toolbarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) { Button("Закрыть", systemImage: "xmark") { dismiss() } }
                if ev.editable {
                    ToolbarItem(placement: .primaryAction) { Button("Изменить", systemImage: "pencil") { model.edit(occ) } }
                }
            }
            .confirmationDialog(occ.recurring ? "Удалить всю серию?" : "Удалить событие?", isPresented: $confirmDelete, titleVisibility: .visible) {
                Button("Удалить", role: .destructive) { model.delete(occ) }
            } message: {
                if occ.recurring { Text("Удаляется вся серия повторов.") }
            }
        }
        .presentationDetents([.medium, .large])
    }

    private var timeText: String {
        let s = date(occ.start), e = date(occ.end)
        if occ.allDay {
            let first = Occ.dateOf(occ.start, allDay: true, model.cal), last = Occ.dateOf(occ.end, allDay: true, model.cal).adding(-1, model.cal)
            let f = first.start(model.cal).formatted(.dateTime.weekday(.wide).day().month(.wide))
            return last > first ? "\(f) — \(last.start(model.cal).formatted(.dateTime.day().month(.wide))), весь день" : "\(f), весь день"
        }
        let day = s.formatted(.dateTime.weekday(.wide).day().month(.wide))
        return "\(day), \(s.formatted(date: .omitted, time: .shortened))–\(e.formatted(date: .omitted, time: .shortened))"
    }

    private func rsvp(_ p: String, _ title: String, _ symbol: String, _ mine: String) -> some View {
        Button { model.rsvp(occ, p) } label: {
            Label(title, systemImage: mine == p ? symbol + ".fill" : symbol)
                .labelStyle(.iconOnly)
                .font(.title2)
                .frame(maxWidth: .infinity)
        }
        .buttonStyle(.borderless)
        .accessibilityLabel(title)
    }

    static func lead(_ m: Int) -> String {
        m % 1440 == 0 ? "\(m / 1440) д" : m % 60 == 0 ? "\(m / 60) ч" : "\(m) мин"
    }

    static func partstatSymbol(_ p: String) -> String {
        switch p {
        case "ACCEPTED": "checkmark.circle.fill"
        case "DECLINED": "xmark.circle.fill"
        case "TENTATIVE": "questionmark.circle.fill"
        default: "circle"
        }
    }

    static func partstatColor(_ p: String) -> Color {
        switch p {
        case "ACCEPTED": .green
        case "DECLINED": .red
        case "TENTATIVE": .orange
        default: .secondary
        }
    }

    static func partstatText(_ p: String) -> String {
        switch p {
        case "ACCEPTED": "Принял"
        case "DECLINED": "Отклонил"
        case "TENTATIVE": "Под вопросом"
        default: "Нет ответа"
        }
    }
}

/// Форма события — по подтверждению (§5б-бис десктопа): кнопки гаснут на
/// время запроса, закрывается только по успеху, отказ — текстом в форме.
/// Календарь — только назначенный пользователем; иначе выбор обязателен.
struct EventFormView: View {
    @Bindable var model: CalendarModel
    @FocusState private var titleFocused: Bool

    var body: some View {
        if let d = model.draft {
            NavigationStack {
                Form {
                    Section {
                        TextField("Название", text: binding(\.summary)).focused($titleFocused)
                        Picker("Календарь", selection: binding(\.calendarID)) {
                            if d.calendarID == 0 { Text("Выберите").tag(Int64(0)) }
                            ForEach(model.writable) { c in
                                Label { Text(c.name) } icon: { Circle().fill(Color(argb: CalendarColors.of(c, id: c.id))) }
                                    .tag(c.id)
                            }
                        }
                        .disabled(d.original != nil)
                    }
                    Section {
                        Toggle("Весь день", isOn: binding(\.allDay))
                        DatePicker("Начало", selection: binding(\.start), displayedComponents: d.allDay ? .date : [.date, .hourAndMinute])
                        DatePicker("Конец", selection: binding(\.end), in: d.start..., displayedComponents: d.allDay ? .date : [.date, .hourAndMinute])
                    }
                    if d.original?.recurring == true {
                        Section {
                            Picker("Изменить", selection: binding(\.scope)) {
                                Text("Все повторы").tag("all")
                                Text("Это и следующие").tag("future")
                            }
                        } footer: {
                            Text("Только это вхождение сервер пока менять не умеет.")
                        }
                    }
                    Section {
                        TextField("Место или ссылка на встречу", text: binding(\.location))
                        TextField("Описание", text: binding(\.description), axis: .vertical).lineLimit(3...10)
                    }
                    if let e = d.error {
                        Section { Label(e, systemImage: "exclamationmark.triangle").foregroundStyle(.red) }
                    }
                }
                .formStyle(.grouped)
                .navigationTitle(d.original == nil ? "Новое событие" : "Изменить событие")
                .toolbarTitleDisplayMode(.inline)
                .toolbar {
                    ToolbarItem(placement: .cancellationAction) {
                        Button("Отмена", systemImage: "xmark") { model.draft = nil }.disabled(d.busy)
                    }
                    ToolbarItem(placement: .confirmationAction) {
                        if d.busy { ProgressView() } else {
                            Button("Сохранить", systemImage: "checkmark") { model.save() }
                                .disabled(model.writable.isEmpty)
                        }
                    }
                }
                .onAppear { if d.original == nil { titleFocused = true } }
                .onChange(of: d.start) { old, new in
                    // Сдвинули начало — конец едет следом, длительность та же.
                    guard let cur = model.draft else { return }
                    model.draft?.end = max(cur.end.addingTimeInterval(new.timeIntervalSince(old)), new)
                }
            }
            .interactiveDismissDisabled(d.busy)
        }
    }

    private func binding<T>(_ kp: WritableKeyPath<EventDraft, T>) -> Binding<T> {
        Binding(
            get: { model.draft?[keyPath: kp] ?? EventDraft()[keyPath: kp] },
            set: {
                model.draft?[keyPath: kp] = $0
                model.draft?.error = nil
            }
        )
    }
}
