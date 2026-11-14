import DDMailAPI
import Foundation

/// Буквенный раздел списка контактов.
public struct ContactSection: Identifiable, Equatable, Sendable {
    public let letter: String
    public let items: [ContactDTO]
    public var id: String { letter }
}

/// Книга, приготовленная к показу и поиску: сортировка по имени, свёрнутые
/// строки поиска (без регистра и диакритики — как `localizedStandardContains`),
/// цифры телефонов. Строится вне главного потока.
public struct ContactIndex: Sendable {
    public private(set) var sorted: [ContactDTO] = []
    public private(set) var byID: [Int64: ContactDTO] = [:]
    private var keys: [String] = []
    private var digits: [String] = []

    public init() {}

    public init(_ list: [ContactDTO]) {
        sorted = Self.sort(list)
        byID = Dictionary(sorted.map { ($0.id, $0) }, uniquingKeysWith: { a, _ in a })
        keys = sorted.map(Self.key)
        digits = sorted.map(Self.phoneDigits)
    }

    public struct Query: Sendable {
        public let text: String
        public let digits: String
        public init(_ q: String) {
            text = ContactIndex.fold(q)
            digits = q.filter(\.isNumber)
        }
    }

    public func search(_ q: Query) -> [ContactDTO] {
        var out: [ContactDTO] = []
        for i in sorted.indices where keys[i].contains(q.text) || (!q.digits.isEmpty && digits[i].contains(q.digits)) {
            out.append(sorted[i])
        }
        return out
    }

    public static func matches(_ c: ContactDTO, _ q: Query) -> Bool {
        key(c).contains(q.text) || (!q.digits.isEmpty && phoneDigits(c).contains(q.digits))
    }

    public static func fold(_ s: String) -> String {
        s.folding(options: [.caseInsensitive, .diacriticInsensitive, .widthInsensitive], locale: nil)
    }

    private static func key(_ c: ContactDTO) -> String {
        fold(([c.fullName, c.organization] + c.emails).joined(separator: "\n"))
    }

    private static func phoneDigits(_ c: ContactDTO) -> String {
        c.phones.map { $0.filter(\.isNumber) }.joined(separator: "\n")
    }

    /// Имя для списка: без имени — первый адрес.
    public static func title(_ c: ContactDTO) -> String {
        let n = c.fullName.trimmingCharacters(in: .whitespaces)
        return n.isEmpty ? (c.emails.first ?? "") : n
    }

    public static func sort(_ list: [ContactDTO]) -> [ContactDTO] {
        list.map { (title($0), $0) }
            .sorted { a, b in
                let r = a.0.localizedStandardCompare(b.0)
                return r == .orderedSame ? a.1.id < b.1.id : r == .orderedAscending
            }
            .map(\.1)
    }

    /// Разделы по первой букве; не буква — «#» в конце.
    public static func sections(_ list: [ContactDTO]) -> [ContactSection] {
        var order: [String] = []
        var groups: [String: [ContactDTO]] = [:]
        for c in list {
            let first = c.fullName.trimmingCharacters(in: .whitespaces).first.map { String($0).uppercased() } ?? "#"
            let letter = first.first?.isLetter == true ? first : "#"
            if groups[letter] == nil { order.append(letter) }
            groups[letter, default: []].append(c)
        }
        let keys = order.filter { $0 != "#" }.sorted { $0.localizedStandardCompare($1) == .orderedAscending }
            + (groups["#"] == nil ? [] : ["#"])
        return keys.map { ContactSection(letter: $0, items: groups[$0] ?? []) }
    }
}
