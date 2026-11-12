import DDMailIMAP
import DDMailNet
import DDMailSMTP
import Foundation

/// Проверка входа перед сохранением IMAP-учётки (контракт внешних учёток §1):
/// IMAP, затем SMTP, у каждого шага 25 с. Ошибка — по-русски, с указанием
/// сервера; текст TLS-ошибки — как есть.
public enum AccountCheck {
    public struct Failure: Error, LocalizedError, Equatable {
        public let message: String
        public let isAuth: Bool
        public var errorDescription: String? { message }
    }

    public static func check(_ a: MailAccount, password: String, connector: MailConnector = SystemMailConnector()) async throws {
        guard let imap = a.imap, let smtp = a.smtp else {
            throw Failure(message: "Заполните серверы входящей и исходящей почты.", isAuth: false)
        }
        do {
            try await step(.seconds(25)) {
                let c = try await IMAPClient.connect(imap, connector: connector, timeout: .seconds(25))
                do {
                    try await c.login(user: a.imapLogin, password: password)
                } catch {
                    await c.close()
                    throw error
                }
                await c.logout()
            }
        } catch {
            throw failure("Входящая почта (IMAP)", error)
        }
        do {
            try await step(.seconds(25)) {
                let creds = a.smtpAuth ? SMTPCredentials(user: a.smtpLogin.isEmpty ? a.imapLogin : a.smtpLogin, password: password) : nil
                try await SMTPClient.check(smtp, credentials: creds, connector: connector, timeout: .seconds(25))
            }
        } catch {
            throw failure("Исходящая почта (SMTP)", error)
        }
    }

    static func failure(_ server: String, _ e: Error) -> Failure {
        let m = e as? MailNetError
        let text: String
        switch m {
        case .auth?: text = "неверный логин или пароль"
        case .timeout?: text = "сервер не отвечает"
        case let .connect(s)?: text = "сервер не отвечает (\(s))"
        case let .tls(s)?: text = s
        case let .server(s)?: text = s
        default: text = e.localizedDescription
        }
        return Failure(message: "\(server): \(text).", isAuth: m?.isAuth ?? false)
    }

    /// Шаг с пределом: не уложился — «сервер не отвечает».
    static func step(_ limit: Duration, _ work: @escaping @Sendable () async throws -> Void) async throws {
        try await withThrowingTaskGroup(of: Bool.self) { group in
            group.addTask {
                try await work()
                return true
            }
            group.addTask {
                try await Task.sleep(for: limit)
                return false
            }
            defer { group.cancelAll() }
            guard let first = try await group.next(), first else { throw MailNetError.timeout }
        }
    }
}
