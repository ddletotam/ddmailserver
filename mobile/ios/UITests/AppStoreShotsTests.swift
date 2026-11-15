import XCTest

/// Скриншоты для карточки App Store (`docs/appstore-listing.md`): только
/// демо-учётка ревью, ничего не меняет. Снимок — весь экран со статус-баром
/// (его заранее приводит в порядок `simctl status_bar override`).
/// Запуск: `DDMAIL_TEST_EMAIL=… DDMAIL_TEST_PASSWORD=… mac.sh uitest iphonemax AppStoreShotsTests`.
/// На Маке приложение идёт в отдельном профиле данных (`DDMAIL_PROFILE=shots`):
/// учётки владельца в обычном профиле не стираются. Снимок — окно приложения,
/// на холст 2880×1800 его кладёт `mobile/store-assets/mac_canvas.py`.
final class AppStoreShotsTests: XCTestCase {
    override func setUp() {
        continueAfterFailure = false
    }

    @MainActor
    private func shot(_ name: String) {
        sleep(2)
        #if os(macOS)
        let a = XCTAttachment(screenshot: XCUIApplication().windows.firstMatch.screenshot())
        #else
        let a = XCTAttachment(screenshot: XCUIScreen.main.screenshot())
        #endif
        a.name = name
        a.lifetime = .keepAlways
        add(a)
    }

    @MainActor
    func testStoreScreenshots() throws {
        let (email, password) = try CalendarDragTests.credentials()
        let app = XCUIApplication.ddmail()
        #if os(macOS)
        app.launchEnvironment["DDMAIL_PROFILE"] = "shots"
        #endif
        app.launchArguments += ["-ddmail-reset", "-ddmail-uitest-ids"]
        app.launch()
        UIFlow.login(email, password, app, self)
        let rows = app.descendants(matching: .any).matching(NSPredicate(format: "identifier BEGINSWITH 'conv.row'"))
        XCTAssertTrue(rows.element(boundBy: 2).waitForExistence(timeout: 30), "в списке меньше трёх бесед")
        let wide = app.windows.firstMatch.frame.width > 700
        if !wide { shot("01-list") }

        // Беседа с фото во вложении — нагляднее одиночного письма.
        let photo = app.descendants(matching: .any).matching(NSPredicate(format: "identifier BEGINSWITH 'conv.row' AND identifier CONTAINS 'olga.mironova'")).firstMatch
        (photo.exists ? photo : rows.element(boundBy: 0)).tap()
        XCTAssertTrue(app.webViews.firstMatch.waitForExistence(timeout: 20), "беседа не открылась")
        sleep(3)
        shot("02-conversation")
        if !wide {
            let back = app.navigationBars.buttons.firstMatch
            if back.exists { back.tap() }
            sleep(1)
        }

        let search = app.textFields["search.field"].firstMatch
        XCTAssertTrue(search.waitForExistence(timeout: 10), "нет поиска")
        search.tap()
        UIFlow.type("anna.petrova@example.com", into: search, app)
        shot("03-compose-by-address")
        UIFlow.replace(search, with: "", app)
        let cancel = app.buttons.matching(NSPredicate(format: "label IN %@", ["Отмена", "Cancel"])).firstMatch
        if cancel.exists { cancel.tap() }
        #if os(iOS)
        app.swipeDown()
        #endif
        sleep(1)

        Nav.go("Календарь", app)
        XCTAssertTrue(app.descendants(matching: .any)["cal.title"].firstMatch.waitForExistence(timeout: 20), "календарь не открылся")
        sleep(2)
        let today = app.buttons["Сегодня"].firstMatch
        if today.exists { today.tap() }
        sleep(3)
        shot("04-calendar")

        Nav.go("Контакты", app)
        let contact = app.descendants(matching: .any).matching(identifier: "contact.row")
        XCTAssertTrue(contact.firstMatch.waitForExistence(timeout: 20), "контакты не загрузились")
        if wide { contact.element(boundBy: min(1, contact.count - 1)).tap() }
        shot("05-contacts")

        Nav.go("Задачи", app)
        sleep(4)
        shot("06-tasks")
    }
}
