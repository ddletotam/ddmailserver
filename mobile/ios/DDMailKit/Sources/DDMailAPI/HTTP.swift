import Foundation

/// Отправка HTTP-запроса. В приложении — `URLSession`, в тестах — подмена.
public protocol HTTPTransport: Sendable {
    func send(_ request: URLRequest) async throws -> (Data, HTTPURLResponse)
}

extension URLSession: HTTPTransport {
    public func send(_ request: URLRequest) async throws -> (Data, HTTPURLResponse) {
        let (data, response) = try await data(for: request)
        guard let http = response as? HTTPURLResponse else {
            throw URLError(.badServerResponse)
        }
        return (data, http)
    }
}

extension HTTPTransport {
    /// Выполнить запрос; 2xx — тело, иначе `APIError`. Отмена задачи
    /// пробрасывается как есть (`CancellationError` / `URLError.cancelled`).
    func body(_ request: URLRequest) async throws -> Data {
        try await checked(request).0
    }

    /// То же, что `body`, но с ответом целиком (тип содержимого у двоичных).
    func checked(_ request: URLRequest) async throws -> (Data, HTTPURLResponse) {
        let data: Data
        let response: HTTPURLResponse
        do {
            (data, response) = try await send(request)
        } catch let e as URLError where e.code == .cancelled {
            throw CancellationError()
        } catch let e as URLError {
            throw APIError.network(e.localizedDescription)
        }
        switch response.statusCode {
        case 200..<300: return (data, response)
        case 401: throw APIError.unauthorized(errorText(data))
        default: throw APIError.server(code: response.statusCode, message: errorText(data))
        }
    }
}

/// Сервер отвечает ошибкой `{"error": "..."}`; не JSON — текст как есть.
func errorText(_ data: Data) -> String {
    if let obj = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
       let e = obj["error"] as? String {
        return e.trimmingCharacters(in: .whitespacesAndNewlines)
    }
    return String(decoding: data, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
}

/// Разбор JSON ответа: несовпадение формы — `APIError.decoding`.
func decode<T: Decodable>(_ type: T.Type, from data: Data) throws -> T {
    do {
        return try JSONDecoder().decode(type, from: data)
    } catch {
        throw APIError.decoding(String(describing: error))
    }
}

extension URLRequest {
    static func json(_ url: URL, method: String, body: some Encodable) throws -> URLRequest {
        var r = URLRequest(url: url)
        r.httpMethod = method
        r.setValue("application/json; charset=utf-8", forHTTPHeaderField: "Content-Type")
        r.httpBody = try JSONEncoder().encode(body)
        return r
    }

    func bearer(_ token: String) -> URLRequest {
        var r = self
        r.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        return r
    }
}
