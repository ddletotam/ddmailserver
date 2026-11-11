import XCTest

/// Сквозной вход на настоящем сервере: адрес почты → автонастройка → пароль →
/// сессия проверена. Учётные данные — из окружения запуска
/// (`DDMAIL_TEST_EMAIL`, `DDMAIL_TEST_PASSWORD`); нет их — тест пропускается.
final class LoginFlowTests: XCTestCase {
    // На Маке у XCUIElement нет tap() — click() то же самое.
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
        #if os(iOS)
        paste(password, into: passwordField, app)
        // Поле пароля скрывает текст, но длину видно по маске.
        XCTAssertEqual((passwordField.value as? String)?.count, password.count, "пароль напечатался не целиком")
        #else
        // На Маке клавиатура аппаратная; подсказку «Пароли…» (автозаполнение
        // связки ключей) закрываем Esc, иначе она забирает ввод.
        passwordField.click()
        app.typeKey(.escape, modifierFlags: [])
        passwordField.click()
        passwordField.typeText(password)
        #endif
        app.buttons["connect.login"].tap()
        // Раздел восстанавливается из прошлого запуска (@SceneStorage). Если после
        // входа не видно ни списка, ни вопроса о загрузке — идём в «Почту». Нажать
        // таб поверх алерта нельзя: XCTest сам «закроет помеху» последней кнопкой.
        let anyRow = app.descendants(matching: .any).matching(identifier: "conv.row").firstMatch
        let question = app.buttons["Только по Wi-Fi"].firstMatch
        let deadline = Date().addingTimeInterval(30)
        while Date() < deadline && !anyRow.exists && !question.exists { sleep(1) }
        if !anyRow.exists && !question.exists { section("Почта", app).tap() }

        // Вход прошёл, синк лёг в кэш — в списке появились беседы.
        // Строка беседы — по идентификатору: на Маке первая «ячейка» — раздел в боковой панели.
        let firstRow = app.descendants(matching: .any).matching(identifier: "conv.row").firstMatch
        XCTAssertTrue(firstRow.waitForExistence(timeout: 60), "список бесед пуст")
        shot(app, "3-list-raw")
        // Системный запрос уведомлений — разрешить.
        #if os(iOS)
        let springboard = XCUIApplication(bundleIdentifier: "com.apple.springboard")
        let allow = springboard.alerts.buttons.matching(NSPredicate(format: "label IN %@", ["Разрешить", "Allow"])).firstMatch
        if allow.waitForExistence(timeout: 10) { allow.tap() }
        #endif
        // Один вопрос о загрузке заранее (§4): ответа по умолчанию нет.
        // На iPhone это alert, на Маке — диалог окна: ищем по кнопке.
        let wifiOnly = app.buttons["Только по Wi-Fi"].firstMatch
        XCTAssertTrue(wifiOnly.waitForExistence(timeout: 10), "нет вопроса о загрузке заранее")
        shot(app, "3a-prefetch")
        wifiOnly.tap()
        shot(app, "3-list")
        firstRow.tap()
        sleep(3)
        shot(app, "4a-tapped")
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

        #if os(macOS)
        // Мак: беседа в отдельном окне — из контекстного меню.
        firstRow.rightClick()
        let openWin = app.menuItems["Открыть в новом окне"]
        XCTAssertTrue(openWin.waitForExistence(timeout: 5), "нет пункта «Открыть в новом окне»")
        openWin.click()
        let second = app.windows.element(boundBy: 1)
        XCTAssertTrue(second.waitForExistence(timeout: 10), "окно беседы не открылось")
        sleep(3)
        shot(app, "5b-window")
        app.typeKey("w", modifierFlags: .command)
        // Меню приложения: команды на месте.
        XCTAssertTrue(app.menuBars.menuItems["Новое письмо"].exists, "нет команды «Новое письмо»")
        XCTAssertTrue(app.menuBars.menuBarItems["Письмо"].exists, "нет меню «Письмо»")
        #endif

        // Календарь: лента месяцев с событиями с сервера.
        // В беседе на iPhone таб-бар скрыт (§6): назад к списку — первой кнопкой
        // панели. На iPad и Маке разделы видны всегда.
        if !section("Календарь", app).exists, app.navigationBars.buttons.count > 0 {
            app.navigationBars.buttons.element(boundBy: 0).tap()
        }
        let calTab = section("Календарь", app)
        XCTAssertTrue(calTab.waitForExistence(timeout: 10), "нет раздела «Календарь»")
        calTab.tap()
        XCTAssertTrue(app.buttons["Сегодня"].waitForExistence(timeout: 10), "календарь не открылся")
        sleep(4)
        shot(app, "6-calendar")

        section("Задачи", app).tap()
        sleep(3)
        shot(app, "7-tasks")

        section("Контакты", app).tap()
        // На Маке имя — часть объединённого элемента строки: ищем по любому типу.
        let contact = app.descendants(matching: .any)
            .matching(NSPredicate(format: "label CONTAINS 'Абай' OR value CONTAINS 'Абай' OR title CONTAINS 'Абай'")).firstMatch
        XCTAssertTrue(contact.waitForExistence(timeout: 20), "контакты не загрузились")
        shot(app, "8-contacts")
    }

    /// Печать только после появления клавиатуры: иначе первые символы теряются.
    @MainActor
    private func type(_ text: String, into field: XCUIElement, _ app: XCUIApplication) {
        #if os(iOS)
        field.tap()
        XCTAssertTrue(app.keyboards.firstMatch.waitForExistence(timeout: 5), "клавиатура не появилась")
        for ch in text { field.typeText(String(ch)) }
        #else
        field.click()
        field.typeText(text)
        #endif
    }

#if os(iOS)
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

#endif

    /// Раздел: кнопка таб-бара (iPhone) или строка боковой панели (iPad, Мак).
    @MainActor
    private func section(_ name: String, _ app: XCUIApplication) -> XCUIElement {
        let button = app.tabBars.buttons[name].firstMatch
        if button.exists { return button }
        let any = app.buttons[name].firstMatch
        if any.exists { return any }
        // Мак: строка боковой панели — ячейка со статическим текстом.
        let text = app.staticTexts[name].firstMatch
        if text.exists { return text }
        return app.cells.containing(.staticText, identifier: name).firstMatch
    }

    @MainActor
    private func shot(_ app: XCUIApplication, _ name: String) {
        let a = XCTAttachment(screenshot: app.screenshot())
        a.name = name
        a.lifetime = .keepAlways
        add(a)
    }
}

#if os(macOS)
extension XCUIElement {
    func tap() { click() }
}
#endif
