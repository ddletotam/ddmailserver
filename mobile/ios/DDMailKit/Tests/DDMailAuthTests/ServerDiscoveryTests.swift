import Synchronization
import Testing
@testable import DDMailAuth

/// Векторы `ServerDiscoveryTest.kt` Android. Время — настоящее, но малое:
/// проверяем порядок и то, кого ждём, а не точные миллисекунды.
struct ServerDiscoveryTests {
    private func discovery(
        alive: [String: Int],
        mx: [String] = [],
        mxDelay: Int = 10,
        deadDelay: Int = 1000
    ) -> ServerDiscovery {
        ServerDiscovery(
            probe: { host in
                let d = alive[host]
                try? await Task.sleep(for: .milliseconds(d ?? deadDelay))
                return d != nil && !Task.isCancelled
            },
            mx: { _ in
                try? await Task.sleep(for: .milliseconds(mxDelay))
                return mx
            }
        )
    }

    private final class Seen: Sendable {
        let all = Mutex<[DiscoveryProgress]>([])
        func add(_ p: DiscoveryProgress) { all.withLock { $0.append(p) } }
        var last: DiscoveryProgress? { all.withLock { $0.last } }
        var monotonic: Bool { all.withLock { a in zip(a, a.dropFirst()).allSatisfy { $1.done >= $0.done } } }
    }

    @Test func mailSubdomainWinsAndDoesNotWaitForMx() async {
        let seen = Seen()
        let start = ContinuousClock.now
        let url = await discovery(alive: ["mail.example.org": 50], mxDelay: 5000)
            .find(email: "bob@example.org") { seen.add($0) }
        #expect(url == "https://mail.example.org")
        #expect(ContinuousClock.now - start < .seconds(1), "не ждали MX")
        #expect(seen.last?.done == ServerDiscovery.total)
    }

    @Test func laterCandidateWaitsForEarlierOnly() async {
        // Сам домен отвечает быстро, но mail.<домен> стоит раньше: ждём его отказа.
        let start = ContinuousClock.now
        let url = await discovery(alive: ["example.org": 10], deadDelay: 300).find(email: "a@example.org") { _ in }
        #expect(url == "https://example.org")
        #expect(ContinuousClock.now - start >= .milliseconds(300))
    }

    @Test func foundOnMxHost() async {
        let url = await discovery(
            alive: ["mx1.hosting.ru": 20],
            mx: ["mx1.hosting.ru", "mx2.hosting.ru", "mx3.hosting.ru"],
            deadDelay: 100
        ).find(email: "a@corp.ru") { _ in }
        #expect(url == "https://mx1.hosting.ru")
    }

    @Test func nothingFoundFillsProgress() async {
        let seen = Seen()
        let url = await discovery(alive: [:], mx: ["mail.x.ru"], deadDelay: 50).find(email: "a@x.ru") { seen.add($0) }
        #expect(url == nil)
        #expect(seen.last?.done == ServerDiscovery.total)
        #expect(seen.monotonic)
    }

    @Test func notAnAddress() async {
        #expect(await discovery(alive: [:]).find(email: "bob") { _ in } == nil)
    }
}

struct MXRecordsTests {
    @Test func parsesPreferenceAndName() {
        // 10 mx1.example.ru.
        let rdata: [UInt8] = [0, 10, 3] + Array("mx1".utf8) + [7] + Array("example".utf8) + [2] + Array("ru".utf8) + [0]
        let r = parseMXRData(rdata)
        #expect(r?.preference == 10)
        #expect(r?.host == "mx1.example.ru")
    }

    @Test func rejectsBroken() {
        #expect(parseMXRData([]) == nil)
        #expect(parseMXRData([0, 10, 5, 0x61]) == nil) // метка обрезана
        #expect(parseMXRData([0, 10, 0xC0, 12]) == nil) // указатель в RDATA
        #expect(parseMXRData([0, 10, 0]) == nil) // пустое имя
    }

    @Test func orderedByPreferenceWithoutDuplicates() {
        let hosts = orderedMXHosts([(20, "b.ru"), (10, "a.ru"), (20, "c.ru"), (30, "a.ru")])
        #expect(hosts == ["a.ru", "b.ru", "c.ru"])
    }
}

struct SessionTests {
    @Test func accountKeyIsLoginAtHost() {
        let s = Session(serverURL: "https://mail.letotam.ru", username: "lucky", userID: 1, token: "t")
        #expect(s.accountKey == "lucky@mail.letotam.ru")
    }
}
