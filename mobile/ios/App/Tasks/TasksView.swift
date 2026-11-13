import DDMailAPI
import SwiftUI

/// Задачи (Android §14): отдельный раздел, окна по времени нет (забираются
/// все), выполненные скрыты — переключатель меняет запрос к серверу;
/// галочка оптимистична, отказ откатывает строку. Создавать задачи сервер
/// пока не умеет — и клиент тоже.
@MainActor @Observable
final class TasksModel {
    let api: SessionAPI
    private let onDead: () -> Void
    private(set) var tasks: [TaskDTO] = []
    /// «Ещё грузится» и «правда пусто» — разные состояния.
    private(set) var loaded = false
    var notice: String?
    let prefs = CalendarPrefs.shared

    init(api: SessionAPI, onDead: @escaping () -> Void) {
        self.api = api
        self.onDead = onDead
    }

    func load() async {
        do {
            tasks = try await api.tasks(includeCompleted: prefs.showCompleted)
            loaded = true
        } catch APIError.unauthorized {
            onDead()
        } catch is CancellationError {
        } catch {
            notice = error.localizedDescription
        }
    }

    func toggle(_ t: TaskDTO) {
        guard t.canWrite, let i = tasks.firstIndex(where: { $0.id == t.id }) else { return }
        let want = !t.completed
        tasks[i].completed = want
        Task {
            do {
                try await api.setTaskCompleted(id: t.id, completed: want)
                if !prefs.showCompleted && want {
                    try? await Task.sleep(for: .milliseconds(600))
                    tasks.removeAll { $0.id == t.id }
                }
            } catch {
                if case APIError.unauthorized = error { onDead() }
                // Отказ — строка возвращается как была.
                if let j = tasks.firstIndex(where: { $0.id == t.id }) { tasks[j].completed = !want }
                notice = "Не удалось: \(error.localizedDescription)"
            }
        }
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

    var body: some View {
        let showDone = model.prefs.showCompleted
        NavigationStack {
            VStack(spacing: 0) {
                // Первая строка — как у десктопа: бургер, «Задачи», «выполненные».
                SectionBar {
                    Text("Задачи").font(.title3.bold())
                    Spacer(minLength: 8)
                    Toggle(isOn: Binding(get: { showDone }, set: {
                        model.prefs.showCompleted = $0
                        Task { await model.load() }
                    })) {
                        Label("Показать выполненные", systemImage: showDone ? "checkmark.square.fill" : "square")
                    }
                    .toggleStyle(.button)
                    .buttonStyle(.borderless)
                    .font(.subheadline.weight(.semibold))
                }
                Divider()
                List {
                    ForEach(model.tasks) { t in
                        TaskRow(task: t) { model.toggle(t) }
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
    }
}

private struct TaskRow: View {
    let task: TaskDTO
    let toggle: () -> Void

    var body: some View {
        HStack(alignment: .top, spacing: 12) {
            Button(action: toggle) {
                Image(systemName: task.completed ? "checkmark.circle.fill" : "circle")
                    .font(.title3)
                    .foregroundStyle(task.completed ? Palette.accent : .secondary)
            }
            .buttonStyle(.plain)
            // Строки коллекций только для чтения на галочку не реагируют.
            .disabled(!task.canWrite)
            .accessibilityLabel(task.completed ? "Выполнена" : "Не выполнена")
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
        }
        .padding(.vertical, 2)
        .accessibilityElement(children: .combine)
    }

    private var color: Color {
        switch TasksModel.tone(task) {
        case .overdue: .red
        case .today: .orange
        default: .secondary
        }
    }
}
