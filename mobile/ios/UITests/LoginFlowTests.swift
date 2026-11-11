import XCTest

/// Сквозной вход на настоящем сервере: адрес почты → автонастройка → пароль →
/// сессия проверена. Учётные данные — из окружения запуска
/// (`DDMAIL_TEST_EMAIL`, `DDMAIL_TEST_PASSWORD`); нет их — тест пропускается.
final class LoginFlowTests: XCTestCase {
    override func setUp() {
        continueAfterFailure = false
    }

    @MainActor
    func testDiscoverAndLogin() throws {
        let env = ProcessInfo.processInfo.environment
        guard let email = env["DDMAIL_TEST_EMAIL"], let password = env["DDMAIL_TEST_PASSWORD"] else {
            throw XCTSkip("нет DDMAIL_TEST_EMAIL / DDMAIL_TEST_PASSWORD")
        }
        let app = XCUIApplication()
        app.launchArguments += ["-ddmail-reset"]
        app.launch()

        let emailField = app.textFields["connect.email"]
        XCTAssertTrue(emailField.waitForExistence(timeout: 10))
        type(email, into: emailField, app)
        shot(app, "1-email")
        app.buttons["connect.find"].tap()

        // Сервер найден — подставлен логин, ждём только пароль.
        let passwordField = app.secureTextFields["connect.password"]
        XCTAssertTrue(passwordField.waitForExistence(timeout: 30), "сервер не найден")
        shot(app, "2-found")
        paste(password, into: passwordField, app)
        // Поле пароля скрывает текст, но длину видно по маске.
        XCTAssertEqual((passwordField.value as? String)?.count, password.count, "пароль напечатался не целиком")
        app.buttons["connect.login"].tap()

        // Вход прошёл, синк лёг в кэш — в списке появились беседы.
        let firstRow = app.cells.firstMatch
        XCTAssertTrue(firstRow.waitForExistence(timeout: 60), "список бесед пуст")
        shot(app, "3-list")
        firstRow.tap()
        // Лента — документ в WKWebView: ждём его и текст пузырей внутри.
        let web = app.webViews.firstMatch
        XCTAssertTrue(web.waitForExistence(timeout: 20), "лента беседы не появилась")
        XCTAssertTrue(web.staticTexts.firstMatch.waitForExistence(timeout: 30), "в ленте нет писем")
        shot(app, "4-opened")

        // Композер: пусто — отправлять нельзя; набрали — можно. Не отправляем:
        // это было бы настоящее письмо с адреса пользователя.
        let message = app.textViews["Сообщение"].exists ? app.textViews["Сообщение"] : app.textFields["Сообщение"]
        XCTAssertTrue(message.waitForExistence(timeout: 5), "нет строки ответа")
        let sendButton = app.buttons["Отправить"]
        XCTAssertFalse(sendButton.isEnabled)
        message.tap()
        message.typeText("Проверка")
        XCTAssertTrue(sendButton.isEnabled)
        shot(app, "5-composer")
    }

    /// Печать только после появления клавиатуры: иначе первые символы теряются.
    @MainActor
    private func type(_ text: String, into field: XCUIElement, _ app: XCUIApplication) {
        field.tap()
        XCTAssertTrue(app.keyboards.firstMatch.waitForExistence(timeout: 5), "клавиатура не появилась")
        // Защищённое поле теряет символы при быстрой печати (смена регистра,
        // переход на цифры) — печатаем по одному.
        for ch in text { field.typeText(String(ch)) }
    }

    /// Программная клавиатура симулятора теряет цифры и знаки в защищённом поле
    /// (iOS 27.2: из «Abcde12345!» доходит «Abcde»). Вставка из буфера обмена
    /// проходит через то же поле и ту же модель — проверка честная.
    @MainActor
    private func paste(_ text: String, into field: XCUIElement, _ app: XCUIApplication) {
        UIPasteboard.general.string = text
        field.tap()
        field.press(forDuration: 1.2)
        let item = app.menuItems.matching(NSPredicate(format: "label IN %@", ["Вставить", "Paste"])).firstMatch
        XCTAssertTrue(item.waitForExistence(timeout: 5), "нет пункта «Вставить»")
        item.tap()
        // iOS спрашивает разрешение на вставку из другого приложения.
        let allow = app.alerts.buttons.matching(NSPredicate(format: "label IN %@", ["Разрешить вставку", "Allow Paste"])).firstMatch
        if allow.waitForExistence(timeout: 2) { allow.tap() }
        UIPasteboard.general.string = ""
    }

    @MainActor
    private func shot(_ app: XCUIApplication, _ name: String) {
        let a = XCTAttachment(screenshot: app.screenshot())
        a.name = name
        a.lifetime = .keepAlways
        add(a)
    }
}
