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
    ],
    dependencies: [
        // Единственная сторонняя зависимость логики (контракт §1): кэш на SQLite
        // с явными миграциями и одним писателем.
        .package(url: "https://github.com/groue/GRDB.swift.git", from: "7.11.0"),
    ],
    targets: [
        .target(name: "DDMailAPI"),
        .target(name: "DDMailAuth", dependencies: ["DDMailAPI"]),
        .target(name: "DDMailStore", dependencies: ["DDMailAPI", .product(name: "GRDB", package: "GRDB.swift")]),
        .target(name: "DDMailSync", dependencies: ["DDMailAPI", "DDMailStore"]),
        .testTarget(name: "DDMailAPITests", dependencies: ["DDMailAPI"]),
        .testTarget(name: "DDMailAuthTests", dependencies: ["DDMailAuth", "DDMailAPI"]),
        .testTarget(name: "DDMailStoreTests", dependencies: ["DDMailStore", "DDMailAPI"]),
        .testTarget(name: "DDMailSyncTests", dependencies: ["DDMailSync", "DDMailStore", "DDMailAPI"]),
    ],
    swiftLanguageModes: [.v6]
)
