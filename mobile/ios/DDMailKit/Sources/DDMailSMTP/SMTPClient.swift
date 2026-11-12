import DDMailNet
import Foundation

/// Ответ SMTP: код и строки (многострочный `250-…`).
public struct SMTPReply: Equatable, Sendable {
    public var code: Int
    public var lines: [String]

    public var text: String { lines.joined(separator: " ") }
}

/// Учётные данные SMTP: `nil` у учётки — сервер без авторизации
/// (внутренний релей), AUTH не посылается вовсе.
public struct SMTPCredentials: Sendable {
    public var user: String
    public var password: String

    public init(user: String, password: String) {
        self.user = user
        self.password = password
    }
}

/// Клиент отправки (RFC 5321) — замена `lettre` десктопа: TLS по порту
/// (465 — сразу, 587/25/2525 — STARTTLS обязателен, открытым текстом не
/// шлём), AUTH PLAIN или LOGIN, точка-экранирование тела.
public actor SMTPClient {
    let stream: LineStream
    private(set) var extensions: [String] = []

    init(stream: LineStream) { self.stream = stream }

    /// Соединиться, поздороваться, включить TLS, войти.
    public static func connect(
        _ endpoint: MailEndpoint, credentials: SMTPCredentials?, connector: MailConnector,
        timeout: Duration = .seconds(25), heloName: String = "localhost"
    ) async throws -> SMTPClient {
        let socket = try await connector.connect(endpoint, timeout: timeout)
        let c = SMTPClient(stream: LineStream(socket: socket, timeout: timeout))
        do {
            try await c.open(startTLS: endpoint.security == .startTLS, credentials: credentials, helo: heloName)
        } catch {
            socket.close()
            throw error
        }
        return c
    }

    private func open(startTLS: Bool, credentials: SMTPCredentials?, helo: String) async throws {
        try await expect(try await read(), 220)
        try await ehlo(helo)
        if startTLS {
            guard has("STARTTLS") else {
                throw MailNetError.server("Сервер исходящей почты не поддерживает STARTTLS — открытым текстом письма не отправляются.")
            }
            try await expect(try await command("STARTTLS"), 220)
            try await stream.startTLS()
            try await ehlo(helo)
        }
        if let credentials { try await auth(credentials) }
    }

    private func ehlo(_ name: String) async throws {
        let r = try await command("EHLO \(name)")
        guard r.code == 250 else {
            throw MailNetError.protocolError("EHLO: \(r.code) \(r.text)")
        }
        extensions = Array(r.lines.dropFirst()).map { $0.uppercased() }
    }

    func has(_ ext: String) -> Bool { extensions.contains { $0 == ext || $0.hasPrefix(ext + " ") } }

    private var authMechanisms: [String] {
        extensions.first { $0.hasPrefix("AUTH ") || $0.hasPrefix("AUTH=") }
            .map { $0.dropFirst(5).split(separator: " ").map(String.init) } ?? []
    }

    private func auth(_ c: SMTPCredentials) async throws {
        let mechs = authMechanisms
        let r: SMTPReply
        if mechs.contains("PLAIN") || !mechs.contains("LOGIN") {
            let token = Data(("\0" + c.user + "\0" + c.password).utf8).base64EncodedString()
            r = try await command("AUTH PLAIN \(token)")
        } else {
            let a = try await command("AUTH LOGIN")
            guard a.code == 334 else { throw authError(a) }
            let b = try await command(Data(c.user.utf8).base64EncodedString())
            guard b.code == 334 else { throw authError(b) }
            r = try await command(Data(c.password.utf8).base64EncodedString())
        }
        guard r.code == 235 else { throw authError(r) }
    }

    /// 535/534/530 — неверный вход (нужен вход); остальное — текст сервера.
    private func authError(_ r: SMTPReply) -> MailNetError {
        [530, 534, 535].contains(r.code) ? .auth(r.text) : .server("\(r.code) \(r.text)")
    }

    /// Отправить письмо. Bcc — только в конверте (RCPT), не в заголовках.
    public func send(from: String, recipients: [String], data: Data) async throws {
        try await expect(try await command("MAIL FROM:<\(from)>"), 250)
        var accepted = 0
        var lastRefusal: SMTPReply?
        for rcpt in recipients {
            let r = try await command("RCPT TO:<\(rcpt)>")
            if r.code == 250 || r.code == 251 { accepted += 1 } else { lastRefusal = r }
        }
        guard accepted > 0 else {
            _ = try? await command("RSET")
            throw MailNetError.server("Сервер не принял адресатов: \(lastRefusal.map { "\($0.code) \($0.text)" } ?? "")")
        }
        if let r = lastRefusal {
            // Хотя бы один адресат отвергнут — письмо не уходит частично.
            _ = try? await command("RSET")
            throw MailNetError.server("Сервер отверг адресата: \(r.code) \(r.text)")
        }
        try await expect(try await command("DATA"), 354)
        try await stream.write(Self.dotStuff(data))
        try await expect(try await read(timeout: .seconds(120)), 250)
    }

    public func quit() async {
        _ = try? await command("QUIT")
        stream.close()
    }

    public func close() { stream.close() }

    // MARK: Протокол

    private func command(_ line: String) async throws -> SMTPReply {
        try await stream.write(line + "\r\n")
        return try await read()
    }

    private func read(timeout: Duration? = nil) async throws -> SMTPReply {
        var lines: [String] = []
        while true {
            let l = try await stream.readLineString(timeout: timeout)
            guard l.count >= 3, let code = Int(l.prefix(3)) else { throw MailNetError.protocolError(l) }
            lines.append(l.count > 4 ? String(l.dropFirst(4)) : "")
            let sep = l.count > 3 ? l[l.index(l.startIndex, offsetBy: 3)] : " "
            if sep != "-" { return SMTPReply(code: code, lines: lines) }
        }
    }

    private func expect(_ r: SMTPReply, _ code: Int) async throws {
        guard r.code == code else {
            if r.code == 421 { throw MailNetError.server("Сервер недоступен: \(r.text)") }
            throw MailNetError.server("\(r.code) \(r.text)")
        }
    }

    /// Тело для DATA: CRLF везде, точка в начале строки удвоена, в конце `.`.
    static func dotStuff(_ data: Data) -> Data {
        var out = Data()
        out.reserveCapacity(data.count + 64)
        var atLineStart = true
        var prev: UInt8 = 0
        for b in data {
            if b == 10 && prev != 13 { out.append(13) }
            if atLineStart && b == 46 { out.append(46) }
            out.append(b)
            atLineStart = b == 10
            prev = b
        }
        if !(out.count >= 2 && out[out.count - 2] == 13 && out[out.count - 1] == 10) { out.append(contentsOf: [13, 10]) }
        out.append(contentsOf: Array(".\r\n".utf8))
        return out
    }

    /// Проверка входа для формы подключения: соединиться, войти, выйти.
    public static func check(_ endpoint: MailEndpoint, credentials: SMTPCredentials?, connector: MailConnector, timeout: Duration = .seconds(25)) async throws {
        let c = try await connect(endpoint, credentials: credentials, connector: connector, timeout: timeout)
        await c.quit()
    }
}
