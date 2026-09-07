package calendar

import (
	"errors"
	"fmt"
	"log"
	"sort"
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
		cal, err := h.FindCalendarForInvite(userID, accountID, invitedIdentities(info.Attendees, idSet))
		if errors.Is(err, ErrNoInviteCalendar) {
			// Leave the message where it is. Consuming it would delete the
			// mail (see the MX and IMAP call sites) and the invite would exist
			// nowhere at all.
			log.Printf("[ics] no destination calendar for uid=%s (account=%d) — leaving the invite as mail: %v",
				info.EventUID, accountID, err)
			return false, nil
		}
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

// ErrNoInviteCalendar means no destination calendar could be chosen for an
// invite without guessing. The caller must leave the message alone: an invite
// that stays a mail in the inbox is recoverable, an invite filed into the
// wrong account is not.
var ErrNoInviteCalendar = errors.New("no calendar matches the invited identity")

// FindCalendarForInvite picks the destination calendar for an incoming invite.
//
// `invited` holds the user's own addresses that appear in the ICS attendee
// list — the addresses this invite is actually for. `accountID` is the mail
// account that delivered it (0 for local MX delivery, which has none).
//
// Priority:
//  1. A calendar whose source is bound to the delivering account.
//  2. A calendar whose source authenticates as one of the invited addresses
//     (CalDAV username) — the same person's calendar on the same provider.
//  3. A local calendar.
//  4. A calendar whose source declares one of the invited addresses as its
//     identity, and only if exactly one source does.
//
// There is deliberately no "any enabled calendar" fallback any more. That
// fallback took allCals[0] from a list ordered by created_at DESC, so the
// destination for every unmatched invite was whatever calendar the user had
// added most recently. A work invite addressed to one account was filed into
// an unrelated account's calendar, reverse sync then pushed it at that
// account's server, and the server refused it with a 403 — for days, one
// warning email per day. Not choosing is better than choosing wrong.
//
// Disabled calendars are excluded — that is the user's "don't touch this one".
// So are read-only ones: writing into a collection the remote will not accept
// is the failure above with extra steps.
func (h *IncomingHandler) FindCalendarForInvite(userID, accountID int64, invited map[string]struct{}) (*models.Calendar, error) {
	cals, err := h.db.GetEnabledCalendarsByUserID(userID)
	if err != nil {
		return nil, fmt.Errorf("failed to load calendars: %w", err)
	}
	sources, err := h.db.GetCalendarSourcesByUserID(userID)
	if err != nil {
		return nil, fmt.Errorf("failed to get calendar sources: %w", err)
	}
	return chooseInviteCalendar(cals, sources, accountID, invited)
}

// chooseInviteCalendar is the decision itself, separated from the loading so
// the priority order can be tested without a database. `cals` must already be
// the user's enabled calendars.
func chooseInviteCalendar(
	cals []*models.Calendar,
	sources []*models.CalendarSource,
	accountID int64,
	invited map[string]struct{},
) (*models.Calendar, error) {
	// Group writable calendars by source, lowest id first within each source.
	//
	// The order matters and must not be "newest": a calendar list reordered by
	// every new subscription silently moves where invites land. Lowest id is
	// the oldest collection of that source, which for a CalDAV account is its
	// primary calendar.
	bySource := make(map[int64][]*models.Calendar, len(sources))
	for _, c := range cals {
		if !c.CanWrite {
			continue
		}
		bySource[c.SourceID] = append(bySource[c.SourceID], c)
	}
	if len(bySource) == 0 {
		return nil, ErrNoInviteCalendar
	}
	for id := range bySource {
		sort.Slice(bySource[id], func(i, j int) bool { return bySource[id][i].ID < bySource[id][j].ID })
	}

	// Sources in a stable order too, so a tie resolves the same way twice.
	sort.Slice(sources, func(i, j int) bool { return sources[i].ID < sources[j].ID })

	firstCalendarOf := func(sourceID int64) *models.Calendar {
		if list := bySource[sourceID]; len(list) > 0 {
			return list[0]
		}
		return nil
	}

	// Pass 1: the source bound to the account that delivered the invite.
	if accountID != 0 {
		for _, src := range sources {
			if src.AccountID == nil || *src.AccountID != accountID {
				continue
			}
			if cal := firstCalendarOf(src.ID); cal != nil {
				return cal, nil
			}
		}
	}

	// Pass 2: the source that logs in as one of the invited addresses.
	for _, src := range sources {
		if !isInvited(src.CalDAVUsername, invited) {
			continue
		}
		if cal := firstCalendarOf(src.ID); cal != nil {
			return cal, nil
		}
	}

	// Pass 3: a local calendar. Nothing is pushed anywhere from here, so a
	// wrong guess costs a stray local event rather than a write into somebody
	// else's account.
	for _, src := range sources {
		if src.SourceType != "local" {
			continue
		}
		if cal := firstCalendarOf(src.ID); cal != nil {
			return cal, nil
		}
	}

	// Pass 4: the declared identity of the source — but only when it singles
	// one out. identity_email defaults to the user's default sending identity,
	// so several sources routinely share it, and "several" is precisely the
	// case this function must not resolve by picking one.
	var byIdentity []*models.Calendar
	for _, src := range sources {
		if !isInvited(src.IdentityEmail, invited) {
			continue
		}
		if cal := firstCalendarOf(src.ID); cal != nil {
			byIdentity = append(byIdentity, cal)
		}
	}
	switch len(byIdentity) {
	case 0:
		return nil, ErrNoInviteCalendar
	case 1:
		return byIdentity[0], nil
	default:
		return nil, fmt.Errorf("%w: %d sources claim it", ErrNoInviteCalendar, len(byIdentity))
	}
}

// isInvited reports whether an address is one of the addresses this invite was
// addressed to. Comparison is case-insensitive: mail addresses arrive in
// whatever case the sender's client felt like using.
func isInvited(addr string, invited map[string]struct{}) bool {
	addr = strings.ToLower(strings.TrimSpace(addr))
	if addr == "" {
		return false
	}
	_, ok := invited[addr]
	return ok
}

// invitedIdentities returns the user's own addresses that this invite names as
// attendees — the intersection of the identities the delivering path knows
// about and the ICS attendee list.
func invitedIdentities(attendees []models.CalendarAttendee, idSet map[string]struct{}) map[string]struct{} {
	out := make(map[string]struct{}, len(idSet))
	for _, a := range attendees {
		email := strings.ToLower(strings.TrimSpace(a.Email))
		if email == "" {
			continue
		}
		if _, ok := idSet[email]; ok {
			out[email] = struct{}{}
		}
	}
	return out
}
