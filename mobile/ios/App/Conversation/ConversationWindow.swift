#if os(macOS)
import DDMailSync
import SwiftUI

/// Беседа в отдельном окне (Мак, контракт §6). У окна своя лента (`WKWebView`)
/// и свой композер: два окна не делят один документ ленты.
struct ConversationWindow: View {
    let app: AppModel
    let conversationID: String
    @State private var model: ConversationModel?
    @State private var compose: ComposeModel?

    var body: some View {
        let mail = app.connection?.mail
        let conv = mail?.all.first { $0.id == conversationID }
        Group {
            if let conv, let model, let compose {
                NavigationStack {
                    ConversationView(conversation: conv, model: model, compose: compose) {}
                }
                .onChange(of: conv) { model.updated(conv) }
            } else if mail == nil {
                ContentUnavailableView("Нет входа", systemImage: "person.crop.circle.badge.exclamationmark")
            } else {
                ContentUnavailableView("Беседа не найдена", systemImage: "bubble.left.and.bubble.right")
            }
        }
        .frame(minWidth: 480, minHeight: 520)
        .navigationTitle(conv?.label ?? "Беседа")
        .task {
            guard model == nil, let mail, let conn = app.connection else { return }
            let m = ConversationModel(engine: mail.engine) { [weak conn] in conn?.report(.dead) }
            compose = ComposeModel(engine: mail.engine, conversation: m,
                                   syncSoon: { Task { await mail.sync() } },
                                   onAuthDead: { [weak conn] in conn?.report(.dead) })
            model = m
            // Лента готовится асинхронно; документ откроется, как только она будет.
            m.open(conv)
        }
    }
}
#endif
