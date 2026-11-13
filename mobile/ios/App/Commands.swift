import SwiftUI

/// Действия окна для меню приложения (Мак, клавиатура iPad): команды берут их
/// у окна в фокусе, поэтому в каждом окне действуют на своё.
struct MailActions {
    /// ⌘N: нового письма вне поиска нет — фокус в поле поиска (mobile-layout.md).
    var newMail: () -> Void
    var reply: (() -> Void)?
    var forward: (() -> Void)?
    var delete: (() -> Void)?
    var refresh: () -> Void
    var go: (AppSection) -> Void
    var settings: () -> Void
}

extension FocusedValues {
    @Entry var mailActions: MailActions?
}

/// Меню как у Apple Mail: ⌘N — новое (фокус в поиск: адрес там же), ⌘R —
/// ответить, ⇧⌘F — переслать, ⇧⌘N — проверить почту, ⌘⌫ — удалить, ⌘1…⌘4 —
/// разделы, ⌘, — настройки. Ведут туда же, куда бургер.
struct MailCommands: Commands {
    @FocusedValue(\.mailActions) private var actions
    @FocusedValue(\.composerFormat) private var format

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
        // Форматирование письма (десктоп §3а: Ctrl+B/I/U, Ctrl+K). На Маке —
        // меню «Формат»; на iPad жирный, курсив и подчёркнутый клавиатурой
        // умеет само системное поле, своё — только ссылка.
        CommandMenu("Формат") {
            #if os(macOS)
            Button("Жирный") { format?.bold() }
                .keyboardShortcut("b")
                .disabled(format == nil)
            Button("Курсив") { format?.italic() }
                .keyboardShortcut("i")
                .disabled(format == nil)
            Button("Подчёркнутый") { format?.underline() }
                .keyboardShortcut("u")
                .disabled(format == nil)
            Divider()
            #endif
            Button("Ссылка из буфера") { format?.link() }
                .keyboardShortcut("k")
                .disabled(format == nil)
        }
        CommandGroup(replacing: .appSettings) {
            Button("Настройки…") { actions?.settings() }
                .keyboardShortcut(",")
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
