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
    ],
    targets: [
        .target(name: "DDMailAPI"),
        .testTarget(name: "DDMailAPITests", dependencies: ["DDMailAPI"]),
    ],
    swiftLanguageModes: [.v6]
)
