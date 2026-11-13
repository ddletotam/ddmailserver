import XCTest

/// Меню пузыря (контракт §8, §9; аудит S-04, S-05, M-08): нативное
/// контекстное меню у самого пузыря — пункты десктопа, без системного меню
/// выделения поверх; «Показать… → Заголовки»; «Удалить письмо» — только на
/// своём тестовом письме себе (`info@help-it-audit.ru`, тема «Проверка ddmail …»).
final class BubbleMenuTests: XCTestCase {
    private let selfAddress = "info@help-it-audit.ru"

    override func setUp() {
        continueAfterFailure = false
    }

    /// На письме, которое у учётки уже есть (отказ о недоставке): пункты,
    /// место меню, «Заголовки». Ничего не отправляет и не удаляет.
    @MainActor
    func testBubbleMenuItemsAndHeaders() throws {
        let app = try start()
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

        UIFlow.contextMenu(text)
        let reply = UIFlow.menuItem("Ответить", app)
        XCTAssertTrue(reply.waitForExistence(timeout: 5), "меню пузыря не открылось")
        sleep(1)
        UIFlow.shot(app, "bubble-1-menu", self)
        for name in ["Переслать", "Копировать текст", "Копировать адрес", "Показать…", "Удалить письмо"] {
            XCTAssertTrue(UIFlow.menuItem(name, app).exists, "в меню пузыря нет «\(name)»")
        }
        #if os(iOS)
        // Одно меню: системного меню выделения поверх нет.
        let system = app.descendants(matching: .any)
            .matching(NSPredicate(format: "label IN %@", ["Найти", "Перевести", "Translate", "Look Up"])).firstMatch
        XCTAssertFalse(system.exists, "вместе с меню письма открылось системное меню выделения")
        // Меню — у пузыря: его пункт по вертикали рядом с пузырём, а не
        // посреди колонки (на iPad меню висело над списком бесед).
        let bubble = text.frame
        let item = reply.frame
        let gap = max(0, max(bubble.minY - item.maxY, item.minY - bubble.maxY))
        XCTAssertLessThan(gap, 260, "меню далеко от пузыря: пузырь \(bubble), пункт \(item)")
        XCTAssertTrue(web.frame.contains(CGPoint(x: item.midX, y: item.midY)), "меню вне колонки беседы: \(item)")
        #endif

        // «Показать… → Заголовки»: просмотр с заголовками письма.
        let show = UIFlow.menuItem("Показать…", app)
        show.tap()
        let headers = UIFlow.menuItem("Заголовки", app)
        XCTAssertTrue(headers.waitForExistence(timeout: 5), "нет пункта «Заголовки»")
        XCTAssertTrue(UIFlow.menuItem("Исходник сообщения", app).exists, "нет пункта «Исходник сообщения»")
        UIFlow.shot(app, "bubble-2-show", self)
        headers.tap()
        let from = app.descendants(matching: .any).matching(NSPredicate(format: "label CONTAINS %@", "From:")).firstMatch
        XCTAssertTrue(from.waitForExistence(timeout: 20), "заголовки не показались")
        sleep(1)
        UIFlow.shot(app, "bubble-3-headers", self)
        let done = app.buttons.matching(NSPredicate(format: "label IN %@", ["Готово", "Done"])).firstMatch
        XCTAssertTrue(done.waitForExistence(timeout: 5), "нет «Готово» у просмотра")
        done.tap()
        XCTAssertTrue(UIFlow.waitGone(from, 10), "просмотр заголовков не закрылся")

        #if os(macOS)
        // Мак: меню закрывается Esc (лист без «Отмены» не закрывался, S-05).
        UIFlow.contextMenu(text)
        XCTAssertTrue(UIFlow.menuItem("Ответить", app).waitForExistence(timeout: 5), "правый клик не открыл меню")
        app.typeKey(.escape, modifierFlags: [])
        XCTAssertTrue(UIFlow.waitGone(UIFlow.menuItem("Ответить", app), 5), "Esc не закрыл меню пузыря")
        #endif

        // Меню чипа вложения (S-15): «Поделиться» везде; Мак — «Загрузки» и
        // программа по умолчанию, iPhone/iPad — «Файлы».
        let chip = web.buttons.matching(NSPredicate(format: "label CONTAINS %@", "unnamed")).firstMatch
        XCTAssertTrue(chip.waitForExistence(timeout: 5), "нет чипа вложения")
        UIFlow.contextMenu(chip)
        let share = UIFlow.menuItem("Поделиться", app)
        XCTAssertTrue(share.waitForExistence(timeout: 5), "в меню вложения нет «Поделиться»")
        XCTAssertTrue(UIFlow.menuItem("Открыть", app).exists, "в меню вложения нет «Открыть»")
        #if os(macOS)
        XCTAssertTrue(UIFlow.menuItem("Сохранить в Загрузки", app).exists, "нет «Сохранить в Загрузки»")
        XCTAssertTrue(UIFlow.menuItem("Открыть программой по умолчанию", app).exists, "нет «Открыть программой по умолчанию»")
        UIFlow.shot(app, "bubble-4-attachment-menu", self)
        app.typeKey(.escape, modifierFlags: [])
        #else
        XCTAssertTrue(UIFlow.menuItem("Сохранить в «Файлы»", app).exists, "нет «Сохранить в «Файлы»»")
        UIFlow.shot(app, "bubble-4-attachment-menu", self)
        share.tap()
        // Системный лист «Поделиться» с файлом.
        let sheet = app.otherElements.matching(NSPredicate(format: "identifier == %@", "ActivityListView")).firstMatch
        let anyShare = app.descendants(matching: .any)
            .matching(NSPredicate(format: "label IN %@", ["Сохранить в Файлы", "Save to Files", "Скопировать", "Copy"])).firstMatch
        XCTAssertTrue(sheet.waitForExistence(timeout: 15) || anyShare.waitForExistence(timeout: 5), "лист «Поделиться» не открылся")
        sleep(1)
        UIFlow.shot(app, "bubble-5-share", self)
        #endif
    }

    /// Своё письмо себе → меню пузыря → «Удалить письмо» → подтверждение →
    /// пузырь уходит из ленты.
    @MainActor
    func testDeleteOwnTestMessage() throws {
        let app = try start()
        let stamp = Self.stamp()
        let body = "Проверка ddmail удаление \(stamp)"
        let search = app.textFields["search.field"].firstMatch
        XCTAssertTrue(search.waitForExistence(timeout: 10), "нет поля поиска")
        search.tap()
        search.typeText(selfAddress)
        let composeRow = app.descendants(matching: .any).matching(identifier: "search.compose").firstMatch
        XCTAssertTrue(composeRow.waitForExistence(timeout: 10), "нет «Написать по адресу…»")
        composeRow.tap()
        let fresh = app.staticTexts["Новая беседа"].firstMatch
        XCTAssertTrue(fresh.waitForExistence(timeout: 10), "новая беседа не открылась")
        let plus = app.buttons["Вложить или поля письма"].firstMatch
        XCTAssertTrue(plus.waitForExistence(timeout: 5), "нет «+» композера")
        plus.tap()
        let fields = UIFlow.menuItem("Кому, копия, тема", app)
        XCTAssertTrue(fields.waitForExistence(timeout: 5), "нет «Кому, копия, тема»")
        fields.tap()
        let subject = app.textFields["composer.subject"].firstMatch
        XCTAssertTrue(subject.waitForExistence(timeout: 5), "нет поля «Тема»")
        subject.tap()
        subject.typeText("Проверка ddmail удаление \(stamp)")
        // Подпись «От» у поля отправителя (M-07).
        XCTAssertTrue(app.staticTexts["От"].firstMatch.exists, "у поля отправителя нет подписи «От»")
        UIFlow.shot(app, "delete-0-fields", self)
        let text = app.textViews["composer.text"].firstMatch
        text.tap()
        text.typeText(body)
        app.buttons["Отправить"].firstMatch.tap()
        XCTAssertTrue(UIFlow.waitGone(fresh, 180), "после отправки беседа не появилась")

        let web = app.webViews.firstMatch
        XCTAssertTrue(web.waitForExistence(timeout: 20), "лента беседы не появилась")
        let mine = web.staticTexts.matching(NSPredicate(format: "label CONTAINS %@", stamp)).firstMatch
        XCTAssertTrue(mine.waitForExistence(timeout: 60), "в беседе нет отправленного письма")
        let pending = web.staticTexts["Отправляется…"].firstMatch
        XCTAssertTrue(UIFlow.waitGone(pending, 150), "письмо так и не пришло синком")
        sleep(3)
        // Тело письма — последнее с меткой (тема в ленте не видна, но пусть).
        let target = web.staticTexts.matching(NSPredicate(format: "label CONTAINS %@", stamp)).allElementsBoundByIndex.last { $0.isHittable } ?? mine
        UIFlow.shot(app, "delete-1-landed", self)

        UIFlow.contextMenu(target)
        let del = UIFlow.menuItem("Удалить письмо", app)
        XCTAssertTrue(del.waitForExistence(timeout: 5), "нет «Удалить письмо»")
        UIFlow.shot(app, "delete-2-menu", self)
        del.tap()
        let confirm = app.buttons.matching(NSPredicate(format: "label == %@", "Удалить")).firstMatch
        XCTAssertTrue(confirm.waitForExistence(timeout: 5), "нет подтверждения удаления")
        UIFlow.shot(app, "delete-3-confirm", self)
        confirm.tap()
        XCTAssertTrue(UIFlow.waitGone(mine, 20), "пузырь удалённого письма остался в ленте")
        sleep(2)
        UIFlow.shot(app, "delete-4-gone", self)
    }

    /// «Настройки → Внешний контент» (M-15): экран со списком «Всегда
    /// показывать картинки от …». Ничего не отзывает.
    @MainActor
    func testExternalContentScreen() throws {
        let app = try start()
        Nav.go("Настройки", app)
        let link = app.descendants(matching: .any).matching(identifier: "settings.external").firstMatch
        XCTAssertTrue(link.waitForExistence(timeout: 10), "в настройках нет «Внешний контент»")
        link.tap()
        let header = app.staticTexts.matching(NSPredicate(format: "label CONTAINS[c] %@", "Всегда показывать картинки от")).firstMatch
        XCTAssertTrue(header.waitForExistence(timeout: 10), "экран «Внешний контент» не открылся")
        sleep(1)
        UIFlow.shot(app, "external-1-screen", self)
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
        return app
    }

    private static func stamp() -> String {
        let f = DateFormatter()
        f.dateFormat = "yyyyMMdd-HHmmss"
        return f.string(from: Date())
    }
}
