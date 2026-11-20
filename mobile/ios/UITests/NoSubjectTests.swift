import XCTest

/// «Отправить без темы?» (десктоп §1) и «Настройки → Отправитель по
/// умолчанию» (§1в) — без фактической отправки: вопрос появляется, «Отмена»
/// оставляет письмо и ставит фокус в «Тему». Учётка — из окружения; нет её —
/// тест пропускается.
final class NoSubjectTests: XCTestCase {
    private let selfAddress = "info@help-it-audit.ru"

    override func setUp() {
        continueAfterFailure = false
    }

    @MainActor
    func testEmptySubjectAsksAndCancelFocusesSubject() throws {
        let env = ProcessInfo.processInfo.environment
        guard let email = env["DDMAIL_TEST_EMAIL"], let password = env["DDMAIL_TEST_PASSWORD"], !email.isEmpty else {
            throw XCTSkip("нет DDMAIL_TEST_EMAIL / DDMAIL_TEST_PASSWORD")
        }
        let app = UIFlow.launch()
        UIFlow.login(email, password, app, self)
        UIFlow.settle(app)

        // Настройки: раздел «Отправитель по умолчанию» с группами.
        Nav.go("Настройки", app)
        let picker = app.descendants(matching: .any).matching(identifier: "settings.defaultSender").firstMatch
        let header = app.staticTexts.matching(NSPredicate(format: "label CONTAINS[c] %@", "Отправитель по умолчанию")).firstMatch
        XCTAssertTrue(header.waitForExistence(timeout: 15), "нет раздела «Отправитель по умолчанию»")
        _ = picker.waitForExistence(timeout: 15)
        UIFlow.shot(app, "nosubject-0-settings", self)
        let done = app.buttons["Готово"].firstMatch
        if done.exists { done.tap() } else { Nav.go("Почта", app) }

        // Новое письмо без темы.
        let search = app.textFields["search.field"].firstMatch
        XCTAssertTrue(search.waitForExistence(timeout: 10), "нет поля поиска")
        search.tap()
        search.typeText(selfAddress)
        let composeRow = app.descendants(matching: .any).matching(identifier: "search.compose").firstMatch
        XCTAssertTrue(composeRow.waitForExistence(timeout: 10), "нет строки «Написать по адресу…»")
        composeRow.tap()
        let body = app.textViews["composer.text"].firstMatch
        XCTAssertTrue(body.waitForExistence(timeout: 10), "нет строки ввода")
        body.tap()
        UIFlow.enter("Проверка вопроса о теме — не отправлять", into: body)
        sleep(1)
        app.buttons["Отправить"].firstMatch.tap()

        let alert = app.alerts["Отправить без темы?"].firstMatch
        XCTAssertTrue(alert.waitForExistence(timeout: 5), "нет вопроса «Отправить без темы?»")
        UIFlow.shot(app, "nosubject-1-question", self)
        alert.buttons["Отмена"].tap()
        XCTAssertTrue(UIFlow.waitGone(alert, 5), "вопрос не закрылся")

        // Письмо осталось, поля раскрыты, фокус — в «Теме».
        let subject = app.textFields["composer.subject"].firstMatch
        XCTAssertTrue(subject.waitForExistence(timeout: 5), "поля не раскрылись")
        sleep(1)
        XCTAssertTrue(subject.value(forKey: "hasKeyboardFocus") as? Bool ?? false, "фокус не в «Теме»")
        XCTAssertTrue((body.value as? String ?? "").contains("не отправлять"), "текст письма пропал")
        XCTAssertFalse(app.descendants(matching: .any).matching(identifier: "compose.sent").firstMatch.exists, "письмо ушло")
        UIFlow.shot(app, "nosubject-2-cancel", self)
    }
}
