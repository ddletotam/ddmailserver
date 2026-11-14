import XCTest

/// Навигация как у десктопа (`docs/mobile-layout.md`): разделы — только из
/// бургера в первой строке раздела; ни таб-бара, ни боковой колонки.
enum Nav {
    /// Бургер видимого раздела. В открытой беседе на iPhone его нет — сначала
    /// назад к списку.
    @MainActor
    static func burger(_ app: XCUIApplication) -> XCUIElement {
        let b = app.buttons["nav.burger"].firstMatch
        if b.exists { return b }
        return app.descendants(matching: .any).matching(identifier: "nav.burger").firstMatch
    }

    /// Открыть меню бургера.
    @MainActor
    static func open(_ app: XCUIApplication) {
        let b = burger(app)
        if !b.waitForExistence(timeout: 3) || !b.isHittable {
            // iPhone: беседа поверх списка — первая кнопка панели («‹»).
            let back = app.navigationBars.buttons.firstMatch
            if back.exists { back.tap() }
        }
        XCTAssertTrue(b.waitForExistence(timeout: 10), "нет бургера в первой строке раздела")
        b.tap()
    }

    /// Пункт открытого меню: кнопка (iOS) или пункт меню (Мак). На Маке те же
    /// названия есть и в меню приложения — берём видимый.
    @MainActor
    static func item(_ name: String, _ app: XCUIApplication) -> XCUIElement {
        #if os(macOS)
        let items = app.menuItems.matching(NSPredicate(format: "title == %@ OR label == %@", name, name))
        let deadline = Date().addingTimeInterval(5)
        repeat {
            if let hit = items.allElementsBoundByIndex.first(where: { $0.isHittable }) { return hit }
            usleep(200_000)
        } while Date() < deadline
        return items.firstMatch
        #else
        return app.buttons.matching(NSPredicate(format: "label == %@", name)).firstMatch
        #endif
    }

    /// Перейти в раздел или открыть «Настройки» через бургер.
    @MainActor
    static func go(_ name: String, _ app: XCUIApplication) {
        open(app)
        let it = item(name, app)
        XCTAssertTrue(it.waitForExistence(timeout: 5), "в меню бургера нет «\(name)»")
        it.tap()
    }
}

#if os(macOS)
extension XCUIElement {
    func tap() { click() }
}
#endif

extension XCUIApplication {
    /// Приложение под тестом. На Маке — без восстановления окон прошлого
    /// сеанса (`-ApplePersistenceIgnoreState`): `-ddmail-reset` чистит данные,
    /// но не состояние окон, и главное окно, оставленное во весь экран,
    /// возвращалось во весь экран — окно беседы открывалось в своём Space, и
    /// тесты видели одно окно («окно беседы не открылось»).
    static func ddmail() -> XCUIApplication {
        let app = XCUIApplication()
        #if os(macOS)
        app.launchArguments += ["-ApplePersistenceIgnoreState", "YES"]
        #endif
        return app
    }
}

/// Видимый текст элемента. На iOS он в `label`; на Маке у `StaticText`
/// (объединённых строк SwiftUI и текста в WKWebView) `label` пустой, а текст —
/// в `value`. Тесты, сверявшие только `label`, на Маке не находили ни строк,
/// ни пузырей.
extension XCUIElement {
    var shownText: String {
        if !label.isEmpty { return label }
        return (value as? String) ?? ""
    }
}

/// Предикаты по видимому тексту — и `label` (iOS), и `value`/`title` (Мак).
enum Shown {
    static func contains(_ s: String) -> NSPredicate {
        NSPredicate(format: "label CONTAINS %@ OR value CONTAINS %@ OR title CONTAINS %@", s, s, s)
    }

    static func containsCI(_ s: String) -> NSPredicate {
        NSPredicate(format: "label CONTAINS[c] %@ OR value CONTAINS[c] %@ OR title CONTAINS[c] %@", s, s, s)
    }

    static func begins(_ s: String) -> NSPredicate {
        NSPredicate(format: "label BEGINSWITH %@ OR value BEGINSWITH %@ OR title BEGINSWITH %@", s, s, s)
    }

    static func equals(_ s: String) -> NSPredicate {
        NSPredicate(format: "label == %@ OR value == %@ OR title == %@", s, s, s)
    }

    /// Новая беседа на экране: подпись шапки (iOS) или строка `conv.new` над
    /// списком (Мак: шапка — в заголовке окна, подписи «Новая беседа» в дереве нет).
    @MainActor
    static func newConversation(_ app: XCUIApplication) -> XCUIElement {
        app.descendants(matching: .any)
            .matching(NSPredicate(format: "identifier == 'conv.new' OR label == 'Новая беседа' OR value == 'Новая беседа'"))
            .firstMatch
    }
}
