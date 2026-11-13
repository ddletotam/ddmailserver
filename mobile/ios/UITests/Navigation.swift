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
