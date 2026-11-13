import XCTest

/// Сквозные сценарии отправки и папок (десктоп §2, §3а, §4) на настоящих
/// серверах. Письма уходят **только себе**: на `info@help-it-audit.ru` —
/// ящик тестовой учётки `lucky`; тема — «Проверка ddmail …». Учётки — из
/// окружения запуска; нет их — тест пропускается.
final class SendFlowTests: XCTestCase {
    /// Свой ящик тестовой учётки: сюда и только сюда.
    private let selfAddress = "info@help-it-audit.ru"

    override func setUp() {
        continueAfterFailure = false
    }

    /// ddmail: «Написать по адресу» → заглушка → «✓ Отправлено» → строка
    /// «Новая беседа» сменяется настоящей беседой, как только синк её принёс.
    @MainActor
    func testNewMailToSelfViaDdmail() throws {
        let env = ProcessInfo.processInfo.environment
        guard let email = env["DDMAIL_TEST_EMAIL"], let password = env["DDMAIL_TEST_PASSWORD"], !email.isEmpty else {
            throw XCTSkip("нет DDMAIL_TEST_EMAIL / DDMAIL_TEST_PASSWORD")
        }
        let app = UIFlow.launch()
        UIFlow.login(email, password, app, self)
        sendToSelf(app, tag: "ddmail")
    }

    /// Внешняя учётка (Mail.ru, IMAP/SMTP): то же — письмо уходит своим
    /// MIME-билдером, беседа приходит после копии в «Отправленных».
    @MainActor
    func testNewMailToSelfViaIMAP() throws {
        let env = ProcessInfo.processInfo.environment
        guard let email = env["DDMAIL_TEST_MAILRU_EMAIL"], let password = env["DDMAIL_TEST_MAILRU_PASSWORD"], !email.isEmpty else {
            throw XCTSkip("нет DDMAIL_TEST_MAILRU_EMAIL / DDMAIL_TEST_MAILRU_PASSWORD")
        }
        let app = UIFlow.launch()
        UIFlow.login(email, password, app, self, discoverTimeout: 90)
        sendToSelf(app, tag: "imap")
    }

    /// Папки без ddmail — локальный документ (на сервер ничего не уходит):
    /// «Настройки → Папки» → новая папка, имя → таб; меню таба; «Не
    /// показывать в списке» → таба нет; «Показать скрытые (1)» → таб снова
    /// виден; «Удалить».
    @MainActor
    func testFoldersLocal() throws {
        let env = ProcessInfo.processInfo.environment
        guard let email = env["DDMAIL_TEST_MAILRU_EMAIL"], let password = env["DDMAIL_TEST_MAILRU_PASSWORD"], !email.isEmpty else {
            throw XCTSkip("нет DDMAIL_TEST_MAILRU_EMAIL / DDMAIL_TEST_MAILRU_PASSWORD")
        }
        let app = UIFlow.launch()
        UIFlow.login(email, password, app, self, discoverTimeout: 90)
        let name = "Проверка ddmail"

        // 1. Настройки → Папки → «Новая папка» → имя.
        UIFlow.settle(app)
        Nav.go("Настройки", app)
        let foldersLink = app.descendants(matching: .any).matching(identifier: "settings.folders").firstMatch
        XCTAssertTrue(foldersLink.waitForExistence(timeout: 10), "в настройках нет «Папки»")
        foldersLink.tap()
        XCTAssertTrue(app.switches["folders.on"].firstMatch.waitForExistence(timeout: 5) || app.checkBoxes["folders.on"].firstMatch.exists,
                      "нет «Показывать папки»")
        let add = app.buttons["folders.add"].firstMatch
        XCTAssertTrue(add.waitForExistence(timeout: 5), "нет «Новая папка»")
        add.tap()
        let nameField = app.textFields["folder.name"].firstMatch
        XCTAssertTrue(nameField.waitForExistence(timeout: 10), "настройка новой папки не открылась")
        UIFlow.replace(nameField, with: name, app)
        sleep(2)
        UIFlow.shot(app, "folders-1-editor", self)
        // Назад к списку папок, затем закрыть настройки.
        app.navigationBars.buttons.firstMatch.tap()
        XCTAssertTrue(app.descendants(matching: .any).matching(NSPredicate(format: "identifier == 'folders.row' AND label CONTAINS %@", name))
            .firstMatch.waitForExistence(timeout: 10), "папки нет в списке настроек")
        UIFlow.shot(app, "folders-2-list", self)
        app.navigationBars.buttons.firstMatch.tap()
        let done = app.buttons["Готово"].firstMatch
        XCTAssertTrue(done.waitForExistence(timeout: 5), "нет «Готово» настроек")
        done.tap()

        // 2. Таб появился; меню таба — как у десктопа.
        let tab = app.buttons.matching(NSPredicate(format: "identifier == 'folder.tab' AND label CONTAINS %@", name)).firstMatch
        XCTAssertTrue(tab.waitForExistence(timeout: 10), "таба новой папки нет")
        UIFlow.contextMenu(tab)
        for item in ["Все прочитаны", "Настроить…", "Удалить", "Новая папка"] {
            XCTAssertTrue(UIFlow.menuItem(item, app).waitForExistence(timeout: 5), "в меню таба нет «\(item)»")
        }
        UIFlow.shot(app, "folders-3-tab-menu", self)
        UIFlow.menuItem("Настроить…", app).tap()

        // 3. «Не показывать в списке» → таба нет.
        let hidden = app.switches["folder.hidden"].firstMatch
        XCTAssertTrue(hidden.waitForExistence(timeout: 10), "настройка папки не открылась из меню")
        UIFlow.toggle(hidden)
        sleep(1)
        UIFlow.shot(app, "folders-4-hidden", self)
        app.buttons["Готово"].firstMatch.tap()
        XCTAssertTrue(UIFlow.waitGone(tab, 10), "скрытая папка осталась табом")

        // 4. Полоса осталась (папки есть, все скрыты) — «Показать скрытые (1)».
        let strip = app.buttons.matching(identifier: "folder.tab").firstMatch
        let area = strip.exists ? strip : app.otherElements["folder.strip"].firstMatch
        XCTAssertTrue(area.waitForExistence(timeout: 5), "полосы табов нет")
        UIFlow.contextMenu(area)
        let show = UIFlow.menuItem("Показать скрытые (1)", app)
        XCTAssertTrue(show.waitForExistence(timeout: 5), "нет «Показать скрытые (1)»")
        show.tap()
        XCTAssertTrue(tab.waitForExistence(timeout: 10), "скрытая папка не показалась")
        UIFlow.shot(app, "folders-5-shown", self)

        // 5. «Удалить» — подтверждение подменю.
        UIFlow.contextMenu(tab)
        UIFlow.menuItem("Удалить", app).tap()
        let confirm = app.descendants(matching: .any)
            .matching(NSPredicate(format: "(elementType == %d OR elementType == %d) AND label BEGINSWITH 'Удалить «'",
                                  XCUIElement.ElementType.button.rawValue, XCUIElement.ElementType.menuItem.rawValue)).firstMatch
        XCTAssertTrue(confirm.waitForExistence(timeout: 5), "нет подтверждения удаления")
        confirm.tap()
        XCTAssertTrue(UIFlow.waitGone(tab, 10), "папка не удалилась")
        UIFlow.shot(app, "folders-6-deleted", self)
    }

    // MARK: Сценарий отправки

    @MainActor
    private func sendToSelf(_ app: XCUIApplication, tag: String) {
        let stamp = Self.stamp()
        let subject = "Проверка ddmail \(stamp) \(tag)"
        let text = "Проверка ddmail \(tag) \(stamp)"
        UIFlow.settle(app)

        // Новое письмо — только через поиск: адрес → «Написать по адресу…».
        let search = app.textFields["search.field"].firstMatch
        XCTAssertTrue(search.waitForExistence(timeout: 10), "нет поля поиска")
        search.tap()
        search.typeText(selfAddress)
        let composeRow = app.descendants(matching: .any).matching(identifier: "search.compose").firstMatch
        XCTAssertTrue(composeRow.waitForExistence(timeout: 10), "нет строки «Написать по адресу…»")
        composeRow.tap()
        let fresh = app.staticTexts["Новая беседа"].firstMatch
        XCTAssertTrue(fresh.waitForExistence(timeout: 10), "новая беседа не открылась")

        // Тема — полем «Тема» (меню «+» → «Кому, копия, тема»).
        let plus = app.buttons["Вложить или поля письма"].firstMatch
        XCTAssertTrue(plus.waitForExistence(timeout: 5), "нет «+» композера")
        plus.tap()
        let fields = UIFlow.menuItem("Кому, копия, тема", app)
        XCTAssertTrue(fields.waitForExistence(timeout: 5), "нет пункта «Кому, копия, тема»")
        fields.tap()
        let subjectField = app.textFields["composer.subject"].firstMatch
        XCTAssertTrue(subjectField.waitForExistence(timeout: 5), "нет поля «Тема»")
        subjectField.tap()
        subjectField.typeText(subject)

        // Тело — жирным: выделить всё и ⌘B (на выделении, как системное меню
        // «Формат» на iPhone; стиль на каретке клавиатура симулятора теряет).
        let body = app.textViews["composer.text"].firstMatch
        XCTAssertTrue(body.waitForExistence(timeout: 5), "нет строки ввода")
        body.tap()
        body.typeText(text)
        sleep(1)
        #if os(iOS)
        // Выделить всё — меню выделения (⌘A клавиатура симулятора теряет).
        body.press(forDuration: 1.2)
        let selectAll = app.menuItems.matching(NSPredicate(format: "label IN %@", ["Выбрать все", "Select All"])).firstMatch
        if selectAll.waitForExistence(timeout: 3) { selectAll.tap() } else { body.typeKey("a", modifierFlags: .command) }
        sleep(1)
        // «Формат» → «Жирный» системного меню выделения — так делает владелец.
        // Пункт «Жирный» в панели «Форматирование» тест пока не находит
        // (подпись не выяснена) — тогда ⌘B; жирный проверяется по снимку.
        if !UIFlow.formatBold(app) { body.typeKey("b", modifierFlags: .command) }
        sleep(1)
        #else
        body.typeKey("a", modifierFlags: .command)
        sleep(1)
        body.typeKey("b", modifierFlags: .command)
        #endif
        sleep(1)
        UIFlow.shot(app, "\(tag)-1-composed", self)

        let send = app.buttons["Отправить"].firstMatch
        XCTAssertTrue(send.isEnabled, "нельзя отправить набранное")
        send.tap()

        // Мгновенная заглушка и «✓ Отправлено» над композером.
        let sent = app.descendants(matching: .any).matching(identifier: "compose.sent").firstMatch
        XCTAssertTrue(sent.waitForExistence(timeout: 30), "нет плашки «✓ Отправлено»")
        UIFlow.shot(app, "\(tag)-2-sent", self)

        // Беседа пришла синком — «Новая беседа» сменилась настоящей.
        XCTAssertTrue(UIFlow.waitGone(fresh, 180), "после отправки беседа не появилась")
        sleep(3)
        UIFlow.shot(app, "\(tag)-3-conversation", self)
        let web = app.webViews.firstMatch
        XCTAssertTrue(web.waitForExistence(timeout: 20), "лента беседы не появилась")
        // Настоящее письмо пришло — заглушка («Отправляется…») ушла.
        let mine = web.staticTexts.matching(NSPredicate(format: "label CONTAINS %@", stamp)).firstMatch
        XCTAssertTrue(mine.waitForExistence(timeout: 60), "в беседе нет отправленного письма")
        let pending = web.staticTexts["Отправляется…"].firstMatch
        XCTAssertTrue(UIFlow.waitGone(pending, 150), "письмо так и не пришло синком — висит заглушка")
        sleep(2)
        UIFlow.shot(app, "\(tag)-4-landed", self)
    }

    private static func stamp() -> String {
        let f = DateFormatter()
        f.dateFormat = "yyyyMMdd-HHmmss"
        return f.string(from: Date())
    }
}

/// Общие шаги сквозных тестов.
enum UIFlow {
    @MainActor
    static func launch() -> XCUIApplication {
        let app = XCUIApplication()
        app.launchArguments += ["-ddmail-reset", "-ddmail-uitest-ids"]
        app.launch()
        return app
    }

    /// Вход: адрес → автонастройка → пароль → список.
    @MainActor
    static func login(_ email: String, _ password: String, _ app: XCUIApplication, _ test: XCTestCase, discoverTimeout: TimeInterval = 30) {
        let emailField = app.textFields["connect.email"]
        XCTAssertTrue(emailField.waitForExistence(timeout: 10))
        type(email, into: emailField, app)
        app.buttons["connect.find"].tap()
        let pw = app.secureTextFields["connect.password"]
        XCTAssertTrue(pw.waitForExistence(timeout: discoverTimeout), "сервер не найден")
        enterPassword(password, into: pw, app)
        app.buttons["connect.login"].tap()
        let rows = app.descendants(matching: .any).matching(NSPredicate(format: "identifier BEGINSWITH 'conv.row'"))
        let deadline = Date().addingTimeInterval(120)
        while Date() < deadline && rows.count == 0 {
            settleOnce(app)
            sleep(1)
        }
        XCTAssertTrue(rows.count > 0, "список бесед пуст")
        settle(app)
    }

    /// Вопрос о загрузке заранее и системный запрос уведомлений.
    @MainActor
    static func settle(_ app: XCUIApplication) {
        for _ in 0..<5 {
            settleOnce(app)
            sleep(1)
        }
    }

    @MainActor
    private static func settleOnce(_ app: XCUIApplication) {
        #if os(iOS)
        let springboard = XCUIApplication(bundleIdentifier: "com.apple.springboard")
        let allow = springboard.alerts.buttons.matching(NSPredicate(format: "label IN %@", ["Разрешить", "Allow"])).firstMatch
        if allow.exists { allow.tap() }
        #endif
        let wifi = app.buttons["Только по Wi-Fi"].firstMatch
        if wifi.exists { wifi.tap() }
    }

    @MainActor
    static func type(_ text: String, into field: XCUIElement, _ app: XCUIApplication) {
        #if os(iOS)
        field.tap()
        XCTAssertTrue(app.keyboards.firstMatch.waitForExistence(timeout: 5), "клавиатура не появилась")
        for ch in text { field.typeText(String(ch)) }
        #else
        field.click()
        field.typeText(text)
        #endif
    }

    @MainActor
    static func replace(_ field: XCUIElement, with text: String, _ app: XCUIApplication) {
        XCTAssertTrue(field.waitForExistence(timeout: 5))
        field.tap()
        let current = (field.value as? String) ?? ""
        #if os(iOS)
        if !current.isEmpty {
            field.coordinate(withNormalizedOffset: CGVector(dx: 0.97, dy: 0.5)).tap()
            field.typeText(String(repeating: XCUIKeyboardKey.delete.rawValue, count: current.count + 2))
        }
        #else
        field.typeKey("a", modifierFlags: .command)
        #endif
        field.typeText(text)
    }

    @MainActor
    static func enterPassword(_ password: String, into field: XCUIElement, _ app: XCUIApplication) {
        #if os(iOS)
        UIPasteboard.general.string = password
        field.tap()
        field.press(forDuration: 1.2)
        let item = app.menuItems.matching(NSPredicate(format: "label IN %@", ["Вставить", "Paste"])).firstMatch
        XCTAssertTrue(item.waitForExistence(timeout: 5), "нет пункта «Вставить»")
        item.tap()
        let allow = app.alerts.buttons.matching(NSPredicate(format: "label IN %@", ["Разрешить вставку", "Allow Paste"])).firstMatch
        if allow.waitForExistence(timeout: 2) { allow.tap() }
        UIPasteboard.general.string = ""
        #else
        field.click()
        app.typeKey(.escape, modifierFlags: [])
        field.click()
        field.typeText(password)
        #endif
    }

    #if os(iOS)
    /// Меню выделения: листать стрелкой до «Формат», в нём — «Жирный».
    @MainActor
    static func formatBold(_ app: XCUIApplication) -> Bool {
        // Пункты меню выделения бывают и `menuItem`, и `button` — ищем по подписи.
        let format = app.descendants(matching: .any).matching(NSPredicate(format: "label IN %@", ["Форматирование", "Формат", "Format"])).firstMatch
        let bold = app.descendants(matching: .any).matching(NSPredicate(format: "label IN %@", ["Жирный", "Bold", "Полужирный"])).firstMatch
        for _ in 0..<4 {
            if bold.exists { bold.tap(); return true }
            if format.exists {
                format.tap()
                sleep(2)
                continue
            }
            // Стрелка «дальше» — последняя кнопка меню выделения.
            let next = app.buttons.matching(NSPredicate(format: "label IN %@", ["Следующая страница", "Далее", "Ещё", "Forward", "Next page", "Show More Items"])).firstMatch
            guard next.exists else { return false }
            next.tap()
            sleep(1)
        }
        return bold.exists
    }
    #endif

    /// Долгое нажатие (iPhone/iPad) или правый клик (Мак).
    @MainActor
    static func contextMenu(_ e: XCUIElement) {
        #if os(iOS)
        e.press(forDuration: 1.2)
        #else
        e.rightClick()
        #endif
    }

    /// Пункт открытого меню: кнопка (iOS) или пункт меню (Мак).
    @MainActor
    static func menuItem(_ name: String, _ app: XCUIApplication) -> XCUIElement {
        #if os(macOS)
        return app.menuItems.matching(NSPredicate(format: "title == %@ OR label == %@", name, name)).firstMatch
        #else
        return app.buttons.matching(NSPredicate(format: "label == %@", name)).firstMatch
        #endif
    }

    /// Переключатель: на iOS — по самому выключателю (касание строки его не трогает).
    @MainActor
    static func toggle(_ e: XCUIElement) {
        #if os(iOS)
        let knob = e.switches.firstMatch
        (knob.exists ? knob : e).tap()
        #else
        e.click()
        #endif
    }

    @MainActor
    static func waitGone(_ e: XCUIElement, _ timeout: TimeInterval) -> Bool {
        let deadline = Date().addingTimeInterval(timeout)
        while Date() < deadline {
            if !e.exists { return true }
            sleep(1)
        }
        return !e.exists
    }

    @MainActor
    static func shot(_ app: XCUIApplication, _ name: String, _ test: XCTestCase) {
        let a = XCTAttachment(screenshot: app.screenshot())
        a.name = name
        a.lifetime = .keepAlways
        test.add(a)
    }
}
