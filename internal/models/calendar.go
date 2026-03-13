package models

import (
	"database/sql"
	"time"
)

// CalendarSource represents a source of calendars (local, CalDAV, or ICS import)
type CalendarSource struct {
	ID         int64  `json:"id"`
	UserID     int64  `json:"user_id"`
	Name       string `json:"name"`
	SourceType string `json:"source_type"` // "local", "caldav", "ics_import", "ics_url"

	// Link to email account for sending invites
	AccountID *int64 `json:"account_id,omitempty"`

	// CalDAV fields
	CalDAVURL      string `json:"caldav_url,omitempty"`
	CalDAVUsername string `json:"caldav_username,omitempty"`
	CalDAVPassword string `json:"-"` // encrypted

	// ICS URL field (for subscribing to remote ICS calendars)
	IcsURL string `json:"ics_url,omitempty"`

	// OAuth fields
	AuthType          string    `json:"auth_type"` // "password", "oauth2_google", "oauth2_microsoft"
	OAuthAccessToken  string    `json:"-"`
	OAuthRefreshToken string    `json:"-"`
	OAuthTokenExpiry  time.Time `json:"oauth_token_expiry,omitempty"`

	// Sync settings
	SyncEnabled  bool      `json:"sync_enabled"`
	SyncInterval int       `json:"sync_interval"` // seconds
	LastSync     time.Time `json:"last_sync,omitempty"`
	LastError    string    `json:"last_error,omitempty"` // last sync error
	SyncToken    string    `json:"-"`                    // for incremental sync

	Color     string    `json:"color"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Joined field (not stored in DB)
	AccountEmail string `json:"account_email,omitempty"`
}

// NeedsSync returns true if the source needs synchronization
func (s *CalendarSource) NeedsSync() bool {
	// Local calendars and one-time imports don't sync
	if !s.SyncEnabled || s.SourceType == "local" || s.SourceType == "ics_import" {
		return false
	}
	if s.LastSync.IsZero() {
		return true
	}
	return time.Since(s.LastSync) >= time.Duration(s.SyncInterval)*time.Second
}

// Calendar represents a calendar (can be local or from external source)
type Calendar struct {
	ID          int64  `json:"id"`
	SourceID    int64  `json:"source_id"`
	UserID      int64  `json:"user_id"`
	RemoteID    string `json:"remote_id,omitempty"` // ID on remote server
	Name        string `json:"name"`
	Description string `json:"description"`
	Color       string `json:"color"`
	Timezone    string `json:"timezone"`
	CTag        string `json:"-"` // for sync
	CanWrite    bool   `json:"can_write"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Joined field (not stored in DB)
	SourceType string `json:"source_type,omitempty"`
}

// CalendarEvent represents a calendar event
type CalendarEvent struct {
	ID         int64  `json:"id"`
	CalendarID int64  `json:"calendar_id"`
	UID        string `json:"uid"` // iCalendar UID
	RemoteID   string `json:"-"`   // ID on remote server
	ICalData   string `json:"-"`   // raw iCalendar VEVENT

	// Indexed fields for display and search
	Summary     string       `json:"summary"`
	Description string       `json:"description"`
	Location    string       `json:"location"`
	DTStart     time.Time    `json:"dtstart"`
	DTEnd       sql.NullTime `json:"dtend"`
	AllDay      bool         `json:"all_day"`

	// Organizer (for invites)
	OrganizerEmail string `json:"organizer_email,omitempty"`
	OrganizerName  string `json:"organizer_name,omitempty"`
	Sequence       int    `json:"sequence"`
	Status         string `json:"status"` // CONFIRMED, TENTATIVE, CANCELLED

	// Recurring events
	RRule        string `json:"rrule,omitempty"`
	RecurrenceID string `json:"recurrence_id,omitempty"`

	// Sync fields
	ETag          string `json:"-"`
	LocalModified bool   `json:"-"` // modified locally, needs push to remote

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Joined fields (not stored in DB)
	Attendees []CalendarAttendee `json:"attendees,omitempty"`
}

// CalendarAttendee represents an attendee of a calendar event
type CalendarAttendee struct {
	ID        int64     `json:"id"`
	EventID   int64     `json:"event_id"`
	Email     string    `json:"email"`
	Name      string    `json:"name"`
	Role      string    `json:"role"`     // CHAIR, REQ-PARTICIPANT, OPT-PARTICIPANT, NON-PARTICIPANT
	PartStat  string    `json:"partstat"` // NEEDS-ACTION, ACCEPTED, DECLINED, TENTATIVE
	RSVP      bool      `json:"rsvp"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
