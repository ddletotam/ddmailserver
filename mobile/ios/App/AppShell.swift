import SwiftUI

/// Разделы приложения (контракт §6, Android §16): таб-бар внизу на iPhone,
/// боковая панель на iPad и Маке — `.sidebarAdaptable` решает сам по ширине окна.
struct AppShell: View {
    let app: AppModel
    let connection: Connection
    @SceneStorage("section") private var section: AppSection = .mail
    @Environment(\.scenePhase) private var scenePhase

    var body: some View {
        TabView(selection: $section) {
            ForEach(AppSection.allCases) { s in
                Tab(s.title, systemImage: s.symbol, value: s) {
                    if s == .mail, let mail = connection.mail {
                        MailView(model: mail, app: app, connection: connection)
                    } else {
                        NavigationStack {
                            SectionPlaceholder(section: s, app: app, connection: connection)
                        }
                    }
                }
            }
        }
        .tabViewStyle(.sidebarAdaptable)
        // «Нужен вход» — несмываемая плашка над содержимым, кэш виден (контракт §2).
        .safeAreaInset(edge: .top, spacing: 0) {
            if connection.status == .dead {
                SessionDeadBanner(relogin: app.relogin)
            }
        }
        // Синк на выход приложения на экран (§4); WebSocket — следующим шагом.
        .task(id: scenePhase) {
            guard scenePhase == .active else {
                // Ушли с экрана — канал закрыт: в фоне его держать нельзя (§7).
                connection.mail?.live.stop()
                return
            }
            if let mail = connection.mail {
                NotificationRouter.shared.attach { [weak mail] id in
                    // Открыть беседу из уведомления: раздел «Почта», её папка.
                    section = .mail
                    mail?.open(id)
                }
                mail.live.start()
                await mail.sync()
                await Notifier.requestPermissionIfNeeded()
            } else {
                await connection.check()
            }
        }
    }
}

/// Плашка «сессия истекла»: почта не обновляется, пока не войти заново.
struct SessionDeadBanner: View {
    let relogin: () -> Void

    var body: some View {
        HStack(spacing: 12) {
            Image(systemName: "person.crop.circle.badge.exclamationmark")
                .font(.title3)
            Text("Сессия истекла: почта не обновляется.")
                .font(.subheadline)
            Spacer(minLength: 8)
            Button("Войти", action: relogin)
                .buttonStyle(.borderedProminent)
        }
        .padding(.horizontal, 16)
        .padding(.vertical, 10)
        .background(.orange.opacity(0.18))
        .accessibilityElement(children: .combine)
    }
}

/// Заглушка раздела до его этапа. Меню учётки — в шапке «Почты».
private struct SectionPlaceholder: View {
    let section: AppSection
    let app: AppModel
    let connection: Connection
    @State private var confirmLogout = false

    var body: some View {
        ContentUnavailableView {
            Label(section.title, systemImage: section.symbol)
        } description: {
            Text(statusText)
                .accessibilityIdentifier("session.status")
        }
        .navigationTitle(section.title)
        .toolbar {
            if section == .mail {
                ToolbarItem(placement: .primaryAction) {
                    Menu("Учётная запись", systemImage: "person.crop.circle") {
                        Text(connection.session.accountKey)
                        Button("Выйти", systemImage: "rectangle.portrait.and.arrow.right", role: .destructive) {
                            confirmLogout = true
                        }
                    }
                }
            }
        }
        .confirmationDialog("Выйти из учётной записи?", isPresented: $confirmLogout, titleVisibility: .visible) {
            Button("Выйти", role: .destructive, action: app.logout)
        } message: {
            Text("Сессия и данные учётной записи будут удалены с устройства.")
        }
    }

    private var statusText: String {
        switch connection.status {
        case .checking: "Проверяем сессию…"
        case .ok: "Вход выполнен: \(connection.session.accountKey)"
        case let .offline(reason): reason
        case .dead: "Нужен вход."
        }
    }
}

enum AppSection: String, CaseIterable, Identifiable {
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
