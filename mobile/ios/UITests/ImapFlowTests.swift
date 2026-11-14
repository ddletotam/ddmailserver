import XCTest

/// Сквозные сценарии внешних учёток на настоящих серверах (контракт внешних
/// учёток §11). Учётки — из окружения запуска; нет их — тест пропускается.
/// Писем не отправляет.
final class ImapFlowTests: XCTestCase {
    override func setUp() {
        continueAfterFailure = false
    }

    /// Mail.ru: адрес → автонастройка находит IMAP/SMTP → пароль приложения →
    /// проверка входа → список и беседа.
    @MainActor
    func testMailRuDiscoveredIMAP() throws {
        let env = ProcessInfo.processInfo.environment
        guard let email = env["DDMAIL_TEST_MAILRU_EMAIL"], let password = env["DDMAIL_TEST_MAILRU_PASSWORD"], !email.isEmpty else {
            throw XCTSkip("нет DDMAIL_TEST_MAILRU_EMAIL / DDMAIL_TEST_MAILRU_PASSWORD")
        }
        let app = XCUIApplication.ddmail()
        app.launchArguments += ["-ddmail-reset", "-ddmail-uitest-ids"]
        app.launch()

        let emailField = app.textFields["connect.email"]
        XCTAssertTrue(emailField.waitForExistence(timeout: 10))
        type(email, into: emailField, app)
        app.buttons["connect.find"].tap()
        // Поиск по всем фазам (ddmail на домене и MX ждёт таймаутов) — до минуты.
        let pw = app.secureTextFields["connect.password"]
        XCTAssertTrue(pw.waitForExistence(timeout: 90), "серверы IMAP/SMTP не найдены")
        shot(app, "imap-1-found")
        let found = app.descendants(matching: .any).matching(NSPredicate(format: "label CONTAINS 'imap.mail.ru' OR value CONTAINS 'imap.mail.ru'")).firstMatch
        XCTAssertTrue(found.waitForExistence(timeout: 5), "найден не тот IMAP")
        enterPassword(password, into: pw, app)
        app.buttons["connect.login"].tap()
        let rows = waitRows(app)
        XCTAssertTrue(!rows.isEmpty, "список бесед пуст")
        shot(app, "imap-2-list")
        let first = app.descendants(matching: .any).matching(NSPredicate(format: "identifier BEGINSWITH 'conv.row'")).firstMatch
        first.tap()
        let web = app.webViews.firstMatch
        XCTAssertTrue(web.waitForExistence(timeout: 20), "лента беседы не появилась")
        XCTAssertTrue(web.staticTexts.firstMatch.waitForExistence(timeout: 40), "в ленте нет писем")
        shot(app, "imap-3-opened")
    }

    /// Учётка `lucky` сначала через ddmail, затем — тот же ящик по IMAP
    /// («Я знаю все настройки»): беседы совпадают по id.
    @MainActor
    func testLuckyIMAPMatchesDdmail() throws {
        let env = ProcessInfo.processInfo.environment
        guard let email = env["DDMAIL_TEST_EMAIL"], let password = env["DDMAIL_TEST_PASSWORD"], !email.isEmpty else {
            throw XCTSkip("нет DDMAIL_TEST_EMAIL / DDMAIL_TEST_PASSWORD")
        }
        let host = "mail." + (email.split(separator: "@").last.map(String.init) ?? "")
        let login = String(email.split(separator: "@").first ?? "")
        let app = XCUIApplication.ddmail()
        app.launchArguments += ["-ddmail-reset", "-ddmail-uitest-ids"]
        app.launch()

        // 1. ddmail
        let emailField = app.textFields["connect.email"]
        XCTAssertTrue(emailField.waitForExistence(timeout: 10))
        type(email, into: emailField, app)
        app.buttons["connect.find"].tap()
        let pw = app.secureTextFields["connect.password"]
        XCTAssertTrue(pw.waitForExistence(timeout: 30), "сервер ddmail не найден")
        enterPassword(password, into: pw, app)
        app.buttons["connect.login"].tap()
        var viaDdmail = waitRows(app)
        XCTAssertFalse(viaDdmail.isEmpty, "через ddmail список пуст")
        shot(app, "lucky-1-ddmail")
        // У ddmail-учётки папки диалогов с сервера: «Входящие» — не все беседы.
        // У IMAP-учётки папок нет (локальный документ пуст) — сравниваем с
        // объединением верха каждой папки (кроме «Спама»: его IMAP не видит).
        viaDdmail += folderRows(app)

        // 2. Удалить учётку: Настройки → Учётки.
        openSettings(app)
        let row = app.descendants(matching: .any).matching(identifier: "account.row").firstMatch
        XCTAssertTrue(row.waitForExistence(timeout: 10), "нет строки учётки в настройках")
        row.tap()
        let remove = app.buttons["account.remove"].firstMatch
        XCTAssertTrue(remove.waitForExistence(timeout: 10))
        remove.tap()
        let confirm = app.buttons.matching(NSPredicate(format: "label == 'Удалить'")).firstMatch
        XCTAssertTrue(confirm.waitForExistence(timeout: 5))
        confirm.tap()

        // 3. Тот же ящик по IMAP вручную.
        XCTAssertTrue(emailField.waitForExistence(timeout: 15), "после удаления последней учётки нет окна подключения")
        type(email, into: emailField, app)
        app.buttons["connect.manualIMAP"].tap()
        let imapHost = app.textFields["connect.imap.host"]
        XCTAssertTrue(imapHost.waitForExistence(timeout: 10), "нет ручной формы")
        replace(imapHost, with: host, app)
        replace(app.textFields["connect.smtp.host"], with: host, app)
        replace(app.textFields["connect.login.name"], with: login, app)
        shot(app, "lucky-2-manual")
        let pw2 = app.secureTextFields["connect.password"]
        enterPassword(password, into: pw2, app)
        app.buttons["connect.login"].tap()
        let viaIMAP = waitRows(app)
        shot(app, "lucky-3-imap")
        XCTAssertFalse(viaIMAP.isEmpty, "по IMAP список пуст")
        let common = Set(viaIMAP).intersection(viaDdmail)
        print("ddmail: \(viaDdmail)\nIMAP: \(viaIMAP)")
        XCTAssertGreaterThanOrEqual(common.count, min(3, viaIMAP.count), "беседы по IMAP не совпали с ddmail по id")

        // 4. Календарь той же учётки — по CalDAV, найденному при подключении (§8).
        // Разделы — только из бургера (mobile-layout.md).
        settle(app)
        Nav.go("Календарь", app)
        let today = app.buttons["Сегодня"].waitForExistence(timeout: 20)
        shot(app, "lucky-4a-calendar-tapped")
        XCTAssertTrue(today, "календарь CalDAV не открылся")
        sleep(4)
        shot(app, "lucky-4-caldav")
    }

    /// Сначала Mail.ru по IMAP, затем «Добавить учётку» → ddmail `lucky`:
    /// почта добавленной учётки синхронизируется сразу, без перезапуска —
    /// её беседы в общем списке, её папки (Small, AppSec) в полосе табов.
    /// 0.0.12: пространство пересобиралось, а синк и WebSocket запускались
    /// только на выход приложения на экран — у ddmail ни одного `/conversations`.
    @MainActor
    func testAddDdmailToIMAP() throws {
        let (ddmail, mailru) = try bothAccounts()
        let app = XCUIApplication.ddmail()
        app.launchArguments += ["-ddmail-reset", "-ddmail-uitest-ids"]
        app.launch()

        connectMailRu(mailru, app)
        let before = Set(waitRows(app))
        XCTAssertFalse(before.isEmpty, "список Mail.ru пуст")
        shot(app, "add-1-mailru")

        addAccount(app)
        connectDdmail(ddmail, app)
        XCTAssertTrue(waitTabs(["Small", "AppSec"], app), "папки ddmail не появились в полосе табов")
        let inbox = app.buttons.matching(identifier: "folder.tab").matching(Shown.contains("Входящие")).firstMatch
        if inbox.exists { inbox.tap() }
        let after = Set(waitRows(app)).union(folderRows(app))
        shot(app, "add-2-ddmail")
        XCTAssertFalse(after.subtracting(before).isEmpty, "бесед ddmail в списке нет")
    }

    /// Обратный порядок: сначала ddmail, затем «Добавить учётку» → Mail.ru.
    @MainActor
    func testAddIMAPToDdmail() throws {
        let (ddmail, mailru) = try bothAccounts()
        let app = XCUIApplication.ddmail()
        app.launchArguments += ["-ddmail-reset", "-ddmail-uitest-ids"]
        app.launch()

        connectDdmail(ddmail, app)
        XCTAssertTrue(waitTabs(["Small", "AppSec"], app), "папки ddmail не появились в полосе табов")
        let inbox = app.buttons.matching(identifier: "folder.tab").matching(Shown.contains("Входящие")).firstMatch
        if inbox.exists { inbox.tap() }
        let before = Set(waitRows(app))
        XCTAssertFalse(before.isEmpty, "список ddmail пуст")
        shot(app, "rev-1-ddmail")

        addAccount(app)
        connectMailRu(mailru, app)
        // Беседы Mail.ru — во «Входящих» общего списка, рядом с ddmail.
        var fresh: Set<String> = []
        let deadline = Date().addingTimeInterval(120)
        while fresh.isEmpty && Date() < deadline {
            settle(app)
            fresh = Set(visibleIDs(app)).subtracting(before)
        }
        shot(app, "rev-2-mailru")
        XCTAssertFalse(fresh.isEmpty, "бесед Mail.ru в списке нет")
        XCTAssertTrue(waitTabs(["Small", "AppSec"], app), "папки ddmail пропали из полосы")
    }

    // MARK: Помощники

    private typealias Creds = (email: String, password: String)

    private func bothAccounts() throws -> (Creds, Creds) {
        let env = ProcessInfo.processInfo.environment
        guard let e = env["DDMAIL_TEST_EMAIL"], let p = env["DDMAIL_TEST_PASSWORD"], !e.isEmpty else {
            throw XCTSkip("нет DDMAIL_TEST_EMAIL / DDMAIL_TEST_PASSWORD")
        }
        guard let me = env["DDMAIL_TEST_MAILRU_EMAIL"], let mp = env["DDMAIL_TEST_MAILRU_PASSWORD"], !me.isEmpty else {
            throw XCTSkip("нет DDMAIL_TEST_MAILRU_EMAIL / DDMAIL_TEST_MAILRU_PASSWORD")
        }
        return ((e, p), (me, mp))
    }

    /// Вход ddmail в открытом окне подключения.
    @MainActor
    private func connectDdmail(_ c: Creds, _ app: XCUIApplication) {
        let emailField = app.textFields["connect.email"]
        XCTAssertTrue(emailField.waitForExistence(timeout: 15), "нет окна подключения")
        type(c.email, into: emailField, app)
        app.buttons["connect.find"].tap()
        let pw = app.secureTextFields["connect.password"]
        XCTAssertTrue(pw.waitForExistence(timeout: 30), "сервер ddmail не найден")
        enterPassword(c.password, into: pw, app)
        app.buttons["connect.login"].tap()
        XCTAssertTrue(waitGone(emailField), "окно подключения не закрылось после входа ddmail")
    }

    /// Вход Mail.ru (автонастройка) в открытом окне подключения.
    @MainActor
    private func connectMailRu(_ c: Creds, _ app: XCUIApplication) {
        let emailField = app.textFields["connect.email"]
        XCTAssertTrue(emailField.waitForExistence(timeout: 15), "нет окна подключения")
        type(c.email, into: emailField, app)
        app.buttons["connect.find"].tap()
        let pw = app.secureTextFields["connect.password"]
        XCTAssertTrue(pw.waitForExistence(timeout: 90), "серверы IMAP/SMTP не найдены")
        enterPassword(c.password, into: pw, app)
        app.buttons["connect.login"].tap()
        XCTAssertTrue(waitGone(emailField), "окно подключения не закрылось после входа Mail.ru")
    }

    /// Настройки → «Добавить учётку»: окно подключения поверх.
    @MainActor
    private func addAccount(_ app: XCUIApplication) {
        openSettings(app)
        let add = app.descendants(matching: .any).matching(identifier: "account.add").firstMatch
        XCTAssertTrue(add.waitForExistence(timeout: 10), "в настройках нет «Добавить учётку»")
        add.tap()
        XCTAssertTrue(app.textFields["connect.email"].waitForExistence(timeout: 15), "окно подключения не открылось")
    }

    @MainActor
    private func waitGone(_ e: XCUIElement) -> Bool {
        let deadline = Date().addingTimeInterval(60)
        while e.exists && Date() < deadline { sleep(1) }
        return !e.exists
    }

    /// Табы с этими названиями появились в полосе (синк папок прошёл).
    @MainActor
    private func waitTabs(_ names: [String], _ app: XCUIApplication) -> Bool {
        let tabs = app.buttons.matching(identifier: "folder.tab")
        let deadline = Date().addingTimeInterval(90)
        while Date() < deadline {
            let labels = tabs.allElementsBoundByIndex.map(\.shownText)
            if names.allSatisfy({ n in labels.contains { $0.contains(n) } }) { return true }
            settle(app)
        }
        return false
    }


    /// Ждём строки списка (и закрываем вопросы: уведомления, загрузка заранее); id видимых бесед.
    @MainActor
    private func waitRows(_ app: XCUIApplication) -> [String] {
        let rows = app.descendants(matching: .any).matching(NSPredicate(format: "identifier BEGINSWITH 'conv.row'"))
        let wifi = app.buttons["Только по Wi-Fi"].firstMatch
        #if os(iOS)
        let springboard = XCUIApplication(bundleIdentifier: "com.apple.springboard")
        let allow = springboard.alerts.buttons.matching(NSPredicate(format: "label IN %@", ["Разрешить", "Allow"])).firstMatch
        #endif
        let deadline = Date().addingTimeInterval(120)
        while Date() < deadline {
            #if os(iOS)
            if allow.exists { allow.tap() }
            #endif
            if wifi.exists { wifi.tap() }
            if rows.count > 0 && !wifi.exists {
                sleep(2)
                #if os(iOS)
                if allow.exists { allow.tap() }
                #endif
                if wifi.exists { wifi.tap() }
                return visibleIDs(app)
            }
            sleep(1)
        }
        return []
    }

    /// id верхних бесед каждой папки полосы табов, затем снова «Входящие».
    @MainActor
    private func folderRows(_ app: XCUIApplication) -> [String] {
        settle(app)
        let tabs = app.buttons.matching(identifier: "folder.tab")
        let names = tabs.allElementsBoundByIndex.map(\.label).filter { !$0.contains("Спам") && !$0.contains("Входящие") }
        var out: [String] = []
        for name in names {
            let tab = tabs.matching(NSPredicate(format: "label == %@", name)).firstMatch
            guard tab.exists else { continue }
            tab.tap()
            sleep(2)
            out += visibleIDs(app)
        }
        let inbox = tabs.matching(NSPredicate(format: "label CONTAINS 'Входящие'")).firstMatch
        if inbox.exists { inbox.tap() }
        return out
    }

    /// id видимых бесед — одним снимком дерева: список обновляется синком, и
    /// поштучный доступ к строкам ловит исчезнувший элемент.
    @MainActor
    private func visibleIDs(_ app: XCUIApplication) -> [String] {
        guard let root = try? app.snapshot() else { return [] }
        var out: [String] = []
        func walk(_ s: XCUIElementSnapshot) {
            if s.identifier.hasPrefix("conv.row:") { out.append(String(s.identifier.dropFirst("conv.row:".count))) }
            for c in s.children { walk(c) }
        }
        walk(root)
        return Array(out.prefix(12))
    }

    @MainActor
    private func openSettings(_ app: XCUIApplication) {
        settle(app)
        // «Настройки» — пункт бургера.
        Nav.go("Настройки", app)
        shot(app, "settings-1-open")
    }

    /// Вопрос о загрузке и системный запрос уведомлений могут прийти уже
    /// после списка — сначала ответить на них, иначе нажатие уйдёт в них.
    @MainActor
    private func settle(_ app: XCUIApplication) {
        let wifi = app.buttons["Только по Wi-Fi"].firstMatch
        #if os(iOS)
        let springboard = XCUIApplication(bundleIdentifier: "com.apple.springboard")
        let allow = springboard.alerts.buttons.matching(NSPredicate(format: "label IN %@", ["Разрешить", "Allow"])).firstMatch
        #endif
        for _ in 0..<6 {
            #if os(iOS)
            if allow.exists { allow.tap() }
            #endif
            if wifi.exists { wifi.tap() }
            sleep(1)
        }
    }

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

    /// Стереть поле и ввести своё.
    @MainActor
    private func replace(_ field: XCUIElement, with text: String, _ app: XCUIApplication) {
        XCTAssertTrue(field.waitForExistence(timeout: 5))
        field.tap()
        let current = (field.value as? String) ?? ""
        #if os(iOS)
        if !current.isEmpty {
            // Курсор — в конец поля, затем стираем.
            field.coordinate(withNormalizedOffset: CGVector(dx: 0.97, dy: 0.5)).tap()
            field.typeText(String(repeating: XCUIKeyboardKey.delete.rawValue, count: current.count + 2))
        }
        for ch in text { field.typeText(String(ch)) }
        #else
        field.typeKey("a", modifierFlags: .command)
        field.typeText(text)
        #endif
    }

    @MainActor
    private func enterPassword(_ password: String, into field: XCUIElement, _ app: XCUIApplication) {
        #if os(iOS)
        // Клавиатура симулятора теряет цифры и знаки в защищённом поле — вставкой.
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

    @MainActor
    private func shot(_ app: XCUIApplication, _ name: String) {
        let a = XCTAttachment(screenshot: app.screenshot())
        a.name = name
        a.lifetime = .keepAlways
        add(a)
    }
}
