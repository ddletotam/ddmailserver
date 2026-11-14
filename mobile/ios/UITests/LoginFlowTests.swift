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
        // Вход прошёл, синк лёг в кэш — в списке появились беседы. Вход с окна
        // подключения всегда открывает «Почту», какой бы раздел окно ни помнило
        // с прошлого запуска (@SceneStorage; на Маке он переживает перезапуск).
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
        sleep(2)
        shot(app, "3-list")

        // Раскладка как у десктопа (mobile-layout.md): первая строка — бургер и
        // поиск; «Спам» скрыт (таб — только по «Показать скрытые», §5 0.0.11);
        // ни «Написать», ни «Ещё», ни таб-бара.
        XCTAssertTrue(Nav.burger(app).exists, "нет бургера")
        let search = app.textFields["search.field"].firstMatch
        XCTAssertTrue(search.exists, "нет поля поиска")
        XCTAssertFalse(app.buttons.matching(NSPredicate(format: "identifier == 'folder.tab' AND label CONTAINS 'Спам'")).firstMatch.exists,
                       "«Спам» — видимый таб, а должен быть скрыт")
        XCTAssertFalse(app.buttons["Новое письмо"].exists, "кнопка «Новое письмо» осталась")
        XCTAssertFalse(app.buttons["Ещё"].exists, "меню «Ещё» осталось")
        #if os(iOS)
        XCTAssertFalse(app.tabBars.firstMatch.exists, "таб-бар остался")
        #endif
        // Бургер раскрыт — все разделы и «Настройки».
        Nav.open(app)
        for name in ["Почта", "Календарь", "Контакты", "Задачи", "Настройки"] {
            XCTAssertTrue(Nav.item(name, app).waitForExistence(timeout: 5), "в меню бургера нет «\(name)»")
        }
        shot(app, "3b-burger")
        Nav.item("Почта", app).tap()

        // Поиск: свой домен находит беседы (адрес получателя — мой).
        let domain = String(email.split(separator: "@").last ?? "")
        search.tap()
        search.typeText(domain)
        let convHit = app.descendants(matching: .any).matching(identifier: "search.conv").firstMatch
        XCTAssertTrue(convHit.waitForExistence(timeout: 10), "поиск не нашёл диалогов")
        sleep(2)
        shot(app, "3c-search")
        clear(search, app)

        // Новое письмо — только через поиск: адрес → «Написать по адресу…» →
        // пустая беседа с композером. Не отправляем.
        search.tap()
        search.typeText("proverka.adresa@example.org")
        let composeRow = app.descendants(matching: .any).matching(identifier: "search.compose").firstMatch
        XCTAssertTrue(composeRow.waitForExistence(timeout: 5), "нет строки «Написать по адресу…»")
        shot(app, "3d-compose-row")
        composeRow.tap()
        let newRow = app.descendants(matching: .any).matching(identifier: "conv.new").firstMatch
        let fresh = Shown.newConversation(app)
        XCTAssertTrue(fresh.waitForExistence(timeout: 10), "новая беседа не открылась")
        let draftField = app.textViews["Сообщение"].exists ? app.textViews["Сообщение"] : app.textFields["Сообщение"]
        XCTAssertTrue(draftField.waitForExistence(timeout: 5), "у новой беседы нет строки ввода")
        sleep(1)
        shot(app, "3e-new-conversation")
        _ = newRow

        // Обычная беседа.
        let back = app.navigationBars.buttons.firstMatch
        if !firstRow.isHittable, back.exists { back.tap() }
        XCTAssertTrue(firstRow.waitForExistence(timeout: 10))
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
        // Меню приложения: команды на месте; ⌘N — фокус в поиск.
        XCTAssertTrue(app.menuBars.menuItems["Новое письмо"].exists, "нет команды «Новое письмо»")
        XCTAssertTrue(app.menuBars.menuBarItems["Письмо"].exists, "нет меню «Письмо»")
        app.typeKey("n", modifierFlags: .command)
        app.typeText("x")
        XCTAssertEqual(search.value as? String, "x", "⌘N не поставил фокус в поиск")
        clear(search, app)
        #endif

        // Календарь: через бургер.
        Nav.go("Календарь", app)
        XCTAssertTrue(app.buttons["Сегодня"].waitForExistence(timeout: 10), "календарь не открылся")
        XCTAssertTrue(Nav.burger(app).exists, "в календаре нет бургера")
        sleep(4)
        shot(app, "6-calendar")

        Nav.go("Задачи", app)
        sleep(3)
        shot(app, "7-tasks")

        Nav.go("Контакты", app)
        // На Маке имя — часть объединённого элемента строки: ищем по любому типу.
        let contact = app.descendants(matching: .any)
            .matching(NSPredicate(format: "label CONTAINS 'Абай' OR value CONTAINS 'Абай' OR title CONTAINS 'Абай'")).firstMatch
        XCTAssertTrue(contact.waitForExistence(timeout: 20), "контакты не загрузились")
        XCTAssertTrue(app.textFields["contacts.search"].exists, "нет поля «Поиск контактов»")
        shot(app, "8-contacts")

        // Настройки — пункт бургера; «Добавить учётку» — в них.
        Nav.go("Настройки", app)
        XCTAssertTrue(app.descendants(matching: .any).matching(identifier: "account.add").firstMatch.waitForExistence(timeout: 10),
                      "в настройках нет «Добавить учётку»")
        shot(app, "9-settings")
    }

    /// Стереть поле поиска кнопкой «Очистить».
    @MainActor
    private func clear(_ field: XCUIElement, _ app: XCUIApplication) {
        let x = app.buttons["Очистить"].firstMatch
        if x.exists { x.tap() }
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

    @MainActor
    private func shot(_ app: XCUIApplication, _ name: String) {
        let a = XCTAttachment(screenshot: app.screenshot())
        a.name = name
        a.lifetime = .keepAlways
        add(a)
    }
}
