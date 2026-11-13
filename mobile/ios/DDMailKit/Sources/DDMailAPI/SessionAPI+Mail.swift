import Foundation

/// Почта: те же эндпоинты, что у Android (`SessionApi.kt`) и десктопа.
extension SessionAPI {
    /// Дельта бесед с водяного знака `sinceMs` (0 — полная выдача).
    public func conversationsDelta(sinceMs: Int64, limit: Int = 5000) async throws -> ConversationsDelta {
        try await get("/conversations?limit=\(limit)&since=\(max(sinceMs, 0))", as: ConversationsDelta.self)
    }

    /// Беседы из спама — всегда полный список. Без `spam=1` сервер старой
    /// версии отдал бы обычный список: берём только помеченные `spam`.
    public func spamConversations(limit: Int = 2000) async throws -> [ConversationDTO] {
        try await get("/conversations?spam=1&limit=\(limit)", as: [ConversationDTO].self).filter(\.spam)
    }

    /// Серверный поиск писем (секция «Сообщения» выпадашки поиска). Удалённые
    /// и события календаря индекс тоже отдаёт — они отсеиваются здесь.
    public func searchMessages(_ query: String, limit: Int = 12) async throws -> [MessageSearchHit] {
        try await get("/search?q=\(queryEscaped(query))&limit=\(limit)", as: [MessageSearchHit].self)
            .filter { $0.type == "email" && !$0.softDeleted }
    }

    public func changes(since: Int64) async throws -> ChangesResponse {
        try await get("/changes?since=\(max(since, 0))", as: ChangesResponse.self)
    }

    public func dialogFolders() async throws -> VersionedDoc {
        try await get("/dialog-folders", as: VersionedDoc.self)
    }

    /// Группы своих адресов (цвета строк) — тот же формат `version` + `doc`.
    public func identityGroups() async throws -> VersionedDoc {
        try await get("/identity-groups", as: VersionedDoc.self)
    }

    /// Документ папок: `409` — его поменяла другая станция, в теле ответа текущий.
    public func saveDialogFolders(baseVersion: Int64, doc: JSONValue) async throws -> VersionedDoc {
        struct Body: Encodable {
            let base_version: Int64
            let doc: JSONValue
        }
        let data = try await send("PUT", "/dialog-folders", body: Body(base_version: baseVersion, doc: doc))
        return try decode(VersionedDoc.self, from: data)
    }

    /// Тела писем по ссылкам. Ключ — стабильный `message_id`, uid — запасной.
    public func messages(_ refs: [MessageRef]) async throws -> [MessageBody] {
        let data = try await send("POST", "/conversations/messages", body: refs)
        return try decode([MessageBody].self, from: data)
    }

    /// Поставить или снять флаг (`\Seen`) на письмах.
    public func setFlag(_ refs: [MessageRef], flag: String, add: Bool) async throws {
        struct Body: Encodable {
            let messages: [Ref]
            let flags: String
            let add: Bool
        }
        try await send("POST", "/messages/flags", body: Body(messages: refs.map(Ref.init), flags: flag, add: add))
    }

    /// Мягкое удаление писем (vault на сервере, у внешних учёток — и на источнике).
    public func delete(_ refs: [MessageRef]) async throws {
        struct Body: Encodable { let messages: [Ref] }
        try await send("POST", "/messages/delete", body: Body(messages: refs.map(Ref.init)))
    }

    /// «Не спам»: письма — во «Входящие». `trust` — ещё и доверять отправителю.
    /// Ответ — сколько вернулось.
    @discardableResult
    public func restoreSpam(_ refs: [MessageRef], trust: Bool = false) async throws -> Int {
        struct Item: Encodable {
            let uid: Int64
            let message_id: String
        }
        struct Body: Encodable {
            let messages: [Item]
            let trust: Bool?
        }
        let items = refs.map { Item(uid: $0.uid, message_id: $0.messageID) }
        let data = try await send("POST", "/spam/restore", body: Body(messages: items, trust: trust ? true : nil))
        let obj = JSONValue.parse(data)
        if case let .int(n)? = obj?["restored"] { return Int(n) }
        return 0
    }

    /// «В спам»: удалить письма и заблокировать отправителей. Отправителя
    /// сервер берёт из самих писем (`messageIDs` — серверные id), а не из
    /// догадки клиента. `scope` — `address` или `domain`.
    public func blacklistAndPurge(scope: String, messageIDs: [Int64], fallbackAddress: String) async throws -> PurgeOutcome {
        struct Body: Encodable {
            let scope: String
            let address: String
            let message_ids: [Int64]
        }
        let data = try await send(
            "POST", "/spam/blacklist-and-purge",
            body: Body(scope: scope, address: fallbackAddress, message_ids: messageIDs)
        )
        return try decode(PurgeOutcome.self, from: data)
    }

    /// Аватар адреса: байты картинки или `nil`, если нет (204 / ошибка).
    public func avatar(email: String) async throws -> Binary? {
        do {
            let b = try await binary("/avatars?email=\(queryEscaped(email))")
            return b.data.isEmpty ? nil : b
        } catch APIError.server {
            return nil
        }
    }

    /// Встроенная часть письма (`cid:`).
    public func part(uid: Int64, cid: String) async throws -> Binary {
        try await binary("/messages/\(uid)/parts/\(pathEscaped(cid))")
    }

    /// Вложение по индексу.
    public func attachment(uid: Int64, index: Int) async throws -> Binary {
        try await binary("/messages/\(uid)/attachments/\(index)")
    }

    func binary(_ path: String) async throws -> Binary {
        let (data, response) = try await authedResponse { URLRequest(url: url(path)).bearer($0) }
        return Binary(data: data, contentType: response.value(forHTTPHeaderField: "Content-Type") ?? "application/octet-stream")
    }
}

/// Ссылка на письмо в теле запросов флагов и удаления.
private struct Ref: Encodable {
    let folder: String
    let uid: Int64
    let message_id: String

    init(_ r: MessageRef) {
        folder = r.folder
        uid = r.uid
        message_id = r.messageID
    }
}

/// Безопасные символы URL (RFC 3986, unreserved) и `@`; всё прочее, включая
/// не-ASCII, — %XX. `alphanumerics` не годится: в нём и кириллица.
private let unreserved = CharacterSet(
    charactersIn: "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~@"
)

/// Значение параметра запроса (`+` и `&` в адресе почты иначе сломали бы запрос).
func queryEscaped(_ s: String) -> String {
    s.addingPercentEncoding(withAllowedCharacters: unreserved) ?? s
}

/// Сегмент пути: `/` и прочее — %XX.
func pathEscaped(_ s: String) -> String {
    s.addingPercentEncoding(withAllowedCharacters: unreserved) ?? s
}

extension SessionAPI {
    /// Поставить письмо в отправку. 413 — вложения больше лимита сервера.
    /// JSON с base64 собирается вне главного потока вызывающего: вложения бывают по 20 МБ.
    /// Ответ — Message-ID копии в «Отправленных» и статус `pending`
    /// (десктоп §1); его нет или он не разобрался — пустой итог: письмо
    /// принято, а беседа узнается по набору адресов.
    @discardableResult
    public func send(_ request: SendRequest) async throws -> SendReceipt {
        let data = try await send("POST", "/send", body: request)
        return (try? decode(SendReceipt.self, from: data)) ?? SendReceipt()
    }
}

extension SessionAPI {
    /// Пуш-канал сервера (`/ws`), пока приложение на экране (§7): поток
    /// событий (`new_message`, `message_sent`, `delivery_status`,
    /// `calendar_updated`, …). Токен — только заголовком `Authorization` (§2):
    /// в URL он осел бы в журналах nginx. Отказ рукопожатия 401/403 —
    /// `APIError.unauthorized`.
    public func events(session: URLSession = .shared) async -> AsyncThrowingStream<ServerEvent, Error> {
        let token = await currentToken()
        let wsURL = URL(string: baseURL.replacingOccurrences(of: "https://", with: "wss://") + "/api/desktop/v1/ws")!
        var req = URLRequest(url: wsURL)
        req.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        let task = session.webSocketTask(with: req)
        return AsyncThrowingStream { cont in
            let reader = Task {
                task.resume()
                do {
                    while !Task.isCancelled {
                        let msg = try await task.receive()
                        let data: Data
                        switch msg {
                        case let .string(s): data = Data(s.utf8)
                        case let .data(d): data = d
                        @unknown default: continue
                        }
                        if let event = ServerEvent.parse(data) { cont.yield(event) }
                    }
                    cont.finish()
                } catch {
                    let code = (task.response as? HTTPURLResponse)?.statusCode
                    cont.finish(throwing: code == 401 || code == 403 ? APIError.unauthorized("ws") : error)
                }
            }
            cont.onTermination = { _ in
                reader.cancel()
                task.cancel(with: .goingAway, reason: nil)
            }
        }
    }
}
