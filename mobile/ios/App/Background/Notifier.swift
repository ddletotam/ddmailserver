import DDMailSync
import Foundation
import UserNotifications

/// Уведомления о новой почте (§5, §7) — из своего кэша, после каждого синка
/// (на экране и в фоне). Беседа — одно уведомление (`threadIdentifier` и id —
/// её id): новое письмо заменяет прежнее. Бейдж — беседы с непрочитанным.
@MainActor
enum Notifier {
    static let category = "mail"

    static func check(engine: SyncEngine, openID: String?) async {
        guard let convs = try? engine.currentConversations() else { return }
        let settings = AppSettings.shared
        let plan = NotificationPlan(
            conversations: convs, folders: engine.currentFolders(),
            openID: openID, previousWatermark: settings.notifiedMax
        )
        settings.notifiedMax = plan.watermark
        let center = UNUserNotificationCenter.current()
        try? await center.setBadgeCount(plan.badge)
        guard settings.notifications, !plan.items.isEmpty else { return }
        let auth = await center.notificationSettings().authorizationStatus
        guard auth == .authorized || auth == .provisional else { return }
        for item in plan.items {
            let t = NotificationPlan.text(item)
            let content = UNMutableNotificationContent()
            content.title = t.title
            content.body = t.body
            content.threadIdentifier = item.conversation.id
            content.categoryIdentifier = category
            content.userInfo = ["conversation": item.conversation.id]
            content.sound = .default
            try? await center.add(UNNotificationRequest(identifier: item.conversation.id, content: content, trigger: nil))
        }
    }

    /// Беседу открыли — её уведомление больше не нужно.
    static func clear(conversation id: String) {
        UNUserNotificationCenter.current().removeDeliveredNotifications(withIdentifiers: [id])
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

    @MainActor func attach(_ open: @escaping (String) -> Void) {
        self.open = open
        if let id = pending {
            pending = nil
            open(id)
        }
    }

    func userNotificationCenter(_ center: UNUserNotificationCenter, didReceive response: UNNotificationResponse) async {
        guard let id = response.notification.request.content.userInfo["conversation"] as? String else { return }
        await MainActor.run {
            if let open { open(id) } else { pending = id }
        }
    }

    /// На экране — тоже показываем (о других беседах), но тихо.
    func userNotificationCenter(_ center: UNUserNotificationCenter, willPresent notification: UNNotification) async -> UNNotificationPresentationOptions {
        [.banner, .list]
    }
}
