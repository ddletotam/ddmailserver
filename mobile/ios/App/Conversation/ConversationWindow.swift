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
        let mail = app.workspace?.mail
        let conv = mail?.all.first { $0.ref == conversationID }
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
            guard model == nil, let mail, let ws = app.workspace else { return }
            let dead: @MainActor (String) -> Void = { [weak ws] key in ws?.link(key)?.report(.dead) }
            let m = ConversationModel(provider: { [weak ws] key in ws?.link(key)?.provider }, onAuthDead: dead)
            compose = ComposeModel(workspace: ws, conversation: m,
                                   syncSoon: { key in Task { await mail.sync(only: key) } },
                                   onAuthDead: dead)
            model = m
            // Лента готовится асинхронно; документ откроется, как только она будет.
            m.open(conv)
        }
    }
}
#endif
