import DDMailAPI
import DDMailAuth
import DDMailNet
import DDMailStore
import DDMailSync
import Foundation
import os

private let catchUpLog = Logger(subsystem: "ru.letotam.ddmail", category: "push")

/// Работа Notification Service Extension (контракт §7): push сказал «есть
/// новые письма» — дельта-синк ddmail-учёток в общий кэш, затем те же
/// правила «что уведомлять», что у приложения (`NotificationPlan`: рубеж по
/// uid, спам и скрытые папки молчат), по всем учёткам кэша. Внешние
/// IMAP-учётки push не имеют и не синкаются (§13.4), но их беседы идут в
/// бейдж.
public struct PushCatchUp: Sendable {
    public struct Outcome: Sendable {
        /// Что показать — по беседе на уведомление, свежие первыми.
        public var items: [NotificationPlan.Item]
        /// Бейдж — беседы с непрочитанным вне скрытых папок и спама.
        public var badge: Int
        /// Все ddmail-учётки синхронизированы: пустой `items` значит «нечего
        /// показывать», а не «не успели узнать».
        public var complete: Bool
        /// «Новые письма» включены в настройках приложения.
        public var enabled: Bool
    }

    let accounts: AccountStore
    let transport: HTTPTransport
    let state: SharedState
    let cacheURL: URL
    let syncLock: ProcessLock
    let notifyLock: ProcessLock

    public init(
        accounts: AccountStore, transport: HTTPTransport, state: SharedState = SharedState(),
        cacheURL: URL = AppGroup.cacheURL, syncLock: ProcessLock = .sync, notifyLock: ProcessLock = .notify
    ) {
        self.accounts = accounts
        self.transport = transport
        self.state = state
        self.cacheURL = cacheURL
        self.syncLock = syncLock
        self.notifyLock = notifyLock
    }

    /// Синк не дольше `budget`, затем план уведомлений. Без кэша и учёток —
    /// пустой неполный итог: показывается обезличенный текст.
    public func run(budget: Duration) async -> Outcome {
        let list = accounts.accounts()
        guard !list.isEmpty, let cache = try? MailStore(url: cacheURL) else {
            return Outcome(items: [], badge: 0, complete: list.isEmpty, enabled: state.notificationsEnabled)
        }
        var engines: [SyncEngine] = []
        var complete = true
        for a in list where a.kind == .ddmail {
            guard let session = accounts.secret(a.key)?.session, !session.token.isEmpty else {
                complete = false // нужен вход: узнать, что пришло, нечем
                continue
            }
            let tokens = StoredTokens(session: session, store: AccountSessionStore(key: a.key, store: accounts))
            let api = SessionAPI(baseURL: session.serverURL, transport: transport, tokens: tokens)
            engines.append(SyncEngine(accountKey: a.key, api: api, store: cache))
        }

        // Фоновый проход приложения сейчас синкает — подождать его немного;
        // не дождались — синкаем всё равно: дельта идемпотентна.
        let held = await syncLock.acquire(timeout: .seconds(5))
        let synced = await Self.syncAll(engines, budget: budget)
        held?.release()
        if !synced { complete = false }
        AppGroup.postCacheChanged()

        let notifyHeld = await notifyLock.acquire(timeout: .seconds(2))
        defer { notifyHeld?.release() }
        var convs: [Conversation] = []
        for a in list {
            do {
                convs += try Self.reader(a, cache: cache).currentConversations()
            } catch {
                catchUpLog.error("push: cache of \(a.key, privacy: .private): \(String(describing: error), privacy: .public)")
            }
        }
        let folders = engines.first?.currentFolders() ?? LocalDialogFolders(store: cache).currentFolders()
        let plan = NotificationPlan(conversations: convs, folders: folders, openRef: nil, previous: state.notifiedMarks)
        state.notifiedMarks = plan.watermarks
        return Outcome(items: plan.items, badge: plan.badge, complete: complete, enabled: state.notificationsEnabled)
    }

    /// Беседы учётки из кэша — без сети.
    static func reader(_ a: MailAccount, cache: MailStore) -> any MailProvider {
        switch a.kind {
        case .ddmail:
            let api = SessionAPI(baseURL: a.serverURL, transport: URLSession.shared, tokens: EphemeralTokens(""))
            return SyncEngine(accountKey: a.key, api: api, store: cache)
        case .imap:
            let fallback = MailEndpoint(host: "", port: 0, security: .tls)
            let config = ImapAccountConfig(
                key: a.key, email: a.email, displayName: a.displayName, login: a.imapLogin,
                imap: a.imap ?? fallback, smtp: a.smtp ?? fallback, smtpLogin: a.smtpLogin, smtpAuth: a.smtpAuth,
                ownAddresses: a.ownAddresses
            )
            return ImapEngine(config: config, store: cache, password: { nil })
        }
    }

    private enum Step: Sendable {
        case synced(Bool)
        case timeout
    }

    /// Все учётки параллельно; `true` — все успели и без ошибок.
    static func syncAll(_ engines: [SyncEngine], budget: Duration) async -> Bool {
        guard !engines.isEmpty else { return true }
        return await withTaskGroup(of: Step.self) { group in
            for e in engines {
                group.addTask {
                    do {
                        try await e.sync()
                        return .synced(true)
                    } catch {
                        catchUpLog.notice("push: sync \(e.accountKey, privacy: .private): \(String(describing: error), privacy: .public)")
                        return .synced(false)
                    }
                }
            }
            group.addTask {
                try? await Task.sleep(for: budget)
                return .timeout
            }
            var ok = true
            var left = engines.count
            for await step in group {
                switch step {
                case let .synced(s):
                    ok = ok && s
                    left -= 1
                case .timeout:
                    ok = false
                    left = 0
                }
                if left == 0 { break }
            }
            group.cancelAll()
            return ok
        }
    }
}
