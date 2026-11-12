import DDMailAPI
import DDMailAuth
import DDMailPush
import DDMailSync
import Foundation
import os
import UserNotifications

private let nseLog = Logger(subsystem: "ru.letotam.ddmail", category: "nse")

/// Notification Service Extension (контракт §7). Сервер шлёт видимый push с
/// обезличенным текстом «Новые письма» и `mutable-content`; система будит
/// расширение до показа (до ~30 с). Расширение делает дельта-синк
/// ddmail-учёток в общий кэш App Group и подменяет текст настоящим —
/// отправитель и тема приходят на устройство по нашему каналу, не через Apple.
///
/// Итоги:
/// - есть что показать — это уведомление становится уведомлением первой
///   беседы, остальные беседы — отдельными уведомлениями (id — беседа, как у
///   приложения); бейдж — из кэша;
/// - всё синхронизировано, а показывать нечего (письмо в скрытой папке,
///   прочитано на другом устройстве, уже показано приложением, уведомления
///   выключены) — тихое уведомление без звука (`passive`): права скрыть
///   push у расширения нет (§12), его снимает приложение;
/// - не успели или нет сети — обезличенный текст как есть; тап открывает
///   приложение, оно синхронизирует и снимает его.
final class NotificationService: UNNotificationServiceExtension, @unchecked Sendable {
    /// Из ~30 с, что даёт система, — на синк; остальное — на запись и показ.
    static let budget: Duration = .seconds(20)

    private let lock = NSLock()
    private var handler: ((UNNotificationContent) -> Void)?
    private var original: UNNotificationContent?
    private var work: Task<Void, Never>?

    override func didReceive(_ request: UNNotificationRequest, withContentHandler contentHandler: @escaping (UNNotificationContent) -> Void) {
        lock.withLock {
            handler = contentHandler
            original = request.content
        }
        work = Task { [weak self] in
            let outcome = await PushCatchUp(accounts: KeychainAccountStore(), transport: Self.session()).run(budget: Self.budget)
            nseLog.notice("nse: items=\(outcome.items.count) complete=\(outcome.complete) badge=\(outcome.badge)")
            await Self.postExtra(outcome)
            self?.deliver(outcome)
        }
    }

    override func serviceExtensionTimeWillExpire() {
        // Система вот-вот покажет push как есть — отдаём обезличенный текст сами.
        work?.cancel()
        deliver(nil)
    }

    /// Отдать содержимое системе — один раз.
    private func deliver(_ outcome: PushCatchUp.Outcome?) {
        let (h, base): (((UNNotificationContent) -> Void)?, UNNotificationContent?) = lock.withLock {
            defer { handler = nil }
            return (handler, original)
        }
        guard let h, let base else { return }
        h(Self.content(base, outcome))
    }

    /// Содержимое по итогу прохода; `nil` — не успели.
    static func content(_ base: UNNotificationContent, _ outcome: PushCatchUp.Outcome?) -> UNNotificationContent {
        let c = (base.mutableCopy() as? UNMutableNotificationContent) ?? UNMutableNotificationContent()
        c.categoryIdentifier = "mail"
        guard let outcome else {
            c.threadIdentifier = "mail"
            c.userInfo["ddmail_push"] = "generic"
            return c
        }
        c.badge = NSNumber(value: outcome.badge)
        if outcome.enabled, let first = outcome.items.first {
            let t = NotificationPlan.text(first)
            c.title = t.title
            c.body = t.body
            c.threadIdentifier = first.conversation.ref
            c.userInfo["conversation"] = first.conversation.ref
            c.userInfo["ddmail_push"] = "conversation"
        } else if outcome.complete || !outcome.enabled {
            c.title = "ddmail"
            c.body = "Почта обновлена"
            c.sound = nil
            c.interruptionLevel = .passive
            c.relevanceScore = 0
            c.threadIdentifier = "ddmail.stale"
            c.userInfo["ddmail_push"] = "stale"
        } else {
            c.threadIdentifier = "mail"
            c.userInfo["ddmail_push"] = "generic"
        }
        return c
    }

    /// Остальные беседы — своими уведомлениями (id — `ref` беседы, как у
    /// приложения: следующее письмо беседы заменит прежнее).
    static func postExtra(_ outcome: PushCatchUp.Outcome) async {
        guard outcome.enabled, outcome.items.count > 1 else { return }
        let center = UNUserNotificationCenter.current()
        for item in outcome.items.dropFirst() {
            let t = NotificationPlan.text(item)
            let c = UNMutableNotificationContent()
            c.title = t.title
            c.body = t.body
            c.threadIdentifier = item.conversation.ref
            c.categoryIdentifier = "mail"
            c.userInfo = ["conversation": item.conversation.ref]
            c.sound = .default
            c.badge = NSNumber(value: outcome.badge)
            do {
                try await center.add(UNNotificationRequest(identifier: item.conversation.ref, content: c, trigger: nil))
            } catch {
                nseLog.error("nse: add: \(error.localizedDescription, privacy: .public)")
            }
        }
    }

    /// Сеть как у приложения: без кук и дискового кэша ответов (в них письма).
    static func session() -> URLSession {
        let c = URLSessionConfiguration.ephemeral
        c.httpCookieStorage = nil
        c.httpShouldSetCookies = false
        c.urlCache = nil
        c.requestCachePolicy = .reloadIgnoringLocalCacheData
        c.timeoutIntervalForRequest = 15
        c.waitsForConnectivity = false
        return URLSession(configuration: c)
    }
}
