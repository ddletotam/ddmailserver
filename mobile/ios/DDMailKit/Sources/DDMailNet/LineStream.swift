import Foundation

/// Построчное чтение поверх `MailSocket` — общее для IMAP и SMTP. Каждое
/// чтение ограничено таймаутом: зависший сервер не повесит синк навсегда
/// (таймаут закрывает соединение — следующий шаг всё равно переподключится).
/// Им владеет один клиент (актор) и зовёт последовательно — отсюда
/// `@unchecked Sendable`: передавать поток между задачами можно, читать
/// одновременно из двух — нельзя.
public final class LineStream: @unchecked Sendable {
    public let socket: MailSocket
    private var buffer: [UInt8] = []
    private var start = 0
    public var timeout: Duration

    public init(socket: MailSocket, timeout: Duration = .seconds(60)) {
        self.socket = socket
        self.timeout = timeout
    }

    /// Строка без CRLF (или LF). Сервер закрыл соединение — `closed`.
    public func readLine(timeout: Duration? = nil) async throws -> [UInt8] {
        while true {
            if let nl = buffer[start...].firstIndex(of: 10) {
                var end = nl
                if end > start, buffer[end - 1] == 13 { end -= 1 }
                let line = Array(buffer[start..<end])
                start = nl + 1
                compact()
                return line
            }
            try await fill(timeout: timeout ?? self.timeout)
        }
    }

    public func readLineString(timeout: Duration? = nil) async throws -> String {
        String(decoding: try await readLine(timeout: timeout), as: UTF8.self)
    }

    /// Ровно `n` байт (литерал IMAP).
    public func readBytes(_ n: Int, timeout: Duration? = nil) async throws -> [UInt8] {
        while buffer.count - start < n { try await fill(timeout: timeout ?? self.timeout) }
        let out = Array(buffer[start..<(start + n)])
        start += n
        compact()
        return out
    }

    public func write(_ s: String) async throws { try await socket.send(Data(s.utf8)) }
    public func write(_ d: Data) async throws { try await socket.send(d) }

    /// STARTTLS: непрочитанного быть не должно — иначе это внедрённые
    /// открытым текстом данные (атака на STARTTLS), соединение рвём.
    public func startTLS() async throws {
        guard buffer.count == start else {
            socket.close()
            throw MailNetError.protocolError("данные после STARTTLS до рукопожатия")
        }
        try await socket.startTLS()
    }

    public func close() { socket.close() }

    private func compact() {
        if start > 64 * 1024 || start == buffer.count {
            buffer.removeFirst(start)
            start = 0
        }
    }

    private func fill(timeout: Duration) async throws {
        let socket = self.socket
        let data: Data = try await withThrowingTaskGroup(of: Data?.self) { group in
            group.addTask { try await socket.receive() }
            group.addTask {
                try await Task.sleep(for: timeout)
                return nil
            }
            defer { group.cancelAll() }
            guard let first = try await group.next() else { throw MailNetError.timeout }
            guard let d = first else {
                socket.close()
                throw MailNetError.timeout
            }
            return d
        }
        if data.isEmpty { throw MailNetError.closed }
        buffer.append(contentsOf: data)
    }
}
