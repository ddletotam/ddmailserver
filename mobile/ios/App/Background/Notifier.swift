import DDMailCalendar
import DDMailStore
import DDMailSync
import Foundation
import UserNotifications

/// Уведомления о новой почте (§5, §7) — из своего кэша, после каждого синка
/// (на экране и в фоне). Беседа — одно уведомление (`threadIdentifier` и id —
/// её id): новое письмо заменяет прежнее. Бейдж — беседы с непрочитанным.
@MainActor
enum Notifier {
    static let category = "mail"

    /// Беседы всех учёток после синка. Идентификатор уведомления — `ref`
    /// беседы (учётка и id): у разных учёток id могут совпасть.
    static func check(conversations convs: [Conversation], folders: FoldersDoc, openRef: String?) async {
        let settings = AppSettings.shared
        // Рубежи общие с расширением уведомлений (§7): прочитать и записать
        // их одним куском — иначе о письме уведомили бы оба.
        let held = await ProcessLock.notify.acquire(timeout: .seconds(2))
        let plan = NotificationPlan(conversations: convs, folders: folders, openRef: openRef, previous: settings.notifiedMarks)
        settings.notifiedMarks = plan.watermarks
        held?.release()
        let center = UNUserNotificationCenter.current()
        try? await center.setBadgeCount(plan.badge)
        // Синк прошёл — обезличенные push («Новые письма», «нечего
        // показать») своё отслужили: о новом теперь говорят уведомления бесед.
        await clearPushPlaceholders()
        guard settings.notifications, !plan.items.isEmpty else { return }
        let auth = await center.notificationSettings().authorizationStatus
        guard auth == .authorized || auth == .provisional else { return }
        for item in plan.items {
            let t = NotificationPlan.text(item)
            let content = UNMutableNotificationContent()
            content.title = t.title
            content.body = t.body
            content.threadIdentifier = item.conversation.ref
            content.categoryIdentifier = category
            content.userInfo = [conversationKey: item.conversation.ref]
            content.sound = .default
            try? await center.add(UNNotificationRequest(identifier: item.conversation.ref, content: content, trigger: nil))
        }
    }

    /// Учётку удалили — её уведомления тоже (и показанные push: у них свой id,
    /// беседа — в `userInfo`).
    static func clear(account key: String) {
        let center = UNUserNotificationCenter.current()
        Task {
            let ids = await center.deliveredNotifications().map(\.request)
                .filter { Conversation.parseRef(conversationRef($0) ?? $0.identifier)?.account == key }
                .map(\.identifier)
            center.removeDeliveredNotifications(withIdentifiers: ids)
        }
    }

    /// Беседу открыли — её уведомление больше не нужно (своё — по id, push,
    /// переписанный расширением, — по беседе в `userInfo`).
    static func clear(conversation id: String) {
        let center = UNUserNotificationCenter.current()
        center.removeDeliveredNotifications(withIdentifiers: [id])
        Task {
            let ids = await center.deliveredNotifications().map(\.request)
                .filter { conversationRef($0) == id }
                .map(\.identifier)
            if !ids.isEmpty { center.removeDeliveredNotifications(withIdentifiers: ids) }
        }
    }

    /// Ключи `userInfo` уведомлений почты (их же ставит расширение).
    nonisolated static let conversationKey = "conversation"
    nonisolated static let pushKey = "ddmail_push"

    nonisolated static func conversationRef(_ r: UNNotificationRequest) -> String? {
        r.content.userInfo[conversationKey] as? String
    }

    /// Push, который расширение не превратило в уведомление беседы:
    /// обезличенный («не успело») или «нечего показать». У расширения нет
    /// права скрыть такой push (§12), поэтому его снимает приложение.
    nonisolated static func isPlaceholder(_ r: UNNotificationRequest) -> Bool {
        conversationRef(r) == nil && (r.content.userInfo[pushKey] != nil || r.content.threadIdentifier == "mail")
    }

    static func clearPushPlaceholders() async {
        let center = UNUserNotificationCenter.current()
        let ids = await center.deliveredNotifications().map(\.request).filter(isPlaceholder).map(\.identifier)
        if !ids.isEmpty { center.removeDeliveredNotifications(withIdentifiers: ids) }
    }

    /// Разрешение спрашиваем один раз, после входа (не на экране подключения).
    static func requestPermissionIfNeeded() async {
        let center = UNUserNotificationCenter.current()
        guard await center.notificationSettings().authorizationStatus == .notDetermined else { return }
        _ = try? await center.requestAuthorization(options: [.alert, .badge, .sound])
    }

    /// Выход: ни уведомлений, ни бейджа чужой учётки.
    static func clearAll() {
        let center = UNUserNotificationCenter.current()
        center.removeAllDeliveredNotifications()
        center.removeAllPendingNotificationRequests()
        center.setBadgeCount(0)
    }
}

/// Нажатие на уведомление — открыть беседу (с переходом в её папку).
final class NotificationRouter: NSObject, UNUserNotificationCenterDelegate, @unchecked Sendable {
    static let shared = NotificationRouter()
    @MainActor var open: ((String) -> Void)?
    @MainActor private var pending: String?
    /// Напоминание: ключ вхождения и действие (смахнули, «позже», нажатие).
    @MainActor var reminder: ((ReminderRow.Key, String) -> Void)?
    @MainActor private var pendingReminder: (ReminderRow.Key, String)?

    @MainActor func attachReminders(_ handle: @escaping (ReminderRow.Key, String) -> Void) {
        reminder = handle
        if let (k, a) = pendingReminder {
            pendingReminder = nil
            handle(k, a)
        }
    }

    @MainActor func attach(_ open: @escaping (String) -> Void) {
        self.open = open
        if let id = pending {
            pending = nil
            open(id)
        }
    }

    /// Вариант с обработчиком завершения, а не `async`: асинхронный вызывает
    /// завершение с фонового потока, а UIKit по нему сохраняет снимок сцены и
    /// падает на проверке главного потока (`_performBlockAfterCATransactionCommitSynchronizes`)
    /// — приложение закрывалось при каждом нажатии на уведомление.
    func userNotificationCenter(
        _ center: UNUserNotificationCenter,
        didReceive response: UNNotificationResponse,
        withCompletionHandler completionHandler: @escaping () -> Void
    ) {
        let action = response.actionIdentifier
        let request = response.notification.request
        let reminderKey = ReminderPlanner.parseNotificationID(request.identifier)
        let conversation = reminderKey == nil ? Notifier.conversationRef(request) : nil
        nonisolated(unsafe) let done = completionHandler
        DispatchQueue.main.async {
            MainActor.assumeIsolated {
                if let key = reminderKey {
                    if let reminder = self.reminder { reminder(key, action) } else { self.pendingReminder = (key, action) }
                } else if let id = conversation {
                    if let open = self.open { open(id) } else { self.pending = id }
                }
            }
            done()
        }
    }

    /// Открытая на экране беседа (`ref`) — о ней не показываем.
    @MainActor var openRef: (() -> String?)?

    /// На экране — тоже показываем (о других беседах), но тихо. Push «нечего
    /// показать» и push об открытой беседе — нет: приложение на экране само
    /// синхронизировано.
    func userNotificationCenter(_ center: UNUserNotificationCenter, willPresent notification: UNNotification) async -> UNNotificationPresentationOptions {
        let request = notification.request
        if request.content.userInfo[Notifier.pushKey] as? String == "stale" { return [] }
        if let ref = Notifier.conversationRef(request), await MainActor.run(body: { openRef?() == ref }) { return [] }
        return [.banner, .list]
    }
}
