package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/emersion/go-ical"
	"github.com/gorilla/mux"
	"github.com/yourusername/mailserver/internal/models"
)

// HandleDesktopEventRSVP updates the requesting user's PARTSTAT on an event.
//
// POST /api/desktop/v1/events/{id}/rsvp  body: {"partstat":"ACCEPTED"|"DECLINED"|"TENTATIVE"}
//
// Resolves the calling user's identity emails (local mailboxes + external
// account aliases) and matches against the event's attendee rows. The new
// PARTSTAT is written both to the indexed attendee row and to the raw iCal
// data so a downstream CalDAV push carries the value to the source server.
// Read-only calendars (ICS) are rejected — the client should hide RSVP pills
// for those.
func (s *Server) HandleDesktopEventRSVP(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		respondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	eventID, err := strconv.ParseInt(mux.Vars(r)["id"], 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "bad event id")
		return
	}

	var req struct {
		PartStat string `json:"partstat"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid body")
		return
	}
	partstat := strings.ToUpper(strings.TrimSpace(req.PartStat))
	switch partstat {
	case "ACCEPTED", "DECLINED", "TENTATIVE":
	default:
		respondError(w, http.StatusBadRequest, "partstat must be ACCEPTED|DECLINED|TENTATIVE")
		return
	}

	event, err := s.database.GetEventByID(eventID)
	if err != nil {
		respondError(w, http.StatusNotFound, "event not found")
		return
	}
	cal, err := s.database.GetCalendarByID(event.CalendarID)
	if err != nil || cal.UserID != user.ID {
		respondError(w, http.StatusNotFound, "calendar not found")
		return
	}
	if cal.SourceType == "ics_url" || cal.SourceType == "ics_import" {
		respondError(w, http.StatusBadRequest, "calendar is read-only")
		return
	}

	myEmails := s.collectUserIdentityEmails(user)
	atts, err := s.database.GetAttendeesByEventID(eventID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load attendees")
		return
	}
	var myEmail string
	for _, a := range atts {
		if myEmails[strings.ToLower(a.Email)] {
			myEmail = strings.ToLower(a.Email)
			break
		}
	}
	if myEmail == "" {
		respondError(w, http.StatusBadRequest, "you are not in the attendee list of this event")
		return
	}

	if err := s.database.UpdateAttendeePartStat(eventID, myEmail, partstat); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to update partstat")
		return
	}

	// Rewrite the ATTENDEE PARTSTAT in the raw iCal so the next CalDAV push
	// carries our answer to the source server. Failure here is logged but
	// non-fatal — the indexed row is the source of truth for our own UI.
	if newICal, ok := mutateAttendeePartStat(event.ICalData, myEmail, partstat); ok {
		event.ICalData = newICal
		event.LocalModified = true
		if err := s.database.UpdateCalendarEvent(event); err == nil {
			if cal.SourceType == "caldav" && cal.ReverseSync && cal.Enabled {
				_ = s.database.QueueCalendarEventSync(
					event.ID, cal.ID, cal.SourceID, event.UID, event.RemoteID,
					event.ICalData, "update",
				)
			}
		}
	}

	respondJSON(w, http.StatusOK, map[string]string{
		"partstat": partstat,
		"email":    myEmail,
	})
}

// mutateAttendeePartStat parses the iCal payload, finds the ATTENDEE prop
// whose value (mailto: target) matches `email` (case-insensitive), updates
// its PARTSTAT param, and re-encodes. Returns ok=false when parsing fails
// or no matching attendee was found — caller treats it as "skip the iCal
// rewrite" rather than failing the request.
func mutateAttendeePartStat(icalData, email, partstat string) (string, bool) {
	dec := ical.NewDecoder(strings.NewReader(icalData))
	cal, err := dec.Decode()
	if err != nil {
		return "", false
	}
	target := strings.ToLower(strings.TrimSpace(email))
	mutated := false
	for _, comp := range cal.Children {
		if comp.Name != ical.CompEvent {
			continue
		}
		for _, prop := range comp.Props.Values(ical.PropAttendee) {
			addr := strings.ToLower(strings.TrimSpace(prop.Value))
			addr = strings.TrimPrefix(addr, "mailto:")
			if addr != target {
				continue
			}
			prop.Params.Set("PARTSTAT", partstat)
			mutated = true
		}
	}
	if !mutated {
		return "", false
	}
	var buf bytes.Buffer
	if err := ical.NewEncoder(&buf).Encode(cal); err != nil {
		return "", false
	}
	return buf.String(), true
}

// collectUserIdentityEmails returns the set of lowercased emails owned by
// this user — local mailboxes plus external account primary + aliases.
// Used for RSVP and "is this me?" checks. Lives here so the RSVP handler
// doesn't need to duplicate the identity-gathering logic from
// HandleDesktopIdentities (which is shaped for serialization to the client).
func (s *Server) collectUserIdentityEmails(user *models.User) map[string]bool {
	out := map[string]bool{}
	uid := user.ID

	if mailboxes, err := s.database.GetMailboxesWithDomainByUserID(uid); err == nil {
		for _, mb := range mailboxes {
			if !mb.Enabled {
				continue
			}
			out[strings.ToLower(fmt.Sprintf("%s@%s", mb.LocalPart, mb.DomainName))] = true
		}
	}

	if accounts, err := s.database.GetAccountsByUserID(uid); err == nil {
		for _, acc := range accounts {
			if acc.Email == "" || !acc.Enabled {
				continue
			}
			out[strings.ToLower(acc.Email)] = true
			for _, alias := range acc.GetAliases() {
				out[strings.ToLower(alias)] = true
			}
		}
	}
	return out
}
