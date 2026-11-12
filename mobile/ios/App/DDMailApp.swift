import DDMailStore
import SwiftUI
import UserNotifications
#if os(iOS)
import BackgroundTasks
#endif

@main
struct DDMailApp: App {
    @State private var app: AppModel
    @Environment(\.scenePhase) private var scenePhase
    // Токен APNs SwiftUI не отдаёт — только делегат приложения (§7).
    #if os(iOS)
    @UIApplicationDelegateAdaptor(AppDelegate.self) private var delegate
    #else
    @NSApplicationDelegateAdaptor(AppDelegate.self) private var delegate
    #endif

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
                await BackgroundSync.withDatabase {
                    await model.workspace?.prefetch()
                }
                task.setTaskCompleted(success: !Task.isCancelled)
            }
            task.expirationHandler = { work.cancel() }
        }
        #endif
    }

    var body: some Scene {
        mainWindow
        #if os(macOS)
        // Беседа в отдельном окне (§6): своя лента и свой композер.
        WindowGroup("Беседа", id: "conversation", for: String.self) { $id in
            if let id { ConversationWindow(app: app, conversationID: id) }
        }
        .defaultSize(width: 720, height: 820)
        #endif
    }

    private var mainWindow: some Scene {
        WindowGroup {
            RootView(app: app)
        }
        .commands { MailCommands() }
        #if os(macOS)
        // Три колонки (разделы, список, беседа) — окну нужен простор.
        .defaultSize(width: 1280, height: 820)
        #endif
        .onChange(of: scenePhase) {
            // Уходим в фон — поставить фоновый синк и докачку (§4, §7).
            if scenePhase == .background { Task { await BackgroundSync.schedule() } }
            #if os(iOS)
            // Кэш в общем контейнере: усыплённое с блокировкой SQLite
            // приложение iOS убивает — в фоне база закрыта для запросов.
            if scenePhase == .background { MailStore.suspend() } else { MailStore.resume() }
            #endif
            if scenePhase == .active {
                // Пока нас не было, кэш мог пополнить push (расширение).
                try? app.workspace?.cache?.externalChange()
            }
        }
        #if os(iOS)
        .backgroundTask(.appRefresh(BackgroundSync.refreshID)) { [app] in
            await BackgroundSync.schedule()
            await BackgroundSync.withDatabase {
                await app.workspace?.backgroundRefresh()
            }
        }
        #endif
    }
}

/// Без учёток — подключение; с учётками — разделы приложения. «Добавить
/// учётку» и «Войти» — то же окно подключения поверх.
struct RootView: View {
    @Bindable var app: AppModel

    var body: some View {
        let sheet = app.connectSheet
        Group {
            switch app.phase {
            case let .connect(model):
                ConnectView(model: model)
            case let .ready(workspace):
                AppShell(app: app, workspace: workspace)
            }
        }
        .sheet(isPresented: Binding(get: { sheet != nil }, set: { if !$0 { app.connectSheet = nil } })) {
            if let sheet {
                ConnectView(model: sheet) { app.connectSheet = nil }
                    #if os(macOS)
                    .frame(minWidth: 520, minHeight: 560)
                    #endif
            }
        }
    }
}
