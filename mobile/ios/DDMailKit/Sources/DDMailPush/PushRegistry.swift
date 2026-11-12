import DDMailAPI
import Foundation
import os

private let pushLog = Logger(subsystem: "ru.letotam.ddmail", category: "push")

/// Учётка ddmail, которой нужен push: ключ и её API.
public struct PushTarget: Sendable {
    public let accountKey: String
    public let api: SessionAPI

    public init(accountKey: String, api: SessionAPI) {
        self.accountKey = accountKey
        self.api = api
    }
}

/// Регистрация токена APNs на серверах ddmail-учёток (контракт §7): каждой
/// учётке — свой сервер и свой пользователь. Что уже зарегистрировано,
/// помнится в `SharedState.registrations`: повторный вызов ничего не шлёт,
/// новый токен заменяет старый (старый снимается), выключенные уведомления
/// снимают регистрации. Ошибки не наверх: следующий выход на экран
/// повторит (сервер старой версии отвечает 404 — тоже повторим потом).
public actor PushRegistry {
    let state: SharedState
    let topic: String
    let environment: APNsEnvironment

    public init(state: SharedState, topic: String, environment: APNsEnvironment) {
        self.state = state
        self.topic = topic
        self.environment = environment
    }

    /// Устройство получило токен (при каждом запуске — тот же или новый).
    /// Ответ — сменился ли он.
    @discardableResult
    public func setDeviceToken(_ token: String) -> Bool {
        let token = token.lowercased()
        guard state.deviceToken != token else { return false }
        state.deviceToken = token
        return true
    }

    /// Привести регистрации учёток к нужному: уведомления включены — у каждой
    /// учётки текущий токен; выключены — ни у одной.
    public func sync(_ targets: [PushTarget]) async {
        let enabled = state.notificationsEnabled
        let token = state.deviceToken
        let mark = token.map { "\($0)|\(environment.rawValue)" }
        for t in targets {
            let recorded = state.registrations[t.accountKey]
            if enabled, let token, let mark {
                guard recorded != mark else { continue }
                // Токен сменился — прежний снять, чтобы сервер не будил то,
                // чего уже нет (APNs его и сам отвергнет, но позже).
                if let old = recorded.flatMap(Self.token), old != token {
                    try? await t.api.unregisterPush(kind: .apns, token: old)
                }
                do {
                    let r = try await t.api.registerPush(kind: .apns, token: token, environment: environment, topic: topic)
                    record(t.accountKey, mark)
                    if !r.delivery { pushLog.notice("push: server of \(t.accountKey, privacy: .private) stores the token but has no APNs key") }
                } catch {
                    pushLog.notice("push: register \(t.accountKey, privacy: .private): \(String(describing: error), privacy: .public)")
                }
            } else if let recorded, let old = Self.token(recorded) {
                do {
                    try await t.api.unregisterPush(kind: .apns, token: old)
                    record(t.accountKey, nil)
                } catch {
                    pushLog.notice("push: unregister \(t.accountKey, privacy: .private): \(String(describing: error), privacy: .public)")
                }
            }
        }
    }

    /// Учётку удаляют: снять её регистрацию (лучшее усилие — сессия сейчас
    /// исчезнет, второй попытки не будет) и забыть. `registration` — запись,
    /// прочитанная до того, как приложение стёрло состояние учётки.
    public func remove(_ target: PushTarget, registration: String? = nil) async {
        if let recorded = registration ?? state.registrations[target.accountKey], let old = Self.token(recorded) {
            try? await target.api.unregisterPush(kind: .apns, token: old)
        }
        record(target.accountKey, nil)
    }

    private func record(_ key: String, _ mark: String?) {
        var r = state.registrations
        r[key] = mark
        state.registrations = r
    }

    static func token(_ mark: String) -> String? {
        mark.split(separator: "|").first.map(String.init)
    }
}

extension Data {
    /// Токен устройства APNs — байты; серверу — hex.
    public var hexString: String { map { String(format: "%02x", $0) }.joined() }
}
