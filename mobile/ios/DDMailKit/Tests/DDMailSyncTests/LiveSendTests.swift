import DDMailAPI
import DDMailNet
import DDMailStore
import Foundation
import Testing
@testable import DDMailSync

/// Живая отправка письма самому себе (контракт внешних учёток §5, §11):
/// SMTP учётки, затем копия в «Отправленных» (сервер кладёт сам или APPEND),
/// и письмо приходит во «Входящие». Только по явному
/// `DDMAIL_LIVE_SEND=1`; адрес получателя — адрес самой учётки.
struct LiveSendTests {
    static var env: [String: String] { ProcessInfo.processInfo.environment }

    @Test(.enabled(if: env["DDMAIL_LIVE_SEND"] == "1"))
    func sendToSelf() async throws {
        let e = Self.env
        let user = e["DDMAIL_LIVE_USER"]!, pw = e["DDMAIL_LIVE_PASSWORD"]!, email = e["DDMAIL_LIVE_EMAIL"]!
        let ih = e["DDMAIL_LIVE_IMAP"]!.split(separator: ":"), sh = e["DDMAIL_LIVE_SMTP"]!.split(separator: ":")
        let ip = Int(ih[1])!, sp = Int(sh[1])!
        let store = try MailStore.inMemory()
        let cfg = ImapAccountConfig(
            key: "\(user)@\(ih[0])", email: email, displayName: "Проверка iOS", login: user,
            imap: MailEndpoint(host: String(ih[0]), port: ip, security: .imap(port: ip)),
            smtp: MailEndpoint(host: String(sh[0]), port: sp, security: .smtp(port: sp)),
            smtpLogin: "", smtpAuth: true, ownAddresses: [email]
        )
        let engine = ImapEngine(config: cfg, store: store, password: { pw })
        try await engine.sync()
        let subject = "Проверка отправки iOS \(Int(Date().timeIntervalSince1970))"
        try await engine.send(SendRequest(
            from: "\"Проверка iOS\" <\(email)>", to: [email], subject: subject,
            html: "<div>Письмо самому себе: <b>внешняя учётка</b>, iOS.</div>", text: "Письмо самому себе: внешняя учётка, iOS.",
            attachments: [OutgoingAttachment(filename: "проверка.txt", mimeType: "text/plain", content: Data("вложение".utf8))]
        ))
        // Копия в «Отправленных» — через 5 с поиск, не нашлась за 15 с — APPEND; ждём и синкаем.
        var inSent = false, inInbox = false
        for _ in 0..<12 {
            try await Task.sleep(for: .seconds(5))
            try await engine.sync()
            let convs = try engine.currentConversations()
            let refs = convs.flatMap(\.messages)
            let rows = try store.imapHeaders(cfg.key, roles: ["sent", "inbox"]).filter { $0.subject == subject }
            inSent = rows.contains { $0.role == "sent" }
            inInbox = rows.contains { $0.role == "inbox" }
            _ = refs
            if inSent && inInbox { break }
        }
        print("отправлено «\(subject)»: в Отправленных \(inSent), во Входящих \(inInbox)")
        let sentCount = try store.imapHeaders(cfg.key, roles: ["sent"]).filter { $0.subject == subject }.count
        print("копий в Отправленных: \(sentCount)")
        #expect(inSent)
        // Сервер ddmail письмо себе может не класть во «Входящие» (только «Отправленные»).
        if e["DDMAIL_LIVE_EXPECT_INBOX"] == "1" { #expect(inInbox) }
        #expect(sentCount == 1)
        // Тело своего письма разбирается: вложение на месте.
        if let c = try engine.currentConversations().first(where: { $0.lastSubject == subject }) {
            let b = try await engine.bodies(c.messages)
            print("вложения: \(b.flatMap(\.attachments).map(\.filename)), исходящее: \(b.map(\.isOutgoing))")
            #expect(b.contains { $0.attachments.contains { $0.filename == "проверка.txt" } })
        }
    }
}
