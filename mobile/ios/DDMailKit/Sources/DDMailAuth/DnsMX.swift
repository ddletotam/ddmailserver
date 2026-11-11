import dnssd
import Foundation

/// MX домена через системный резолвер (`DNSServiceQueryRecord`, контракт §3):
/// тот же DNS, что у всего устройства, поэтому работают и внутренние домены
/// (корпоративный VPN, свой DNS). Ошибка или таймаут — пустой список.
public enum DnsMX {
    public static func lookup(_ domain: String, timeout: TimeInterval = 5) async -> [String] {
        await withCheckedContinuation { cont in
            // dns_sd — блокирующий цикл на сокете; держим его вне пула задач Swift.
            Thread.detachNewThread {
                cont.resume(returning: orderedMXHosts(query(domain, timeout: timeout)))
            }
        }
    }

    /// Сборщик ответов: колбэк dns_sd — C-функция без захвата, контекст — указатель на него.
    private final class Collector {
        var records: [(preference: Int, host: String)] = []
        var finished = false
    }

    private static func query(_ domain: String, timeout: TimeInterval) -> [(preference: Int, host: String)] {
        let collector = Collector()
        let ctx = Unmanaged.passUnretained(collector).toOpaque()
        var ref: DNSServiceRef?
        let callback: DNSServiceQueryRecordReply = { _, flags, _, error, _, _, _, rdlen, rdata, _, context in
            guard let context else { return }
            let c = Unmanaged<Collector>.fromOpaque(context).takeUnretainedValue()
            if error == kDNSServiceErr_NoError, let rdata, rdlen > 0 {
                let bytes = Array(UnsafeRawBufferPointer(start: rdata, count: Int(rdlen)))
                if let mx = parseMXRData(bytes) { c.records.append(mx) }
            }
            // Нет «ещё будут» — первая порция ответа собрана.
            if flags & kDNSServiceFlagsMoreComing == 0 { c.finished = true }
        }
        let err = DNSServiceQueryRecord(
            &ref, kDNSServiceFlagsReturnIntermediates, 0, domain,
            UInt16(kDNSServiceType_MX), UInt16(kDNSServiceClass_IN), callback, ctx
        )
        guard err == kDNSServiceErr_NoError, let ref else { return [] }
        defer { DNSServiceRefDeallocate(ref) }

        let fd = DNSServiceRefSockFD(ref)
        let deadline = Date().addingTimeInterval(timeout)
        while !collector.finished {
            let left = deadline.timeIntervalSinceNow
            if left <= 0 { break }
            var pfd = pollfd(fd: fd, events: Int16(POLLIN), revents: 0)
            let n = poll(&pfd, 1, Int32(left * 1000))
            if n <= 0 { break }
            if DNSServiceProcessResult(ref) != kDNSServiceErr_NoError { break }
        }
        return collector.records
    }
}
