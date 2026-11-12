import DDMailAPI
import DDMailSync
import Foundation

/// WebSocket, пока приложение на экране (§7, порт `LiveUpdates` Android):
/// любое событие сервера — синхронизация, слитая в одну на пачку событий.
/// В фоне канала нет: там — push и фоновая синхронизация.
///
/// Обрыв — переподключение с растущей паузой (2 с … 1 мин). Сервер отверг
/// токен — сначала синхронизация (она обменяет токен), потом снова.
@MainActor
final class LiveUpdates {
    private let api: SessionAPI
    private let sync: () async -> Void
    private let onCalendar: () -> Void
    private var loop: Task<Void, Never>?
    private var debounce: Task<Void, Never>?

    init(api: SessionAPI, sync: @escaping () async -> Void, onCalendar: @escaping () -> Void = {}) {
        self.api = api
        self.sync = sync
        self.onCalendar = onCalendar
    }

    func start() {
        guard loop == nil else { return }
        loop = Task { [weak self] in await self?.run() }
    }

    func stop() {
        loop?.cancel()
        loop = nil
        debounce?.cancel()
    }

    private func run() async {
        var attempt = 0
        while !Task.isCancelled {
            if attempt > 0 {
                let pause = min(60, 2 << min(attempt - 1, 5))
                try? await Task.sleep(for: .seconds(pause))
                if Task.isCancelled { return }
            }
            do {
                mailLog.debug("ws: connecting (attempt \(attempt))")
                for try await type in await api.events() {
                    mailLog.debug("ws: event \(type, privacy: .public)")
                    attempt = 0
                    if type == "calendar_updated" { onCalendar() } else { event() }
                }
            } catch APIError.unauthorized {
                mailLog.notice("ws: token refused, syncing")
                await sync()
            } catch {
                mailLog.notice("ws: closed: \(error.localizedDescription, privacy: .public)")
            }
            attempt += 1
        }
    }

    /// События приходят пачками (по письму на событие) — одна синхронизация на пачку.
    private func event() {
        debounce?.cancel()
        debounce = Task { [weak self] in
            try? await Task.sleep(for: .milliseconds(400))
            guard !Task.isCancelled else { return }
            await self?.sync()
        }
    }

}

/// IMAP-учётка на экране (внешние учётки §6): IDLE на INBOX своим
/// соединением; событие — синк этой учётки, слитый в один на пачку событий.
@MainActor
final class ImapLive: LiveChannel {
    private let engine: ImapEngine
    private let sync: () async -> Void
    private let onDead: () -> Void
    private var loop: Task<Void, Never>?
    private var debounce: Task<Void, Never>?

    init(engine: ImapEngine, sync: @escaping () async -> Void, onDead: @escaping () -> Void) {
        self.engine = engine
        self.sync = sync
        self.onDead = onDead
    }

    func start() {
        guard loop == nil else { return }
        loop = Task { [weak self, engine] in
            await engine.watch(
                onEvent: { [weak self] in await self?.event() },
                onAuthFailure: { [weak self] in await self?.dead() }
            )
        }
    }

    func stop() {
        loop?.cancel()
        loop = nil
        debounce?.cancel()
    }

    private func event() {
        debounce?.cancel()
        debounce = Task { [weak self] in
            try? await Task.sleep(for: .milliseconds(400))
            guard !Task.isCancelled else { return }
            await self?.sync()
        }
    }

    private func dead() { onDead() }

}
