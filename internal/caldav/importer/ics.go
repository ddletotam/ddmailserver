package importer

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	"github.com/emersion/go-ical"
	"github.com/yourusername/mailserver/internal/db"
	"github.com/yourusername/mailserver/internal/models"
)

// ParseICS parses ICS data and returns events without importing
// Use this when you need transactional control over the import
func ParseICS(icsData string) ([]*models.CalendarEvent, error) {
	decoder := ical.NewDecoder(strings.NewReader(icsData))
	var events []*models.CalendarEvent

	for {
		cal, err := decoder.Decode()
		if err != nil {
			break
		}

		for _, event := range cal.Events() {
			modelEvent, err := parseICalEvent(&event, 0, cal)
			if err != nil {
				continue
			}
			modelEvent.ETag = generateETag(modelEvent.ICalData)
			events = append(events, modelEvent)
		}
	}

	// Fallback to simple parsing if go-ical didn't parse any events
	if len(events) == 0 {
		vevents := extractVEvents(icsData)
		for _, vevent := range vevents {
			event := parseVEvent(vevent, 0)
			if event != nil && event.UID != "" {
				event.ETag = generateETag(event.ICalData)
				events = append(events, event)
			}
		}
	}

	return events, nil
}

// ImportICS imports events from ICS data into a calendar
// Returns the number of imported events
func ImportICS(database *db.DB, calendarID int64, icsData []byte) (int, error) {
	// Parse the ICS data using go-ical
	decoder := ical.NewDecoder(strings.NewReader(string(icsData)))

	imported := 0

	for {
		cal, err := decoder.Decode()
		if err != nil {
			break
		}

		// Process each VEVENT in the calendar
		for _, event := range cal.Events() {
			modelEvent, err := parseICalEvent(&event, calendarID, cal)
			if err != nil {
				continue // Skip invalid events
			}

			// Check if event already exists
			existing, err := database.GetEventByUID(calendarID, modelEvent.UID)
			if err != nil {
				return imported, fmt.Errorf("failed to check existing event: %w", err)
			}

			if existing != nil {
				// Update existing event
				existing.ICalData = modelEvent.ICalData
				existing.Summary = modelEvent.Summary
				existing.Description = modelEvent.Description
				existing.Location = modelEvent.Location
				existing.DTStart = modelEvent.DTStart
				existing.DTEnd = modelEvent.DTEnd
				existing.AllDay = modelEvent.AllDay
				existing.RRule = modelEvent.RRule
				existing.ETag = generateETag(modelEvent.ICalData)

				if err := database.UpdateCalendarEvent(existing); err != nil {
					return imported, fmt.Errorf("failed to update event: %w", err)
				}
			} else {
				// Create new event
				modelEvent.ETag = generateETag(modelEvent.ICalData)
				if err := database.CreateCalendarEvent(modelEvent); err != nil {
					return imported, fmt.Errorf("failed to create event: %w", err)
				}
			}

			imported++
		}
	}

	return imported, nil
}

// ImportICSSimple imports events from ICS data using simple parsing
// This is a fallback for when the go-ical decoder doesn't work
func ImportICSSimple(database *db.DB, calendarID int64, icsData []byte) (int, error) {
	content := string(icsData)
	events := extractVEvents(content)

	imported := 0
	for _, vevent := range events {
		event := parseVEvent(vevent, calendarID)
		if event == nil || event.UID == "" {
			continue
		}

		// Check if event already exists
		existing, err := database.GetEventByUID(calendarID, event.UID)
		if err != nil {
			return imported, fmt.Errorf("failed to check existing event: %w", err)
		}

		if existing != nil {
			// Update existing event
			existing.ICalData = event.ICalData
			existing.Summary = event.Summary
			existing.Description = event.Description
			existing.Location = event.Location
			existing.DTStart = event.DTStart
			existing.DTEnd = event.DTEnd
			existing.AllDay = event.AllDay
			existing.RRule = event.RRule
			existing.ETag = generateETag(event.ICalData)

			if err := database.UpdateCalendarEvent(existing); err != nil {
				return imported, fmt.Errorf("failed to update event: %w", err)
			}
		} else {
			// Create new event
			event.ETag = generateETag(event.ICalData)
			if err := database.CreateCalendarEvent(event); err != nil {
				return imported, fmt.Errorf("failed to create event: %w", err)
			}
		}

		imported++
	}

	return imported, nil
}

// parseICalEvent parses a go-ical event into our model
func parseICalEvent(event *ical.Event, calendarID int64, cal *ical.Calendar) (*models.CalendarEvent, error) {
	modelEvent := &models.CalendarEvent{
		CalendarID: calendarID,
	}

	// Get UID
	if prop := event.Props.Get(ical.PropUID); prop != nil {
		modelEvent.UID = prop.Value
	}
	if modelEvent.UID == "" {
		return nil, fmt.Errorf("event has no UID")
	}

	// Get Summary
	if prop := event.Props.Get(ical.PropSummary); prop != nil {
		modelEvent.Summary = prop.Value
	}

	// Get Description
	if prop := event.Props.Get(ical.PropDescription); prop != nil {
		modelEvent.Description = prop.Value
	}

	// Get Location
	if prop := event.Props.Get(ical.PropLocation); prop != nil {
		modelEvent.Location = prop.Value
	}

	// Get RRULE
	if prop := event.Props.Get(ical.PropRecurrenceRule); prop != nil {
		modelEvent.RRule = prop.Value
	}

	// Get DTSTART
	if prop := event.Props.Get(ical.PropDateTimeStart); prop != nil {
		t, err := prop.DateTime(nil)
		if err == nil {
			modelEvent.DTStart = t
		}
		// Check if all-day event
		if prop.Params.Get(ical.ParamValue) == "DATE" {
			modelEvent.AllDay = true
		}
	}

	// Get DTEND
	if prop := event.Props.Get(ical.PropDateTimeEnd); prop != nil {
		t, err := prop.DateTime(nil)
		if err == nil {
			modelEvent.DTEnd.Time = t
			modelEvent.DTEnd.Valid = true
		}
	}

	// Serialize just this single event wrapped in a VCALENDAR
	// Don't serialize the entire calendar with all events!
	singleCal := ical.NewCalendar()
	singleCal.Props.SetText(ical.PropVersion, "2.0")
	singleCal.Props.SetText(ical.PropProductID, "-//DDMailServer//Calendar//EN")
	singleCal.Children = append(singleCal.Children, event.Component)

	var buf strings.Builder
	encoder := ical.NewEncoder(&buf)
	if err := encoder.Encode(singleCal); err == nil {
		modelEvent.ICalData = buf.String()
	} else {
		// Generate minimal iCal data
		modelEvent.ICalData = wrapVEvent(generateVEvent(modelEvent))
	}

	return modelEvent, nil
}

// extractVEvents extracts VEVENT blocks from ICS content
func extractVEvents(content string) []string {
	var events []string
	lines := strings.Split(content, "\n")

	var currentEvent strings.Builder
	inEvent := false

	for _, line := range lines {
		line = strings.TrimRight(line, "\r")

		if strings.HasPrefix(line, "BEGIN:VEVENT") {
			inEvent = true
			currentEvent.Reset()
		}

		if inEvent {
			currentEvent.WriteString(line)
			currentEvent.WriteString("\r\n")
		}

		if strings.HasPrefix(line, "END:VEVENT") {
			inEvent = false
			events = append(events, currentEvent.String())
		}
	}

	return events
}

// parseVEvent parses a VEVENT string into a CalendarEvent
func parseVEvent(vevent string, calendarID int64) *models.CalendarEvent {
	event := &models.CalendarEvent{
		CalendarID: calendarID,
		ICalData:   wrapVEvent(vevent),
	}

	lines := strings.Split(vevent, "\n")
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")

		if strings.HasPrefix(line, "UID:") {
			event.UID = strings.TrimPrefix(line, "UID:")
		} else if strings.HasPrefix(line, "SUMMARY:") {
			event.Summary = strings.TrimPrefix(line, "SUMMARY:")
		} else if strings.HasPrefix(line, "DESCRIPTION:") {
			event.Description = strings.TrimPrefix(line, "DESCRIPTION:")
		} else if strings.HasPrefix(line, "LOCATION:") {
			event.Location = strings.TrimPrefix(line, "LOCATION:")
		} else if strings.HasPrefix(line, "RRULE:") {
			event.RRule = strings.TrimPrefix(line, "RRULE:")
		} else if strings.HasPrefix(line, "DTSTART") {
			value := extractValue(line)
			event.DTStart, event.AllDay = parseDateTimeSimple(value, line)
		} else if strings.HasPrefix(line, "DTEND") {
			value := extractValue(line)
			dtend, _ := parseDateTimeSimple(value, line)
			event.DTEnd.Time = dtend
			event.DTEnd.Valid = true
		}
	}

	return event
}

// extractValue extracts the value from a line like "DTSTART;VALUE=DATE:20210101"
func extractValue(line string) string {
	parts := strings.SplitN(line, ":", 2)
	if len(parts) == 2 {
		return parts[1]
	}
	return ""
}

// parseDateTimeSimple parses iCalendar date/time formats
func parseDateTimeSimple(value, line string) (time.Time, bool) {
	allDay := false

	// Check for VALUE=DATE in the line
	if strings.Contains(line, "VALUE=DATE") && !strings.Contains(line, "VALUE=DATE-TIME") {
		allDay = true
	}

	value = strings.TrimSpace(value)

	// Try various formats
	formats := []string{
		"20060102T150405Z",     // UTC
		"20060102T150405",      // Local time
		"20060102",             // Date only
		"2006-01-02T15:04:05Z", // ISO format
		"2006-01-02",           // ISO date
	}

	for _, format := range formats {
		if t, err := time.Parse(format, value); err == nil {
			if len(value) == 8 { // Date only
				allDay = true
			}
			return t, allDay
		}
	}

	return time.Time{}, allDay
}

// wrapVEvent wraps a VEVENT in a VCALENDAR
func wrapVEvent(vevent string) string {
	return fmt.Sprintf("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//DDMailServer//Calendar//EN\r\n%sEND:VCALENDAR\r\n", vevent)
}

// generateVEvent generates a VEVENT from a CalendarEvent
func generateVEvent(event *models.CalendarEvent) string {
	dtstart := event.DTStart.Format("20060102T150405Z")
	var dtend string
	if event.DTEnd.Valid {
		dtend = event.DTEnd.Time.Format("20060102T150405Z")
	} else {
		dtend = event.DTStart.Add(time.Hour).Format("20060102T150405Z")
	}

	vevent := "BEGIN:VEVENT\r\n"
	vevent += "UID:" + event.UID + "\r\n"
	vevent += "SUMMARY:" + event.Summary + "\r\n"
	if event.Description != "" {
		vevent += "DESCRIPTION:" + event.Description + "\r\n"
	}
	if event.Location != "" {
		vevent += "LOCATION:" + event.Location + "\r\n"
	}
	vevent += "DTSTART:" + dtstart + "\r\n"
	vevent += "DTEND:" + dtend + "\r\n"
	if event.RRule != "" {
		vevent += "RRULE:" + event.RRule + "\r\n"
	}
	vevent += "END:VEVENT\r\n"

	return vevent
}

// generateETag generates an ETag from content
func generateETag(content string) string {
	hash := sha256.Sum256([]byte(content))
	return fmt.Sprintf("\"%x\"", hash[:8])
}
