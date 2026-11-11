import Foundation
import Observation

/// Настройки на устройстве (Android `AppSettings`): загрузка писем заранее,
/// уведомления, рубеж уведомлений.
@MainActor @Observable
final class AppSettings {
    /// «Загружать письма заранее?» (§4). Ответа по умолчанию нет: трафик
    /// пользователя, решать не нам.
    enum Prefetch: String, CaseIterable, Identifiable {
        case always, wifi, never
        var id: Self { self }

        var title: String {
            switch self {
            case .always: "Всегда"
            case .wifi: "Только по Wi-Fi"
            case .never: "Нет, при открытии"
            }
        }
    }

    static let shared = AppSettings()
    private let d = UserDefaults.standard

    var prefetch: Prefetch? {
        didSet { d.set(prefetch?.rawValue, forKey: "settings.prefetch") }
    }

    var notifications: Bool {
        didSet { d.set(notifications, forKey: "settings.notifications") }
    }

    private init() {
        prefetch = d.string(forKey: "settings.prefetch").flatMap(Prefetch.init)
        notifications = d.object(forKey: "settings.notifications") as? Bool ?? true
    }

    /// Самый большой uid, о котором уже уведомили (`nil` — ещё не было прохода).
    var notifiedMax: Int64? {
        get { (d.object(forKey: "notify.max") as? NSNumber)?.int64Value }
        set { d.set(newValue.map { NSNumber(value: $0) }, forKey: "notify.max") }
    }

    /// Выход из учётки: состояние чужой учётки не остаётся.
    func clearAccountState() {
        prefetch = nil
        notifiedMax = nil
        for k in ["mail.activeFolder", "mail.lastConversation"] { d.removeObject(forKey: k) }
    }
}
