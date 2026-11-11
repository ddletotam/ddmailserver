import SwiftUI

@main
struct DDMailApp: App {
    @State private var app = AppModel()

    var body: some Scene {
        WindowGroup {
            RootView(app: app)
        }
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
