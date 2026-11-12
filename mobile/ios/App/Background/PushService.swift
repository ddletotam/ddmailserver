import DDMailAPI
import DDMailAuth
import DDMailPush
import DDMailStore
import Foundation
import os
import UserNotifications
#if os(iOS)
import UIKit
#else
import AppKit
#endif

private let pushLog = Logger(subsystem: "ru.letotam.ddmail", category: "push")

/// Push в приложении (контракт §7): токен APNs у системы, регистрация на
/// сервере каждой ddmail-учётки, снятие при выходе и выключенных
/// уведомлениях. Внешние IMAP-учётки push не имеют (§13.4).
@MainActor
final class PushService {
    static let shared = PushService()

    let registry: PushRegistry
    private weak var workspace: Workspace?
    private var observing = false

    private init() {
        // Окружение APNs — из права `aps-environment` этой сборки (project.yml
        // кладёт его в Info.plist): отладочная — sandbox, релизная — production.
        let entitlement = Bundle.main.object(forInfoDictionaryKey: "DDMailAPSEnvironment") as? String ?? "development"
        registry = PushRegistry(
            state: AppSettings.shared.shared,
            topic: Bundle.main.bundleIdentifier ?? "ru.letotam.ddmail",
            environment: APNsEnvironment(entitlement: entitlement)
        )
        AppSettings.shared.onNotificationsChanged = { [weak self] in
            Task { await self?.sync() }
        }
    }

    /// Рабочее пространство на экране: токен у системы и регистрации учёток.
    /// APNs советует просить токен при каждом запуске — он может смениться.
    func attach(_ workspace: Workspace) {
        self.workspace = workspace
        observeCacheChanges()
        Task {
            let status = await UNUserNotificationCenter.current().notificationSettings().authorizationStatus
            // Без разрешения видимый push не покажется и расширение не проснётся.
            guard status == .authorized || status == .provisional else { return }
            #if os(iOS)
            UIApplication.shared.registerForRemoteNotifications()
            #else
            NSApplication.shared.registerForRemoteNotifications()
            #endif
            await sync()
        }
    }

    /// Система выдала токен.
    func received(token: Data) {
        let hex = token.hexString
        pushLog.notice("push: device token \(hex.prefix(8), privacy: .public)…")
        Task {
            await registry.setDeviceToken(hex)
            await sync()
        }
    }

    func failed(_ error: Error) {
        pushLog.error("push: registration failed: \(error.localizedDescription, privacy: .public)")
    }

    /// Регистрации — к нужному виду: у живых ddmail-учёток текущий токен
    /// (или ни у одной, если уведомления выключены).
    func sync() async {
        guard let workspace else { return }
        let targets = workspace.links.compactMap { l -> PushTarget? in
            guard l.status != .dead, let api = l.api else { return nil }
            return PushTarget(accountKey: l.account.key, api: api)
        }
        await registry.sync(targets)
    }

    /// Учётку удаляют: снять регистрацию её сессией, пока она есть. Запись
    /// читается сейчас — состояние учётки сотрут раньше, чем дойдёт запрос.
    /// Запрос — без хранилища токена: обмен не вернёт стёртую сессию в Keychain.
    func remove(account: MailAccount, session: Session?) {
        guard account.kind == .ddmail, let session, !session.token.isEmpty else { return }
        let registration = AppSettings.shared.shared.registrations[account.key]
        guard registration != nil else { return }
        let api = SessionAPI(baseURL: session.serverURL, transport: AppModel.makeSession(), tokens: EphemeralTokens(session.token))
        let target = PushTarget(accountKey: account.key, api: api)
        Task { [registry] in await registry.remove(target, registration: registration) }
    }

    /// Расширение записало в общий кэш: наблюдения GRDB этого процесса чужую
    /// запись не видят — списки перечитываются по уведомлению Darwin.
    private func observeCacheChanges() {
        guard !observing else { return }
        observing = true
        let center = CFNotificationCenterGetDarwinNotifyCenter()
        CFNotificationCenterAddObserver(
            center, Unmanaged.passUnretained(self).toOpaque(),
            { _, _, _, _, _ in
                Task { @MainActor in PushService.shared.cacheChanged() }
            },
            AppGroup.cacheChangedNotification as CFString, nil, .deliverImmediately
        )
    }

    func cacheChanged() {
        try? workspace?.cache?.externalChange()
    }
}

#if os(iOS)
/// Делегат приложения — только ради токена APNs (SwiftUI его не отдаёт).
final class AppDelegate: NSObject, UIApplicationDelegate {
    func application(_ application: UIApplication, didRegisterForRemoteNotificationsWithDeviceToken deviceToken: Data) {
        PushService.shared.received(token: deviceToken)
    }

    func application(_ application: UIApplication, didFailToRegisterForRemoteNotificationsWithError error: Error) {
        PushService.shared.failed(error)
    }
}
#else
final class AppDelegate: NSObject, NSApplicationDelegate {
    func application(_ application: NSApplication, didRegisterForRemoteNotificationsWithDeviceToken deviceToken: Data) {
        PushService.shared.received(token: deviceToken)
    }

    func application(_ application: NSApplication, didFailToRegisterForRemoteNotificationsWithError error: Error) {
        PushService.shared.failed(error)
    }
}
#endif
