import Foundation
#if os(iOS)
import UIKit
#else
import AppKit
#endif

/// Меню ленты (пузырь, чип вложения) — одно описание на обе системы:
/// iPhone/iPad — `UIMenu` у пузыря, Mac — `NSMenu` в точке клика (§8, §9).
struct MenuEntry {
    enum Kind {
        case action(@MainActor () -> Void)
        case submenu([MenuEntry])
        /// Строка-пояснение без действия (статус доставки).
        case note
        case divider
    }

    var title = ""
    var image: String?
    var destructive = false
    var kind: Kind

    static func action(_ title: String, _ image: String?, destructive: Bool = false, _ run: @escaping @MainActor () -> Void) -> MenuEntry {
        MenuEntry(title: title, image: image, destructive: destructive, kind: .action(run))
    }

    static func submenu(_ title: String, _ image: String?, _ items: [MenuEntry]) -> MenuEntry {
        MenuEntry(title: title, image: image, kind: .submenu(items))
    }

    static func note(_ title: String, _ image: String?) -> MenuEntry {
        MenuEntry(title: title, image: image, kind: .note)
    }

    static var divider: MenuEntry { MenuEntry(kind: .divider) }
}

/// Где просили меню — в CSS px от начала документа ленты: прямоугольник
/// пузыря (чипа) и точка нажатия (`askMenu` в `conversation.js`).
struct DocAnchor {
    let rect: CGRect
    let point: CGPoint

    /// Шесть чисел моста после обязательных аргументов; нет их — `nil`.
    init?(_ n: [Double]) {
        guard n.count >= 6 else { return nil }
        rect = CGRect(x: n[0], y: n[1], width: n[2], height: n[3])
        point = CGPoint(x: n[4], y: n[5])
    }
}

#if os(iOS)
extension MenuEntry {
    /// Пункты в `UIMenu`: разделители — встроенными секциями (так их рисует iOS).
    @MainActor
    static func uiMenu(_ title: String, _ entries: [MenuEntry]) -> UIMenu {
        UIMenu(title: title, children: uiElements(entries))
    }

    @MainActor
    private static func uiElements(_ entries: [MenuEntry]) -> [UIMenuElement] {
        var groups: [[UIMenuElement]] = [[]]
        for e in entries {
            let image = e.image.flatMap { UIImage(systemName: $0) }
            switch e.kind {
            case .divider:
                groups.append([])
            case let .action(run):
                groups[groups.count - 1].append(UIAction(title: e.title, image: image, attributes: e.destructive ? .destructive : []) { _ in run() })
            case .note:
                groups[groups.count - 1].append(UIAction(title: e.title, image: image, attributes: .disabled) { _ in })
            case let .submenu(items):
                groups[groups.count - 1].append(UIMenu(title: e.title, image: image, children: uiElements(items)))
            }
        }
        groups.removeAll(where: \.isEmpty)
        if groups.count == 1 { return groups[0] }
        return groups.map { UIMenu(options: .displayInline, children: $0) }
    }
}

/// Невидимая кнопка над пузырём: у `UIContextMenuInteraction` нет показа по
/// команде, а кнопка с `showsMenuAsPrimaryAction` показывает своё меню по
/// `performPrimaryAction()` — системное контекстное меню, привязанное к
/// её рамке (то есть к пузырю).
final class MenuAnchorButton: UIButton {
    var onEnd: (() -> Void)?

    override func contextMenuInteraction(_ interaction: UIContextMenuInteraction, willEndFor configuration: UIContextMenuConfiguration,
                                         animator: (any UIContextMenuInteractionAnimating)?) {
        super.contextMenuInteraction(interaction, willEndFor: configuration, animator: animator)
        guard let animator else { return finish() }
        animator.addCompletion { [weak self] in self?.finish() }
    }

    private func finish() {
        let end = onEnd
        onEnd = nil
        end?()
    }
}
#else
extension MenuEntry {
    @MainActor
    static func nsMenu(_ entries: [MenuEntry]) -> NSMenu {
        let menu = NSMenu()
        menu.autoenablesItems = false
        for e in entries {
            let image = e.image.flatMap { NSImage(systemSymbolName: $0, accessibilityDescription: nil) }
            switch e.kind {
            case .divider:
                menu.addItem(.separator())
            case let .action(run):
                let item = ClosureMenuItem(e.title, run)
                item.image = image
                menu.addItem(item)
            case .note:
                let item = NSMenuItem(title: e.title, action: nil, keyEquivalent: "")
                item.image = image
                item.isEnabled = false
                menu.addItem(item)
            case let .submenu(items):
                let item = NSMenuItem(title: e.title, action: nil, keyEquivalent: "")
                item.image = image
                item.submenu = nsMenu(items)
                menu.addItem(item)
            }
        }
        return menu
    }
}

/// Пункт `NSMenu` с замыканием вместо пары target/action.
final class ClosureMenuItem: NSMenuItem {
    private let run: @MainActor () -> Void

    init(_ title: String, _ run: @escaping @MainActor () -> Void) {
        self.run = run
        super.init(title: title, action: #selector(fire), keyEquivalent: "")
        target = self
    }

    @available(*, unavailable)
    required init(coder: NSCoder) { fatalError("не из nib") }

    @objc private func fire() {
        let run = run
        MainActor.assumeIsolated { run() }
    }
}
#endif
