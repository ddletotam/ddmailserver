package generator

import (
	"fmt"
	"strings"
	"time"

	"github.com/yourusername/mailserver/internal/models"
)

// GenerateInvite generates an iCal invite (REQUEST, REPLY, or CANCEL)
func GenerateInvite(event *models.CalendarEvent, attendees []models.CalendarAttendee, method string) string {
	var sb strings.Builder

	sb.WriteString("BEGIN:VCALENDAR\r\n")
	sb.WriteString("VERSION:2.0\r\n")
	sb.WriteString("PRODID:-//DDMailServer//Calendar//EN\r\n")
	sb.WriteString(fmt.Sprintf("METHOD:%s\r\n", method))

	sb.WriteString("BEGIN:VEVENT\r\n")
	sb.WriteString(fmt.Sprintf("UID:%s\r\n", event.UID))
	sb.WriteString(fmt.Sprintf("DTSTAMP:%s\r\n", formatDateTime(time.Now().UTC())))

	// Start time
	if event.AllDay {
		sb.WriteString(fmt.Sprintf("DTSTART;VALUE=DATE:%s\r\n", formatDate(event.DTStart)))
	} else {
		sb.WriteString(fmt.Sprintf("DTSTART:%s\r\n", formatDateTime(event.DTStart)))
	}

	// End time
	if event.DTEnd.Valid {
		if event.AllDay {
			sb.WriteString(fmt.Sprintf("DTEND;VALUE=DATE:%s\r\n", formatDate(event.DTEnd.Time)))
		} else {
			sb.WriteString(fmt.Sprintf("DTEND:%s\r\n", formatDateTime(event.DTEnd.Time)))
		}
	}

	// Summary
	if event.Summary != "" {
		sb.WriteString(fmt.Sprintf("SUMMARY:%s\r\n", escapeText(event.Summary)))
	}

	// Description
	if event.Description != "" {
		sb.WriteString(fmt.Sprintf("DESCRIPTION:%s\r\n", escapeText(event.Description)))
	}

	// Location
	if event.Location != "" {
		sb.WriteString(fmt.Sprintf("LOCATION:%s\r\n", escapeText(event.Location)))
	}

	// Sequence
	sb.WriteString(fmt.Sprintf("SEQUENCE:%d\r\n", event.Sequence))

	// Status
	status := event.Status
	if status == "" {
		status = "CONFIRMED"
	}
	sb.WriteString(fmt.Sprintf("STATUS:%s\r\n", status))

	// Organizer
	if event.OrganizerEmail != "" {
		if event.OrganizerName != "" {
			sb.WriteString(fmt.Sprintf("ORGANIZER;CN=%s:mailto:%s\r\n",
				escapeParam(event.OrganizerName), event.OrganizerEmail))
		} else {
			sb.WriteString(fmt.Sprintf("ORGANIZER:mailto:%s\r\n", event.OrganizerEmail))
		}
	}

	// Attendees
	for _, att := range attendees {
		sb.WriteString(formatAttendee(att))
	}

	// Recurrence rule
	if event.RRule != "" {
		sb.WriteString(fmt.Sprintf("RRULE:%s\r\n", event.RRule))
	}

	sb.WriteString("END:VEVENT\r\n")
	sb.WriteString("END:VCALENDAR\r\n")

	return sb.String()
}

// GenerateReply generates an iCal REPLY for an invite
func GenerateReply(event *models.CalendarEvent, attendeeEmail, attendeeName, partstat string) string {
	attendee := models.CalendarAttendee{
		Email:    attendeeEmail,
		Name:     attendeeName,
		PartStat: partstat,
		RSVP:     false,
	}
	return GenerateInvite(event, []models.CalendarAttendee{attendee}, "REPLY")
}

// GenerateCancel generates an iCal CANCEL for an event
func GenerateCancel(event *models.CalendarEvent, attendees []models.CalendarAttendee) string {
	cancelEvent := *event
	cancelEvent.Status = "CANCELLED"
	return GenerateInvite(&cancelEvent, attendees, "CANCEL")
}

// GenerateRequest generates an iCal REQUEST for a new invite
func GenerateRequest(event *models.CalendarEvent, attendees []models.CalendarAttendee) string {
	return GenerateInvite(event, attendees, "REQUEST")
}

// formatAttendee formats a single ATTENDEE line
func formatAttendee(att models.CalendarAttendee) string {
	var parts []string

	if att.Name != "" {
		parts = append(parts, fmt.Sprintf("CN=%s", escapeParam(att.Name)))
	}

	role := att.Role
	if role == "" {
		role = "REQ-PARTICIPANT"
	}
	parts = append(parts, fmt.Sprintf("ROLE=%s", role))

	partstat := att.PartStat
	if partstat == "" {
		partstat = "NEEDS-ACTION"
	}
	parts = append(parts, fmt.Sprintf("PARTSTAT=%s", partstat))

	if att.RSVP {
		parts = append(parts, "RSVP=TRUE")
	}

	if len(parts) > 0 {
		return fmt.Sprintf("ATTENDEE;%s:mailto:%s\r\n", strings.Join(parts, ";"), att.Email)
	}
	return fmt.Sprintf("ATTENDEE:mailto:%s\r\n", att.Email)
}

// formatDateTime formats a time in iCal UTC format
func formatDateTime(t time.Time) string {
	return t.UTC().Format("20060102T150405Z")
}

// formatDate formats a date in iCal date format (no time)
func formatDate(t time.Time) string {
	return t.Format("20060102")
}

// escapeText escapes text for iCal (newlines, semicolons, commas, backslashes)
func escapeText(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, ";", "\\;")
	s = strings.ReplaceAll(s, ",", "\\,")
	s = strings.ReplaceAll(s, "\n", "\\n")
	return s
}

// escapeParam escapes a parameter value (needs quoting if contains special chars)
func escapeParam(s string) string {
	if strings.ContainsAny(s, ":;,") {
		return fmt.Sprintf("\"%s\"", strings.ReplaceAll(s, "\"", "'"))
	}
	return s
}
