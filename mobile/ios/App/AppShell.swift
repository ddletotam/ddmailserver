import SwiftUI

/// Разделы приложения (контракт §6, Android §16): таб-бар внизу на iPhone,
/// боковая панель на iPad и Маке — `.sidebarAdaptable` решает сам по ширине окна.
struct AppShell: View {
    @SceneStorage("section") private var section: Section = .mail

    var body: some View {
        TabView(selection: $section) {
            ForEach(Section.allCases) { s in
                Tab(s.title, systemImage: s.symbol, value: s) {
                    NavigationStack {
                        ContentUnavailableView(s.title, systemImage: s.symbol, description: Text("Скоро"))
                            .navigationTitle(s.title)
                    }
                }
            }
        }
        .tabViewStyle(.sidebarAdaptable)
    }
}

enum Section: String, CaseIterable, Identifiable {
    case mail, calendar, tasks, contacts

    var id: Self { self }

    var title: String {
        switch self {
        case .mail: "Почта"
        case .calendar: "Календарь"
        case .tasks: "Задачи"
        case .contacts: "Контакты"
        }
    }

    var symbol: String {
        switch self {
        case .mail: "envelope"
        case .calendar: "calendar"
        case .tasks: "checklist"
        case .contacts: "person.crop.circle"
        }
    }
}

#Preview {
    AppShell()
}
