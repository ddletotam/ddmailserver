import DDMailAPI
import DDMailCalendar
import SwiftUI

/// Задачи (Android §14, iOS §11): отдельный раздел, окна по времени нет
/// (забираются все), выполненные скрыты — переключатель меняет запрос к
/// серверу. Галочка, создание, правка и удаление оптимистичны (`TaskBoard`):
/// строка меняется сразу, отказ откатывает её и показывает причину.
@MainActor @Observable
final class TasksModel {
    let board: TaskBoard
    let prefs = CalendarPrefs.shared
    /// Открытая форма задачи: новой или существующей.
    var draft: TaskDraft?

    init(api: any TaskAPI, onDead: @escaping () -> Void) {
        board = TaskBoard(api: api, onDead: onDead)
    }

    var tasks: [TaskDTO] { board.tasks }
    var loaded: Bool { board.loaded }
    var notice: String? {
        get { board.notice }
        set { board.notice = newValue }
    }

    func load() async {
        async let lists: Void = board.loadLists()
        await board.load(includeCompleted: prefs.showCompleted)
        await lists
    }

    func toggle(_ t: TaskDTO) {
        let hide = !prefs.showCompleted && !t.completed
        Task {
            await board.toggle(t)
            // Выполненные скрыты — отмеченная уходит из списка, не сразу, чтобы галочку было видно.
            if hide {
                try? await Task.sleep(for: .milliseconds(600))
                board.dropIfDone(t.id)
            }
        }
    }

    func newTask() {
        draft = TaskDraft.new(lists: board.lists)
        // Списки могли не успеть загрузиться — форма подхватит первый, когда придут.
        if board.lists.isEmpty {
            Task {
                await board.loadLists()
                if var d = draft, d.original == nil, d.calendarID == 0 {
                    d.calendarID = board.lists.first?.id ?? 0
                    draft = d
                }
            }
        }
    }

    func edit(_ t: TaskDTO) {
        guard t.id > 0 else { return } // ещё не создана на сервере
        draft = TaskDraft.of(t)
    }

    /// Форма закрывается сразу, строка меняется сразу; отказ сервера откатывает её.
    func save() {
        guard let d = draft else { return }
        if let err = board.validate(d) {
            draft?.error = err
            return
        }
        draft = nil
        Task { await board.save(d) }
    }

    func delete(_ t: TaskDTO) {
        draft = nil
        Task { await board.delete(t) }
    }

    enum DueTone { case overdue, today, later }

    static func tone(_ t: TaskDTO, now: Date = Date(), cal: Calendar = .current) -> DueTone? {
        guard let due = t.due, !t.completed else { return nil }
        let d = Date(timeIntervalSince1970: TimeInterval(due) / 1000)
        if cal.isDate(d, inSameDayAs: now) { return .today }
        return d < now ? .overdue : .later
    }
}

struct TasksView: View {
    @Bindable var model: TasksModel
    @Environment(\.horizontalSizeClass) private var sizeClass

    var body: some View {
        let showDone = model.prefs.showCompleted
        let compact = sizeClass == .compact
        NavigationStack {
            VStack(spacing: 0) {
                // Первая строка — как у десктопа: бургер, «Задачи», «выполненные»;
                // «+» — как в «Контактах».
                SectionBar {
                    Text("Задачи").font(.title3.bold())
                    Spacer(minLength: 8)
                    // На узком экране — иконкой (iOS §0 «иконки вместо надписей»):
                    // надпись переносилась в две строки и раздвигала первую строку.
                    if compact {
                        showDoneToggle(showDone)
                            .labelStyle(.iconOnly)
                            .font(.title3)
                            .frame(width: 36, height: 36)
                    } else {
                        showDoneToggle(showDone).font(.subheadline.weight(.semibold))
                    }
                    Button("Новая задача", systemImage: "plus") { model.newTask() }
                        .labelStyle(.iconOnly)
                        .font(.title3)
                        .buttonStyle(.borderless)
                        .frame(width: 36, height: 36)
                }
                Divider()
                List {
                    ForEach(model.tasks) { t in
                        TaskRow(task: t, toggle: { model.toggle(t) }, open: { model.edit(t) })
                    }
                }
                .listStyle(.plain)
                .overlay {
                    if !model.loaded {
                        ProgressView()
                    } else if model.tasks.isEmpty {
                        ContentUnavailableView(showDone ? "Задач нет" : "Невыполненных задач нет", systemImage: "checklist")
                    }
                }
                .refreshable { await model.load() }
            }
            .sectionRoot()
            .overlay(alignment: .bottom) { NoticeBanner(text: $model.notice) }
            .task { await model.load() }
        }
        .sheet(isPresented: Binding(get: { model.draft != nil }, set: { if !$0 { model.draft = nil } })) {
            TaskFormView(model: model)
        }
    }

    /// «Показать выполненные» — тумблер-кнопка; надпись — и для VoiceOver.
    private func showDoneToggle(_ on: Bool) -> some View {
        Toggle(isOn: Binding(get: { on }, set: {
            model.prefs.showCompleted = $0
            Task { await model.load() }
        })) {
            Label("Показать выполненные", systemImage: on ? "checkmark.circle.fill" : "checkmark.circle")
        }
        .toggleStyle(.button)
        .buttonStyle(.borderless)
        .accessibilityLabel("Показать выполненные")
        .accessibilityIdentifier("tasks.showDone")
    }
}

private struct TaskRow: View {
    let task: TaskDTO
    let toggle: () -> Void
    let open: () -> Void

    var body: some View {
        HStack(alignment: .top, spacing: 12) {
            Button(action: toggle) {
                Image(systemName: task.completed ? "checkmark.circle.fill" : "circle")
                    .font(.title3)
                    .foregroundStyle(task.completed ? Palette.accent : .secondary)
            }
            .buttonStyle(.plain)
            // Строки коллекций только для чтения на галочку не реагируют.
            .disabled(!task.canWrite || task.id < 0)
            .accessibilityLabel(task.completed ? "Выполнена" : "Не выполнена")
            // Тап по строке — форма задачи (правка, удаление).
            Button(action: open) {
                VStack(alignment: .leading, spacing: 3) {
                    Text(task.summary.isEmpty ? "(без названия)" : task.summary)
                        .strikethrough(task.completed)
                        .foregroundStyle(task.completed ? .secondary : .primary)
                    HStack(spacing: 8) {
                        if let due = task.due {
                            Text(Date(timeIntervalSince1970: TimeInterval(due) / 1000)
                                .formatted(task.allDay ? .dateTime.day().month(.abbreviated) : .dateTime.day().month(.abbreviated).hour().minute()))
                                .foregroundStyle(color)
                        }
                        if !task.calendarName.isEmpty { Text(task.calendarName).foregroundStyle(.secondary) }
                    }
                    .font(.caption)
                }
                .frame(maxWidth: .infinity, alignment: .leading)
                .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
        }
        .padding(.vertical, 2)
    }

    private var color: Color {
        switch TasksModel.tone(task) {
        case .overdue: .red
        case .today: .orange
        default: .secondary
        }
    }
}

/// Форма задачи: заголовок, заметка, срок (датой или со временем), список —
/// только у новой (перенос между списками сервер не делает). У существующей —
/// удаление. Задача из списка только для чтения открывается без правки.
private struct TaskFormView: View {
    @Bindable var model: TasksModel
    @State private var askDelete = false

    var body: some View {
        if let f = model.draft {
            let ro = f.readOnly
            NavigationStack {
                Form {
                    Section {
                        TextField("Заголовок", text: bind(\.summary))
                        TextField("Заметка", text: bind(\.description), axis: .vertical)
                            .lineLimit(3...8)
                    }
                    .disabled(ro)
                    Section {
                        Toggle("Срок", isOn: bind(\.hasDue))
                        if f.hasDue {
                            Toggle("Весь день", isOn: bind(\.allDay))
                            DatePicker(
                                f.allDay ? "Дата" : "Дата и время", selection: bind(\.due),
                                displayedComponents: f.allDay ? [.date] : [.date, .hourAndMinute]
                            )
                        }
                    }
                    .disabled(ro)
                    if let orig = f.original {
                        if !orig.calendarName.isEmpty {
                            Section { LabeledContent("Список", value: orig.calendarName) }
                        }
                    } else {
                        Section {
                            if model.board.lists.isEmpty {
                                Text("Нет списков задач, доступных на запись").foregroundStyle(.red)
                            } else {
                                Picker("Список", selection: bind(\.calendarID)) {
                                    ForEach(model.board.lists) { c in Text(c.name.isEmpty ? "Задачи" : c.name).tag(c.id) }
                                }
                            }
                        }
                    }
                    if ro {
                        Section { Label("Список только для чтения", systemImage: "lock").foregroundStyle(.secondary) }
                    }
                    if let e = f.error {
                        Section { Label(e, systemImage: "exclamationmark.triangle").foregroundStyle(.red) }
                    }
                    if let orig = f.original, !ro {
                        Section {
                            Button("Удалить задачу", systemImage: "trash", role: .destructive) { askDelete = true }
                        }
                        .confirmationDialog("Удалить задачу?", isPresented: $askDelete, titleVisibility: .visible) {
                            Button("Удалить", role: .destructive) { model.delete(orig) }
                        } message: {
                            Text(orig.summary.isEmpty ? "(без названия)" : orig.summary)
                        }
                    }
                }
                .formStyle(.grouped)
                .navigationTitle(f.original == nil ? "Новая задача" : "Задача")
                .toolbarTitleDisplayMode(.inline)
                .toolbar {
                    ToolbarItem(placement: .cancellationAction) { Button("Отмена", systemImage: "xmark") { model.draft = nil } }
                    if !ro {
                        ToolbarItem(placement: .confirmationAction) { Button("Сохранить", systemImage: "checkmark") { model.save() } }
                    }
                }
            }
        }
    }

    private func bind<T>(_ kp: WritableKeyPath<TaskDraft, T>) -> Binding<T> {
        Binding(
            get: { model.draft?[keyPath: kp] ?? TaskDraft(due: Date())[keyPath: kp] },
            set: {
                model.draft?[keyPath: kp] = $0
                model.draft?.error = nil
            }
        )
    }
}

