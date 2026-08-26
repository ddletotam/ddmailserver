package models

import (
	"strings"

	"github.com/yourusername/mailserver/internal/timeutil"
)

// CalendarSource represents a source of calendars (local, CalDAV, or ICS import)
type CalendarSource struct {
	ID         int64  `json:"id"`
	UserID     int64  `json:"user_id"`
	Name       string `json:"name"`
	SourceType string `json:"source_type"` // "local", "caldav", "ics_import", "ics_url"

	// Identity (email address) this source belongs to. Mandatory — the
	// desktop client is identity-keyed and there are no orphan sources.
	IdentityEmail string `json:"identity_email"`

	// Link to email account for sending invites
	AccountID *int64 `json:"account_id,omitempty"`

	// CalDAV fields
	CalDAVURL      string `json:"caldav_url,omitempty"`
	CalDAVUsername string `json:"caldav_username,omitempty"`
	CalDAVPassword string `json:"-"` // encrypted

	// ICS URL field (for subscribing to remote ICS calendars)
	IcsURL string `json:"ics_url,omitempty"`

	// OAuth fields
	AuthType          string `json:"auth_type"` // "password", "oauth2_google", "oauth2_microsoft"
	OAuthAccessToken  string `json:"-"`
	OAuthRefreshToken string `json:"-"`
	OAuthTokenExpiry  int64  `json:"oauth_token_expiry,omitempty"`

	// Sync settings
	SyncEnabled  bool   `json:"sync_enabled"`
	SyncInterval int    `json:"sync_interval"` // seconds
	LastSync     int64  `json:"last_sync,omitempty"`
	LastError    string `json:"last_error,omitempty"`
	SyncToken    string `json:"-"`

	Color     string `json:"color"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`

	// Default alarm settings
	DefaultAlarmEnabled bool   `json:"default_alarm_enabled"`
	DefaultAlarmBefore  int    `json:"default_alarm_before"`
	DefaultAlarmUnit    string `json:"default_alarm_unit"`

	// Joined field (not stored in DB)
	AccountEmail string `json:"account_email,omitempty"`
}

// NeedsSync returns true if the source needs synchronization
func (s *CalendarSource) NeedsSync() bool {
	if !s.SyncEnabled || s.SourceType == "local" || s.SourceType == "ics_import" {
		return false
	}
	if s.LastSync == 0 {
		return true
	}
	elapsed := timeutil.Now() - s.LastSync
	return elapsed >= int64(s.SyncInterval)*1000
}

// Calendar represents a calendar (can be local or from external source)
type Calendar struct {
	ID          int64  `json:"id"`
	SourceID    int64  `json:"source_id"`
	UserID      int64  `json:"user_id"`
	RemoteID    string `json:"remote_id,omitempty"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Color       string `json:"color"`
	Timezone    string `json:"timezone"`
	CTag        string `json:"-"`
	CanWrite    bool   `json:"can_write"`
	ReverseSync bool   `json:"reverse_sync"`
	Enabled     bool   `json:"enabled"`

	CreatedAt int64 `json:"created_at"`
	UpdatedAt int64 `json:"updated_at"`

	// SupportedComponents lists what this collection accepts, comma-separated:
	// "VEVENT", "VTODO", or both. Stored rather than assumed — assuming it is
	// what made us advertise task support we did not have, and push tasks at an
	// iCloud event collection that answered 403 forever. See migrations/048.
	SupportedComponents string `json:"supported_components,omitempty"`

	// Joined fields
	SourceType    string `json:"source_type,omitempty"`
	IdentityEmail string `json:"identity_email,omitempty"`
}

// Calendar component names, as they appear in iCalendar and in the
// `component` column.
const (
	ComponentEvent = "VEVENT"
	ComponentTodo  = "VTODO"
)

// Supports reports whether this collection accepts the given component.
// An empty SupportedComponents means events only: the conservative reading,
// and the one that matches every calendar that existed before tasks did.
func (c *Calendar) Supports(component string) bool {
	list := c.SupportedComponents
	if strings.TrimSpace(list) == "" {
		return component == ComponentEvent
	}
	for _, part := range strings.Split(list, ",") {
		if strings.EqualFold(strings.TrimSpace(part), component) {
			return true
		}
	}
	return false
}

// ComponentList splits SupportedComponents into its parts, normalised to upper
// case, defaulting to events only.
func (c *Calendar) ComponentList() []string {
	var out []string
	for _, part := range strings.Split(c.SupportedComponents, ",") {
		part = strings.ToUpper(strings.TrimSpace(part))
		if part != "" {
			out = append(out, part)
		}
	}
	if len(out) == 0 {
		return []string{ComponentEvent}
	}
	return out
}

// CalendarEvent represents a calendar event
type CalendarEvent struct {
	ID         int64  `json:"id"`
	CalendarID int64  `json:"calendar_id"`
	UID        string `json:"uid"`
	RemoteID   string `json:"-"`
	ICalData   string `json:"-"`

	// Component is "VEVENT" or "VTODO". Tasks live in this table rather than a
	// parallel one because they share nearly all of it; see migrations/048.
	Component string `json:"component"`

	// Indexed fields
	Summary     string `json:"summary"`
	Description string `json:"description"`
	Location    string `json:"location"`
	DTStart     int64  `json:"dtstart"`
	DTStartTZ   int16  `json:"dtstart_tz"` // UTC offset in minutes
	DTEnd       *int64 `json:"dtend"`      // nullable
	DTEndTZ     int16  `json:"dtend_tz"`
	AllDay      bool   `json:"all_day"`

	// VTODO-only. Due is the deadline — deliberately not reused from DTEnd: an
	// event with no DTEnd lasts an hour, a task with no Due has no deadline,
	// and one nullable column per meaning beats a shared one every reader has
	// to disambiguate.
	Due   *int64 `json:"due,omitempty"`
	DueTZ int16  `json:"due_tz,omitempty"`
	// CompletedAt is when the task was finished (RFC 5545 COMPLETED), not a
	// boolean — clients display the moment.
	CompletedAt     *int64 `json:"completed_at,omitempty"`
	PercentComplete *int16 `json:"percent_complete,omitempty"`
	// Priority: 0 undefined, 1 highest … 9 lowest (RFC 5545 §3.8.1.9).
	Priority *int16 `json:"priority,omitempty"`

	// Organizer
	OrganizerEmail string `json:"organizer_email,omitempty"`
	OrganizerName  string `json:"organizer_name,omitempty"`
	Sequence       int    `json:"sequence"`
	Status         string `json:"status"`

	// Recurring events
	RRule        string `json:"rrule,omitempty"`
	RecurrenceID string `json:"recurrence_id,omitempty"`

	// Sync fields
	ETag          string `json:"-"`
	LocalModified bool   `json:"-"`

	CreatedAt int64 `json:"created_at"`
	UpdatedAt int64 `json:"updated_at"`

	// Soft delete
	SoftDeletedAt *int64 `json:"soft_deleted_at,omitempty"`

	// Joined fields
	Attendees []CalendarAttendee `json:"attendees,omitempty"`
}

// IsTodo reports whether this row is a task rather than an event. An empty
// Component reads as an event: everything that existed before tasks did was
// one, and the column defaults that way.
func (e *CalendarEvent) IsTodo() bool {
	return e.Component == ComponentTodo
}

// ComponentOrDefault returns the component name, filling in VEVENT for rows
// loaded from before the column existed.
func (e *CalendarEvent) ComponentOrDefault() string {
	if e.Component == "" {
		return ComponentEvent
	}
	return e.Component
}

// IsCompleted reports whether a task is done — by STATUS or by carrying a
// COMPLETED timestamp. Both spellings occur in the wild, and a client that
// only set one would otherwise show a finished task as outstanding.
func (e *CalendarEvent) IsCompleted() bool {
	return e.CompletedAt != nil || strings.EqualFold(e.Status, "COMPLETED")
}

// DTStartAsTime returns DTStart as time.Time for CalDAV/iCal serialization.
func (e *CalendarEvent) DTStartAsTime() interface{} {
	return timeutil.FromMs(e.DTStart)
}

// CalendarAttendee represents an attendee of a calendar event
type CalendarAttendee struct {
	ID        int64  `json:"id"`
	EventID   int64  `json:"event_id"`
	Email     string `json:"email"`
	Name      string `json:"name"`
	Role      string `json:"role"`
	PartStat  string `json:"partstat"`
	RSVP      bool   `json:"rsvp"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}
