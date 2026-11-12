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

    /// Рубежи уведомлений по учёткам: самый большой uid, о котором уже
    /// уведомили (учётки нет — ещё не было прохода).
    var notifiedMarks: [String: Int64] {
        get { (d.dictionary(forKey: "notify.marks") as? [String: NSNumber])?.mapValues(\.int64Value) ?? [:] }
        set { d.set(newValue.mapValues { NSNumber(value: $0) }, forKey: "notify.marks") }
    }

    /// Учётку удалили — её рубеж тоже.
    func forget(account key: String) {
        notifiedMarks.removeValue(forKey: key)
    }

    /// Учёток не осталось: состояние чужого человека не остаётся.
    func clearAccountState() {
        prefetch = nil
        for k in ["notify.marks", "notify.max", "mail.activeFolder", "mail.lastConversation"] { d.removeObject(forKey: k) }
    }
}
