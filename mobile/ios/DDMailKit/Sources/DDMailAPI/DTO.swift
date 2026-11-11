import Foundation

// Формы ответов /api/desktop/v1 — зеркало Desktop*-структур
// internal/web/handlers_desktop.go (и Dto.kt Android). Лишние поля
// игнорируются; отсутствующее поле и null дают значение по умолчанию
// (Go отдаёт nil-срез как null).

/// Свой адрес отправки.
public struct Identity: Decodable, Equatable, Sendable {
    public var email = ""
    public var name = ""
    public var signature = ""
    public var isDefault = false

    enum CodingKeys: String, CodingKey {
        case email, name, signature
        case isDefault = "is_default"
    }

    public init(email: String = "", name: String = "", signature: String = "", isDefault: Bool = false) {
        self.email = email
        self.name = name
        self.signature = signature
        self.isDefault = isDefault
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        email = try c.decodeIfPresent(String.self, forKey: .email) ?? ""
        name = try c.decodeIfPresent(String.self, forKey: .name) ?? ""
        signature = try c.decodeIfPresent(String.self, forKey: .signature) ?? ""
        isDefault = try c.decodeIfPresent(Bool.self, forKey: .isDefault) ?? false
    }
}
