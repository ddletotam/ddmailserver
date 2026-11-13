import DDMailSync
import Foundation
import SwiftUI

/// Тело письма с форматированием (десктоп §3а): жирный, курсив,
/// подчёркнутый, ссылки. Редактор — системный `TextEditor` над
/// `AttributedString` (iOS/macOS 26+): форматируют выделенное системным меню
/// выделения (iPhone/iPad) или ⌘B/⌘I/⌘U, ссылка — ⌘K или «+» → «Ссылка из
/// буфера» (адрес из буфера обмена на выделение, как Ctrl+K десктопа).
///
/// Письмо уходит в двух версиях: `text/plain` — символы как есть, `text/html`
/// — `<div>`-абзацы (пустой — `<div><br></div>`), `<b>/<i>/<u>/<a>`; адреса в
/// тексте становятся ссылками (автоссылки). Прочие атрибуты (кегль, цвет),
/// которые может поставить системная панель, в письмо не уходят: письмо
/// пишется в стиле получателя, как у десктопа.
enum RichText {
    struct Style: Equatable {
        var bold = false
        var italic = false
        var underline = false
        var link: URL?
    }

    /// Стиль прогона. Жирный и курсив системная панель ставит шрифтом
    /// (`Font`, разрешаем в контексте экрана) или смыслом
    /// (`inlinePresentationIntent`) — понимаем оба.
    static func style(_ c: AttributeContainer, _ ctx: Font.Context) -> Style {
        var s = Style()
        if let f = c[AttributeScopes.SwiftUIAttributes.FontAttribute.self] {
            let r = f.resolve(in: ctx)
            s.bold = r.isBold
            s.italic = r.isItalic
        }
        if let i = c[AttributeScopes.FoundationAttributes.InlinePresentationIntentAttribute.self] {
            if i.contains(.stronglyEmphasized) { s.bold = true }
            if i.contains(.emphasized) { s.italic = true }
        }
        s.underline = c[AttributeScopes.SwiftUIAttributes.UnderlineStyleAttribute.self] != nil || platformUnderline(c)
        s.link = c[AttributeScopes.FoundationAttributes.LinkAttribute.self]
        return s
    }

    private static func platformUnderline(_ c: AttributeContainer) -> Bool {
        #if os(iOS)
        c[AttributeScopes.UIKitAttributes.UnderlineStyleAttribute.self].map { $0 != [] } ?? false
        #else
        c[AttributeScopes.AppKitAttributes.UnderlineStyleAttribute.self].map { $0 != [] } ?? false
        #endif
    }

    /// Текстовая версия: символы без объектов-вставок (U+FFFC).
    static func plain(_ s: AttributedString) -> String {
        String(s.characters).replacingOccurrences(of: "\u{FFFC}", with: "")
    }

    /// Версия HTML. Без единого стиля и ссылки — пусто: тогда тело HTML
    /// строится из текста (`Draft.bodyHTML`) и не отличается от прежнего.
    static func html(_ s: AttributedString, _ ctx: Font.Context) -> String {
        var runs: [(String, Style)] = []
        for run in s.runs {
            let text = String(s[run.range].characters).replacingOccurrences(of: "\u{FFFC}", with: "")
            if text.isEmpty { continue }
            runs.append((text, style(run.attributes, ctx)))
        }
        let styled = runs.contains { $0.1 != Style() }
        let plainText = runs.map(\.0).joined()
        if !styled && !hasURL(plainText) { return "" }

        var out = "<div>"
        var lineEmpty = true
        for (text, st) in runs {
            let lines = text.components(separatedBy: "\n")
            for (n, line) in lines.enumerated() {
                if n > 0 {
                    out += lineEmpty ? "<br></div><div>" : "</div><div>"
                    lineEmpty = true
                }
                if line.isEmpty { continue }
                lineEmpty = false
                out += wrap(line, st)
            }
        }
        out += lineEmpty ? "<br></div>" : "</div>"
        return out
    }

    private static func wrap(_ text: String, _ st: Style) -> String {
        var inner = st.link == nil ? autolink(text) : escapeHTMLText(text)
        if st.underline { inner = "<u>\(inner)</u>" }
        if st.italic { inner = "<i>\(inner)</i>" }
        if st.bold { inner = "<b>\(inner)</b>" }
        if let url = st.link, isSafe(url) {
            inner = "<a href=\"\(escapeHTMLText(url.absoluteString))\">\(inner)</a>"
        }
        return inner
    }

    /// Ссылки — только http(s) и mailto: `javascript:` и подобное в письмо не уходит.
    static func isSafe(_ url: URL) -> Bool {
        ["http", "https", "mailto"].contains(url.scheme?.lowercased() ?? "")
    }

    nonisolated(unsafe) private static let detector = try? NSDataDetector(types: NSTextCheckingResult.CheckingType.link.rawValue)

    private static func hasURL(_ s: String) -> Bool {
        guard let d = detector else { return false }
        return d.firstMatch(in: s, range: NSRange(s.startIndex..., in: s)).map { m in m.url.map(isSafe) ?? false } ?? false
    }

    /// Адреса в тексте — ссылками (автоссылки десктопа).
    private static func autolink(_ s: String) -> String {
        guard let d = detector else { return escapeHTMLText(s) }
        var out = ""
        var cursor = s.startIndex
        for m in d.matches(in: s, range: NSRange(s.startIndex..., in: s)) {
            guard let url = m.url, isSafe(url), let r = Range(m.range, in: s) else { continue }
            out += escapeHTMLText(String(s[cursor..<r.lowerBound]))
            out += "<a href=\"\(escapeHTMLText(url.absoluteString))\">\(escapeHTMLText(String(s[r])))</a>"
            cursor = r.upperBound
        }
        return out + escapeHTMLText(String(s[cursor...]))
    }

    static func escapeHTMLText(_ s: String) -> String {
        s.replacingOccurrences(of: "&", with: "&amp;").replacingOccurrences(of: "<", with: "&lt;")
            .replacingOccurrences(of: ">", with: "&gt;").replacingOccurrences(of: "\"", with: "&quot;")
    }

    // MARK: Команды формата (⌘B/⌘I/⌘U/⌘K)

    enum Toggle { case bold, italic, underline }

    /// Включить или снять стиль на выделении (на каретке — для следующего
    /// ввода, как у десктопа): включён на выделении — снимается.
    static func toggle(_ t: Toggle, text: inout AttributedString, selection: inout AttributedTextSelection, _ ctx: Font.Context) {
        let on = style(selection.typingAttributes(in: text), ctx)
        text.transformAttributes(in: &selection) { c in
            switch t {
            case .bold, .italic:
                let f = c[AttributeScopes.SwiftUIAttributes.FontAttribute.self] ?? .body
                let r = f.resolve(in: ctx)
                let bold = t == .bold ? !on.bold : r.isBold
                let italic = t == .italic ? !on.italic : r.isItalic
                c[AttributeScopes.SwiftUIAttributes.FontAttribute.self] = Font.body.bold(bold).italic(italic)
                c[AttributeScopes.FoundationAttributes.InlinePresentationIntentAttribute.self] = nil
            case .underline:
                c[AttributeScopes.SwiftUIAttributes.UnderlineStyleAttribute.self] = on.underline ? nil : .single
            }
        }
    }

    /// Ссылка из буфера обмена на выделение. В буфере не адрес — `false`.
    @discardableResult
    static func link(text: inout AttributedString, selection: inout AttributedTextSelection, url: URL?) -> Bool {
        guard let url, isSafe(url) else { return false }
        if case .insertionPoint = selection.indices(in: text) {
            // Ничего не выделено — вставляем сам адрес ссылкой.
            var a = AttributedString(url.absoluteString)
            a.link = url
            text.replaceSelection(&selection, with: a)
            return true
        }
        text.transformAttributes(in: &selection) { $0.link = url }
        return true
    }
}

/// Буфер обмена: адрес для ⌘K и картинка для вставки в тело.
enum Pasteboard {
    @MainActor static var url: URL? {
        #if os(iOS)
        if let u = UIPasteboard.general.url { return u }
        return UIPasteboard.general.string.flatMap { URL(string: $0.trimmingCharacters(in: .whitespacesAndNewlines)) }
        #else
        let s = NSPasteboard.general.string(forType: .string)?.trimmingCharacters(in: .whitespacesAndNewlines)
        return s.flatMap(URL.init(string:))
        #endif
    }

    @MainActor static var hasImage: Bool {
        #if os(iOS)
        UIPasteboard.general.hasImages
        #else
        NSPasteboard.general.canReadObject(forClasses: [NSImage.self], options: nil)
        #endif
    }

    /// Картинка из буфера — PNG.
    @MainActor static var imagePNG: Data? {
        #if os(iOS)
        UIPasteboard.general.image?.pngData()
        #else
        guard let img = NSPasteboard.general.readObjects(forClasses: [NSImage.self], options: nil)?.first as? NSImage,
              let tiff = img.tiffRepresentation, let rep = NSBitmapImageRep(data: tiff)
        else { return nil }
        return rep.representation(using: .png, properties: [:])
        #endif
    }
}

/// Команды формата для меню приложения (Мак: «Формат» с ⌘B/⌘I/⌘U/⌘K;
/// iPad с клавиатурой — ⌘K, остальное умеет системное поле само).
struct ComposerFormat {
    var bold: () -> Void
    var italic: () -> Void
    var underline: () -> Void
    var link: () -> Void
}

extension FocusedValues {
    @Entry var composerFormat: ComposerFormat?
}
