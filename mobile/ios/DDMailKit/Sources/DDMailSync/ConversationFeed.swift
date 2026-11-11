import DDMailAPI
import Foundation
import Synchronization

/// Решение перехватчика: пускать ли запрос `/remote` (внешняя картинка через
/// прокси приложения) — порт `RemoteImageGate` Android (§5 «Спам», §8).
///
/// Обычная беседа: что документ попросил через `/remote`, то и грузится
/// (документ сам не просит до разрешения картинок).
///
/// Беседа спама: только картинки письма, которому в этом показе нажали
/// «Показать картинки», и только запросы за картинкой (`Accept` с `image/`):
/// шрифт или стиль через тот же `/remote` не пройдёт, даже если письмо как-то
/// протащило такой URL мимо чистки. Разрешение живёт в памяти до открытия
/// другой беседы; «Всегда от отправителя» на спам не действует.
///
/// Вызывается с потока обработчика схемы WebKit и с главного — потокобезопасно.
public final class RemoteImageGate: Sendable {
    private struct State {
        var spam = false
        var known: Set<Int64> = []
        var allowed: Set<Int64> = []
    }

    private let state = Mutex(State())

    public init() {}

    /// Открыта беседа: прежние разрешения забываются.
    public func open(spam: Bool, uids: some Sequence<Int64>) {
        state.withLock { $0 = State(spam: spam, known: Set(uids), allowed: []) }
    }

    /// В открытую беседу пришли новые письма.
    public func extend(_ uids: some Sequence<Int64>) {
        state.withLock { $0.known.formUnion(uids) }
    }

    /// «Показать картинки» у письма `uid` — на этот показ беседы.
    public func allow(_ uid: Int64) {
        state.withLock { if $0.known.contains(uid) { $0.allowed.insert(uid) } }
    }

    public func permits(uid: Int64?, headers: [String: String]) -> Bool {
        state.withLock { s in
            guard s.spam else { return true }
            guard let uid, s.allowed.contains(uid) else { return false }
            return Self.looksLikeImage(headers)
        }
    }

    /// Запрос за картинкой: `Sec-Fetch-Dest` (если он есть) решает сам; иначе —
    /// `Accept` с `image/`, который WebKit ставит картинкам, а шрифтам и стилям —
    /// нет. Нет ни того, ни другого — не картинка.
    public static func looksLikeImage(_ headers: [String: String]) -> Bool {
        func header(_ name: String) -> String? {
            headers.first { $0.key.caseInsensitiveCompare(name) == .orderedSame }?.value.trimmingCharacters(in: .whitespaces)
        }
        if let dest = header("Sec-Fetch-Dest"), !dest.isEmpty {
            return dest.caseInsensitiveCompare("image") == .orderedSame
        }
        guard let accept = header("Accept") else { return false }
        return accept.split(separator: ",").contains {
            $0.trimmingCharacters(in: .whitespaces).lowercased().hasPrefix("image/")
        }
    }
}

/// Порция писем для документа ленты (`ddm.set/prepend/append`) — форма `Msg`
/// из `mobile/shared/conversation-doc/conversation.js`, как у Android.
public struct FeedBatch: Encodable, Sendable {
    public struct Attachment: Encodable, Sendable {
        let index: Int
        let filename: String
        let size: Int64
    }

    public struct Message: Encodable, Sendable {
        let uid: Int64
        let outgoing: Bool
        let fromName: String
        let fromAddr: String
        /// Картинки этого отправителя разрешены всегда (у спама — никогда).
        let imagesAllowed: Bool
        let spam: Bool
        let dateTs: Int64
        let html: String?
        let text: String?
        let attachments: [Attachment]
        /// В группе подписываем входящие именем отправителя.
        let showFrom: Bool
    }

    public let messages: [Message]
    public let hasOlder: Bool
    public let focusUid: Int64?

    public init(conversation c: Conversation, bodies: [MessageBody], hasOlder: Bool, focusUid: Int64?, imageSenders: Set<String>) {
        messages = bodies.map { b in
            Message(
                uid: b.uid, outgoing: b.isOutgoing, fromName: b.from, fromAddr: b.fromAddr,
                // Спам: сохранённые разрешения по отправителю не действуют.
                imagesAllowed: !c.spam && imageSenders.contains(b.fromAddr.lowercased()),
                spam: c.spam, dateTs: b.dateTs,
                html: b.html.flatMap { $0.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty ? nil : $0 },
                text: b.text,
                attachments: b.attachments.map { Attachment(index: $0.index, filename: $0.filename, size: $0.size) },
                showFrom: c.isGroup
            )
        }
        self.hasOlder = hasOlder
        self.focusUid = focusUid
    }

    /// JSON для вызова `ddm.*(…)`. JSON — подмножество литералов JS, кроме
    /// U+2028/U+2029 в строках: их экранируем, иначе старый разбор JS сломается.
    public func javaScriptLiteral() -> String {
        let e = JSONEncoder()
        e.outputFormatting = [.withoutEscapingSlashes]
        let s = String(decoding: (try? e.encode(self)) ?? Data("{}".utf8), as: UTF8.self)
        return s.replacingOccurrences(of: "\u{2028}", with: "\\u2028").replacingOccurrences(of: "\u{2029}", with: "\\u2029")
    }
}

/// Окно отрисовки беседы (§8): сначала последние `page` писем, старые —
/// порциями по просьбе документа, новые — снизу. «Последние» — по uid:
/// у ссылок нет дат, а качать тела всей беседы ради сортировки — сотни писем.
public struct FeedWindow: Sendable {
    public static let page = 20

    public private(set) var order: [MessageRef] = []
    public private(set) var loaded = 0

    public init(_ refs: [MessageRef]) {
        order = refs.sorted { $0.uid > $1.uid }
        loaded = min(Self.page, order.count)
    }

    public var first: [MessageRef] { Array(order.prefix(loaded)) }
    public var hasOlder: Bool { loaded < order.count }

    /// Первое непрочитанное в первой порции — к нему прокрутить (как у десктопа).
    public var focus: Int64? { first.filter { !$0.seen }.map(\.uid).min() }

    /// Следующая порция старых.
    public mutating func older() -> [MessageRef] {
        let next = Array(order.dropFirst(loaded).prefix(Self.page))
        loaded += next.count
        return next
    }

    /// Беседа обновилась: письма, которых ещё не было, — для дописывания снизу.
    public mutating func fresh(_ refs: [MessageRef]) -> [MessageRef] {
        let known = Set(order.map(\.uid))
        let new = refs.filter { !known.contains($0.uid) }
        guard !new.isEmpty else { return [] }
        order = (new + order).sorted { $0.uid > $1.uid }
        loaded += new.count
        return new
    }
}

/// Имя файла без разделителей пути и управляющих символов.
public func safeFileName(_ name: String) -> String {
    let bad = CharacterSet(charactersIn: "\\/:*?\"<>|").union(.controlCharacters)
    var s = String(String.UnicodeScalarView(name.unicodeScalars.map { bad.contains($0) ? "_" : $0 }))
    s = s.trimmingCharacters(in: .whitespaces)
    while s.hasSuffix(".") { s.removeLast() }
    return s.isEmpty ? "attachment" : s
}
