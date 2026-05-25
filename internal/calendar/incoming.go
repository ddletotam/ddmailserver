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
	"github.com/yourusername/mailserver/internal/timeutil"
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
	Method         string    // REQUEST, REPLY, CANCEL, COUNTER, PUBLISH
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

	// All attendees from the VEVENT (used for PUBLISH self-check, REQUEST
	// import, COUNTER reset).
	Attendees []models.CalendarAttendee

	// For REPLY messages (single replying attendee in METHOD=REPLY)
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

		// TEXT-typed properties — decode RFC 5545 escapes via Text() so we
		// don't leak literal `\,` into Summary/Description/Location.
		if prop := event.Props.Get(ical.PropSummary); prop != nil {
			if v, err := prop.Text(); err == nil {
				info.Summary = v
			} else {
				info.Summary = prop.Value
			}
		}
		if prop := event.Props.Get(ical.PropDescription); prop != nil {
			if v, err := prop.Text(); err == nil {
				info.Description = v
			} else {
				info.Description = prop.Value
			}
		}
		if prop := event.Props.Get(ical.PropLocation); prop != nil {
			if v, err := prop.Text(); err == nil {
				info.Location = v
			} else {
				info.Location = prop.Value
			}
		}

		// Organizer
		info.OrganizerEmail, info.OrganizerName = importer.ParseOrganizer(&event)

		// All attendees — used for PUBLISH self-check + REQUEST/COUNTER
		// import of the participant roster. Caller still inspects single-
		// attendee REPLY via AttendeeEmail/PartStat below.
		info.Attendees = importer.ParseAttendees(&event)

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
	info.DTStart = timeutil.FromMs(event.DTStart)
	if event.DTEnd != nil && *event.DTEnd != 0 {
		info.DTEnd = timeutil.FromMs(*event.DTEnd)
	}
	info.AllDay = event.AllDay

	// Parse organizer and attendees from raw ICS
	info.OrganizerEmail, info.OrganizerName = importer.ParseOrganizerSimple(icsData)
	info.Attendees = importer.ParseAttendeesSimple(icsData)
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

// ProcessAndDispatch is the single entry point used by inbound paths (MX
// delivery + IMAP sync). Parses .ics attachments from the message, routes
// each invite by METHOD, and returns true when at least one invite was
// successfully consumed — the caller deletes the email row in that case so
// service-only iTIP messages don't clutter the conversation list.
//
// `recipientIdentities` is the set of the user's email addresses that this
// particular delivery could have been addressed to (the primary email + any
// aliases for IMAP sync, just the envelope-to address for local MX). Used
// for the PUBLISH self-check and for picking MY attendee on REQUEST /
// COUNTER (whose PartStat we force back to NEEDS-ACTION).
func (h *IncomingHandler) ProcessAndDispatch(
	parsedMsg *parser.ParsedMessage,
	userID, accountID int64,
	recipientIdentities []string,
) (bool, error) {
	invites, err := h.ProcessIncomingMessage(parsedMsg)
	if err != nil {
		return false, err
	}
	if len(invites) == 0 {
		return false, nil
	}

	idSet := make(map[string]struct{}, len(recipientIdentities))
	for _, e := range recipientIdentities {
		e = strings.ToLower(strings.TrimSpace(e))
		if e != "" {
			idSet[e] = struct{}{}
		}
	}

	handled := false
	for _, info := range invites {
		ok, derr := h.dispatchInvite(info, userID, accountID, idSet)
		if derr != nil {
			log.Printf("[ics] dispatch (%s, uid=%s): %v", info.Method, info.EventUID, derr)
			continue
		}
		if ok {
			handled = true
		}
	}
	return handled, nil
}

func (h *IncomingHandler) dispatchInvite(info *InviteInfo, userID, accountID int64, idSet map[string]struct{}) (bool, error) {
	method := strings.ToUpper(strings.TrimSpace(info.Method))
	if method == "" {
		method = "REQUEST"
	}

	switch method {
	case "PUBLISH":
		// PUBLISH is "FYI, here's an event". Skip unless one of the user's
		// identities is in the attendee list — otherwise random external
		// feeds would inject events into the user's calendar.
		if !attendeesIntersect(info.Attendees, idSet) {
			return false, nil
		}
		fallthrough
	case "REQUEST", "COUNTER":
		// REQUEST creates / updates with MY PartStat reset to NEEDS-ACTION.
		// COUNTER (= proposing a different time) takes the same path: the
		// updated time lands in the event, and the user is bumped back to
		// NEEDS-ACTION so they re-confirm.
		cal, err := h.FindUserCalendarForInvites(userID, accountID)
		if err != nil {
			return false, err
		}
		if _, err := h.HandleInviteRequest(cal.ID, idSet, info); err != nil {
			return false, err
		}
		return true, nil
	case "CANCEL":
		return h.handleCancel(userID, info)
	case "REPLY":
		return h.handleReply(userID, info)
	default:
		return false, fmt.Errorf("unknown METHOD: %s", method)
	}
}

func attendeesIntersect(attendees []models.CalendarAttendee, idSet map[string]struct{}) bool {
	for _, a := range attendees {
		if _, ok := idSet[strings.ToLower(a.Email)]; ok {
			return true
		}
	}
	return false
}

// HandleInviteRequest creates-or-updates a calendar event from a REQUEST /
// COUNTER / addressed-PUBLISH. Attendees are replaced wholesale and any
// attendee in `idSet` (the user's identity emails) is forced to
// NEEDS-ACTION (user opens the calendar to decide).
func (h *IncomingHandler) HandleInviteRequest(calendarID int64, idSet map[string]struct{}, info *InviteInfo) (*models.CalendarEvent, error) {
	existing, err := h.db.GetEventByUID(calendarID, info.EventUID)
	if err != nil {
		return nil, fmt.Errorf("failed to check existing event: %w", err)
	}

	dtStartMs := timeutil.ToMs(info.DTStart)
	var dtEndPtr *int64
	if !info.DTEnd.IsZero() {
		ms := timeutil.ToMs(info.DTEnd)
		dtEndPtr = &ms
	}

	var event *models.CalendarEvent
	if existing != nil {
		if info.Sequence < existing.Sequence {
			// Stale invite — keep what we have, but still consider this
			// "handled" so the email can go.
			return existing, nil
		}
		existing.ICalData = info.ICSData
		existing.Summary = info.Summary
		existing.Description = info.Description
		existing.Location = info.Location
		existing.DTStart = dtStartMs
		existing.DTEnd = dtEndPtr
		existing.AllDay = info.AllDay
		existing.OrganizerEmail = info.OrganizerEmail
		existing.OrganizerName = info.OrganizerName
		existing.Sequence = info.Sequence
		existing.Status = info.Status
		if err := h.db.UpdateCalendarEvent(existing); err != nil {
			return nil, fmt.Errorf("failed to update event: %w", err)
		}
		event = existing
	} else {
		event = &models.CalendarEvent{
			CalendarID:     calendarID,
			UID:            info.EventUID,
			ICalData:       info.ICSData,
			Summary:        info.Summary,
			Description:    info.Description,
			Location:       info.Location,
			DTStart:        dtStartMs,
			DTEnd:          dtEndPtr,
			AllDay:         info.AllDay,
			OrganizerEmail: info.OrganizerEmail,
			OrganizerName:  info.OrganizerName,
			Sequence:       info.Sequence,
			Status:         info.Status,
		}
		if err := h.db.CreateCalendarEvent(event); err != nil {
			return nil, fmt.Errorf("failed to create event: %w", err)
		}
	}

	// Replace attendees, forcing MY PartStat to NEEDS-ACTION regardless of
	// what the organizer originally wrote.
	if len(info.Attendees) > 0 {
		atts := make([]*models.CalendarAttendee, 0, len(info.Attendees))
		for i := range info.Attendees {
			a := info.Attendees[i]
			if _, mine := idSet[strings.ToLower(a.Email)]; mine {
				a.PartStat = "NEEDS-ACTION"
			}
			atts = append(atts, &a)
		}
		if err := h.db.ReplaceAttendees(event.ID, atts); err != nil {
			log.Printf("[ics] ReplaceAttendees event=%d: %v", event.ID, err)
		}
	}

	return event, nil
}

// handleReply updates the responding attendee's PartStat on whichever of the
// user's calendars holds the event. Per spec, a REPLY carries exactly one
// ATTENDEE — the responder. Missing events are silently accepted (caller
// deletes the email regardless: stale REPLY for a deleted local event is a
// "process and forget" case).
func (h *IncomingHandler) handleReply(userID int64, info *InviteInfo) (bool, error) {
	if info.AttendeeEmail == "" || info.AttendeePartStat == "" {
		return false, fmt.Errorf("REPLY missing attendee/partstat")
	}
	event, err := h.findEventByUIDForUser(userID, info.EventUID)
	if err != nil {
		return false, err
	}
	if event == nil {
		log.Printf("[ics] REPLY for unknown event uid=%s; consumed", info.EventUID)
		return true, nil
	}
	if err := h.db.UpdateAttendeePartStat(event.ID, strings.ToLower(info.AttendeeEmail), info.AttendeePartStat); err != nil {
		return false, fmt.Errorf("UpdateAttendeePartStat: %w", err)
	}
	return true, nil
}

// handleCancel hard-deletes the matching event row across all of the user's
// calendars. Per the product brief: organizer cancelled → event goes away.
func (h *IncomingHandler) handleCancel(userID int64, info *InviteInfo) (bool, error) {
	event, err := h.findEventByUIDForUser(userID, info.EventUID)
	if err != nil {
		return false, err
	}
	if event == nil {
		log.Printf("[ics] CANCEL for unknown event uid=%s; consumed", info.EventUID)
		return true, nil
	}
	if err := h.db.DeleteCalendarEvent(event.ID); err != nil {
		return false, fmt.Errorf("DeleteCalendarEvent: %w", err)
	}
	return true, nil
}

// findEventByUIDForUser scans every calendar the user owns and returns the
// first matching event by UID. Used for REPLY / CANCEL where the original
// invite may have been imported into any calendar of any account.
func (h *IncomingHandler) findEventByUIDForUser(userID int64, uid string) (*models.CalendarEvent, error) {
	cals, err := h.db.GetCalendarsByUserID(userID)
	if err != nil {
		return nil, fmt.Errorf("GetCalendarsByUserID: %w", err)
	}
	for _, c := range cals {
		ev, err := h.db.GetEventByUID(c.ID, uid)
		if err != nil {
			log.Printf("[ics] GetEventByUID cal=%d: %v", c.ID, err)
			continue
		}
		if ev != nil {
			return ev, nil
		}
	}
	return nil, nil
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

// FindUserCalendarForInvites picks a destination calendar for an incoming
// invite. Priority:
//  1. Any *enabled* calendar from a CalDAV source bound to the recipient's
//     mail account (the user said: "if many calendars per account, any
//     active one is fine").
//  2. Any enabled local calendar of the user.
//  3. Any enabled calendar at all.
//
// Disabled calendars are excluded — they're the user's signal of "don't
// touch this one." Returns an error if the user has no calendars.
func (h *IncomingHandler) FindUserCalendarForInvites(userID int64, accountID int64) (*models.Calendar, error) {
	allCals, err := h.db.GetEnabledCalendarsByUserID(userID)
	if err != nil {
		return nil, fmt.Errorf("failed to load calendars: %w", err)
	}
	if len(allCals) == 0 {
		return nil, fmt.Errorf("no enabled calendars for user %d", userID)
	}

	sources, err := h.db.GetCalendarSourcesByUserID(userID)
	if err != nil {
		return nil, fmt.Errorf("failed to get calendar sources: %w", err)
	}

	// Build source.ID → source for membership checks.
	sourceByID := make(map[int64]*models.CalendarSource, len(sources))
	for i := range sources {
		sourceByID[sources[i].ID] = sources[i]
	}

	// Pass 1: calendars whose source is bound to the recipient's account.
	for _, c := range allCals {
		if src, ok := sourceByID[c.SourceID]; ok && src.AccountID != nil && *src.AccountID == accountID {
			return c, nil
		}
	}
	// Pass 2: local calendars.
	for _, c := range allCals {
		if src, ok := sourceByID[c.SourceID]; ok && src.SourceType == "local" {
			return c, nil
		}
	}
	// Pass 3: anything enabled.
	return allCals[0], nil
}
