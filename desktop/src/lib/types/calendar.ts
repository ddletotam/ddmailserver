export interface DesktopCalendar {
  id: number;
  name: string;
  description?: string;
  color?: string;       // server-suggested; client overrides via localStorage
  source_type: string;  // "local" | "caldav" | "ics_import" | "ics_url"
  can_write: boolean;
  enabled: boolean;
  timezone?: string;
}

export interface DesktopCalendarAttendee {
  email: string;
  name?: string;
  role?: string;
  partstat?: string;  // "ACCEPTED" | "DECLINED" | "TENTATIVE" | "NEEDS-ACTION" | "DELEGATED"
}

export interface DesktopCalendarEvent {
  id: number;
  calendar_id: number;
  uid: string;
  summary: string;
  description?: string;
  location?: string;
  dtstart: number;        // ms since epoch
  dtend: number | null;   // ms since epoch
  all_day: boolean;
  organizer_email?: string;
  organizer_name?: string;
  status?: string;
  rrule?: string;
  recurrence_id?: string;
  /** Deleted-instance starts (ms since epoch) for recurring events. The
   *  client must skip these when expanding `rrule`, otherwise removed
   *  occurrences keep rendering on the calendar. */
  exdates?: number[];
  attendees?: DesktopCalendarAttendee[];
}
