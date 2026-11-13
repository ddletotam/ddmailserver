import Foundation

/// Перенос и растяжение события на шкале дня и недели (Android §12, десктоп
/// §5и): шаг 15 минут; перенос — целиком, в неделе — и между днями;
/// растяжение — за верхнюю (начало) или нижнюю (конец) кромку. Короче шага
/// событие не становится.
public enum EventDrag {
    public enum Kind: Sendable, Equatable {
        /// Целиком: та же длительность, новое начало.
        case move
        /// Верхняя кромка: двигается начало.
        case top
        /// Нижняя кромка: двигается конец.
        case bottom
    }

    /// Шаг, минут.
    public static let step = 15
    static let stepMs = Int64(step) * 60_000

    /// Смещение по шкале (точки) → минуты, кратные шагу.
    public static func snap(_ dy: Double, hourHeight: Double) -> Int {
        guard hourHeight > 0 else { return 0 }
        return Int((dy / hourHeight * 60 / Double(step)).rounded()) * step
    }

    /// Какая часть события под пальцем: кромки — растяжение, остальное —
    /// перенос. Кромка — до 10 точек, но не больше трети высоты (у короткого
    /// события середина остаётся для переноса).
    public static func kind(at y: Double, height: Double) -> Kind {
        let edge = min(10, height / 3)
        if y <= edge { return .top }
        if y >= height - edge { return .bottom }
        return .move
    }

    /// Новые начало и конец (мс). Перенос между днями — по стенным часам
    /// календаря (через переход на летнее время событие остаётся в том же
    /// часу), минуты — абсолютным сдвигом.
    public static func apply(_ kind: Kind, start: Int64, end: Int64, minutes: Int, days: Int = 0,
                             calendar cal: Calendar) -> (start: Int64, end: Int64) {
        let delta = Int64(minutes) * 60_000
        switch kind {
        case .move:
            func shift(_ t: Int64) -> Int64 {
                let d = Date(timeIntervalSince1970: TimeInterval(t) / 1000)
                let moved = days == 0 ? d : cal.date(byAdding: .day, value: days, to: d) ?? d
                return Int64((moved.timeIntervalSince1970 * 1000).rounded()) + delta
            }
            return (shift(start), shift(end))
        case .top:
            return (min(start + delta, end - stepMs), end)
        case .bottom:
            return (start, max(end + delta, start + stepMs))
        }
    }
}
