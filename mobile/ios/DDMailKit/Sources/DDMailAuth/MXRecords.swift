/// Разбор RDATA записи MX (RFC 1035 §3.3.9): приоритет (2 байта) и имя
/// обменника в формате меток. `dns_sd` отдаёт RDATA без сжатия, но указатели
/// на всякий случай не ломают разбор — имя с указателем отвергается.
/// Битые данные — `nil`: MX для поиска сервера только подсказка.
func parseMXRData(_ bytes: [UInt8]) -> (preference: Int, host: String)? {
    guard bytes.count >= 3 else { return nil }
    let pref = Int(bytes[0]) << 8 | Int(bytes[1])
    var labels: [String] = []
    var pos = 2
    while pos < bytes.count {
        let len = Int(bytes[pos])
        if len == 0 {
            let host = labels.joined(separator: ".").lowercased()
            return host.isEmpty ? nil : (pref, host)
        }
        // Указатель сжатия (0xC0) в RDATA от dns_sd не встречается.
        guard len & 0xC0 == 0, pos + 1 + len <= bytes.count else { return nil }
        guard let label = String(bytes: bytes[(pos + 1)..<(pos + 1 + len)], encoding: .ascii) else { return nil }
        labels.append(label)
        pos += 1 + len
    }
    return nil
}

/// Хосты MX по возрастанию приоритета, без повторов.
func orderedMXHosts(_ records: [(preference: Int, host: String)]) -> [String] {
    var seen = Set<String>()
    return records.enumerated()
        .sorted { ($0.element.preference, $0.offset) < ($1.element.preference, $1.offset) }
        .map(\.element.host)
        .filter { seen.insert($0).inserted }
}
