import XCTest

/// Находки сквозного аудита 2026-11-14 (`docs/audit/cross-device-audit-2026-11-14.md`)
/// — на настоящих серверах. Письма — **только себе** (`info@help-it-audit.ru`,
/// ящик тестовой учётки); контакт — временный, в конце удаляется.
final class AuditFixTests: XCTestCase {
    private let selfAddress = "info@help-it-audit.ru"

    override func setUp() {
        continueAfterFailure = false
    }

    /// №4, №10: в «От» нового письма — адрес внешней учётки (Mail.ru), и
    /// письмо с него уходит через SMTP Mail.ru; «Тема» нового письма не
    /// подсказывает «Re: …», «Кому» — подсказывает адрес беседы.
    @MainActor
    func testFromOffersExternalAccountAndSendsThroughIt() throws {
        let env = ProcessInfo.processInfo.environment
        guard let email = env["DDMAIL_TEST_EMAIL"], let password = env["DDMAIL_TEST_PASSWORD"], !email.isEmpty,
              let mailru = env["DDMAIL_TEST_MAILRU_EMAIL"], let mailruPassword = env["DDMAIL_TEST_MAILRU_PASSWORD"], !mailru.isEmpty
        else { throw XCTSkip("нет учёток ddmail и Mail.ru в окружении") }
        let app = UIFlow.launch()
        UIFlow.login(email, password, app, self)
        // «Добавить учётку» → Mail.ru.
        Nav.go("Настройки", app)
        let add = app.descendants(matching: .any).matching(identifier: "account.add").firstMatch
        XCTAssertTrue(add.waitForExistence(timeout: 10), "в настройках нет «Добавить учётку»")
        add.tap()
        UIFlow.login(mailru, mailruPassword, app, self, discoverTimeout: 90)

        // Новое письмо себе: поиск → «Написать по адресу…». Окно подключения
        // ещё уходит, а вопросы (уведомления) могут прийти после списка.
        UIFlow.settle(app)
        let search = app.textFields["search.field"].firstMatch
        XCTAssertTrue(search.waitForExistence(timeout: 10), "нет поля поиска")
        #if os(iOS)
        for _ in 0..<5 where !app.keyboards.firstMatch.exists {
            search.tap()
            _ = app.keyboards.firstMatch.waitForExistence(timeout: 3)
        }
        #endif
        search.typeText(selfAddress)
        let composeRow = app.descendants(matching: .any).matching(identifier: "search.compose").firstMatch
        XCTAssertTrue(composeRow.waitForExistence(timeout: 10), "нет строки «Написать по адресу…»")
        composeRow.tap()
        let fresh = Shown.newConversation(app)
        XCTAssertTrue(fresh.waitForExistence(timeout: 10), "новая беседа не открылась")

        let plus = UIFlow.composerPlus(app)
        XCTAssertTrue(plus.waitForExistence(timeout: 5), "нет «+» композера")
        plus.tap()
        UIFlow.menuItem("Кому, копия, тема", app).tap()
        let subject = app.textFields["composer.subject"].firstMatch
        XCTAssertTrue(subject.waitForExistence(timeout: 5), "нет поля «Тема»")
        XCTAssertFalse((subject.placeholderValue ?? "").hasPrefix("Re:"), "тема нового письма подсказывает «\(subject.placeholderValue ?? "")»")
        let to = app.textFields["composer.to"].firstMatch
        XCTAssertEqual(to.placeholderValue, selfAddress, "«Кому» не подсказывает адрес новой беседы")
        UIFlow.shot(app, "from-1-fields", self)

        // «От»: адрес Mail.ru — в выпадашке, среди первых строк.
        let from = app.descendants(matching: .any).matching(identifier: "composer.from").firstMatch
        XCTAssertTrue(from.waitForExistence(timeout: 5), "нет «От»")
        from.tap()
        let types = [XCUIElement.ElementType.button.rawValue, XCUIElement.ElementType.menuItem.rawValue]
        let item = app.descendants(matching: .any)
            .matching(NSPredicate(format: "(label CONTAINS %@ OR title CONTAINS %@) AND elementType IN %@", mailru, mailru, types)).firstMatch
        XCTAssertTrue(item.waitForExistence(timeout: 5), "адреса \(mailru) нет в «От»")
        UIFlow.shot(app, "from-2-menu", self)
        XCTAssertTrue(item.isHittable, "адрес \(mailru) в «От» не виден без прокрутки")
        item.tap()
        sleep(1)

        let stamp = Self.stamp()
        let body = app.textViews["composer.text"].firstMatch
        XCTAssertTrue(body.waitForExistence(timeout: 5), "нет строки ввода")
        body.tap()
        UIFlow.enter("Проверка ddmail «От» Mail.ru \(stamp)", into: body)
        subject.tap()
        UIFlow.enter("Проверка ddmail От Mail.ru \(stamp)", into: subject)
        UIFlow.shot(app, "from-3-composed", self)
        app.buttons["Отправить"].firstMatch.tap()
        let sent = app.descendants(matching: .any).matching(identifier: "compose.sent").firstMatch
        XCTAssertTrue(sent.waitForExistence(timeout: 60), "нет «✓ Отправлено» (SMTP Mail.ru)")
        UIFlow.shot(app, "from-4-sent", self)
        // Беседа письма — у учётки Mail.ru (копия в её «Отправленных»).
        XCTAssertTrue(UIFlow.waitGone(fresh, 180), "после отправки беседа не появилась")
        sleep(2)
        UIFlow.shot(app, "from-5-conversation", self)
        print("AUDIT-FROM-STAMP \(stamp)")
    }

    /// №5, №18: новый контакт сразу виден — карточкой, строкой списка и
    /// поиском раздела, даже если по алфавиту он дальше пятитысячной строки;
    /// «Ещё телефон» даёт второе поле, и номера не сливаются. В конце
    /// контакт удаляется.
    @MainActor
    func testNewContactVisibleInListAndSearch() throws {
        let env = ProcessInfo.processInfo.environment
        guard let email = env["DDMAIL_TEST_EMAIL"], let password = env["DDMAIL_TEST_PASSWORD"], !email.isEmpty else {
            throw XCTSkip("нет DDMAIL_TEST_EMAIL / DDMAIL_TEST_PASSWORD")
        }
        let app = UIFlow.launch()
        UIFlow.login(email, password, app, self)
        Nav.go("Контакты", app)
        let rows = app.descendants(matching: .any).matching(identifier: "contact.row")
        XCTAssertTrue(rows.firstMatch.waitForExistence(timeout: 60), "список контактов пуст")

        let stamp = Self.stamp()
        // «Яяя» — в самом конце алфавита: за пятитысячной строкой книги владельца.
        let name = "Яяя Проверка ddmail \(stamp)"
        app.buttons["Новый контакт"].firstMatch.tap()
        let nameField = app.textFields["Имя"].firstMatch
        XCTAssertTrue(nameField.waitForExistence(timeout: 10), "форма контакта не открылась")
        nameField.tap()
        UIFlow.enter(name, into: nameField)
        let mail = app.textFields["contact.email"].firstMatch
        mail.tap()
        mail.typeText("check-\(stamp)@example.invalid")
        let phone = app.textFields["contact.phone"].firstMatch
        phone.tap()
        phone.typeText("+70000000001")
        // «Ещё телефон»; на iPad он бывает под клавиатурой — тогда Return в
        // поле (то же действие).
        let addPhone = app.buttons["contact.addPhone"].firstMatch
        if addPhone.isHittable { addPhone.tap() } else { phone.typeText("\n") }
        let phones = app.textFields.matching(identifier: "contact.phone")
        XCTAssertTrue(phones.element(boundBy: 1).waitForExistence(timeout: 5), "«Ещё телефон» не добавил поле")
        sleep(1)
        // Фокус — уже в новом поле: печатаем без касания.
        app.typeText("+70000000002")
        XCTAssertEqual(phones.element(boundBy: 0).value as? String, "+70000000001", "номера слились в одно поле")
        XCTAssertEqual(phones.element(boundBy: 1).value as? String, "+70000000002")

        // Книга — «Contacts · Local» (не GAL, не безымянная из №7).
        let book = app.buttons.matching(NSPredicate(format: "label BEGINSWITH 'Адресная книга'")).firstMatch
        if book.waitForExistence(timeout: 3), (book.value as? String).map({ !$0.contains("Contacts") }) ?? true {
            #if os(iOS)
            // На iPad строка книги — под клавиатурой: убрать её касанием заголовка.
            if !book.isHittable { app.navigationBars.firstMatch.coordinate(withNormalizedOffset: CGVector(dx: 0.5, dy: 0.5)).tap() }
            sleep(1)
            #endif
            book.tap()
            let local = app.descendants(matching: .any).matching(NSPredicate(format: "label BEGINSWITH 'Contacts'")).firstMatch
            XCTAssertTrue(local.waitForExistence(timeout: 5), "нет книги «Contacts · Local»")
            local.tap()
        }
        UIFlow.shot(app, "contact-1-filled", self)
        app.buttons["Сохранить"].firstMatch.tap()

        // Карточка открыта сразу.
        let title = app.staticTexts.matching(Shown.equals(name)).firstMatch
        XCTAssertTrue(title.waitForExistence(timeout: 20), "карточка нового контакта не открылась")
        UIFlow.shot(app, "contact-2-card", self)
        let phoneRows = app.buttons.matching(NSPredicate(format: "label CONTAINS '+7000000000'"))
        XCTAssertEqual(phoneRows.count, 2, "в карточке не два телефона")

        // Строка в списке (iPhone — назад к списку).
        if !app.textFields["contacts.search"].firstMatch.isHittable { app.navigationBars.buttons.firstMatch.tap() }
        let row = rows.matching(Shown.contains(stamp)).firstMatch
        XCTAssertTrue(row.waitForExistence(timeout: 20), "нового контакта нет в списке")
        UIFlow.shot(app, "contact-3-list", self)

        // Поиск раздела.
        let search = app.textFields["contacts.search"].firstMatch
        search.tap()
        search.typeText(stamp)
        XCTAssertTrue(row.waitForExistence(timeout: 10), "новый контакт не находится поиском")
        XCTAssertEqual(rows.count, 1, "поиск по метке нашёл не один контакт")
        UIFlow.shot(app, "contact-4-search", self)

        // Убрать за собой.
        row.tap()
        let del = app.buttons.matching(NSPredicate(format: "label == 'Удалить контакт'")).firstMatch
        XCTAssertTrue(del.waitForExistence(timeout: 10), "нет «Удалить контакт»")
        del.tap()
        let confirm = app.buttons.matching(NSPredicate(format: "label == 'Удалить'")).firstMatch
        XCTAssertTrue(confirm.waitForExistence(timeout: 5))
        confirm.tap()
        XCTAssertTrue(UIFlow.waitGone(row, 20), "контакт не удалился")
        // И на сервере: поиск заново (локально и сервером) его не находит.
        let again = app.textFields["contacts.search"].firstMatch
        if !again.isHittable { app.navigationBars.buttons.firstMatch.tap() }
        UIFlow.replace(again, with: "ddmail \(stamp)", app)
        sleep(3)
        XCTAssertFalse(rows.matching(Shown.contains(stamp)).firstMatch.exists, "удалённый контакт снова находится")
        UIFlow.shot(app, "contact-5-deleted", self)
    }

    /// №8, №9, №17: «Спам» по «Показать скрытые» — таб виден целиком, в его
    /// меню нет «Все прочитаны»; на широкой раскладке справа — беседа (после
    /// входа и после смены таба), а не «Беседа не выбрана».
    @MainActor
    func testSpamTabMenuStripAndRightPane() throws {
        let env = ProcessInfo.processInfo.environment
        guard let email = env["DDMAIL_TEST_EMAIL"], let password = env["DDMAIL_TEST_PASSWORD"], !email.isEmpty else {
            throw XCTSkip("нет DDMAIL_TEST_EMAIL / DDMAIL_TEST_PASSWORD")
        }
        let app = UIFlow.launch()
        UIFlow.login(email, password, app, self)
        let wide = app.windows.firstMatch.frame.width > 700
        let empty = app.staticTexts.matching(Shown.equals("Беседа не выбрана")).firstMatch
        if wide {
            sleep(2)
            XCTAssertFalse(empty.exists, "после входа справа «Беседа не выбрана»")
        }

        let tabs = app.buttons.matching(identifier: "folder.tab")
        XCTAssertTrue(tabs.firstMatch.waitForExistence(timeout: 30), "нет полосы табов")
        let strip = app.otherElements["folder.strip"].firstMatch
        UIFlow.contextMenu(strip.exists ? strip : tabs.firstMatch)
        let show = app.descendants(matching: .any).matching(NSPredicate(format: "label BEGINSWITH 'Показать скрытые'")).firstMatch
        XCTAssertTrue(show.waitForExistence(timeout: 5), "нет «Показать скрытые (N)»")
        show.tap()
        let spam = tabs.matching(Shown.contains("Спам")).firstMatch
        XCTAssertTrue(spam.waitForExistence(timeout: 10), "таба «Спам» нет")
        spam.tap()
        sleep(2)
        UIFlow.shot(app, "spam-1-tab", self)
        // Выбранный таб и «← Входящие» — в видимой части полосы.
        let window = app.windows.firstMatch.frame
        XCTAssertTrue(window.contains(spam.frame), "выбранный «Спам» за краем полосы: \(spam.frame)")
        let inbox = tabs.matching(Shown.contains("Входящие")).firstMatch
        XCTAssertTrue(inbox.exists && inbox.frame.minX >= window.minX, "«← Входящие» за левым краем: \(inbox.frame)")

        UIFlow.contextMenu(spam)
        XCTAssertTrue(UIFlow.menuItem("Новая папка", app).waitForExistence(timeout: 5), "меню таба «Спам» не открылось")
        XCTAssertFalse(UIFlow.menuItem("Все прочитаны", app).exists, "в меню «Спама» есть «Все прочитаны»")
        XCTAssertFalse(UIFlow.menuItem("Настроить…", app).exists, "в меню «Спама» есть «Настроить…»")
        UIFlow.shot(app, "spam-2-menu", self)
        #if os(iOS)
        // Закрыть меню касанием мимо.
        app.coordinate(withNormalizedOffset: CGVector(dx: 0.5, dy: 0.95)).tap()
        #endif
        sleep(1)
        let spamRows = app.descendants(matching: .any).matching(NSPredicate(format: "identifier BEGINSWITH 'conv.row'"))
        if wide && spamRows.count > 0 {
            XCTAssertFalse(empty.exists, "после смены таба справа «Беседа не выбрана»")
        }
        UIFlow.shot(app, "spam-3-pane", self)
    }

    private static func stamp() -> String {
        let f = DateFormatter()
        f.dateFormat = "yyyyMMdd-HHmmss"
        return f.string(from: Date())
    }
}
