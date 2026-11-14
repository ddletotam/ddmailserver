import XCTest

/// Статус доставки исходящих (контракт §5) на настоящем сервере, **без
/// отправки писем**: у тестовой учётки уже есть письмо на мёртвый домен
/// (`logdoc.org`, статус `failed`, к нему пришёл отказ о недоставке). Строка
/// беседы — с красным значком «Не доставлено», меню пузыря — с причиной.
final class DeliveryStatusTests: XCTestCase {
    /// Адресат письма, которое сервер не смог доставить.
    private let deadDomain = "logdoc.org"

    override func setUp() {
        continueAfterFailure = false
    }

    @MainActor
    func testFailedDeliveryIsShown() throws {
        let env = ProcessInfo.processInfo.environment
        guard let email = env["DDMAIL_TEST_EMAIL"], let password = env["DDMAIL_TEST_PASSWORD"], !email.isEmpty else {
            throw XCTSkip("нет DDMAIL_TEST_EMAIL / DDMAIL_TEST_PASSWORD")
        }
        let app = UIFlow.launch()
        UIFlow.login(email, password, app, self)
        UIFlow.settle(app)

        // Беседа с мёртвым доменом — поиском по «Диалогам»: переход в её
        // папку и прокрутка к строке.
        let search = app.textFields["search.field"].firstMatch
        XCTAssertTrue(search.waitForExistence(timeout: 10), "нет поля поиска")
        search.tap()
        search.typeText(deadDomain)
        let hit = app.descendants(matching: .any).matching(identifier: "search.conv").firstMatch
        XCTAssertTrue(hit.waitForExistence(timeout: 15), "поиск не нашёл беседу с \(deadDomain)")
        UIFlow.shot(app, "delivery-1-search", self)
        hit.tap()
        sleep(3)

        // Лента беседы и меню пузыря: «Не доставлено» с причиной.
        let web = app.webViews.firstMatch
        XCTAssertTrue(web.waitForExistence(timeout: 20), "лента беседы не появилась")
        sleep(3)
        UIFlow.shot(app, "delivery-2-conversation", self)
        let bubble = web.staticTexts.allElementsBoundByIndex.last { $0.isHittable && !$0.shownText.isEmpty }
        // Меню пузыря открывает `contextmenu` документа; долгое нажатие
        // XCUITest его вызывает не везде — тогда проверка меню пропускается.
        if let bubble {
            UIFlow.contextMenu(bubble)
            let copy = UIFlow.menuItem("Копировать текст", app)
            if copy.waitForExistence(timeout: 5) {
                let menuText = app.descendants(matching: .any).matching(Shown.begins("Не доставлено")).firstMatch
                XCTAssertTrue(menuText.waitForExistence(timeout: 5), "в меню пузыря нет «Не доставлено»")
            }
            UIFlow.shot(app, "delivery-3-bubble-menu", self)
            // Контекстное меню закрывается касанием мимо (Мак — Esc).
            #if os(iOS)
            app.navigationBars.firstMatch.coordinate(withNormalizedOffset: CGVector(dx: 0.5, dy: 0.5)).tap()
            #else
            app.typeKey(.escape, modifierFlags: [])
            #endif
            sleep(1)
        }

        // Строка беседы: значок «Не доставлено» у времени. На iPhone беседа
        // поверх списка — назад к нему.
        let row = app.descendants(matching: .any)
            .matching(NSPredicate(format: "identifier ENDSWITH %@", "|loko@\(deadDomain)")).firstMatch
        // «Назад» панели (первой кнопкой бывает не она); первый тап после
        // закрытия меню пузыря может уйти в его анимацию — до трёх попыток.
        let back = app.navigationBars.buttons.matching(NSPredicate(format: "identifier == 'BackButton' OR label == 'Назад'")).firstMatch
        for _ in 0..<3 where !(row.waitForExistence(timeout: 3) && row.isHittable) {
            if back.exists { back.tap() } else if app.navigationBars.buttons.firstMatch.exists { app.navigationBars.buttons.firstMatch.tap() }
            sleep(1)
        }
        XCTAssertTrue(row.waitForExistence(timeout: 10), "нет строки беседы с \(deadDomain)")
        XCTAssertTrue(row.shownText.contains("Не доставлено"), "у строки нет отметки «Не доставлено»: \(row.shownText)")
        UIFlow.shot(app, "delivery-4-row", self)
    }
}
