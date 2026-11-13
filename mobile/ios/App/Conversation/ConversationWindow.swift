#if os(macOS)
import DDMailSync
import SwiftUI

/// Беседа в отдельном окне (Мак, контракт §6). У окна своя лента (`WKWebView`)
/// и свой композер: два окна не делят один документ ленты. Меню «Письмо» и
/// горячие клавиши действуют на эту беседу (`MailActions` окна), «В спам» и
/// удаление — с теми же подтверждениями, что в главном окне; после них окно
/// закрывается (S-08).
struct ConversationWindow: View {
    let app: AppModel
    let conversationID: String
    @State private var model: ConversationModel?
    @State private var compose: ComposeModel?
    @State private var confirmSpam: Set<String>?
    @State private var confirmDelete: Set<String>?
    @Environment(\.dismiss) private var dismiss

    var body: some View {
        let mail = app.workspace?.mail
        let conv = mail?.all.first { $0.ref == conversationID }
        // Наблюдаемое — в теле (контракт §0): замыкания `sheet` и `Binding` ленивы.
        let forwarding = compose?.sheet != nil
        Group {
            if let conv, let model, let compose {
                NavigationStack {
                    ConversationView(conversation: conv, model: model, compose: compose) { confirmSpam = [conv.ref] }
                }
                .onChange(of: conv) { model.updated(conv) }
            } else if mail == nil {
                ContentUnavailableView("Нет входа", systemImage: "person.crop.circle.badge.exclamationmark")
            } else {
                ContentUnavailableView("Беседа не найдена", systemImage: "bubble.left.and.bubble.right")
            }
        }
        .frame(minWidth: 480, minHeight: 520)
        // Имя беседы — для меню «Окно»; в панели окна — только шапка беседы,
        // без системного заголовка рядом со светофором (M-12).
        .navigationTitle(conv?.label ?? "Беседа")
        .toolbar(removing: .title)
        .focusedSceneValue(\.mailActions, actions(mail, conv))
        .modifier(OptionalConfirmations(mail: mail, spam: $confirmSpam, delete: $confirmDelete) { dismiss() })
        .sheet(isPresented: Binding(get: { forwarding }, set: { if !$0 { compose?.sheet = nil } })) {
            if let compose { ComposeSheetView(model: compose) }
        }
        .task {
            guard model == nil, let mail, let ws = app.workspace else { return }
            let dead: @MainActor (String) -> Void = { [weak ws] key in ws?.link(key)?.report(.dead) }
            let m = ConversationModel(provider: { [weak ws] key in ws?.link(key)?.provider }, onAuthDead: dead)
            let c = ComposeModel(workspace: ws, conversation: m,
                                 syncSoon: { key in Task { await mail.sync(only: key) } },
                                 onAuthDead: dead)
            c.enter(conversationID)
            compose = c
            model = m
            // Лента готовится асинхронно; документ откроется, как только она будет.
            m.open(conv)
        }
    }

    /// Меню «Письмо» этого окна: ответ и пересылка — в его композере,
    /// удаление — с подтверждением. Поиска и разделов у окна нет.
    private func actions(_ mail: MailModel?, _ conv: Conversation?) -> MailActions? {
        guard let mail else { return nil }
        let last = model?.lastIncoming ?? model?.last
        let canReply = conv.map { !$0.spam } ?? false
        return MailActions(
            newMail: nil,
            reply: canReply ? { [compose] in compose?.replyTo = last } : nil,
            forward: canReply ? last.map { b in { [compose] in compose?.openForward(b) } } : nil,
            delete: conv.map { c in { confirmDelete = [c.ref] } },
            refresh: { Task { await mail.sync() } },
            go: nil,
            settings: nil
        )
    }
}

/// Подтверждения «В спам» и удаления, когда почты может не быть.
private struct OptionalConfirmations: ViewModifier {
    let mail: MailModel?
    @Binding var spam: Set<String>?
    @Binding var delete: Set<String>?
    let done: () -> Void

    func body(content: Content) -> some View {
        if let mail {
            content
                .modifier(SpamConfirmation(model: mail, ids: $spam, done: done))
                .modifier(DeleteConfirmation(model: mail, ids: $delete, done: done))
        } else {
            content
        }
    }
}
#endif
