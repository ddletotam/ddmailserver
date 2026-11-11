import Foundation

/// Произвольный JSON без потерь — для документов, которые сервер хранит, а
/// толкует клиент (папки диалогов, группы адресов): неизвестные поля должны
/// пережить чтение и запись (как `JsonObject` у Android).
public enum JSONValue: Codable, Equatable, Hashable, Sendable {
    case null
    case bool(Bool)
    case int(Int64)
    case double(Double)
    case string(String)
    case array([JSONValue])
    case object([String: JSONValue])

    public init(from decoder: Decoder) throws {
        let c = try decoder.singleValueContainer()
        if c.decodeNil() { self = .null }
        else if let b = try? c.decode(Bool.self) { self = .bool(b) }
        else if let i = try? c.decode(Int64.self) { self = .int(i) }
        else if let d = try? c.decode(Double.self) { self = .double(d) }
        else if let s = try? c.decode(String.self) { self = .string(s) }
        else if let a = try? c.decode([JSONValue].self) { self = .array(a) }
        else { self = .object(try c.decode([String: JSONValue].self)) }
    }

    public func encode(to encoder: Encoder) throws {
        var c = encoder.singleValueContainer()
        switch self {
        case .null: try c.encodeNil()
        case let .bool(b): try c.encode(b)
        case let .int(i): try c.encode(i)
        case let .double(d): try c.encode(d)
        case let .string(s): try c.encode(s)
        case let .array(a): try c.encode(a)
        case let .object(o): try c.encode(o)
        }
    }

    public subscript(key: String) -> JSONValue? {
        if case let .object(o) = self { o[key] } else { nil }
    }

    public var string: String? { if case let .string(s) = self { s } else { nil } }
    public var bool: Bool? { if case let .bool(b) = self { b } else { nil } }
    public var array: [JSONValue]? { if case let .array(a) = self { a } else { nil } }
    public var object: [String: JSONValue]? { if case let .object(o) = self { o } else { nil } }

    public static func parse(_ data: Data) -> JSONValue? {
        try? JSONDecoder().decode(JSONValue.self, from: data)
    }

    public func data() -> Data {
        let e = JSONEncoder()
        e.outputFormatting = [.sortedKeys]
        return (try? e.encode(self)) ?? Data("null".utf8)
    }
}

extension KeyedDecodingContainer {
    /// Поле или значение по умолчанию: нет поля и `null` — одно и то же
    /// (Go отдаёт nil-срез как `null`).
    func value<T: Decodable>(_ key: Key, _ fallback: T) throws -> T {
        try decodeIfPresent(T.self, forKey: key) ?? fallback
    }
}
