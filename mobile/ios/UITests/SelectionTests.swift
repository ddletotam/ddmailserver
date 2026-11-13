import XCTest

/// Выделение текста письма (контракт §8): двойной тап — слово, тройной —
/// фраза, у выделения системные маркеры и «Скопировать»; долгое нажатие —
/// по-прежнему меню письма. На настоящем сервере, **без отправки писем**:
/// текст — отказ о недоставке, который у тестовой учётки уже есть.
final class SelectionTests: XCTestCase {
    /// Правый маркер выделения слова «отвечает» на iPhone 18 Pro (точки
    /// экрана) — по снимку `select-1-word`.
    static let endHandle = CGPoint(x: 86, y: 305)

    override func setUp() {
        continueAfterFailure = false
    }

    #if os(iOS)
    @MainActor
    func testDoubleAndTripleTapSelect() throws {
        let env = ProcessInfo.processInfo.environment
        guard let email = env["DDMAIL_TEST_EMAIL"], let password = env["DDMAIL_TEST_PASSWORD"], !email.isEmpty else {
            throw XCTSkip("нет DDMAIL_TEST_EMAIL / DDMAIL_TEST_PASSWORD")
        }
        let app = UIFlow.launch()
        UIFlow.login(email, password, app, self)
        UIFlow.settle(app)

        // Беседа с отказом о недоставке — поиском.
        let search = app.textFields["search.field"].firstMatch
        XCTAssertTrue(search.waitForExistence(timeout: 10), "нет поля поиска")
        search.tap()
        search.typeText("mailer-daemon")
        let hit = app.descendants(matching: .any).matching(identifier: "search.conv").firstMatch
        XCTAssertTrue(hit.waitForExistence(timeout: 15), "поиск не нашёл отказ о недоставке")
        hit.tap()
        let web = app.webViews.firstMatch
        XCTAssertTrue(web.waitForExistence(timeout: 20), "лента беседы не появилась")
        let text = web.staticTexts.matching(NSPredicate(format: "label CONTAINS %@", "доставить")).firstMatch
        XCTAssertTrue(text.waitForExistence(timeout: 20), "нет текста письма")
        sleep(2)
        UIFlow.shot(app, "select-0-open", self)

        // Двойной тап — слово, маркеры и меню с «Скопировать».
        let copy = app.descendants(matching: .any)
            .matching(NSPredicate(format: "label IN %@", ["Скопировать", "Копировать", "Copy"])).firstMatch
        let grabber = app.descendants(matching: .any)
            .matching(NSPredicate(format: "identifier BEGINSWITH %@", "com.apple.text.grabber")).firstMatch
        let word = text.coordinate(withNormalizedOffset: CGVector(dx: 0.08, dy: 0.25))
        word.doubleTap()
        sleep(1)
        UIFlow.shot(app, "select-1-word", self)
        // Маркеры в дереве доступности есть не всегда — они на снимке.
        _ = grabber.waitForExistence(timeout: 2)
        XCTAssertTrue(copy.waitForExistence(timeout: 5), "у выделения слова нет «Скопировать»")

        copy.tap()
        sleep(1)
        let copied = pasted(app)
        XCTAssertFalse(copied.isEmpty, "«Скопировать» ничего не скопировало")
        XCTAssertFalse(copied.contains(" "), "двойной тап выделил не одно слово: «\(copied)»")

        // Маркер — системный: взять его — лупа и системное меню выделения
        // («Скопировать / Найти / Перевести»). Протяжку синтетическим жестом
        // XCUITest WebKit отрабатывает лишь на символ, поэтому здесь
        // проверяется только, что маркер живой и после него копируется.
        // Точка маркера — по снимку iPhone; на iPad шаг пропускается.
        if UIDevice.current.userInterfaceIdiom == .phone {
            word.doubleTap()
            sleep(1)
            XCTAssertTrue(copy.waitForExistence(timeout: 5), "у выделения слова нет «Скопировать»")
            let origin = app.coordinate(withNormalizedOffset: .zero)
            let handle = origin.withOffset(CGVector(dx: SelectionTests.endHandle.x, dy: SelectionTests.endHandle.y))
            handle.press(forDuration: 0.4, thenDragTo: origin.withOffset(CGVector(dx: 250, dy: 352)),
                         withVelocity: .slow, thenHoldForDuration: 0.5)
            sleep(1)
            UIFlow.shot(app, "select-1b-handle", self)
            XCTAssertTrue(copy.waitForExistence(timeout: 3), "после маркера нет «Скопировать»")
            copy.tap()
            sleep(1)
            XCTAssertTrue(pasted(app).hasPrefix(copied), "после маркера скопировалось не выделение")
        }

        // Тройной тап — фраза.
        text.tap(withNumberOfTaps: 3, numberOfTouches: 1)
        sleep(1)
        UIFlow.shot(app, "select-2-sentence", self)
        XCTAssertTrue(copy.waitForExistence(timeout: 5), "у выделения фразы нет «Скопировать»")
        copy.tap()
        sleep(1)
        let phrase = pasted(app)
        XCTAssertTrue(phrase.contains(" "), "тройной тап выделил не фразу: «\(phrase)»")

        // Одиночный тап снимает выделение, долгое нажатие — меню письма.
        text.tap()
        sleep(1)
        UIFlow.contextMenu(text)
        let reply = UIFlow.menuItem("Ответить", app)
        XCTAssertTrue(reply.waitForExistence(timeout: 5), "долгое нажатие не открыло меню письма")
        sleep(1)
        UIFlow.shot(app, "select-3-bubble-menu", self)
        // Системное выделение с меню «Найти / Перевести» поверх меню письма не начинается.
        let system = app.descendants(matching: .any)
            .matching(NSPredicate(format: "label IN %@", ["Найти", "Перевести", "Translate", "Look Up"])).firstMatch
        XCTAssertFalse(system.exists, "долгое нажатие начало и системное выделение")
    }

    /// Что лежит в буфере обмена — вставкой в строку ввода (так, как сделал
    /// бы человек): буфер приложения тест читать не может. Строка ввода
    /// после этого очищается.
    @MainActor
    private func pasted(_ app: XCUIApplication) -> String {
        let field = app.textViews["composer.text"].firstMatch
        XCTAssertTrue(field.waitForExistence(timeout: 5), "нет строки ввода")
        field.tap()
        field.press(forDuration: 1.2)
        let paste = app.menuItems.matching(NSPredicate(format: "label IN %@", ["Вставить", "Paste"])).firstMatch
        guard paste.waitForExistence(timeout: 5) else { return "" }
        paste.tap()
        let allow = app.alerts.buttons.matching(NSPredicate(format: "label IN %@", ["Разрешить вставку", "Allow Paste"])).firstMatch
        if allow.waitForExistence(timeout: 2) { allow.tap() }
        sleep(1)
        let text = (field.value as? String ?? "").trimmingCharacters(in: .whitespacesAndNewlines)
        // Очистить: выделить всё и удалить.
        field.press(forDuration: 1.2)
        let all = app.menuItems.matching(NSPredicate(format: "label IN %@", ["Выбрать все", "Select All"])).firstMatch
        if all.waitForExistence(timeout: 3) {
            all.tap()
            field.typeText(XCUIKeyboardKey.delete.rawValue)
        }
        // Клавиатура закрывает ленту — убрать её.
        app.navigationBars.firstMatch.coordinate(withNormalizedOffset: CGVector(dx: 0.5, dy: 0.5)).tap()
        sleep(1)
        return text
    }
    #endif
}
