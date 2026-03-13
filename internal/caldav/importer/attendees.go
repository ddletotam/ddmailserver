package importer

import (
	"fmt"
	"strings"

	"github.com/emersion/go-ical"
	"github.com/yourusername/mailserver/internal/models"
)

// ParseAttendees extracts attendees from an iCal event
func ParseAttendees(event *ical.Event) []models.CalendarAttendee {
	var attendees []models.CalendarAttendee

	for _, prop := range event.Props.Values(ical.PropAttendee) {
		attendee := models.CalendarAttendee{
			Email:    extractEmail(prop.Value),
			Name:     prop.Params.Get("CN"),
			Role:     prop.Params.Get("ROLE"),
			PartStat: prop.Params.Get("PARTSTAT"),
			RSVP:     strings.ToUpper(prop.Params.Get("RSVP")) == "TRUE",
		}

		// Set defaults
		if attendee.Role == "" {
			attendee.Role = "REQ-PARTICIPANT"
		}
		if attendee.PartStat == "" {
			attendee.PartStat = "NEEDS-ACTION"
		}

		if attendee.Email != "" {
			attendees = append(attendees, attendee)
		}
	}

	return attendees
}

// ParseOrganizer extracts the organizer from an iCal event
func ParseOrganizer(event *ical.Event) (email, name string) {
	if prop := event.Props.Get(ical.PropOrganizer); prop != nil {
		email = extractEmail(prop.Value)
		name = prop.Params.Get("CN")
	}
	return
}

// extractEmail extracts email from a mailto: URI or plain email
func extractEmail(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "mailto:")
	value = strings.TrimPrefix(value, "MAILTO:")
	return strings.ToLower(value)
}

// ParseAttendeesSimple extracts attendees from raw VEVENT text
// Fallback for when go-ical parser doesn't work
func ParseAttendeesSimple(vevent string) []models.CalendarAttendee {
	var attendees []models.CalendarAttendee
	lines := strings.Split(vevent, "\n")

	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(line, "ATTENDEE") {
			continue
		}

		attendee := models.CalendarAttendee{
			Role:     "REQ-PARTICIPANT",
			PartStat: "NEEDS-ACTION",
		}

		// Parse parameters and value
		// Format: ATTENDEE;PARAM=VALUE;PARAM=VALUE:mailto:email@example.com
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}

		// Extract email from value
		attendee.Email = extractEmail(parts[1])
		if attendee.Email == "" {
			continue
		}

		// Parse parameters
		paramPart := parts[0]
		params := strings.Split(paramPart, ";")
		for _, param := range params[1:] { // Skip "ATTENDEE"
			kv := strings.SplitN(param, "=", 2)
			if len(kv) != 2 {
				continue
			}
			key := strings.ToUpper(kv[0])
			value := strings.Trim(kv[1], "\"")

			switch key {
			case "CN":
				attendee.Name = value
			case "ROLE":
				attendee.Role = value
			case "PARTSTAT":
				attendee.PartStat = value
			case "RSVP":
				attendee.RSVP = strings.ToUpper(value) == "TRUE"
			}
		}

		attendees = append(attendees, attendee)
	}

	return attendees
}

// ParseOrganizerSimple extracts organizer from raw VEVENT text
func ParseOrganizerSimple(vevent string) (email, name string) {
	lines := strings.Split(vevent, "\n")

	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(line, "ORGANIZER") {
			continue
		}

		// Parse parameters and value
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}

		email = extractEmail(parts[1])

		// Parse CN parameter
		paramPart := parts[0]
		params := strings.Split(paramPart, ";")
		for _, param := range params[1:] {
			kv := strings.SplitN(param, "=", 2)
			if len(kv) == 2 && strings.ToUpper(kv[0]) == "CN" {
				name = strings.Trim(kv[1], "\"")
				break
			}
		}

		return
	}

	return
}

// GetMethod extracts the METHOD from iCal data
func GetMethod(icsData string) string {
	lines := strings.Split(icsData, "\n")
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, "METHOD:") {
			return strings.TrimPrefix(line, "METHOD:")
		}
	}
	return ""
}

// GetSequence extracts the SEQUENCE from iCal data
func GetSequence(icsData string) int {
	lines := strings.Split(icsData, "\n")
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, "SEQUENCE:") {
			seq := strings.TrimPrefix(line, "SEQUENCE:")
			var n int
			fmt.Sscanf(seq, "%d", &n)
			return n
		}
	}
	return 0
}

// GetStatus extracts the STATUS from iCal data
func GetStatus(icsData string) string {
	lines := strings.Split(icsData, "\n")
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, "STATUS:") {
			return strings.TrimPrefix(line, "STATUS:")
		}
	}
	return "CONFIRMED"
}
