import SwiftUI
import UserNotifications
#if os(iOS)
import BackgroundTasks
#endif

@main
struct DDMailApp: App {
    @State private var app: AppModel
    @Environment(\.scenePhase) private var scenePhase

    init() {
        let model = AppModel()
        _app = State(initialValue: model)
        UNUserNotificationCenter.current().delegate = NotificationRouter.shared
        Task { await ReminderService.registerCategory() }
        #if os(iOS)
        // Докачка тел — задача обработки: SwiftUI `.backgroundTask` её не
        // умеет, регистрируем до конца запуска, как требует BGTaskScheduler.
        BGTaskScheduler.shared.register(forTaskWithIdentifier: BackgroundSync.prefetchID, using: nil) { task in
            nonisolated(unsafe) let task = task
            let work = Task { @MainActor in
                await model.connection?.prefetch()
                task.setTaskCompleted(success: !Task.isCancelled)
            }
            task.expirationHandler = { work.cancel() }
        }
        #endif
    }

    var body: some Scene {
        WindowGroup {
            RootView(app: app)
        }
        #if os(macOS)
        // Три колонки (разделы, список, беседа) — окну нужен простор.
        .defaultSize(width: 1280, height: 820)
        #endif
        .onChange(of: scenePhase) {
            // Уходим в фон — поставить фоновый синк и докачку (§4, §7).
            if scenePhase == .background { Task { await BackgroundSync.schedule() } }
        }
        #if os(iOS)
        .backgroundTask(.appRefresh(BackgroundSync.refreshID)) {
            await BackgroundSync.schedule()
            await app.connection?.backgroundRefresh()
        }
        #endif
    }
}

/// Без сессии — подключение; с сессией — разделы приложения.
struct RootView: View {
    let app: AppModel

    var body: some View {
        switch app.phase {
        case let .connect(model):
            ConnectView(model: model)
        case let .connected(connection):
            AppShell(app: app, connection: connection)
        }
    }
}
