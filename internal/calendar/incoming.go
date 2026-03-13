package calendar

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/emersion/go-ical"
	"github.com/yourusername/mailserver/internal/caldav/importer"
	"github.com/yourusername/mailserver/internal/db"
	"github.com/yourusername/mailserver/internal/models"
	"github.com/yourusername/mailserver/internal/parser"
)

// IncomingHandler handles incoming calendar invites
type IncomingHandler struct {
	db *db.DB
}

// NewIncomingHandler creates a new incoming invite handler
func NewIncomingHandler(database *db.DB) *IncomingHandler {
	return &IncomingHandler{db: database}
}

// InviteInfo contains extracted information from an incoming invite
type InviteInfo struct {
	Method         string    // REQUEST, REPLY, CANCEL
	EventUID       string    // Event UID
	Summary        string    // Event title
	Description    string    // Event description
	Location       string    // Event location
	OrganizerEmail string    // Organizer email
	OrganizerName  string    // Organizer name
	DTStart        time.Time // Start time
	DTEnd          time.Time // End time
	AllDay         bool      // All day event
	Sequence       int       // Event sequence number
	Status         string    // CONFIRMED, CANCELLED, etc.

	// For REPLY messages
	AttendeeEmail    string // Who replied
	AttendeeName     string // Reply sender name
	AttendeePartStat string // ACCEPTED, DECLINED, TENTATIVE

	// Raw data
	ICSData string // Raw ICS content
}

// ProcessIncomingMessage checks a message for .ics attachments and processes them
func (h *IncomingHandler) ProcessIncomingMessage(msg *parser.ParsedMessage) ([]*InviteInfo, error) {
	var invites []*InviteInfo

	for _, att := range msg.Attachments {
		// Check for .ics file
		if !isICSAttachment(att) {
			continue
		}

		icsData := string(att.Data)
		info, err := h.ParseICSInvite(icsData)
		if err != nil {
			log.Printf("Failed to parse ICS attachment: %v", err)
			continue
		}

		invites = append(invites, info)
	}

	return invites, nil
}

// ParseICSInvite parses ICS data and extracts invite information
func (h *IncomingHandler) ParseICSInvite(icsData string) (*InviteInfo, error) {
	info := &InviteInfo{
		ICSData: icsData,
	}

	// Get method
	info.Method = importer.GetMethod(icsData)
	if info.Method == "" {
		info.Method = "REQUEST" // Default to REQUEST if not specified
	}

	// Parse with go-ical
	decoder := ical.NewDecoder(strings.NewReader(icsData))
	cal, err := decoder.Decode()
	if err != nil {
		// Try simple parsing as fallback
		return h.parseICSSimple(icsData)
	}

	// Get method from calendar
	if prop := cal.Props.Get(ical.PropMethod); prop != nil {
		info.Method = prop.Value
	}

	// Find the first VEVENT
	for _, event := range cal.Events() {
		// UID
		if prop := event.Props.Get(ical.PropUID); prop != nil {
			info.EventUID = prop.Value
		}

		// Summary
		if prop := event.Props.Get(ical.PropSummary); prop != nil {
			info.Summary = prop.Value
		}

		// Description
		if prop := event.Props.Get(ical.PropDescription); prop != nil {
			info.Description = prop.Value
		}

		// Location
		if prop := event.Props.Get(ical.PropLocation); prop != nil {
			info.Location = prop.Value
		}

		// Organizer
		info.OrganizerEmail, info.OrganizerName = importer.ParseOrganizer(&event)

		// DTSTART
		if prop := event.Props.Get(ical.PropDateTimeStart); prop != nil {
			t, err := prop.DateTime(nil)
			if err == nil {
				info.DTStart = t
			}
			if prop.Params.Get(ical.ParamValue) == "DATE" {
				info.AllDay = true
			}
		}

		// DTEND
		if prop := event.Props.Get(ical.PropDateTimeEnd); prop != nil {
			t, err := prop.DateTime(nil)
			if err == nil {
				info.DTEnd = t
			}
		}

		// Sequence
		if prop := event.Props.Get(ical.PropSequence); prop != nil {
			fmt.Sscanf(prop.Value, "%d", &info.Sequence)
		}

		// Status
		if prop := event.Props.Get(ical.PropStatus); prop != nil {
			info.Status = prop.Value
		}

		// For REPLY: get the attendee's response
		if info.Method == "REPLY" {
			attendees := importer.ParseAttendees(&event)
			if len(attendees) > 0 {
				info.AttendeeEmail = attendees[0].Email
				info.AttendeeName = attendees[0].Name
				info.AttendeePartStat = attendees[0].PartStat
			}
		}

		break // Only process first event
	}

	return info, nil
}

// parseICSSimple is a fallback parser for when go-ical fails
func (h *IncomingHandler) parseICSSimple(icsData string) (*InviteInfo, error) {
	info := &InviteInfo{
		ICSData: icsData,
		Method:  importer.GetMethod(icsData),
	}

	if info.Method == "" {
		info.Method = "REQUEST"
	}

	// Parse events using importer
	events, err := importer.ParseICS(icsData)
	if err != nil || len(events) == 0 {
		return nil, fmt.Errorf("failed to parse ICS: %w", err)
	}

	event := events[0]
	info.EventUID = event.UID
	info.Summary = event.Summary
	info.Description = event.Description
	info.Location = event.Location
	info.DTStart = event.DTStart
	if event.DTEnd.Valid {
		info.DTEnd = event.DTEnd.Time
	}
	info.AllDay = event.AllDay

	// Parse organizer and attendees from raw ICS
	info.OrganizerEmail, info.OrganizerName = importer.ParseOrganizerSimple(icsData)
	info.Sequence = importer.GetSequence(icsData)
	info.Status = importer.GetStatus(icsData)

	// For REPLY
	if info.Method == "REPLY" {
		attendees := importer.ParseAttendeesSimple(icsData)
		if len(attendees) > 0 {
			info.AttendeeEmail = attendees[0].Email
			info.AttendeeName = attendees[0].Name
			info.AttendeePartStat = attendees[0].PartStat
		}
	}

	return info, nil
}

// HandleInviteRequest processes an incoming invite REQUEST
func (h *IncomingHandler) HandleInviteRequest(userID int64, calendarID int64, info *InviteInfo) (*models.CalendarEvent, error) {
	// Check if event already exists
	existing, err := h.db.GetEventByUID(calendarID, info.EventUID)
	if err != nil {
		return nil, fmt.Errorf("failed to check existing event: %w", err)
	}

	event := &models.CalendarEvent{
		CalendarID:     calendarID,
		UID:            info.EventUID,
		ICalData:       info.ICSData,
		Summary:        info.Summary,
		Description:    info.Description,
		Location:       info.Location,
		DTStart:        info.DTStart,
		AllDay:         info.AllDay,
		OrganizerEmail: info.OrganizerEmail,
		OrganizerName:  info.OrganizerName,
		Sequence:       info.Sequence,
		Status:         info.Status,
	}

	if !info.DTEnd.IsZero() {
		event.DTEnd.Time = info.DTEnd
		event.DTEnd.Valid = true
	}

	if existing != nil {
		// Update existing event if sequence is higher
		if info.Sequence >= existing.Sequence {
			existing.ICalData = info.ICSData
			existing.Summary = info.Summary
			existing.Description = info.Description
			existing.Location = info.Location
			existing.DTStart = info.DTStart
			existing.DTEnd = event.DTEnd
			existing.AllDay = info.AllDay
			existing.OrganizerEmail = info.OrganizerEmail
			existing.OrganizerName = info.OrganizerName
			existing.Sequence = info.Sequence
			existing.Status = info.Status

			if err := h.db.UpdateCalendarEvent(existing); err != nil {
				return nil, fmt.Errorf("failed to update event: %w", err)
			}
			return existing, nil
		}
		return existing, nil // Ignore older sequence
	}

	// Create new event
	if err := h.db.CreateCalendarEvent(event); err != nil {
		return nil, fmt.Errorf("failed to create event: %w", err)
	}

	return event, nil
}

// HandleInviteReply processes an incoming REPLY
func (h *IncomingHandler) HandleInviteReply(info *InviteInfo) error {
	if info.AttendeeEmail == "" {
		return fmt.Errorf("no attendee in reply")
	}

	// Find the event by UID across all calendars
	// (Organizer might have created the event in any of their calendars)
	// For now, we need to search by UID

	log.Printf("Received REPLY from %s for event %s: %s",
		info.AttendeeEmail, info.EventUID, info.AttendeePartStat)

	// This is a simplified implementation - in a full implementation,
	// you would search for the event and update the attendee's status
	return nil
}

// HandleInviteCancel processes a CANCEL
func (h *IncomingHandler) HandleInviteCancel(userID int64, calendarID int64, info *InviteInfo) error {
	event, err := h.db.GetEventByUID(calendarID, info.EventUID)
	if err != nil || event == nil {
		return nil // Event doesn't exist, nothing to cancel
	}

	// Mark event as cancelled
	event.Status = "CANCELLED"
	event.Sequence = info.Sequence
	event.ICalData = info.ICSData

	if err := h.db.UpdateCalendarEvent(event); err != nil {
		return fmt.Errorf("failed to update event status: %w", err)
	}

	log.Printf("Event %s cancelled by organizer", info.EventUID)
	return nil
}

// isICSAttachment checks if an attachment is an ICS file
func isICSAttachment(att parser.ParsedAttachment) bool {
	// Check content type
	if strings.Contains(strings.ToLower(att.ContentType), "text/calendar") {
		return true
	}
	if strings.Contains(strings.ToLower(att.ContentType), "application/ics") {
		return true
	}

	// Check filename
	filename := strings.ToLower(att.Filename)
	return strings.HasSuffix(filename, ".ics") || strings.HasSuffix(filename, ".ical")
}

// FindUserCalendarForInvites finds the default calendar for accepting invites
// Returns the first calendar linked to the account, or any calendar if none linked
func (h *IncomingHandler) FindUserCalendarForInvites(userID int64, accountID int64) (*models.Calendar, error) {
	// First, try to find a calendar source linked to this account
	sources, err := h.db.GetCalendarSourcesByUserID(userID)
	if err != nil {
		return nil, fmt.Errorf("failed to get calendar sources: %w", err)
	}

	for _, source := range sources {
		if source.AccountID != nil && *source.AccountID == accountID {
			calendars, err := h.db.GetCalendarsBySourceID(source.ID)
			if err != nil {
				continue
			}
			if len(calendars) > 0 {
				return calendars[0], nil
			}
		}
	}

	// Fallback: find any local calendar
	for _, source := range sources {
		if source.SourceType == "local" {
			calendars, err := h.db.GetCalendarsBySourceID(source.ID)
			if err != nil {
				continue
			}
			if len(calendars) > 0 {
				return calendars[0], nil
			}
		}
	}

	return nil, fmt.Errorf("no calendar found for accepting invites")
}
