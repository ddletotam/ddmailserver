import DDMailSync
import Foundation
import os
#if canImport(BackgroundTasks) && os(iOS)
import BackgroundTasks
#endif

/// Фон (§4, §7): `BGAppRefreshTask` — страховка от потерянного push (синк и
/// уведомления), `BGProcessingTask` — докачка тел писем по выбору
/// пользователя. Время запуска выбирает iOS. На Маке фоновых задач нет:
/// приложение живёт и с закрытым окном.
enum BackgroundSync {
    static let refreshID = "ru.letotam.ddmail.refresh"
    static let prefetchID = "ru.letotam.ddmail.prefetch"

    /// Поставить обе задачи (повторная постановка заменяет прежнюю).
    static func schedule() async {
        #if os(iOS)
        let refresh = BGAppRefreshTaskRequest(identifier: refreshID)
        refresh.earliestBeginDate = Date(timeIntervalSinceNow: 15 * 60)
        await submit(refresh)
        if let mode = AppSettingsSnapshot.prefetch, mode != "never" {
            let p = BGProcessingTaskRequest(identifier: prefetchID)
            p.requiresNetworkConnectivity = true
            p.requiresExternalPower = false
            p.earliestBeginDate = Date(timeIntervalSinceNow: 30 * 60)
            await submit(p)
        }
        #endif
    }

    #if os(iOS)
    private static func submit(_ r: BGTaskRequest) async {
        do {
            try await BGTaskScheduler.shared.submitTaskRequest(r)
        } catch {
            // В симуляторе фоновые задачи не ставятся — это не ошибка приложения.
            bgLog.notice("bg submit \(r.identifier, privacy: .public): \(error.localizedDescription, privacy: .public)")
        }
    }
    #endif
}

private let bgLog = Logger(subsystem: "ru.letotam.ddmail", category: "background")

/// Чтение настроек вне главного потока (фон стартует не на нём).
enum AppSettingsSnapshot {
    static var prefetch: String? { UserDefaults.standard.string(forKey: "settings.prefetch") }
}
