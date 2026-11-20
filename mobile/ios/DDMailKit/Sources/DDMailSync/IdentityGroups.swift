import DDMailAPI
import DDMailStore
import Foundation

/// Документ групп айдентик — порт `IdentityDoc` десктопа (`identities.rs`,
/// десктоп §1в; Android `IdentityGroups.kt`). Толкование документа на Apple —
/// только здесь.
///
/// Документ общий с десктопом (`/identity-groups` сервера ddmail; без ddmail —
/// локальный, как `identities.json` десктопа), поэтому правка трогает только
/// своё поле, а всё прочее (подписи, неизвестные поля) остаётся как было:
/// источник — `json`, модель — его прочтение.
public struct IdentityGroups: Equatable, Sendable {
    /// Адрес группы; `email` — в нижнем регистре.
    public struct Address: Equatable, Sendable {
        public let email: String
        public let hidden: Bool
    }

    public struct Group: Equatable, Sendable {
        public let id: String
        public let name: String
        public let color: String
        /// Поле `sender` как есть; адрес-отправитель — `sender`.
        public let senderField: String
        public let addresses: [Address]
        public let strict: Bool

        /// Адрес, с которого пишет группа: `sender`, если он в группе, иначе первый адрес.
        public var sender: String {
            addresses.contains { $0.email == senderField } ? senderField : (addresses.first?.email ?? "")
        }

        /// «Имя · адрес» или только адрес — как `group_title` десктопа.
        public var title: String {
            let n = name.trimmingCharacters(in: .whitespaces)
            return n.isEmpty ? sender : "\(n) · \(sender)"
        }
    }

    /// Адрес, найденный в учётке: начальные имя и цвет новой группы.
    public struct Source: Equatable, Sendable {
        public var email: String
        public var name: String
        public var color: String

        public init(email: String, name: String = "", color: String = "") {
            self.email = email
            self.name = name
            self.color = color
        }
    }

    public let json: [String: JSONValue]
    public let groups: [Group]
    /// Поле `default_group`: id группы нового письма.
    public let defaultGroupID: String

    public static let empty = IdentityGroups(json: [:])

    public init(json: [String: JSONValue]) {
        self.json = json
        defaultGroupID = json["default_group"]?.string ?? ""
        groups = (json["groups"]?.array ?? []).compactMap { g in
            guard let o = g.object else { return nil }
            func str(_ k: String) -> String { o[k]?.string ?? "" }
            let addrs: [Address] = (o["addresses"]?.array ?? []).compactMap { a in
                let email = (a["email"]?.string ?? "").trimmingCharacters(in: .whitespaces).lowercased()
                return email.isEmpty ? nil : Address(email: email, hidden: a["hidden"]?.bool ?? false)
            }
            return Group(
                id: str("id"), name: str("name"), color: str("color"),
                senderField: str("sender").trimmingCharacters(in: .whitespaces).lowercased(),
                addresses: addrs, strict: o["strict"]?.bool ?? false
            )
        }
    }

    public static func parse(_ doc: JSONValue?) -> IdentityGroups {
        doc?.object.map(IdentityGroups.init(json:)) ?? .empty
    }

    public static func parse(_ s: String?) -> IdentityGroups {
        parse(s.flatMap { JSONValue.parse(Data($0.utf8)) })
    }

    public var isEmpty: Bool { groups.isEmpty }
    public var value: JSONValue { .object(json) }

    private func locateExact(_ email: String) -> (Int, Int)? {
        for (gi, g) in groups.enumerated() {
            if let ai = g.addresses.firstIndex(where: { $0.email == email }) { return (gi, ai) }
        }
        return nil
    }

    /// (группа, адрес); не добавленный явно `user+tag@` сводится к `user@`.
    public func locate(_ email: String) -> (Int, Int)? {
        let lc = email.trimmingCharacters(in: .whitespaces).lowercased()
        return locateExact(lc) ?? IdentityColors.baseOfTagged(lc).flatMap(locateExact)
    }

    public func group(of email: String) -> Group? { locate(email).map { groups[$0.0] } }
    public func group(id: String) -> Group? { groups.first { $0.id == id } }
    public func isMine(_ email: String) -> Bool { locate(email) != nil }

    /// Группа по умолчанию (★): `default_group`, нет такой — первая.
    public var defaultGroup: Group? { group(id: defaultGroupID) ?? groups.first }

    /// С какого адреса отвечать на письмо, пришедшее на `receivedBy`:
    /// отправитель его группы. Адрес не мой — отправитель группы по
    /// умолчанию, а если она строгая — первой нестрогой. Нет и такой — `nil`:
    /// адрес выбирают руками (`IdentityDoc::reply_sender`).
    public func replySender(_ receivedBy: String) -> String? {
        let g = group(of: receivedBy) ?? defaultGroup.flatMap { $0.strict ? nil : $0 } ?? groups.first { !$0.strict }
        return g.map(\.sender).flatMap { $0.isEmpty ? nil : $0 }
    }

    /// Отправитель нового письма — группы по умолчанию (`new_mail_sender`).
    public func newMailSender() -> String? {
        defaultGroup.map(\.sender).flatMap { $0.isEmpty ? nil : $0 }
    }

    /// Отправитель по умолчанию: `nil`-контекст — новое письмо, иначе адрес, на который пришло письмо.
    public func autoSender(_ context: String?) -> String? {
        context.map(replySender) ?? newMailSender()
    }

    /// Можно ли писать с `from` в контексте письма, пришедшего на `context`
    /// (`nil` — новое письмо). Нестрогая группа и чужой адрес — всегда;
    /// строгая — только новое письмо или письмо, пришедшее на её же адрес.
    public func allowed(_ from: String, context: String?) -> Bool {
        guard let (gi, _) = locate(from), groups[gi].strict else { return true }
        guard let context else { return true }
        return locate(context)?.0 == gi
    }

    /// Предлагать ли адрес в «От кого»: не скрыт и разрешён в контексте.
    public func offered(_ from: String, context: String?) -> Bool {
        let hidden = locate(from).map { groups[$0.0].addresses[$0.1].hidden } ?? false
        return !hidden && allowed(from, context: context)
    }

    /// Добавить адреса, которых ещё нет: каждый — новой группой из одного
    /// (`merge_sources`). Ничего не удаляет и известных адресов не трогает.
    public func merged(_ sources: [Source]) -> IdentityGroups {
        var cur = self
        var changed = false
        for s in sources {
            let email = s.email.trimmingCharacters(in: .whitespaces).lowercased()
            guard Self.looksLikeEmail(email), cur.locateExact(email) == nil else { continue }
            let c = s.color.trimmingCharacters(in: .whitespaces)
            let group: JSONValue = .object([
                "id": .string(cur.newID()),
                "name": .string(s.name.trimmingCharacters(in: .whitespaces)),
                "color": .string(c.isEmpty ? cur.nextColor() : c),
                "sender": .string(email),
                "addresses": .array([.object(["email": .string(email), "hidden": .bool(false)])]),
                "signature": .array([]),
                "strict": .bool(false),
            ])
            var j = cur.json
            j["groups"] = .array((cur.json["groups"]?.array ?? []) + [group])
            cur = IdentityGroups(json: j)
            changed = true
        }
        if changed, cur.group(id: cur.defaultGroupID) == nil, let first = cur.groups.first {
            cur = cur.withDefault(first.id)
        }
        return cur
    }

    /// Сделать группой по умолчанию (★) группу `id`; поля документа — как были.
    public func withDefault(_ id: String) -> IdentityGroups {
        var j = json
        j["default_group"] = .string(id)
        return IdentityGroups(json: j)
    }

    /// Цвета строк по этому документу.
    public var colors: IdentityColors { IdentityColors.parse(value) }

    /// `IDENT_PASTEL` десктопа строками — так их пишет `merge_sources`.
    static let pastelHex: [String] = IdentityColors.pastel.map { String(format: "#%06X", $0 & 0xFFFFFF) }

    private func nextColor() -> String {
        let used = Set(groups.map { $0.color.uppercased() })
        return Self.pastelHex.first { !used.contains($0.uppercased()) } ?? Self.pastelHex[groups.count % Self.pastelHex.count]
    }

    private func newID() -> String {
        var n = groups.count + 1
        while group(id: "g\(n)") != nil { n += 1 }
        return "g\(n)"
    }

    /// Похоже на email: одна `@`, непустые части, точка в домене, без пробелов (`looks_like_email`).
    public static func looksLikeEmail(_ s: String) -> Bool {
        let t = s.trimmingCharacters(in: .whitespaces)
        guard let at = t.firstIndex(of: "@"), at > t.startIndex else { return false }
        let domain = t[t.index(after: at)...]
        return !domain.isEmpty && !domain.contains("@") && domain.contains(".")
            && !domain.hasPrefix(".") && !domain.hasSuffix(".") && !t.contains(where: \.isWhitespace)
    }

    public static func == (a: IdentityGroups, b: IdentityGroups) -> Bool { a.json == b.json }
}

/// Документ групп три раза подряд менялся под нами — правка не записана.
public struct IdentityGroupsBusy: Error, LocalizedError {
    public init() {}
    public var errorDescription: String? { "Айдентики сейчас меняет другая станция — попробуйте ещё раз" }
}

/// Адреса выбранной группы больше нет в документе (его поменяли на другой станции).
public struct IdentityGroupGone: Error, LocalizedError {
    public init() {}
    public var errorDescription: String? { "Группы больше нет — айдентики изменены на другой станции" }
}

/// Документ групп: ddmail-учётки (сервер) или локальный (десктоп §1в «Где хранится»).
public protocol IdentityGroupSource: AnyObject, Sendable {
    func identityGroups() -> AsyncStream<IdentityGroups>
    func currentIdentityGroups() -> IdentityGroups
    /// Сохранить уже слитый с адресами учёток документ (только локальный:
    /// серверному адреса уходят с первой правкой).
    func storeMerged(_ doc: IdentityGroups) throws
    /// «Отправитель по умолчанию»: группа адреса `sender` — `default_group`.
    func setDefaultSender(_ sender: String, sources: [IdentityGroups.Source]) async throws
}

/// Без ddmail-учётки группы живут на устройстве — документ того же формата,
/// что `identities.json` десктопа, в кэше.
public final class LocalIdentityGroups: IdentityGroupSource {
    static let key = "local/identity_groups"
    let store: MailStore

    public init(store: MailStore) { self.store = store }

    public func identityGroups() -> AsyncStream<IdentityGroups> {
        let s = store.observeMeta(Self.key)
        return AsyncStream { cont in
            let t = Task {
                for await v in s { cont.yield(IdentityGroups.parse(v)) }
                cont.finish()
            }
            cont.onTermination = { _ in t.cancel() }
        }
    }

    public func currentIdentityGroups() -> IdentityGroups {
        IdentityGroups.parse((try? store.meta(Self.key)) ?? nil)
    }

    public func storeMerged(_ doc: IdentityGroups) throws {
        try store.setMeta(Self.key, String(decoding: doc.value.data(), as: UTF8.self))
    }

    public func setDefaultSender(_ sender: String, sources: [IdentityGroups.Source]) async throws {
        let doc = currentIdentityGroups().merged(sources)
        guard let g = doc.group(of: sender) else { throw IdentityGroupGone() }
        try storeMerged(doc.withDefault(g.id))
    }
}
