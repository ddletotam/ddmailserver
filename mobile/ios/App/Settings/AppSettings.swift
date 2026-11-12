import DDMailPush
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

    /// «Новые письма». Общая с расширением уведомлений настройка (App Group):
    /// выключено — расширение тоже молчит, а регистрации push снимаются.
    var notifications: Bool {
        didSet {
            shared.notificationsEnabled = notifications
            onNotificationsChanged?()
        }
    }

    /// Состояние, общее с Notification Service Extension (контракт §7).
    let shared = SharedState()
    /// Выключатель уведомлений переключили — push перерегистрируется.
    @ObservationIgnored var onNotificationsChanged: (() -> Void)?

    private init() {
        prefetch = d.string(forKey: "settings.prefetch").flatMap(Prefetch.init)
        // До App Group рубежи и выключатель жили в настройках приложения.
        shared.migrate(from: d)
        notifications = shared.notificationsEnabled
    }

    /// Рубежи уведомлений по учёткам: самый большой uid, о котором уже
    /// уведомили (учётки нет — ещё не было прохода). Общие с расширением.
    var notifiedMarks: [String: Int64] {
        get { shared.notifiedMarks }
        set { shared.notifiedMarks = newValue }
    }

    /// Учётку удалили — её рубеж и регистрация push тоже.
    func forget(account key: String) {
        shared.forget(account: key)
    }

    /// Учёток не осталось: состояние чужого человека не остаётся.
    func clearAccountState() {
        prefetch = nil
        shared.clearAccountState()
        for k in ["notify.marks", "notify.max", "mail.activeFolder", "mail.lastConversation"] { d.removeObject(forKey: k) }
    }
}
