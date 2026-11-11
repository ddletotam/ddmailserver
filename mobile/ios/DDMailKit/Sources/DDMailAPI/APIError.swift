import Foundation

/// Ошибки API, разделённые по тому, что с ними делать (контракт §2, как
/// `ApiException` Android): `unauthorized` ретраи не лечат — нужен вход;
/// `network` и `server` — временные или ошибка конкретного действия.
public enum APIError: Error, Equatable, Sendable {
    /// 401: неверный пароль или мёртвая сессия. 403 сюда НЕ относится: им
    /// сервер отказывает в конкретном действии при живой сессии (календарь
    /// только для чтения, чужой адрес отправителя) — это `server`.
    case unauthorized(String)
    /// Прочий не-2xx ответ; текст — из `{"error": "..."}` сервера.
    case server(code: Int, message: String)
    /// Нет соединения, таймаут, TLS.
    case network(String)
    /// Ответ пришёл, но не того вида — сервер другой версии или не ddmail.
    case decoding(String)
}

extension APIError: LocalizedError {
    /// Текст для пользователя — те же формулировки, что у Android.
    public var errorDescription: String? {
        switch self {
        case .unauthorized: "Неверный логин или пароль."
        case let .server(code, message): message.isEmpty ? "Сервер ответил ошибкой \(code)." : message
        case let .network(reason): "Нет соединения с сервером: \(reason)"
        case .decoding: "Сервер ответил не так, как ожидалось."
        }
    }
}
