import Foundation
import Synchronization
import Testing
@testable import DDMailAPI

/// Запись задач: POST/PATCH/DELETE /tasks — пути, тела, разбор ответа.
struct TaskWriteTests {
    private let base = "https://mail.example.ru"
    private let taskJSON = #"{"id":101,"calendar_id":1,"summary":"Оплатить","due":1794528000000,"all_day":true,"status":"NEEDS-ACTION","completed":false,"can_write":true,"calendar_name":"Напоминания"}"#

    private func bodies(_ t: FakeTransportWithBodies) -> [[String: Any]] {
        t.bodies.withLock { $0 }.map { (try? JSONSerialization.jsonObject(with: $0)) as? [String: Any] ?? [:] }
    }

    @Test func createPostsAndDecodesTask() async throws {
        let t = FakeTransportWithBodies { _ in (201, taskJSON) }
        let api = SessionAPI(baseURL: base, transport: t, tokens: MemoryTokens("T"))
        let task = try await api.createTask(TaskCreateRequest(calendarID: 1, summary: "Оплатить", description: "до пятницы", due: 1_794_528_000_000, allDay: true))
        #expect(task.id == 101)
        #expect(task.calendarName == "Напоминания")
        #expect(t.log.withLock { $0 } == ["POST /api/desktop/v1/tasks Bearer T"])
        let b = bodies(t)[0]
        #expect(b["calendar_id"] as? Int == 1)
        #expect(b["summary"] as? String == "Оплатить")
        #expect(b["description"] as? String == "до пятницы")
        #expect(b["due"] as? Int64 == 1_794_528_000_000)
        #expect(b["all_day"] as? Bool == true)
    }

    @Test func createWithoutDueSendsNoDue() async throws {
        let t = FakeTransportWithBodies { _ in (201, taskJSON) }
        _ = try await SessionAPI(baseURL: base, transport: t, tokens: MemoryTokens("T")).createTask(TaskCreateRequest(calendarID: 1, summary: "x"))
        #expect(bodies(t)[0]["due"] == nil)
    }

    @Test func patchSendsOnlyGivenFields() async throws {
        let t = FakeTransportWithBodies { _ in (200, taskJSON) }
        _ = try await SessionAPI(baseURL: base, transport: t, tokens: MemoryTokens("T")).patchTask(id: 101, TaskPatchRequest(summary: "Новое", due: 0))
        #expect(t.log.withLock { $0 } == ["PATCH /api/desktop/v1/tasks/101 Bearer T"])
        #expect(Set(bodies(t)[0].keys) == ["summary", "due"])
    }

    @Test func deleteUsesDelete() async throws {
        let t = FakeTransportWithBodies { _ in (204, "") }
        try await SessionAPI(baseURL: base, transport: t, tokens: MemoryTokens("T")).deleteTask(id: 101)
        #expect(t.log.withLock { $0 } == ["DELETE /api/desktop/v1/tasks/101 Bearer T"])
    }

    @Test func readOnlyIsServerErrorNotLogout() async {
        let t = FakeTransportWithBodies { _ in (403, #"{"error":"calendar is read-only"}"#) }
        await #expect(throws: APIError.server(code: 403, message: "calendar is read-only")) {
            _ = try await SessionAPI(baseURL: base, transport: t, tokens: MemoryTokens("T")).createTask(TaskCreateRequest(calendarID: 3, summary: "x"))
        }
    }

    @Test func calendarsTellTaskLists() throws {
        let json = #"""
        [{"id":1,"source_type":"caldav","can_write":true,"components":["VTODO"]},
         {"id":2,"source_type":"caldav","can_write":true,"components":["VEVENT"]},
         {"id":3,"source_type":"ics_url","can_write":true,"components":["VEVENT","VTODO"]},
         {"id":4,"source_type":"caldav","can_write":true}]
        """#
        let cals = try JSONDecoder().decode([CalendarDTO].self, from: Data(json.utf8))
        #expect(cals.filter(\.acceptsTasks).map(\.id) == [1])
        #expect(cals[3].components.isEmpty)
    }
}

/// Как `FakeTransport`, но ещё и запоминает тела запросов.
final class FakeTransportWithBodies: HTTPTransport {
    typealias Handler = @Sendable (URLRequest) async throws -> (Int, String)
    private let handler: Handler
    let log = Mutex<[String]>([])
    let bodies = Mutex<[Data]>([])

    init(_ handler: @escaping Handler) { self.handler = handler }

    func send(_ request: URLRequest) async throws -> (Data, HTTPURLResponse) {
        let line = "\(request.httpMethod ?? "GET") \(request.url!.path()) \(request.value(forHTTPHeaderField: "Authorization") ?? "-")"
        log.withLock { $0.append(line) }
        if let b = request.httpBody { bodies.withLock { $0.append(b) } }
        let (code, body) = try await handler(request)
        return (Data(body.utf8), HTTPURLResponse(url: request.url!, statusCode: code, httpVersion: nil, headerFields: nil)!)
    }
}
