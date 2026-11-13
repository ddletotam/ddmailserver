import XCTest

/// Список бесед, выбор и команды (контракт §5, §6, §9) на настоящем сервере,
/// **без отправки и без удаления писем**: диалоги удаления и «В спам»
/// только открываются и закрываются Esc / «Отмена».
final class MailListTests: XCTestCase {
    override func setUp() {
        continueAfterFailure = false
    }

    /// Строки бесед в порядке списка.
    @MainActor
    private func rows(_ app: XCUIApplication) -> XCUIElementQuery {
        app.descendants(matching: .any).matching(NSPredicate(format: "identifier BEGINSWITH 'conv.row'"))
    }

    @MainActor
    private func start() throws -> XCUIApplication {
        let env = ProcessInfo.processInfo.environment
        guard let email = env["DDMAIL_TEST_EMAIL"], let password = env["DDMAIL_TEST_PASSWORD"], !email.isEmpty else {
            throw XCTSkip("нет DDMAIL_TEST_EMAIL / DDMAIL_TEST_PASSWORD")
        }
        let app = UIFlow.launch()
        UIFlow.login(email, password, app, self)
        UIFlow.settle(app)
        XCTAssertTrue(rows(app).element(boundBy: 2).waitForExistence(timeout: 20), "в списке меньше трёх бесед")
        return app
    }

    /// Отметить / снять строку в режиме выбора: касание (клик).
    @MainActor
    private func togglePick(_ row: XCUIElement) {
        row.tap()
    }

    /// B-01, S-16, M-17, S-07: «Выбрать» оставляет список на экране (iPhone —
    /// не уводит на пустую беседу), открытая беседа справа остаётся (iPad,
    /// Мак), снятие последней отметки выходит из выбора.
    @MainActor
    func testPickingStaysOnList() throws {
        let app = try start()
        let list = rows(app)
        let wide = app.windows.firstMatch.frame.width > 700
        if wide {
            // Открытая беседа — до выбора.
            list.element(boundBy: 0).tap()
            XCTAssertTrue(app.webViews.firstMatch.waitForExistence(timeout: 20), "беседа не открылась")
        }
        UIFlow.contextMenu(list.element(boundBy: 1))
        let pick = UIFlow.menuItem("Выбрать", app)
        XCTAssertTrue(pick.waitForExistence(timeout: 5), "в меню строки нет «Выбрать»")
        pick.tap()

        let count = app.staticTexts["picking.count"].firstMatch
        XCTAssertTrue(count.waitForExistence(timeout: 5), "нет строки «Выбрано: N»")
        XCTAssertEqual(count.label, "Выбрано: 1")
        XCTAssertFalse(app.staticTexts["Беседа не выбрана"].exists, "выбор увёл на «Беседа не выбрана»")
        if wide {
            XCTAssertTrue(app.webViews.firstMatch.exists, "выбор закрыл открытую беседу")
        }
        sleep(1)
        UIFlow.shot(app, "pick-1-started", self)

        togglePick(list.element(boundBy: 2))
        XCTAssertTrue(waitLabel(count, "Выбрано: 2"), "вторая отметка не встала: «\(count.label)»")
        XCTAssertTrue(count.isHittable, "список выбора ушёл с экрана")
        sleep(1)
        UIFlow.shot(app, "pick-2-two", self)

        // Снять обе — выбор закончен сам, на месте — строка поиска.
        togglePick(list.element(boundBy: 2))
        XCTAssertTrue(waitLabel(count, "Выбрано: 1"), "отметка не снялась: «\(count.label)»")
        togglePick(list.element(boundBy: 1))
        XCTAssertTrue(UIFlow.waitGone(count, 5), "снятие последней отметки не вышло из выбора")
        let search = app.textFields["search.field"].firstMatch
        XCTAssertTrue(search.waitForExistence(timeout: 5) && search.isHittable, "после выбора не видно списка")
        if wide {
            XCTAssertTrue(app.webViews.firstMatch.exists, "после выбора открытая беседа закрылась")
        }
        UIFlow.shot(app, "pick-3-done", self)
    }

    /// B-02: ⌘⌫ в поле ввода стирает текст до начала строки и не зовёт
    /// «Удалить беседу»; вне поля (Мак, фокус в списке) — зовёт, с подтверждением.
    @MainActor
    func testCommandBackspaceInTextField() throws {
        #if os(iOS)
        // Симулятор без подключённой аппаратной клавиатуры клавиши `typeKey`
        // полю не доставляет вовсе (даже ⌫ без модификаторов) — проверить
        // нечем; на iPad системное поведение поля и так впереди сочетаний меню.
        throw XCTSkip("⌘⌫ проверяется на Маке")
        #else
        let app = try start()
        rows(app).element(boundBy: 0).tap()
        XCTAssertTrue(app.webViews.firstMatch.waitForExistence(timeout: 20), "беседа не открылась")
        let dialog = app.descendants(matching: .any)
            .matching(NSPredicate(format: "label BEGINSWITH %@", "Удалить бесед")).firstMatch

        let composer = app.textViews["composer.text"].firstMatch
        XCTAssertTrue(composer.waitForExistence(timeout: 10), "нет строки ввода")
        composer.tap()
        composer.typeText("Проверка стирания строки")
        sleep(1)
        composer.typeKey(.delete, modifierFlags: .command)
        sleep(1)
        UIFlow.shot(app, "cmd-del-1-composer", self)
        XCTAssertFalse(dialog.exists, "⌘⌫ в строке ввода позвал удаление беседы")
        let left = (composer.value as? String) ?? ""
        XCTAssertFalse(left.contains("Проверка"), "⌘⌫ не стёр строку ввода: «\(left)»")

        let search = app.textFields["search.field"].firstMatch
        XCTAssertTrue(search.waitForExistence(timeout: 5), "нет поля поиска")
        search.tap()
        search.typeText("zzqx")
        sleep(1)
        search.typeKey(.delete, modifierFlags: .command)
        sleep(1)
        UIFlow.shot(app, "cmd-del-2-search", self)
        XCTAssertFalse(dialog.exists, "⌘⌫ в поле поиска позвал удаление беседы")
        let query = (search.value as? String) ?? ""
        XCTAssertFalse(query.contains("zzqx"), "⌘⌫ не стёр поле поиска: «\(query)»")

        // Фокус в списке — команда меню работает, но только с подтверждением.
        app.typeKey(.escape, modifierFlags: [])
        rows(app).element(boundBy: 0).click()
        sleep(1)
        app.typeKey(.delete, modifierFlags: .command)
        XCTAssertTrue(dialog.waitForExistence(timeout: 5), "⌘⌫ вне поля ввода не позвал удаление беседы")
        UIFlow.shot(app, "cmd-del-3-list", self)
        app.typeKey(.escape, modifierFlags: [])
        XCTAssertTrue(UIFlow.waitGone(dialog, 5), "диалог удаления не закрылся по Esc")
        #endif
    }

    #if os(macOS)
    /// S-02, S-03, S-08, M-12: на Маке нет кнопки боковой панели; на старте
    /// открыта беседа, на которой закончили; окно беседы — без второго
    /// заголовка, «В спам» спрашивает подтверждение, меню «Письмо» действует.
    @MainActor
    func testMacWindowAndRestore() throws {
        var app = try start()
        let first = rows(app).element(boundBy: 0)
        first.click()
        XCTAssertTrue(app.webViews.firstMatch.waitForExistence(timeout: 20), "беседа не открылась")
        sleep(2)
        UIFlow.shot(app, "mac-1-open", self)
        let toggle = app.buttons.matching(NSPredicate(format: "identifier CONTAINS[c] 'sidebar' OR label CONTAINS[c] 'sidebar' OR label CONTAINS[c] 'боков'")).firstMatch
        XCTAssertFalse(toggle.exists && toggle.isHittable, "в окне есть кнопка боковой панели")

        // Перезапуск без сброса — беседа открыта сразу.
        app.terminate()
        app = XCUIApplication()
        app.launchArguments += ["-ddmail-uitest-ids"]
        app.launch()
        XCTAssertTrue(app.webViews.firstMatch.waitForExistence(timeout: 30), "на старте не открылась последняя беседа")
        XCTAssertFalse(app.staticTexts["Беседа не выбрана"].exists)
        sleep(2)
        UIFlow.shot(app, "mac-2-restored", self)

        // Окно беседы.
        let row = rows(app).element(boundBy: 0)
        XCTAssertTrue(row.waitForExistence(timeout: 20))
        UIFlow.contextMenu(row)
        let newWindow = UIFlow.menuItem("Открыть в новом окне", app)
        XCTAssertTrue(newWindow.waitForExistence(timeout: 5), "нет «Открыть в новом окне»")
        newWindow.click()
        let windows = app.windows
        let deadline = Date().addingTimeInterval(10)
        while windows.count < 2 && Date() < deadline { usleep(300_000) }
        XCTAssertGreaterThanOrEqual(windows.count, 2, "окно беседы не открылось")
        sleep(3)
        UIFlow.shot(app, "mac-3-window", self)

        let window = windows.element(boundBy: 0)
        let spam = window.buttons["В спам"].firstMatch
        if spam.waitForExistence(timeout: 5) {
            spam.click()
            let ask = app.staticTexts["В спам?"].firstMatch
            XCTAssertTrue(ask.waitForExistence(timeout: 5), "«В спам» в окне беседы ничего не спросил")
            UIFlow.shot(app, "mac-4-window-spam", self)
            app.typeKey(.escape, modifierFlags: [])
            XCTAssertTrue(UIFlow.waitGone(ask, 5))
        }

        app.menuBars.menuBarItems["Письмо"].click()
        let delete = app.menuBars.menuItems["Удалить беседу"].firstMatch
        XCTAssertTrue(delete.waitForExistence(timeout: 5))
        sleep(1)
        UIFlow.shot(app, "mac-5-window-menu", self)
        XCTAssertTrue(delete.isEnabled, "в окне беседы меню «Письмо» погашено")
        XCTAssertTrue(app.menuBars.menuItems["Ответить"].firstMatch.isEnabled, "в окне беседы погашено «Ответить»")
        app.typeKey(.escape, modifierFlags: [])
    }
    #endif

    @MainActor
    private func waitLabel(_ e: XCUIElement, _ label: String, _ timeout: TimeInterval = 5) -> Bool {
        let deadline = Date().addingTimeInterval(timeout)
        while Date() < deadline {
            if e.exists && e.label == label { return true }
            usleep(200_000)
        }
        return e.exists && e.label == label
    }
}
