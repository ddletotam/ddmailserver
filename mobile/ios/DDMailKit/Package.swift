// swift-tools-version: 6.0
// Логика клиента без UI (контракт §1): собирается и тестируется `swift test`
// на Маке без симулятора. Модули — по таблице §1; добавляются по мере портирования.
import PackageDescription

let package = Package(
    name: "DDMailKit",
    defaultLocalization: "ru",
    platforms: [.iOS("27.0"), .macOS("27.0")],
    products: [
        .library(name: "DDMailAPI", targets: ["DDMailAPI"]),
        .library(name: "DDMailAuth", targets: ["DDMailAuth"]),
        .library(name: "DDMailStore", targets: ["DDMailStore"]),
        .library(name: "DDMailSync", targets: ["DDMailSync"]),
        .library(name: "DDMailCalendar", targets: ["DDMailCalendar"]),
        .library(name: "DDMailMIME", targets: ["DDMailMIME"]),
        .library(name: "DDMailNet", targets: ["DDMailNet"]),
        .library(name: "DDMailIMAP", targets: ["DDMailIMAP"]),
        .library(name: "DDMailSMTP", targets: ["DDMailSMTP"]),
    ],
    dependencies: [
        // Единственная сторонняя зависимость логики (контракт §1): кэш на SQLite
        // с явными миграциями и одним писателем.
        .package(url: "https://github.com/groue/GRDB.swift.git", from: "7.11.0"),
    ],
    targets: [
        .target(name: "DDMailAPI"),
        // Внешние учётки (mobile-external-accounts.md): MIME, сеть, IMAP, SMTP
        // — свои модули без сторонних зависимостей (решение ios §1).
        .target(name: "DDMailMIME"),
        .target(name: "DDMailNet"),
        .target(name: "DDMailIMAP", dependencies: ["DDMailNet", "DDMailMIME"]),
        .target(name: "DDMailSMTP", dependencies: ["DDMailNet", "DDMailMIME"]),
        .target(name: "DDMailAuth", dependencies: ["DDMailAPI", "DDMailNet", "DDMailIMAP", "DDMailSMTP"]),
        .target(name: "DDMailStore", dependencies: ["DDMailAPI", .product(name: "GRDB", package: "GRDB.swift")]),
        .target(name: "DDMailSync", dependencies: ["DDMailAPI", "DDMailStore", "DDMailMIME", "DDMailNet", "DDMailIMAP", "DDMailSMTP"]),
        .target(name: "DDMailCalendar", dependencies: ["DDMailAPI"]),
        .testTarget(name: "DDMailAPITests", dependencies: ["DDMailAPI"]),
        .testTarget(name: "DDMailAuthTests", dependencies: ["DDMailAuth", "DDMailAPI"]),
        .testTarget(name: "DDMailStoreTests", dependencies: ["DDMailStore", "DDMailAPI"]),
        .testTarget(name: "DDMailSyncTests", dependencies: ["DDMailSync", "DDMailStore", "DDMailAPI", "DDMailIMAP", "DDMailNet", "DDMailMIME"]),
        .testTarget(name: "DDMailCalendarTests", dependencies: ["DDMailCalendar", "DDMailAPI"]),
        .testTarget(name: "DDMailMIMETests", dependencies: ["DDMailMIME"]),
        .testTarget(name: "DDMailIMAPTests", dependencies: ["DDMailIMAP", "DDMailNet", "DDMailMIME"]),
        .testTarget(name: "DDMailSMTPTests", dependencies: ["DDMailSMTP", "DDMailNet", "DDMailMIME"]),
    ],
    swiftLanguageModes: [.v6]
)
