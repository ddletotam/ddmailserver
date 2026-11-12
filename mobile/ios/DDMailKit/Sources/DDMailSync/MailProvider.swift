import DDMailAPI
import DDMailNet
import DDMailStore
import Foundation

/// Как учётка умеет «В спам» (контракт внешних учёток §4).
public enum SpamSupport: Equatable, Sendable {
    /// ddmail: письма удаляются, отправитель или домен блокируется правилом сервера.
    case rules
    /// IMAP: перенос в папку Junk, правил нет — одна кнопка «В спам».
    case folder
    /// Папки Junk нет — пункта нет.
    case none
}

/// Источник почты одной учётки. Сервер ddmail (`SyncEngine`) — одна из
/// реализаций, IMAP (`ImapEngine`) — другая; экран не знает, откуда беседа.
///
/// Ошибки — `APIError` в обоих случаях: `.unauthorized` — нужен вход
/// (ретраи не лечат), `.network` — временное, `.server` — отказ действия.
public protocol MailProvider: AnyObject, Sendable {
    var accountKey: String { get }
    /// Беседы учётки (со спамом, `spam = true`) при каждом изменении кэша.
    func conversations() -> AsyncStream<[Conversation]>
    func currentConversations() throws -> [Conversation]
    /// Свои адреса учётки — для «От кого» и ролей.
    func identities() -> [Identity]
    func spamSupport() -> SpamSupport

    func sync() async throws
    func bodies(_ refs: [MessageRef]) async throws -> [MessageBody]
    /// Докачка тел заранее; `constrained` — «только по Wi-Fi».
    @discardableResult
    func prefetchBodies(constrained: Bool, allowed: @escaping @Sendable () async -> Bool) async throws -> Int
    func part(uid: Int64, cid: String) async throws -> Binary
    func attachment(uid: Int64, index: Int) async throws -> Binary
    func send(_ request: SendRequest) async throws

    func markRead(_ c: Conversation) async throws
    func markUnread(_ c: Conversation) async throws
    func deleteConversations(_ convs: [Conversation]) async throws
    /// `scope` — `address`/`domain` у ddmail; IMAP его не знает.
    func reportSpam(_ convs: [Conversation], scope: String) async throws -> PurgeOutcome
    @discardableResult
    func restoreSpam(_ convs: [Conversation], trust: Bool) async throws -> Int
}

/// Папки диалогов: документ ddmail-учётки (сервер) или локальный (§4).
public protocol DialogFolderSource: AnyObject, Sendable {
    func folders() -> AsyncStream<FoldersDoc>
    func currentFolders() -> FoldersDoc
    func assignFolder(_ convs: [Conversation], folderID: String?) async throws
}

/// Без ddmail-учётки папки диалогов живут на устройстве — документ того же
/// формата, что `dialog_folders.json` десктопа, в кэше.
public final class LocalDialogFolders: DialogFolderSource {
    static let key = "local/dialog_folders"
    let store: MailStore

    public init(store: MailStore) { self.store = store }

    public func folders() -> AsyncStream<FoldersDoc> {
        let s = store.observeMeta(Self.key)
        return AsyncStream { cont in
            let t = Task {
                for await v in s { cont.yield(SyncEngine.parseFolders(v)) }
                cont.finish()
            }
            cont.onTermination = { _ in t.cancel() }
        }
    }

    public func currentFolders() -> FoldersDoc {
        SyncEngine.parseFolders((try? store.meta(Self.key)) ?? nil)
    }

    public func assignFolder(_ convs: [Conversation], folderID: String?) async throws {
        let doc = currentFolders()
        let target = folderID.flatMap { id in doc.folders.firstIndex { $0.id == id } }
        if folderID != nil && target == nil { return }
        let edited = doc.assign(convs, to: target)
        try store.setMeta(Self.key, String(decoding: edited.toJSON().data(), as: UTF8.self))
    }
}

/// Нужен вход: ретраи не лечат.
public func isAuthFailure(_ e: Error) -> Bool {
    if case APIError.unauthorized = e { return true }
    if let m = e as? MailNetError, m.isAuth { return true }
    return false
}

/// Ошибки сети и протокола почты → `APIError` (единый язык для экрана).
public func apiError(_ e: Error) -> Error {
    switch e {
    case let m as MailNetError:
        switch m {
        case let .auth(text): return APIError.unauthorized(text)
        case let .server(text): return APIError.server(code: 0, message: text)
        default: return APIError.network(m.localizedDescription)
        }
    default:
        return e
    }
}

/// Последовательность → `AsyncStream` (для требований протокола).
func stream<S: AsyncSequence & Sendable>(_ s: S) -> AsyncStream<S.Element> where S.Element: Sendable {
    AsyncStream { cont in
        let t = Task {
            do { for try await v in s { cont.yield(v) } } catch {}
            cont.finish()
        }
        cont.onTermination = { _ in t.cancel() }
    }
}
