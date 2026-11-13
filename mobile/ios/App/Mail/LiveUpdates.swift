import DDMailAPI
import DDMailSync
import Foundation

/// WebSocket, пока приложение на экране (§7, порт `LiveUpdates` Android):
/// любое событие сервера — синхронизация, слитая в одну на пачку событий.
/// В фоне канала нет: там — push и фоновая синхронизация.
///
/// `delivery_status` (§5) несёт сам статус: он ложится в кэш сразу
/// (`onDelivery`), и пузырь со строкой меняются, не дожидаясь синка; синк —
/// следом, как на любое событие (он же доносит то, чего в кэше ещё нет).
/// `message_sent` — копия своего письма легла в «Отправленные»: синк
/// приносит беседу.
///
/// Обрыв — переподключение с растущей паузой (2 с … 1 мин). Сервер отверг
/// токен — сначала синхронизация (она обменяет токен), потом снова.
@MainActor
final class LiveUpdates {
    private let api: SessionAPI
    private let sync: () async -> Void
    private let onCalendar: () -> Void
    private let onDelivery: (DeliveryUpdate) -> Void
    private var loop: Task<Void, Never>?
    private var debounce: Task<Void, Never>?

    init(api: SessionAPI, sync: @escaping () async -> Void, onCalendar: @escaping () -> Void = {},
         onDelivery: @escaping (DeliveryUpdate) -> Void = { _ in }) {
        self.api = api
        self.sync = sync
        self.onCalendar = onCalendar
        self.onDelivery = onDelivery
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
                for try await e in await api.events() {
                    mailLog.debug("ws: event \(e.type, privacy: .public)")
                    attempt = 0
                    if e.type == ServerEvent.calendarUpdated {
                        onCalendar()
                        continue
                    }
                    if let d = e.delivery {
                        mailLog.notice("ws: delivery \(d.uid) → \(d.status, privacy: .public)")
                        onDelivery(d)
                    }
                    event()
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
