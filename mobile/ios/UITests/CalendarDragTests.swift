import XCTest

/// Календарь (контракт §11, Android §12, десктоп §5и): перенос и растяжение
/// событий на шкале дня и недели (шаг 15 мин), вид «Год», лента месяцев без
/// предела, долгое нажатие на день — новое событие. На настоящем сервере:
/// тестовое событие «Проверка ddmail …» заводится через API в записываемом
/// календаре и удаляется в конце, что бы ни случилось.
final class CalendarDragTests: XCTestCase {
    override func setUp() {
        continueAfterFailure = false
    }

    /// День и неделя: перенос на 2 ч, растяжение на 1 ч, перенос на соседний день.
    @MainActor
    func testDragAndResizeEvent() async throws {
        let (email, password) = try Self.credentials()
        let api = try await TestCalendarAPI.login(email: email, password: password)
        let cal = Calendar.current
        let tomorrow = cal.date(byAdding: .day, value: 1, to: cal.startOfDay(for: Date()))!
        let at10 = cal.date(bySettingHour: 10, minute: 0, second: 0, of: tomorrow)!
        let title = "Проверка ddmail перенос \(Int(Date().timeIntervalSince1970) % 100_000)"
        let id = try await api.create(title, start: at10, end: at10.addingTimeInterval(3600))
        addTeardownBlock { try? await api.delete(id) }

        let app = UIFlow.launch()
        UIFlow.login(email, password, app, self)
        Nav.go("Календарь", app)
        XCTAssertTrue(app.buttons["Сегодня"].waitForExistence(timeout: 10), "календарь не открылся")
        let wide = segment("День", app).waitForExistence(timeout: 3)
        if wide {
            segment("День", app).tap()
            app.buttons["Сегодня"].tap()
            app.buttons["Вперёд"].tap()
        } else {
            // iPhone: день из ленты месяцев.
            let d = cal.component(.day, from: tomorrow)
            let cell = visible(Shown.begins("\(d), событий:"), app)
            XCTAssertNotNil(cell, "нет дня \(d) в ленте")
            cell?.tap()
        }
        let chip = event(title, app)
        XCTAssertTrue(chip.waitForExistence(timeout: 20), "событие не появилось на шкале")
        sleep(1)
        UIFlow.shot(app, "cal-1-before", self)

        // Перенос: долгое нажатие на середине и ведение вниз на 2 ч (104 точки).
        drag(chip, from: CGVector(dx: 0.5, dy: 0.5), by: CGVector(dx: 0, dy: 104))
        try await expect(api, id, start: at10.addingTimeInterval(2 * 3600), end: at10.addingTimeInterval(3 * 3600), "перенос")
        UIFlow.shot(app, "cal-2-moved", self)

        // Растяжение за нижнюю кромку: конец на час позже.
        let moved = event(title, app)
        XCTAssertTrue(moved.waitForExistence(timeout: 10))
        let h = moved.frame.height
        drag(moved, from: CGVector(dx: 0.5, dy: (h - 3) / h), by: CGVector(dx: 0, dy: 52))
        try await expect(api, id, start: at10.addingTimeInterval(2 * 3600), end: at10.addingTimeInterval(4 * 3600), "растяжение")
        UIFlow.shot(app, "cal-3-resized", self)

        if wide {
            // Неделя: перенос на соседний день (на колонку вправо) и на 15 минут раньше.
            segment("Неделя", app).tap()
            let w = event(title, app)
            XCTAssertTrue(w.waitForExistence(timeout: 10))
            sleep(1)
            UIFlow.shot(app, "cal-4-week", self)
            let header = app.descendants(matching: .any).matching(identifier: "cal.week.header").firstMatch
            let timeline = app.descendants(matching: .any).matching(identifier: "cal.timeline").firstMatch
            // S-06: заголовки дней — сразу под первой строкой, шкала — под ними.
            XCTAssertLessThan(header.frame.height, 60, "заголовок недели раздут: \(header.frame)")
            // Между ними — только строка «весь день», если она есть (до ~3 плашек).
            XCTAssertLessThan(abs(timeline.frame.minY - header.frame.maxY), 80, "шкала недели не под заголовками")
            let column = w.frame.width + 2
            let dayIndex = (cal.component(.weekday, from: tomorrow) + 5) % 7
            let dx: CGFloat = dayIndex < 6 ? column : -column
            drag(w, from: CGVector(dx: 0.5, dy: 0.4), by: CGVector(dx: dx, dy: -13))
            let shiftDays = dayIndex < 6 ? 1 : -1
            let s = cal.date(byAdding: .day, value: shiftDays, to: at10.addingTimeInterval(2 * 3600 - 900))!
            let e = cal.date(byAdding: .day, value: shiftDays, to: at10.addingTimeInterval(4 * 3600 - 900))!
            try await expect(api, id, start: s, end: e, "перенос между днями")
            UIFlow.shot(app, "cal-5-week-moved", self)
        }
    }

    /// «Год» (iPad, Мак): 12 мини-месяцев; касание названия — месяц.
    @MainActor
    func testYearViewAndMonthLongPress() throws {
        let (email, password) = try Self.credentials()
        let app = UIFlow.launch()
        UIFlow.login(email, password, app, self)
        Nav.go("Календарь", app)
        XCTAssertTrue(app.buttons["Сегодня"].waitForExistence(timeout: 10), "календарь не открылся")
        let title = app.staticTexts["cal.title"]
        let year = Calendar.current.component(.year, from: Date())
        if segment("Год", app).waitForExistence(timeout: 3) {
            segment("Год", app).tap()
            XCTAssertTrue(app.descendants(matching: .any).matching(identifier: "cal.year").firstMatch.waitForExistence(timeout: 5), "нет вида «Год»")
            XCTAssertEqual(title.shownText, "\(year)")
            sleep(1)
            UIFlow.shot(app, "year-1", self)
            app.buttons["Вперёд"].tap()
            XCTAssertEqual(title.shownText, "\(year + 1)")
            app.buttons["Назад"].tap()
            let march = app.descendants(matching: .any).matching(identifier: "cal.year.month.3").firstMatch
            XCTAssertTrue(march.waitForExistence(timeout: 5))
            march.tap()
            XCTAssertEqual(title.shownText, "Март \(year)")
            XCTAssertTrue(chosen(segment("Месяц", app)), "касание месяца не открыло «Месяц»")
            sleep(1)
            UIFlow.shot(app, "year-2-march", self)
            // Месяц на всю высоту: последняя строка недель — у нижнего края.
            app.buttons["Сегодня"].tap()
            sleep(2)
            UIFlow.shot(app, "month-1-full-height", self)
            let day = try XCTUnwrap(visible(Shown.begins("15, событий:"), app), "нет 15-го числа")
            day.press(forDuration: 1.0)
        } else {
            // iPhone: лента без предела — 30 месяцев вперёд и назад кнопками.
            let now = Date()
            for _ in 0..<30 { app.buttons["Вперёд"].tap() }
            let ahead = Self.monthTitle(Calendar.current.date(byAdding: .month, value: 30, to: now)!)
            XCTAssertEqual(title.shownText, ahead, "лента не дошла на 30 месяцев вперёд")
            // Не только заголовок: лента и правда прокрутилась к этому месяцу.
            XCTAssertNotNil(visible(NSCompoundPredicate(andPredicateWithSubpredicates: [Shown.equals(ahead), NSPredicate(format: "NOT identifier == 'cal.title'")]), app), "месяца +30 нет на экране")
            UIFlow.shot(app, "feed-1-plus30", self)
            for _ in 0..<60 { app.buttons["Назад"].tap() }
            let back = Self.monthTitle(Calendar.current.date(byAdding: .month, value: -30, to: now)!)
            XCTAssertEqual(title.shownText, back, "лента не дошла на 30 месяцев назад")
            XCTAssertNotNil(visible(NSCompoundPredicate(andPredicateWithSubpredicates: [Shown.equals(back), NSPredicate(format: "NOT identifier == 'cal.title'")]), app), "месяца −30 нет на экране")
            UIFlow.shot(app, "feed-2-minus30", self)
            app.buttons["Сегодня"].tap()
            sleep(1)
            let day = try XCTUnwrap(visible(Shown.begins("15, событий:"), app), "нет 15-го числа")
            day.press(forDuration: 1.0)
        }
        // Долгое нажатие на день — форма нового события; без сохранения.
        XCTAssertTrue(app.navigationBars["Новое событие"].waitForExistence(timeout: 5)
                      || app.staticTexts.matching(Shown.equals("Новое событие")).firstMatch.waitForExistence(timeout: 1), "долгое нажатие на день не открыло форму")
        UIFlow.shot(app, "month-2-new-event", self)
        app.buttons["Отмена"].firstMatch.tap()
    }

    // MARK: Помощники

    static func credentials() throws -> (String, String) {
        let env = ProcessInfo.processInfo.environment
        guard let email = env["DDMAIL_TEST_EMAIL"], let password = env["DDMAIL_TEST_PASSWORD"], !email.isEmpty else {
            throw XCTSkip("нет DDMAIL_TEST_EMAIL / DDMAIL_TEST_PASSWORD")
        }
        return (email, password)
    }

    static func monthTitle(_ d: Date) -> String {
        var c = Calendar.current
        c.locale = Locale(identifier: "ru_RU")
        let m = c.component(.month, from: d)
        return "\(c.standaloneMonthSymbols[m - 1].capitalized) \(c.component(.year, from: d))"
    }

    /// Переключатель вида: на iOS — сегменты, на Маке — радиокнопки.
    @MainActor
    private func segment(_ name: String, _ app: XCUIApplication) -> XCUIElement {
        #if os(macOS)
        return app.radioButtons[name].firstMatch
        #else
        return app.segmentedControls.buttons[name].firstMatch
        #endif
    }

    /// Выбран ли сегмент: на iOS — `isSelected`, у радиокнопки Мака — значение 1.
    @MainActor
    private func chosen(_ e: XCUIElement) -> Bool {
        if e.isSelected { return true }
        if let n = e.value as? NSNumber { return n.intValue == 1 }
        return (e.value as? String) == "1"
    }

    /// Первый видимый элемент: в ленте месяцев те же числа есть и у соседних месяцев.
    @MainActor
    private func visible(_ p: NSPredicate, _ app: XCUIApplication) -> XCUIElement? {
        let all = app.descendants(matching: .any).matching(p)
        let deadline = Date().addingTimeInterval(10)
        repeat {
            if let hit = all.allElementsBoundByIndex.first(where: { $0.isHittable }) { return hit }
            sleep(1)
        } while Date() < deadline
        return nil
    }

    @MainActor
    private func event(_ title: String, _ app: XCUIApplication) -> XCUIElement {
        app.descendants(matching: .any).matching(identifier: "cal.event")
            .matching(Shown.contains(title)).firstMatch
    }

    /// Долгое нажатие и ведение (на Маке — нажатие кнопки мыши и ведение).
    @MainActor
    private func drag(_ e: XCUIElement, from: CGVector, by: CGVector) {
        let start = e.coordinate(withNormalizedOffset: from)
        #if os(macOS)
        // На Маке `press(forDuration:thenDragTo:)` мышь не двигает (это жест
        // трекпада) — плашка не получала ни одного события. Мышью — click-drag.
        start.click(forDuration: 0.3, thenDragTo: start.withOffset(by), withVelocity: .slow, thenHoldForDuration: 0.4)
        #else
        start.press(forDuration: 0.8, thenDragTo: start.withOffset(by), withVelocity: .slow, thenHoldForDuration: 0.4)
        #endif
        sleep(1)
    }

    /// Сервер получил новое время (ждём до 20 с).
    @MainActor
    private func expect(_ api: TestCalendarAPI, _ id: Int64, start: Date, end: Date, _ what: String) async throws {
        let deadline = Date().addingTimeInterval(20)
        var last: (Int64, Int64)?
        while Date() < deadline {
            last = try await api.times(id, around: start)
            if let last, last == (Int64(start.timeIntervalSince1970 * 1000), Int64(end.timeIntervalSince1970 * 1000)) { return }
            try await Task.sleep(for: .seconds(1))
        }
        XCTFail("\(what): на сервере \(String(describing: last.map { (Date(timeIntervalSince1970: Double($0.0) / 1000), Date(timeIntervalSince1970: Double($0.1) / 1000)) })), ждали \(start)–\(end)")
    }
}

/// Прямой доступ к календарю сервера — завести и убрать тестовое событие.
struct TestCalendarAPI: Sendable {
    let base: URL
    let token: String

    static func login(email: String, password: String) async throws -> TestCalendarAPI {
        let env = ProcessInfo.processInfo.environment
        let base = URL(string: (env["DDMAIL_TEST_SERVER"] ?? "https://mail.letotam.ru") + "/api/desktop/v1")!
        let user = email.split(separator: "@").first.map(String.init) ?? email
        let r = try await call(base.appending(path: "auth/login"), "POST", token: nil, body: ["username": user, "password": password])
        guard let token = (r as? [String: Any])?["token"] as? String else { throw XCTSkip("вход в API не удался") }
        return TestCalendarAPI(base: base, token: token)
    }

    /// Записываемый календарь событий: «Персональный календарь», иначе первый подходящий.
    func calendarID() async throws -> Int64 {
        let list = try await Self.call(base.appending(path: "calendars"), "GET", token: token) as? [[String: Any]] ?? []
        let ok = list.filter { ($0["can_write"] as? Bool) == true && (($0["components"] as? [String])?.contains("VEVENT") ?? true) }
        let pick = ok.first { ($0["name"] as? String) == "Персональный календарь" } ?? ok.first
        guard let id = (pick?["id"] as? NSNumber)?.int64Value else { throw XCTSkip("нет календаря на запись") }
        return id
    }

    func create(_ title: String, start: Date, end: Date) async throws -> Int64 {
        let cal = try await calendarID()
        _ = try await Self.call(base.appending(path: "events"), "POST", token: token, body: [
            "calendar_id": cal, "summary": title, "description": "", "location": "",
            "dtstart": Int64(start.timeIntervalSince1970 * 1000), "dtend": Int64(end.timeIntervalSince1970 * 1000), "all_day": false,
        ])
        let deadline = Date().addingTimeInterval(20)
        while Date() < deadline {
            if let e = try await events(around: start).first(where: { ($0["summary"] as? String) == title }),
               let id = (e["id"] as? NSNumber)?.int64Value { return id }
            try await Task.sleep(for: .seconds(1))
        }
        throw XCTSkip("тестовое событие не появилось на сервере")
    }

    func events(around d: Date) async throws -> [[String: Any]] {
        let from = Int64(d.timeIntervalSince1970 * 1000) - 3 * 86_400_000, to = from + 6 * 86_400_000
        var u = URLComponents(url: base.appending(path: "calendar-events"), resolvingAgainstBaseURL: false)!
        u.queryItems = [URLQueryItem(name: "from", value: "\(from)"), URLQueryItem(name: "to", value: "\(to)")]
        return try await Self.call(u.url!, "GET", token: token) as? [[String: Any]] ?? []
    }

    func times(_ id: Int64, around d: Date) async throws -> (Int64, Int64)? {
        guard let e = try await events(around: d).first(where: { ($0["id"] as? NSNumber)?.int64Value == id }),
              let s = (e["dtstart"] as? NSNumber)?.int64Value, let en = (e["dtend"] as? NSNumber)?.int64Value else { return nil }
        return (s, en)
    }

    func delete(_ id: Int64) async throws {
        _ = try await Self.call(base.appending(path: "events/\(id)"), "DELETE", token: token)
    }

    static func call(_ url: URL, _ method: String, token: String?, body: [String: Any]? = nil) async throws -> Any? {
        var r = URLRequest(url: url)
        r.httpMethod = method
        if let token { r.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization") }
        if let body {
            r.setValue("application/json", forHTTPHeaderField: "Content-Type")
            r.httpBody = try JSONSerialization.data(withJSONObject: body)
        }
        let (data, resp) = try await URLSession.shared.data(for: r)
        guard let http = resp as? HTTPURLResponse, (200..<300).contains(http.statusCode) else {
            throw NSError(domain: "api", code: (resp as? HTTPURLResponse)?.statusCode ?? 0,
                          userInfo: [NSLocalizedDescriptionKey: String(decoding: data, as: UTF8.self)])
        }
        return data.isEmpty ? nil : try? JSONSerialization.jsonObject(with: data)
    }
}
