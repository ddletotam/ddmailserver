import SwiftUI

/// Действия окна для меню приложения (Мак, клавиатура iPad): команды берут их
/// у окна в фокусе, поэтому в каждом окне действуют на своё.
struct MailActions {
    var newMail: () -> Void
    var reply: (() -> Void)?
    var forward: (() -> Void)?
    var delete: (() -> Void)?
    var refresh: () -> Void
    var go: (AppSection) -> Void
}

extension FocusedValues {
    @Entry var mailActions: MailActions?
}

/// Меню как у Apple Mail: ⌘N — новое, ⌘R — ответить, ⇧⌘F — переслать,
/// ⇧⌘N — проверить почту, ⌘⌫ — удалить, ⌘1…⌘4 — разделы.
struct MailCommands: Commands {
    @FocusedValue(\.mailActions) private var actions

    var body: some Commands {
        CommandGroup(replacing: .newItem) {
            Button("Новое письмо") { actions?.newMail() }
                .keyboardShortcut("n")
                .disabled(actions == nil)
        }
        CommandMenu("Письмо") {
            Button("Ответить") { actions?.reply?() }
                .keyboardShortcut("r")
                .disabled(actions?.reply == nil)
            Button("Переслать") { actions?.forward?() }
                .keyboardShortcut("f", modifiers: [.command, .shift])
                .disabled(actions?.forward == nil)
            Divider()
            Button("Удалить беседу") { actions?.delete?() }
                .keyboardShortcut(.delete)
                .disabled(actions?.delete == nil)
            Divider()
            Button("Проверить почту") { actions?.refresh() }
                .keyboardShortcut("n", modifiers: [.command, .shift])
                .disabled(actions == nil)
        }
        CommandGroup(after: .sidebar) {
            ForEach(Array(AppSection.allCases.enumerated()), id: \.offset) { i, s in
                Button(s.title) { actions?.go(s) }
                    .keyboardShortcut(KeyEquivalent(Character("\(i + 1)")))
                    .disabled(actions == nil)
            }
        }
    }
}
