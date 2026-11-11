import DDMailAPI
import DDMailSync
import QuickLook
import SwiftUI
import WebKit

/// Открытая беседа: лента — общий документ в одном `WKWebView` (§8), вокруг —
/// нативное: шапка, меню пузыря и вложений, просмотр и сохранение файлов.
struct ConversationView: View {
    let conversation: Conversation
    @Bindable var model: ConversationModel
    let compose: ComposeModel
    let onSpam: () -> Void
    @State private var exportShown = false
    @Environment(\.horizontalSizeClass) private var sizeClass

    var body: some View {
        WebViewContainer(web: model.web)
            .background(Color(white: 0.96))

            .navigationTitle(conversation.label)
            .toolbarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .principal) { header }
                if !conversation.spam {
                    ToolbarItem(placement: .primaryAction) {
                        Button("В спам", systemImage: "hand.raised", action: onSpam)
                    }
                }
            }
            .quickLookPreview($model.preview)
            .fileMover(isPresented: $exportShown, file: model.exporting) { result in
                if case let .failure(e) = result { model.notice = "Не удалось сохранить: \(e.localizedDescription)" }
                model.exporting = nil
            }
            .onChange(of: model.exporting) { exportShown = model.exporting != nil }
            .confirmationDialog(bubbleTitle, isPresented: present($model.bubbleMenu), titleVisibility: .visible) {
                if let b = model.bubbleMenu {
                    if !conversation.spam {
                        Button("Ответить", systemImage: "arrowshape.turn.up.left") { compose.replyTo = b }
                        Button("Переслать", systemImage: "arrowshape.turn.up.right") { compose.openForward(b) }
                    }
                    Button("Копировать текст", systemImage: "doc.on.doc") { Clipboard.copy(PlainText.of(b)) }
                    if !b.fromAddr.isEmpty {
                        Button("Копировать адрес", systemImage: "at") { Clipboard.copy(b.fromAddr) }
                    }
                }
            }
            .confirmationDialog(model.attachmentMenu?.name ?? "", isPresented: present($model.attachmentMenu), titleVisibility: .visible) {
                if let a = model.attachmentMenu {
                    Button("Открыть", systemImage: "eye") { model.openAttachment(uid: a.uid, index: a.index) }
                    Button("Сохранить в «Файлы»", systemImage: "square.and.arrow.down") { model.saveAttachment(uid: a.uid, index: a.index) }
                }
            }
            .overlay(alignment: .bottom) { NoticeBanner(text: $model.notice) }
            // Ответа в спам нет: на спам не отвечают (§5).
            .safeAreaInset(edge: .bottom, spacing: 0) {
                if !conversation.spam { ComposerBar(conversation: conversation, model: compose) }
            }
            .hidingTabBar(sizeClass == .compact)
    }

    /// Имя беседы и её адреса: кто → на какой мой адрес пришло.
    private var header: some View {
        VStack(spacing: 1) {
            Text(conversation.label).font(.headline).lineLimit(1)
            Text(addresses).font(.caption).foregroundStyle(.secondary).lineLimit(1)
        }
        .accessibilityElement(children: .combine)
    }

    private var addresses: String {
        let cps = conversation.counterparts.map(\.addr).filter { !$0.isEmpty }.joined(separator: ", ")
        return conversation.receivedBy.isEmpty ? cps : "\(cps) → \(conversation.receivedBy)"
    }

    private var bubbleTitle: String {
        guard let b = model.bubbleMenu else { return "" }
        return b.subject.isEmpty ? "(без темы)" : b.subject
    }

    private func present<T>(_ v: Binding<T?>) -> Binding<Bool> {
        // Значение читается сразу (в теле), а не в ленивом замыкании.
        let shown = v.wrappedValue != nil
        return Binding(get: { shown }, set: { if !$0 { v.wrappedValue = nil } })
    }
}

/// Хозяин общего `WKWebView`: тот же экземпляр переезжает между раскладками.
#if os(iOS)
struct WebViewContainer: UIViewRepresentable {
    let web: ConversationWeb

    func makeUIView(context: Context) -> UIView {
        let host = UIView()
        attach(to: host)
        return host
    }

    func updateUIView(_ host: UIView, context: Context) { attach(to: host) }

    func sizeThatFits(_ proposal: ProposedViewSize, uiView: UIView, context: Context) -> CGSize? {
        CGSize(width: proposal.width ?? 320, height: proposal.height ?? 480)
    }

    private func attach(to host: UIView) {
        guard let v = web.view, v.superview !== host else { return }
        v.removeFromSuperview()
        v.translatesAutoresizingMaskIntoConstraints = false
        host.addSubview(v)
        NSLayoutConstraint.activate([
            v.leadingAnchor.constraint(equalTo: host.leadingAnchor),
            v.trailingAnchor.constraint(equalTo: host.trailingAnchor),
            v.topAnchor.constraint(equalTo: host.topAnchor),
            v.bottomAnchor.constraint(equalTo: host.bottomAnchor),
        ])
    }
}
#else
struct WebViewContainer: NSViewRepresentable {
    let web: ConversationWeb

    func makeNSView(context: Context) -> NSView {
        let host = NSView()
        attach(to: host)
        return host
    }

    func updateNSView(_ host: NSView, context: Context) { attach(to: host) }

    /// Занять всё предложенное: без этого AppKit-обёртка сжимается до своего
    /// «естественного» размера, и лента с композером съезжают от края колонки.
    func sizeThatFits(_ proposal: ProposedViewSize, nsView: NSView, context: Context) -> CGSize? {
        CGSize(width: proposal.width ?? 480, height: proposal.height ?? 480)
    }

    private func attach(to host: NSView) {
        guard let v = web.view, v.superview !== host else { return }
        v.removeFromSuperview()
        v.translatesAutoresizingMaskIntoConstraints = false
        host.addSubview(v)
        NSLayoutConstraint.activate([
            v.leadingAnchor.constraint(equalTo: host.leadingAnchor),
            v.trailingAnchor.constraint(equalTo: host.trailingAnchor),
            v.topAnchor.constraint(equalTo: host.topAnchor),
            v.bottomAnchor.constraint(equalTo: host.bottomAnchor),
        ])
    }
}
#endif

/// Ссылки — во внешнем приложении (браузер, почта, звонилка).
enum LinkOpener {
    @MainActor static func open(_ url: URL) {
        #if os(iOS)
        UIApplication.shared.open(url)
        #else
        NSWorkspace.shared.open(url)
        #endif
    }
}

enum Clipboard {
    @MainActor static func copy(_ s: String) {
        #if os(iOS)
        UIPasteboard.general.string = s
        #else
        NSPasteboard.general.clearContents()
        NSPasteboard.general.setString(s, forType: .string)
        #endif
    }
}

/// Текст письма для копирования: текстовая часть, иначе HTML без тегов.
enum PlainText {
    static func of(_ b: MessageBody) -> String {
        if let t = b.text, !t.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty { return t }
        guard let html = b.html else { return "" }
        var s = html.replacingOccurrences(of: "(?is)<(script|style)[^>]*>.*?</\\1>", with: "", options: .regularExpression)
        s = s.replacingOccurrences(of: "(?i)<br\\s*/?>|</p>|</div>|</li>|</tr>", with: "\n", options: .regularExpression)
        s = s.replacingOccurrences(of: "<[^>]+>", with: "", options: .regularExpression)
        for (e, c) in ["&nbsp;": " ", "&lt;": "<", "&gt;": ">", "&quot;": "\"", "&#39;": "'", "&amp;": "&"] {
            s = s.replacingOccurrences(of: e, with: c)
        }
        return s.replacingOccurrences(of: "\n{3,}", with: "\n\n", options: .regularExpression)
            .trimmingCharacters(in: .whitespacesAndNewlines)
    }
}

private extension View {
    /// Открытая беседа на узком экране — без таб-бара разделов (Android §16): у
    /// неё своя шапка, а внизу место строке ввода. На широком (iPad, Мак)
    /// разделы видны всегда (§6).
    @ViewBuilder func hidingTabBar(_ hide: Bool) -> some View {
        #if os(iOS)
        toolbarVisibility(hide ? .hidden : .automatic, for: .tabBar)
        #else
        self
        #endif
    }
}
