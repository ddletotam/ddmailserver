import DDMailAPI
import Foundation

/// Папки диалогов — порт `DialogFolders.kt` Android и `folders.rs` десктопа
/// (десктопный контракт §4 «Папки диалогов»). Обязан совпадать с ними по
/// смыслу: документ общий, а толкует его только клиент.
///
/// - диалог лежит строго в одной папке или ни в одной (инбокс);
/// - ручное `include` побеждает правила; среди правил — первая подходящая
///   папка в порядке табов;
/// - между категориями правил «И», внутри списка «ИЛИ»; папка без правил
///   набирается только руками.
///
/// Неизвестные поля документа и папок переживают чтение и запись: каждый
/// объект держит исходный JSON и при записи меняет в нём только своё.
///
/// Системная папка «Спам» (`system: "spam"`): состав задаёт сервер (беседы с
/// `spam`), а не правила и не руки; всегда скрыта.
public enum DialogKind: Sendable { case any, direct, group }

public let systemSpam = "spam"

public struct FolderRules: Equatable, Sendable {
    public var identities: [String] = []
    public var domains: [String] = []
    public var kind: DialogKind = .any

    public init(identities: [String] = [], domains: [String] = [], kind: DialogKind = .any) {
        self.identities = identities
        self.domains = domains
        self.kind = kind
    }

    public var isEmpty: Bool { identities.isEmpty && domains.isEmpty && kind == .any }

    public func matches(_ c: Conversation) -> Bool {
        if isEmpty { return false }
        if !identities.isEmpty, !identities.contains(where: { $0.caseInsensitiveCompare(c.receivedBy) == .orderedSame }) {
            return false
        }
        if !domains.isEmpty, !c.counterparts.contains(where: { domainMatches($0.addr) }) { return false }
        switch kind {
        case .any: return true
        case .direct: return !c.isGroup
        case .group: return c.isGroup
        }
    }

    /// `acme.com` ловит и `mail.acme.com`.
    private func domainMatches(_ addr: String) -> Bool {
        let lower = addr.lowercased()
        guard let at = lower.lastIndex(of: "@") else { return false }
        let host = String(lower[lower.index(after: at)...])
        guard !host.isEmpty else { return false }
        return domains.contains { raw in
            var d = raw.trimmingCharacters(in: .whitespaces).lowercased()
            while d.hasPrefix("@") { d.removeFirst() }
            return !d.isEmpty && (host == d || host.hasSuffix("." + d))
        }
    }
}

public struct DialogFolder: Equatable, Sendable {
    public var id: String
    public var name: String
    public var icon: String
    public var rules: FolderRules
    public var include: [DialogKey]
    public var exclude: [DialogKey]
    /// «Не показывать в списке»: таба нет, диалоги не видны нигде.
    public var hidden: Bool
    /// «Письма сразу прочитаны» — исполняет только десктоп (Android §5).
    public var autoRead: Bool
    /// Системная папка (`systemSpam`); пусто — обычная.
    public var system: String
    var raw: [String: JSONValue]

    public init(
        id: String, name: String, icon: String = "", rules: FolderRules = FolderRules(),
        include: [DialogKey] = [], exclude: [DialogKey] = [], hidden: Bool = false,
        autoRead: Bool = false, system: String = ""
    ) {
        self.id = id; self.name = name; self.icon = icon; self.rules = rules
        self.include = include; self.exclude = exclude; self.hidden = hidden
        self.autoRead = autoRead; self.system = system; raw = [:]
    }

    public var isSystem: Bool { !system.isEmpty }

    /// Сравнение — по смыслу, без исходного JSON.
    public static func == (a: DialogFolder, b: DialogFolder) -> Bool {
        a.id == b.id && a.name == b.name && a.icon == b.icon && a.rules == b.rules
            && a.include == b.include && a.exclude == b.exclude && a.hidden == b.hidden
            && a.autoRead == b.autoRead && a.system == b.system
    }
}

public struct FoldersDoc: Equatable, Sendable {
    public var folders: [DialogFolder]
    var raw: [String: JSONValue]

    public init(folders: [DialogFolder] = []) {
        self.folders = folders
        raw = [:]
    }

    public static func == (a: FoldersDoc, b: FoldersDoc) -> Bool { a.folders == b.folders }

    public var spamIndex: Int? { folders.firstIndex { $0.system == systemSpam } }

    /// Индекс папки диалога или `nil` — инбокс. Спам — только в «Спам», и
    /// ничто другое туда не попадает; спам без папки «Спам» не виден нигде.
    public func folderOf(_ c: Conversation) -> Int? {
        if c.spam { return spamIndex }
        let k = c.key
        if let i = folders.firstIndex(where: { !$0.isSystem && $0.include.contains(k) }) { return i }
        return folders.firstIndex { !$0.isSystem && !$0.exclude.contains(k) && $0.rules.matches(c) }
    }

    /// Папка «Спам» ровно одна, скрыта, без правил и ручных назначений — как
    /// `FoldersDoc::ensure_spam` десктопа. Нет — добавляется в конец (в
    /// документ на сервере она попадёт с первой правкой).
    public func withSpam() -> FoldersDoc {
        var seen = false
        var kept = folders.filter { f in
            guard f.system == systemSpam else { return true }
            defer { seen = true }
            return !seen
        }
        if let i = kept.firstIndex(where: { $0.system == systemSpam }) {
            var f = kept[i]
            if f.name.trimmingCharacters(in: .whitespaces).isEmpty { f.name = "Спам" }
            f.rules = FolderRules()
            f.include = []
            f.exclude = []
            f.hidden = true
            f.autoRead = false
            f.raw["name"] = .string(f.name)
            f.raw["rules"] = .object([:])
            f.raw["hidden"] = .bool(true)
            f.raw["auto_read"] = .bool(false)
            kept[i] = f
        } else {
            let id = kept.contains { $0.id == systemSpam }
                ? "f\(String(UInt64(Date().timeIntervalSince1970 * 1000), radix: 16))0"
                : systemSpam
            var f = DialogFolder(id: id, name: "Спам", icon: "🚫", hidden: true, system: systemSpam)
            f.raw = [
                "id": .string(id), "name": .string("Спам"), "icon": .string("🚫"),
                "rules": .object([:]), "hidden": .bool(true), "auto_read": .bool(false),
                "system": .string(systemSpam),
            ]
            kept.append(f)
        }
        var out = self
        out.folders = kept
        return out
    }

    /// Положить диалоги в папку `target` руками (`nil` — в инбокс). Ручной
    /// выбор побеждает всё: ключ снимается из чужих `include`, а каждая папка,
    /// чьи правила его ловят, получает его в `exclude`. Одна правка документа
    /// на весь набор — для множественного выбора (Android §5).
    public func assign(_ convs: some Collection<Conversation>, to target: Int?) -> FoldersDoc {
        // Руками в «Спам» не кладут и из него не выносят («Не спам» — сервер).
        if let target, !folders.indices.contains(target) || folders[target].isSystem { return self }
        var out = folders
        for c in convs where !c.spam {
            let k = c.key
            for i in out.indices {
                out[i].include.removeAll { $0 == k }
                out[i].exclude.removeAll { $0 == k }
                if i == target { out[i].include.append(k) }
                else if out[i].rules.matches(c) { out[i].exclude.append(k) }
            }
        }
        var doc = self
        doc.folders = out
        return doc
    }

    /// Видимая часть списка для папки `active` (`nil` — инбокс); порядок исходный.
    public func filter(_ convs: [Conversation], active: Int?) -> [Conversation] {
        convs.filter { c in
            let f = folderOf(c)
            return f == active && (f != nil || !c.spam)
        }
    }

    /// Диалоги с непрочитанным — по папкам и в инбоксе (диалоги, не письма).
    public func unreadCounts(_ convs: [Conversation]) -> (folders: [Int], inbox: Int) {
        var out = [Int](repeating: 0, count: folders.count)
        var inbox = 0
        for c in convs where c.unreadCount > 0 {
            if let i = folderOf(c) { out[i] += 1 } else if !c.spam { inbox += 1 }
        }
        return (out, inbox)
    }

    public func toJSON() -> JSONValue {
        var o = raw
        o["folders"] = .array(folders.map { $0.toJSON() })
        return .object(o)
    }

    public static func parse(_ v: JSONValue?) -> FoldersDoc {
        guard case let .object(obj)? = v else { return FoldersDoc() }
        var doc = FoldersDoc(folders: (obj["folders"]?.array ?? []).compactMap { $0.object.map(parseFolder) })
        doc.raw = obj
        return doc
    }

    private static func parseFolder(_ o: [String: JSONValue]) -> DialogFolder {
        let rules = o["rules"]?.object ?? [:]
        var f = DialogFolder(
            id: o["id"]?.string ?? "",
            name: o["name"]?.string ?? "",
            icon: o["icon"]?.string ?? "",
            rules: FolderRules(
                identities: strings(rules["identities"]),
                domains: strings(rules["domains"]),
                kind: kind(rules["kind"]?.string)
            ),
            include: keys(o["include"]),
            exclude: keys(o["exclude"]),
            hidden: o["hidden"]?.bool ?? false,
            autoRead: o["auto_read"]?.bool ?? false,
            system: o["system"]?.string ?? ""
        )
        f.raw = o
        return f
    }

    private static func kind(_ s: String?) -> DialogKind {
        switch s {
        case "direct": .direct
        case "group": .group
        default: .any
        }
    }

    private static func strings(_ v: JSONValue?) -> [String] {
        (v?.array ?? []).compactMap(\.string)
    }

    private static func keys(_ v: JSONValue?) -> [DialogKey] {
        (v?.array ?? []).compactMap { el in
            guard let o = el.object else { return nil }
            let k = DialogKey(account: o["account"]?.string ?? "", id: o["id"]?.string ?? "")
            return k.id.isEmpty ? nil : k
        }
    }
}

extension DialogFolder {
    /// Все известные поля пишутся из модели (правка в настройках меняет их),
    /// неизвестные — как пришли.
    func toJSON() -> JSONValue {
        func keys(_ l: [DialogKey]) -> JSONValue {
            .array(l.map { .object(["account": .string($0.account), "id": .string($0.id)]) })
        }
        var o = raw
        o["id"] = .string(id)
        o["name"] = .string(name)
        o["icon"] = .string(icon)
        var r = raw["rules"]?.object ?? [:]
        r["identities"] = .array(rules.identities.map(JSONValue.string))
        r["domains"] = .array(rules.domains.map(JSONValue.string))
        r["kind"] = .string(rules.kind.json)
        o["rules"] = .object(r)
        o["include"] = keys(include)
        o["exclude"] = keys(exclude)
        o["hidden"] = .bool(hidden)
        o["auto_read"] = .bool(autoRead)
        if system.isEmpty { o["system"] = nil } else { o["system"] = .string(system) }
        return .object(o)
    }
}

extension DialogKind {
    /// Как в документе (`Kind` десктопа, `rename_all = "lowercase"`).
    var json: String {
        switch self {
        case .any: "any"
        case .direct: "direct"
        case .group: "group"
        }
    }
}

// MARK: Правка документа (раздел «Папки» настроек, меню полосы табов)

extension FoldersDoc {
    /// Скрытых папок — для «Показать скрытые (N)» (как `hidden-folders`
    /// десктопа; «Спам» у мобильных — всегда таб, его не считаем).
    public var hiddenCount: Int { folders.filter { $0.hidden && !$0.isSystem }.count }

    /// Папки, которые настраивают руками (системные — нет).
    public var userFolders: [DialogFolder] { folders.filter { !$0.isSystem } }

    public func index(of id: String) -> Int? { folders.firstIndex { $0.id == id } }

    /// Новый id: `f<мс hex><n>`, как `new_id` десктопа.
    public func newID(now: Date = Date()) -> String {
        let base = String(UInt64(max(0, now.timeIntervalSince1970) * 1000), radix: 16)
        var n = 0
        while index(of: "f\(base)\(n)") != nil { n += 1 }
        return "f\(base)\(n)"
    }

    /// «Новая папка»: «Папка N» с 📁 в конец, как `folder_add` десктопа.
    /// Возвращает документ и id новой папки.
    public func adding(id: String? = nil, now: Date = Date()) -> (doc: FoldersDoc, id: String) {
        let id = id ?? newID(now: now)
        var f = DialogFolder(id: id, name: "Папка \(userFolders.count + 1)", icon: "📁")
        f.raw = [:]
        var doc = self
        // Перед «Спамом»: системная папка остаётся последней.
        let at = doc.spamIndex ?? doc.folders.count
        doc.folders.insert(f, at: at)
        return (doc, id)
    }

    /// Удалить папку (системную — нельзя). Диалоги, положенные в неё руками,
    /// возвращаются во «Входящие» (или в папку по правилам).
    public func removing(_ id: String) -> FoldersDoc {
        var doc = self
        doc.folders.removeAll { $0.id == id && !$0.isSystem }
        return doc
    }

    /// Переставить папку на место `to` среди всех папок: порядок табов — это
    /// и приоритет правил. Системные папки не двигаются.
    public func moving(_ id: String, to: Int) -> FoldersDoc {
        guard let from = index(of: id), !folders[from].isSystem else { return self }
        var doc = self
        let f = doc.folders.remove(at: from)
        let limit = doc.spamIndex ?? doc.folders.count
        doc.folders.insert(f, at: max(0, min(to, limit)))
        return doc
    }

    /// Правка одной папки по id (системные не правятся). Нет такой — как было.
    public func updating(_ id: String, _ edit: (inout DialogFolder) -> Void) -> FoldersDoc {
        guard let i = index(of: id), !folders[i].isSystem else { return self }
        var doc = self
        edit(&doc.folders[i])
        return doc
    }

    /// «Показывать папки» выключено (настройка устройства): список как без
    /// папок — остаются только системные (у мобильных «Спам» — таб полосы).
    public func onlySystem() -> FoldersDoc {
        var doc = self
        doc.folders = folders.filter(\.isSystem)
        return doc
    }
}

/// Список адресов или доменов из поля ввода: через запятую, точку с запятой,
/// пробел или перевод строки; в нижнем регистре (`split_list` десктопа).
public func splitFolderList(_ text: String) -> [String] {
    text.split(whereSeparator: { ",; \n".contains($0) })
        .map { $0.trimmingCharacters(in: .whitespaces).lowercased() }
        .filter { !$0.isEmpty }
}

/// Значок папки — не длиннее четырёх символов (графем), как `clip_icon`
/// десктопа: эмодзи с модификаторами не режутся.
public func clipFolderIcon(_ text: String) -> String {
    String(text.trimmingCharacters(in: .whitespaces).prefix(4))
}
