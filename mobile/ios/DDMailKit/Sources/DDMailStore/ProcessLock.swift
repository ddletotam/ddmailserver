import DDMailAPI
import Foundation

/// Межпроцессная блокировка — `flock` на файле в App Group (контракт §4, §7):
/// Notification Service Extension и фоновый проход приложения не синкают
/// одновременно, а рубежи уведомлений читаются и пишутся под ней одним
/// куском. Блокировка держится секунды: процесс, усыплённый системой с
/// блокировкой файла общего контейнера, iOS убивает (0xdead10cc).
public final class ProcessLock: Sendable {
    public let url: URL

    public init(url: URL) {
        self.url = url
    }

    /// Блокировка проходов синхронизации.
    public static var sync: ProcessLock { ProcessLock(url: AppGroup.dataDirectory.appending(path: "sync.lock")) }
    /// Блокировка рубежей уведомлений.
    public static var notify: ProcessLock { ProcessLock(url: AppGroup.dataDirectory.appending(path: "notify.lock")) }

    /// Взять, ожидая не дольше `timeout`; `nil` — не дождались (или файл не
    /// открылся). Держатель отпускает `release()` или просто теряет ссылку.
    public func acquire(timeout: Duration) async -> Held? {
        try? FileManager.default.createDirectory(at: url.deletingLastPathComponent(), withIntermediateDirectories: true)
        let fd = open(url.path, O_RDWR | O_CREAT | O_CLOEXEC, 0o600)
        guard fd >= 0 else { return nil }
        let deadline = ContinuousClock.now + timeout
        while true {
            if flock(fd, LOCK_EX | LOCK_NB) == 0 { return Held(fd: fd) }
            guard errno == EWOULDBLOCK || errno == EINTR, ContinuousClock.now < deadline, !Task.isCancelled else {
                close(fd)
                return nil
            }
            try? await Task.sleep(for: .milliseconds(50))
        }
    }

    /// Взятая блокировка.
    public final class Held: @unchecked Sendable {
        private let lock = NSLock()
        private var fd: Int32

        init(fd: Int32) { self.fd = fd }

        public func release() {
            lock.withLock {
                guard fd >= 0 else { return }
                flock(fd, LOCK_UN)
                close(fd)
                fd = -1
            }
        }

        deinit { release() }
    }
}
