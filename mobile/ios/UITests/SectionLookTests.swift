import XCTest

/// Задачи и контакты (аудит M-01, M-11): «Показать выполненные» на iPhone —
/// иконкой, первая строка в одну строку; выбранный контакт — зелёной
/// строкой. Только просмотр, ничего не меняет.
final class SectionLookTests: XCTestCase {
    override func setUp() {
        continueAfterFailure = false
    }

    @MainActor
    func testTasksToggleAndContactSelection() throws {
        let (email, password) = try CalendarDragTests.credentials()
        let app = UIFlow.launch()
        UIFlow.login(email, password, app, self)

        Nav.go("Задачи", app)
        let toggle = app.descendants(matching: .any).matching(identifier: "tasks.showDone").firstMatch
        XCTAssertTrue(toggle.waitForExistence(timeout: 10), "нет «Показать выполненные»")
        XCTAssertEqual(toggle.label, "Показать выполненные", "у переключателя нет подписи для VoiceOver")
        #if os(iOS)
        if UIDevice.current.userInterfaceIdiom == .phone {
            XCTAssertLessThanOrEqual(toggle.frame.width, 48, "на iPhone переключатель не иконкой: \(toggle.frame)")
            XCTAssertLessThanOrEqual(toggle.frame.height, 48, "первая строка разъехалась: \(toggle.frame)")
        }
        #endif
        sleep(2)
        UIFlow.shot(app, "tasks-1", self)

        Nav.go("Контакты", app)
        let row = app.descendants(matching: .any)
            .matching(NSPredicate(format: "label CONTAINS 'Абай' OR value CONTAINS 'Абай' OR title CONTAINS 'Абай'")).firstMatch
        XCTAssertTrue(row.waitForExistence(timeout: 20), "контакты не загрузились")
        row.tap()
        sleep(2)
        UIFlow.shot(app, "contacts-1-selected", self)
    }
}
