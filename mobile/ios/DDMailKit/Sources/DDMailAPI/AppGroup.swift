import Foundation

/// Общий контейнер приложения и Notification Service Extension (контракт §7):
/// кэш почты, список учёток, рубежи уведомлений и блокировки — в App Group,
/// иначе расширение их не увидит.
public enum AppGroup {
    /// Идентификатор группы — из Info.plist процесса (`DDMailAppGroup`,
    /// project.yml): на iOS `group.ru.letotam.ddmail`, на macOS — с префиксом
    /// команды. Без ключа (тесты пакета) — iOS-вариант.
    public static var id: String {
        Bundle.main.object(forInfoDictionaryKey: "DDMailAppGroup") as? String ?? "group.ru.letotam.ddmail"
    }

    /// Контейнер группы; `nil` — у процесса нет права (`swift test`, сборка
    /// без подписи).
    public static var containerURL: URL? {
        FileManager.default.containerURL(forSecurityApplicationGroupIdentifier: id)
    }

    /// Каталог данных ddmail в группе (`Library/Application Support/ddmail`);
    /// без группы — свой каталог процесса, как до App Group.
    public static var dataDirectory: URL {
        guard let c = containerURL else { return legacyDataDirectory }
        return c.appending(path: "Library/Application Support/ddmail", directoryHint: .isDirectory)
    }

    /// Где данные лежали до App Group — Application Support приложения.
    public static var legacyDataDirectory: URL {
        URL.applicationSupportDirectory.appending(path: "ddmail", directoryHint: .isDirectory)
    }

    public static var cacheURL: URL { dataDirectory.appending(path: "cache.sqlite") }
    public static var accountsURL: URL { dataDirectory.appending(path: "accounts.json") }

    /// Общие настройки (рубежи уведомлений, токен push); без группы — свои.
    public static var defaults: UserDefaults {
        containerURL == nil ? .standard : UserDefaults(suiteName: id) ?? .standard
    }

    /// Имя уведомления Darwin «расширение записало в кэш» — приложение на
    /// экране перечитывает списки (наблюдение GRDB чужой процесс не видит).
    public static let cacheChangedNotification = "ru.letotam.ddmail.cache-changed"

    /// Сообщить другим процессам группы, что кэш изменился.
    public static func postCacheChanged() {
        CFNotificationCenterPostNotification(
            CFNotificationCenterGetDarwinNotifyCenter(),
            CFNotificationName(cacheChangedNotification as CFString), nil, nil, true
        )
    }
}
