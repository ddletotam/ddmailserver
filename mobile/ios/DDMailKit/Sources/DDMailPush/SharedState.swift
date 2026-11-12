import DDMailAPI
import Foundation

/// Состояние, общее для приложения и Notification Service Extension
/// (контракт §7): рубежи уведомлений, «уведомлять ли», токен APNs и какие
/// учётки его уже зарегистрировали. Лежит в общих настройках App Group.
public struct SharedState: @unchecked Sendable {
    let d: UserDefaults

    public init(_ defaults: UserDefaults = AppGroup.defaults) {
        d = defaults
    }

    enum Key {
        static let marks = "notify.marks"
        static let notifications = "settings.notifications"
        static let deviceToken = "push.deviceToken"
        static let registrations = "push.registrations"
        static let migrated = "shared.migrated"
    }

    /// Рубежи уведомлений по учёткам: самый большой uid, о котором уже
    /// уведомили (учётки нет — ещё не было прохода).
    public var notifiedMarks: [String: Int64] {
        get { (d.dictionary(forKey: Key.marks) as? [String: NSNumber])?.mapValues(\.int64Value) ?? [:] }
        nonmutating set { d.set(newValue.mapValues { NSNumber(value: $0) }, forKey: Key.marks) }
    }

    /// «Новые письма» в настройках; по умолчанию — да.
    public var notificationsEnabled: Bool {
        get { d.object(forKey: Key.notifications) as? Bool ?? true }
        nonmutating set { d.set(newValue, forKey: Key.notifications) }
    }

    /// Токен APNs устройства (hex); `nil` — ещё не получен.
    public var deviceToken: String? {
        get { d.string(forKey: Key.deviceToken) }
        nonmutating set { d.set(newValue, forKey: Key.deviceToken) }
    }

    /// Что зарегистрировано на сервере каждой учётки: `токен|окружение`.
    public var registrations: [String: String] {
        get { d.dictionary(forKey: Key.registrations) as? [String: String] ?? [:] }
        nonmutating set { d.set(newValue, forKey: Key.registrations) }
    }

    /// Учётку удалили — её рубеж и регистрация тоже.
    public func forget(account key: String) {
        var m = notifiedMarks
        m.removeValue(forKey: key)
        notifiedMarks = m
        var r = registrations
        r.removeValue(forKey: key)
        registrations = r
    }

    /// Учёток не осталось.
    public func clearAccountState() {
        for k in [Key.marks, Key.registrations] { d.removeObject(forKey: k) }
    }

    /// До App Group рубежи и выключатель уведомлений жили в настройках
    /// приложения; переносятся один раз, без потери.
    public func migrate(from old: UserDefaults) {
        guard old !== d, !d.bool(forKey: Key.migrated) else { return }
        for k in [Key.marks, Key.notifications] {
            if d.object(forKey: k) == nil, let v = old.object(forKey: k) { d.set(v, forKey: k) }
            old.removeObject(forKey: k)
        }
        d.set(true, forKey: Key.migrated)
    }
}
