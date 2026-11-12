import Foundation

// Календарь, задачи, контакты — зеркало CalendarDto.kt Android и
// Desktop*-структур internal/web/handlers_desktop_{calendar,tasks,contacts,event_*}.go.

public struct CalendarDTO: Decodable, Equatable, Identifiable, Sendable {
    public var id: Int64
    public var name: String
    public var description: String
    public var color: String
    public var sourceType: String
    public var canWrite: Bool
    public var enabled: Bool
    public var timezone: String

    enum CodingKeys: String, CodingKey {
        case id, name, description, color, enabled, timezone
        case sourceType = "source_type"
        case canWrite = "can_write"
    }

    public init(id: Int64, name: String = "", color: String = "", canWrite: Bool = false, enabled: Bool = true) {
        self.id = id; self.name = name; description = ""; self.color = color; sourceType = ""
        self.canWrite = canWrite; self.enabled = enabled; timezone = ""
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(Int64.self, forKey: .id)
        name = try c.value(.name, "")
        description = try c.value(.description, "")
        color = try c.value(.color, "")
        sourceType = try c.value(.sourceType, "")
        canWrite = try c.value(.canWrite, false)
        enabled = try c.value(.enabled, true)
        timezone = try c.value(.timezone, "")
    }
}

public struct AttendeeDTO: Codable, Equatable, Hashable, Sendable {
    public var email: String
    public var name: String
    public var role: String
    public var partstat: String

    public init(email: String, name: String = "", role: String = "", partstat: String = "") {
        self.email = email; self.name = name; self.role = role; self.partstat = partstat
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        email = try c.value(.email, "")
        name = try c.value(.name, "")
        role = try c.value(.role, "")
        partstat = try c.value(.partstat, "")
    }
}

public struct EventExtraDTO: Codable, Equatable, Hashable, Sendable {
    public var name: String
    public var value: String

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        name = try c.value(.name, "")
        value = try c.value(.value, "")
    }
}

/// Событие. Повторяющееся приходит мастером — развёртывает клиент.
public struct EventDTO: Decodable, Equatable, Identifiable, Sendable {
    public var id: Int64
    public var calendarID: Int64
    public var uid: String
    public var summary: String
    public var description: String
    public var location: String
    /// Миллисекунды.
    public var dtstart: Int64
    public var dtend: Int64?
    public var allDay: Bool
    public var organizerEmail: String
    public var organizerName: String
    public var status: String
    public var rrule: String
    public var recurrenceID: String
    public var exdates: [Int64]
    public var attendees: [AttendeeDTO]
    public var alarmLeadMin: Int
    public var alarmLeads: [Int]
    public var extras: [EventExtraDTO]
    public var color: String
    public var editable: Bool
    public var deletable: Bool
    public var identityEmail: String

    enum CodingKeys: String, CodingKey {
        case id, uid, summary, description, location, dtstart, dtend, status, rrule, exdates, attendees, extras, color, editable, deletable
        case calendarID = "calendar_id"
        case allDay = "all_day"
        case organizerEmail = "organizer_email"
        case organizerName = "organizer_name"
        case recurrenceID = "recurrence_id"
        case alarmLeadMin = "alarm_lead_min"
        case alarmLeads = "alarm_leads"
        case identityEmail = "identity_email"
    }

    public init(
        id: Int64, calendarID: Int64 = 0, summary: String = "", dtstart: Int64, dtend: Int64? = nil,
        allDay: Bool = false, rrule: String = "", exdates: [Int64] = [], alarmLeadMin: Int = 0, alarmLeads: [Int] = []
    ) {
        self.id = id; self.calendarID = calendarID; uid = ""; self.summary = summary; description = ""; location = ""
        self.dtstart = dtstart; self.dtend = dtend; self.allDay = allDay; organizerEmail = ""; organizerName = ""
        status = ""; self.rrule = rrule; recurrenceID = ""; self.exdates = exdates; attendees = []
        self.alarmLeadMin = alarmLeadMin; self.alarmLeads = alarmLeads; extras = []; color = ""
        editable = false; deletable = false; identityEmail = ""
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(Int64.self, forKey: .id)
        calendarID = try c.value(.calendarID, 0)
        uid = try c.value(.uid, "")
        summary = try c.value(.summary, "")
        description = try c.value(.description, "")
        location = try c.value(.location, "")
        dtstart = try c.value(.dtstart, 0)
        dtend = try c.decodeIfPresent(Int64.self, forKey: .dtend)
        allDay = try c.value(.allDay, false)
        organizerEmail = try c.value(.organizerEmail, "")
        organizerName = try c.value(.organizerName, "")
        status = try c.value(.status, "")
        rrule = try c.value(.rrule, "")
        recurrenceID = try c.value(.recurrenceID, "")
        exdates = try c.value(.exdates, [])
        attendees = try c.value(.attendees, [])
        alarmLeadMin = try c.value(.alarmLeadMin, 0)
        alarmLeads = try c.value(.alarmLeads, [])
        extras = try c.value(.extras, [])
        color = try c.value(.color, "")
        editable = try c.value(.editable, false)
        deletable = try c.value(.deletable, false)
        identityEmail = try c.value(.identityEmail, "")
    }
}

public struct EventCreateRequest: Encodable, Sendable {
    public var calendar_id: Int64
    public var summary: String
    public var description: String
    public var location: String
    public var dtstart: Int64
    public var dtend: Int64?
    public var all_day: Bool

    public init(calendarID: Int64, summary: String, description: String = "", location: String = "", dtstart: Int64, dtend: Int64?, allDay: Bool) {
        calendar_id = calendarID; self.summary = summary; self.description = description; self.location = location
        self.dtstart = dtstart; self.dtend = dtend; all_day = allDay
    }
}

/// Правка: `nil` — поле не трогать. `scope` — all | future (single сервер пока не умеет).
public struct EventPatchRequest: Encodable, Sendable {
    public var scope: String
    public var recurrence_id: Int64
    public var summary: String?
    public var description: String?
    public var location: String?
    public var dtstart: Int64?
    public var dtend: Int64?
    public var all_day: Bool?

    public init(scope: String = "all", recurrenceID: Int64 = 0, summary: String? = nil, description: String? = nil,
                location: String? = nil, dtstart: Int64? = nil, dtend: Int64? = nil, allDay: Bool? = nil) {
        self.scope = scope; recurrence_id = recurrenceID; self.summary = summary; self.description = description
        self.location = location; self.dtstart = dtstart; self.dtend = dtend; all_day = allDay
    }

    /// `nil` в JSON не пишется: сервер отличает «не трогать» от «стереть».
    public func encode(to encoder: Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try c.encode(scope, forKey: .scope)
        try c.encode(recurrence_id, forKey: .recurrence_id)
        try c.encodeIfPresent(summary, forKey: .summary)
        try c.encodeIfPresent(description, forKey: .description)
        try c.encodeIfPresent(location, forKey: .location)
        try c.encodeIfPresent(dtstart, forKey: .dtstart)
        try c.encodeIfPresent(dtend, forKey: .dtend)
        try c.encodeIfPresent(all_day, forKey: .all_day)
    }

    enum CodingKeys: String, CodingKey {
        case scope, recurrence_id, summary, description, location, dtstart, dtend, all_day
    }
}

public struct TaskDTO: Decodable, Equatable, Identifiable, Sendable {
    public var id: Int64
    public var calendarID: Int64
    public var summary: String
    public var description: String
    public var start: Int64?
    public var due: Int64?
    public var allDay: Bool
    public var status: String
    public var completed: Bool
    public var completedAt: Int64?
    public var priority: Int?
    public var color: String
    public var canWrite: Bool
    public var calendarName: String

    enum CodingKeys: String, CodingKey {
        case id, summary, description, start, due, status, completed, priority, color
        case calendarID = "calendar_id"
        case allDay = "all_day"
        case completedAt = "completed_at"
        case canWrite = "can_write"
        case calendarName = "calendar_name"
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(Int64.self, forKey: .id)
        calendarID = try c.value(.calendarID, 0)
        summary = try c.value(.summary, "")
        description = try c.value(.description, "")
        start = try c.decodeIfPresent(Int64.self, forKey: .start)
        due = try c.decodeIfPresent(Int64.self, forKey: .due)
        allDay = try c.value(.allDay, false)
        status = try c.value(.status, "")
        completed = try c.value(.completed, false)
        completedAt = try c.decodeIfPresent(Int64.self, forKey: .completedAt)
        priority = try c.decodeIfPresent(Int.self, forKey: .priority)
        color = try c.value(.color, "")
        canWrite = try c.value(.canWrite, false)
        calendarName = try c.value(.calendarName, "")
    }
}

public struct ContactDTO: Decodable, Equatable, Identifiable, Sendable {
    public var id: Int64
    public var fullName: String
    public var emails: [String]
    public var phones: [String]
    public var organization: String
    public var title: String
    public var photoURL: String

    enum CodingKeys: String, CodingKey {
        case id, emails, phones, organization, title
        case fullName = "full_name"
        case photoURL = "photo_url"
    }

    public init(id: Int64, fullName: String, emails: [String] = [], phones: [String] = [], organization: String = "", title: String = "", photoURL: String = "") {
        self.id = id; self.fullName = fullName; self.emails = emails; self.phones = phones
        self.organization = organization; self.title = title; self.photoURL = photoURL
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(Int64.self, forKey: .id)
        fullName = try c.value(.fullName, "")
        emails = try c.value(.emails, [])
        phones = try c.value(.phones, [])
        organization = try c.value(.organization, "")
        title = try c.value(.title, "")
        photoURL = try c.value(.photoURL, "")
    }
}

/// Адресная книга — куда записать новый контакт (выбирает пользователь, не сервер).
public struct AddressBookDTO: Decodable, Equatable, Identifiable, Sendable {
    public var id: Int64
    public var name: String
    public var sourceName: String
    public var canWrite: Bool

    enum CodingKeys: String, CodingKey {
        case id, name
        case sourceName = "source_name"
        case canWrite = "can_write"
    }

    public init(id: Int64, name: String, sourceName: String = "", canWrite: Bool = false) {
        self.id = id; self.name = name; self.sourceName = sourceName; self.canWrite = canWrite
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(Int64.self, forKey: .id)
        name = try c.value(.name, "")
        sourceName = try c.value(.sourceName, "")
        canWrite = try c.value(.canWrite, false)
    }
}

public struct ContactWriteRequest: Encodable, Sendable {
    public var full_name: String
    public var emails: [String]
    public var phones: [String]
    public var organization: String
    public var title: String
    /// Книга нового контакта; 0 — не передаётся (правка).
    public var address_book_id: Int64?

    public init(fullName: String, emails: [String], phones: [String], organization: String = "", title: String = "", addressBookID: Int64?) {
        full_name = fullName; self.emails = emails; self.phones = phones; self.organization = organization
        self.title = title; address_book_id = addressBookID
    }
}
