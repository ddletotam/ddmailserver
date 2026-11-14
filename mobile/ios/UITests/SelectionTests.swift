import XCTest

/// Выделение текста письма (контракт §8): двойной тап — слово, тройной —
/// фраза, у выделения системные маркеры и «Скопировать»; долгое нажатие —
/// по-прежнему меню письма. На настоящем сервере, на своём письме: тест сам
/// кладёт себе письмо с известным текстом (API `send`, свой адрес по умолчанию → он же)
/// и удаляет его в конце, что бы ни случилось. Раньше текст брался из отказа о
/// недоставке, который «уже есть» у учётки, — новые отказы меняли и выдачу
/// поиска, и текст под пальцем.
final class SelectionTests: XCTestCase {
    /// Первое слово тела — его выделяет двойной тап.
    static let firstWord = "Выделение"
    /// Шрифт ленты (`conversation.css`: 15px, межстрочный 1.45).
    static let fontSize: CGFloat = 15
    static let lineHeight: CGFloat = 15 * 1.45

    override func setUp() {
        continueAfterFailure = false
    }

    #if os(iOS)
    @MainActor
    func testDoubleAndTripleTapSelect() async throws {
        let env = ProcessInfo.processInfo.environment
        guard let email = env["DDMAIL_TEST_EMAIL"], let password = env["DDMAIL_TEST_PASSWORD"], !email.isEmpty else {
            throw XCTSkip("нет DDMAIL_TEST_EMAIL / DDMAIL_TEST_PASSWORD")
        }
        // Своё письмо: одна фраза, начинается с известного слова, метка — в ней же.
        let stamp = Self.stamp()
        let subject = "Проверка ddmail выделение \(stamp)"
        let body = "\(Self.firstWord) текста письма проверяется двойным и тройным касанием, метка \(stamp)."
        let api = try await TestMailAPI.login(email: email, password: password)
        let me = try await api.defaultIdentity()
        let mid = try await api.send(from: me, to: me, subject: subject, text: body)
        addTeardownBlock { await api.delete(mid) }
        try await api.waitDelivered(mid)

        let app = UIFlow.launch()
        UIFlow.login(email, password, app, self)
        UIFlow.settle(app)

        // Беседа с письмом — поиском по метке: тема последнего письма беседы
        // («Диалоги»), иначе — серверный поиск («Сообщения»).
        let search = app.textFields["search.field"].firstMatch
        XCTAssertTrue(search.waitForExistence(timeout: 10), "нет поля поиска")
        search.tap()
        search.typeText(stamp)
        let hit = app.descendants(matching: .any)
            .matching(NSPredicate(format: "identifier IN %@", ["search.conv", "search.message"])).firstMatch
        XCTAssertTrue(hit.waitForExistence(timeout: 20), "поиск не нашёл беседу со своим письмом")
        hit.tap()
        let web = app.webViews.firstMatch
        XCTAssertTrue(web.waitForExistence(timeout: 20), "лента беседы не появилась")
        let texts = web.staticTexts.matching(Shown.contains(stamp))
        XCTAssertTrue(texts.firstMatch.waitForExistence(timeout: 30), "нет текста письма")
        sleep(2)
        // Видимая нижняя строка с меткой.
        let text = texts.allElementsBoundByIndex.last { $0.isHittable } ?? texts.firstMatch
        XCTAssertTrue(text.label.hasPrefix(Self.firstWord), "текст письма не тот: «\(text.label)»")
        UIFlow.shot(app, "select-0-open", self)

        // Двойной тап — слово, маркеры и меню с «Скопировать».
        let copy = app.descendants(matching: .any)
            .matching(NSPredicate(format: "label IN %@", ["Скопировать", "Копировать", "Copy"])).firstMatch
        let grabber = app.descendants(matching: .any)
            .matching(NSPredicate(format: "identifier BEGINSWITH %@", "com.apple.text.grabber")).firstMatch
        // Середина первого слова в первой строке — от левого верхнего угла текста.
        let wordWidth = Self.width(Self.firstWord)
        let origin = text.coordinate(withNormalizedOffset: .zero)
        let word = origin.withOffset(CGVector(dx: wordWidth / 2, dy: Self.lineHeight / 2))
        word.doubleTap()
        sleep(1)
        UIFlow.shot(app, "select-1-word", self)
        // Маркеры в дереве доступности есть не всегда — они на снимке.
        _ = grabber.waitForExistence(timeout: 2)
        XCTAssertTrue(copy.waitForExistence(timeout: 5), "у выделения слова нет «Скопировать»")

        copy.tap()
        sleep(1)
        let copied = pasted(app)
        XCTAssertFalse(copied.isEmpty, "«Скопировать» ничего не скопировало")
        XCTAssertEqual(copied, Self.firstWord, "двойной тап выделил не первое слово")

        // Маркер — системный: взять его — лупа и системное меню выделения
        // («Скопировать / Найти / Перевести»). Протяжку синтетическим жестом
        // XCUITest WebKit отрабатывает лишь на символ, поэтому здесь
        // проверяется только, что маркер живой и после него копируется.
        // Правый маркер — у конца первого слова, внизу первой строки; на iPad
        // шаг пропускается.
        if UIDevice.current.userInterfaceIdiom == .phone {
            word.doubleTap()
            sleep(1)
            XCTAssertTrue(copy.waitForExistence(timeout: 5), "у выделения слова нет «Скопировать»")
            let handle = origin.withOffset(CGVector(dx: wordWidth, dy: Self.lineHeight - 2))
            handle.press(forDuration: 0.4, thenDragTo: handle.withOffset(CGVector(dx: 164, dy: 47)),
                         withVelocity: .slow, thenHoldForDuration: 0.5)
            sleep(1)
            UIFlow.shot(app, "select-1b-handle", self)
            XCTAssertTrue(copy.waitForExistence(timeout: 3), "после маркера нет «Скопировать»")
            copy.tap()
            sleep(1)
            XCTAssertTrue(pasted(app).hasPrefix(copied), "после маркера скопировалось не выделение")
        }

        // Тройной тап — фраза.
        text.tap(withNumberOfTaps: 3, numberOfTouches: 1)
        sleep(1)
        UIFlow.shot(app, "select-2-sentence", self)
        XCTAssertTrue(copy.waitForExistence(timeout: 5), "у выделения фразы нет «Скопировать»")
        copy.tap()
        sleep(1)
        let phrase = pasted(app)
        XCTAssertTrue(phrase.contains(" "), "тройной тап выделил не фразу: «\(phrase)»")

        // Одиночный тап снимает выделение, долгое нажатие — меню письма.
        text.tap()
        sleep(1)
        UIFlow.contextMenu(text)
        let reply = UIFlow.menuItem("Ответить", app)
        XCTAssertTrue(reply.waitForExistence(timeout: 5), "долгое нажатие не открыло меню письма")
        sleep(1)
        UIFlow.shot(app, "select-3-bubble-menu", self)
        // Системное выделение с меню «Найти / Перевести» поверх меню письма не начинается.
        let system = app.descendants(matching: .any)
            .matching(NSPredicate(format: "label IN %@", ["Найти", "Перевести", "Translate", "Look Up"])).firstMatch
        XCTAssertFalse(system.exists, "долгое нажатие начало и системное выделение")
    }

    /// Ширина слова шрифтом ленты (system-ui 15px = системный шрифт 15 pt).
    static func width(_ s: String) -> CGFloat {
        (s as NSString).size(withAttributes: [.font: UIFont.systemFont(ofSize: fontSize)]).width
    }

    static func stamp() -> String {
        let f = DateFormatter()
        f.dateFormat = "yyyyMMdd-HHmmss"
        return f.string(from: Date())
    }

    /// Что лежит в буфере обмена — вставкой в строку ввода (так, как сделал
    /// бы человек): буфер приложения тест читать не может. Строка ввода
    /// после этого очищается.
    @MainActor
    private func pasted(_ app: XCUIApplication) -> String {
        let field = app.textViews["composer.text"].firstMatch
        XCTAssertTrue(field.waitForExistence(timeout: 5), "нет строки ввода")
        field.tap()
        field.press(forDuration: 1.2)
        let paste = app.menuItems.matching(NSPredicate(format: "label IN %@", ["Вставить", "Paste"])).firstMatch
        guard paste.waitForExistence(timeout: 5) else { return "" }
        paste.tap()
        let allow = app.alerts.buttons.matching(NSPredicate(format: "label IN %@", ["Разрешить вставку", "Allow Paste"])).firstMatch
        if allow.waitForExistence(timeout: 2) { allow.tap() }
        sleep(1)
        let text = (field.value as? String ?? "").trimmingCharacters(in: .whitespacesAndNewlines)
        // Очистить: выделить всё и удалить.
        field.press(forDuration: 1.2)
        let all = app.menuItems.matching(NSPredicate(format: "label IN %@", ["Выбрать все", "Select All"])).firstMatch
        if all.waitForExistence(timeout: 3) {
            all.tap()
            field.typeText(XCUIKeyboardKey.delete.rawValue)
        }
        // Клавиатура закрывает ленту (своё письмо — внизу, под ней) — убрать
        // её первым тапом по ленте (контракт §8), в поле слева от пузырей, и
        // дождаться. Тап по шапке беседы её не убирал, и следующий двойной тап
        // приходился на клавиши.
        let keyboard = app.keyboards.firstMatch
        if keyboard.exists {
            let web = app.webViews.firstMatch
            web.coordinate(withNormalizedOffset: .zero).withOffset(CGVector(dx: 4, dy: web.frame.height * 0.3)).tap()
        }
        XCTAssertTrue(UIFlow.waitGone(keyboard, 5), "клавиатура не закрылась")
        sleep(1)
        return text
    }
    #endif
}

/// Почта сервера напрямую — положить себе тестовое письмо и убрать его.
/// Письмо находится по своему Message-ID (ответ `send`), а не поиском
/// сервера: свежие письма попадают в индекс поиска не сразу.
struct TestMailAPI: Sendable {
    let api: TestCalendarAPI

    static func login(email: String, password: String) async throws -> TestMailAPI {
        TestMailAPI(api: try await TestCalendarAPI.login(email: email, password: password))
    }

    /// Адрес отправителя по умолчанию — свой ящик на сервере.
    func defaultIdentity() async throws -> String {
        let list = try await TestCalendarAPI.call(api.base.appending(path: "identities"), "GET", token: api.token) as? [[String: Any]] ?? []
        let pick = list.first { ($0["is_default"] as? Bool) == true } ?? list.first
        guard let email = pick?["email"] as? String, !email.isEmpty else { throw XCTSkip("у учётки нет адреса отправителя") }
        return email
    }

    /// Отправить; вернуть Message-ID письма (без угловых скобок).
    func send(from: String, to: String, subject: String, text: String) async throws -> String {
        let r = try await TestCalendarAPI.call(api.base.appending(path: "send"), "POST", token: api.token, body: [
            "from": from, "to": [to], "subject": subject, "text": text, "html": "",
        ]) as? [String: Any]
        guard let mid = r?["rfc_message_id"] as? String, !mid.isEmpty else { throw XCTSkip("send не вернул Message-ID") }
        return Self.bare(mid)
    }

    /// Строки письма в беседах, изменённых за последние полчаса.
    func refs(_ messageID: String) async throws -> [[String: Any]] {
        var u = URLComponents(url: api.base.appending(path: "conversations"), resolvingAgainstBaseURL: false)!
        let since = Int64((Date().timeIntervalSince1970 - 1800) * 1000)
        u.queryItems = [URLQueryItem(name: "since", value: "\(since)")]
        let r = try await TestCalendarAPI.call(u.url!, "GET", token: api.token) as? [String: Any]
        let convs = r?["conversations"] as? [[String: Any]] ?? []
        return convs.flatMap { ($0["messages"] as? [[String: Any]]) ?? [] }
            .filter { Self.bare(($0["message_id"] as? String) ?? "") == messageID }
    }

    /// Письмо доставлено (статус копии в «Отправленных»), до 60 с.
    func waitDelivered(_ messageID: String) async throws {
        let deadline = Date().addingTimeInterval(60)
        var last: [[String: Any]] = []
        while Date() < deadline {
            last = try await refs(messageID)
            if last.contains(where: { ($0["delivery_status"] as? String) == "delivered" }) { return }
            try await Task.sleep(for: .seconds(1))
        }
        XCTFail("тестовое письмо не доставлено за минуту: \(last)")
    }

    /// Удалить письмо — все его строки (себе сервер хранит одну, в «Отправленных»).
    func delete(_ messageID: String) async {
        guard let refs = try? await refs(messageID) else { return }
        let list = refs.compactMap { r -> [String: Any]? in
            guard let uid = (r["uid"] as? NSNumber)?.int64Value else { return nil }
            return ["folder": (r["folder"] as? String) ?? "", "uid": uid, "message_id": ""]
        }
        guard !list.isEmpty else { return }
        _ = try? await TestCalendarAPI.call(api.base.appending(path: "messages/delete"), "POST", token: api.token,
                                            body: ["messages": list])
    }

    static func bare(_ id: String) -> String {
        id.trimmingCharacters(in: CharacterSet(charactersIn: "<> "))
    }
}
