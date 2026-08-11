package importer

import (
	"crypto/sha256"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-ical"
	"github.com/yourusername/mailserver/internal/db"
	"github.com/yourusername/mailserver/internal/models"
	"github.com/yourusername/mailserver/internal/timeutil"
)

// ParseICS parses ICS data and returns events without importing
// Use this when you need transactional control over the import
func ParseICS(icsData string) ([]*models.CalendarEvent, error) {
	decoder := ical.NewDecoder(strings.NewReader(icsData))
	tz := newZoneResolver(icsData)
	var events []*models.CalendarEvent

	for {
		cal, err := decoder.Decode()
		if err != nil {
			break
		}

		for _, event := range cal.Events() {
			modelEvent, err := parseICalEvent(&event, 0, tz, time.UTC)
			if err != nil {
				log.Printf("importer: skipping event: %v", err)
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
			event := parseVEvent(vevent, 0, tz, time.UTC)
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
	tz := newZoneResolver(string(icsData))

	imported := 0

	for {
		cal, err := decoder.Decode()
		if err != nil {
			break
		}

		// Process each VEVENT in the calendar
		for _, event := range cal.Events() {
			modelEvent, err := parseICalEvent(&event, calendarID, tz, time.UTC)
			if err != nil {
				log.Printf("importer: skipping event: %v", err)
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
	tz := newZoneResolver(content)

	imported := 0
	for _, vevent := range events {
		event := parseVEvent(vevent, calendarID, tz, time.UTC)
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

// parseICalEvent parses a go-ical event into our model. Zone-qualified times
// are resolved through tz; times with no zone of their own fall back to
// fallback (see zoneResolver).
func parseICalEvent(event *ical.Event, calendarID int64, tz *zoneResolver, fallback *time.Location) (*models.CalendarEvent, error) {
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

	// TEXT-typed properties: Prop.Value is the raw escaped form (`\,` `\;`
	// `\\` `\n` per RFC 5545). Text() decodes them — otherwise rendered
	// summaries look like "массаж\, Маргарита".
	if prop := event.Props.Get(ical.PropSummary); prop != nil {
		if v, err := prop.Text(); err == nil {
			modelEvent.Summary = v
		} else {
			modelEvent.Summary = prop.Value
		}
	}
	if prop := event.Props.Get(ical.PropDescription); prop != nil {
		if v, err := prop.Text(); err == nil {
			modelEvent.Description = v
		} else {
			modelEvent.Description = prop.Value
		}
	}
	if prop := event.Props.Get(ical.PropLocation); prop != nil {
		if v, err := prop.Text(); err == nil {
			modelEvent.Location = v
		} else {
			modelEvent.Location = prop.Value
		}
	}

	// Get RRULE
	if prop := event.Props.Get(ical.PropRecurrenceRule); prop != nil {
		modelEvent.RRule = prop.Value
	}

	// DTSTART and DTEND go through the zone resolver rather than go-ical's
	// prop.DateTime. DateTime resolves TZID against this host only: it ignores
	// whatever VTIMEZONE the feed shipped, and it returns an error for a zone
	// name the host does not know — which this function used to discard, leaving
	// DTStart at zero and silently parking the event at the Unix epoch.
	//
	// A missing DTSTART is left alone rather than rejected: this function also
	// parses invitations arriving by mail, and a METHOD:CANCEL or METHOD:REPLY
	// identifies its event by UID, with no start of its own to carry. Only a
	// DTSTART that is present and unreadable fails the event.
	if startProp := event.Props.Get(ical.PropDateTimeStart); startProp != nil {
		allDay := isDateValue(startProp.Params.Get(ical.ParamValue), startProp.Value)
		start, err := tz.parseProp(startProp.Value, startProp.Params.Get(ical.ParamTimezoneID), allDay, fallback)
		if err != nil {
			return nil, fmt.Errorf("event %s: DTSTART %q: %w", modelEvent.UID, startProp.Value, err)
		}
		modelEvent.DTStart = timeutil.ToMs(start)
		modelEvent.AllDay = allDay
	}

	if prop := event.Props.Get(ical.PropDateTimeEnd); prop != nil {
		end, err := tz.parseProp(prop.Value, prop.Params.Get(ical.ParamTimezoneID),
			isDateValue(prop.Params.Get(ical.ParamValue), prop.Value), fallback)
		if err != nil {
			return nil, fmt.Errorf("event %s: DTEND %q: %w", modelEvent.UID, prop.Value, err)
		}
		ms := timeutil.ToMs(end)
		modelEvent.DTEnd = &ms
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
		modelEvent.ICalData = withVTimezones(buf.String(), tz, eventTZIDs(event))
	} else {
		// Generate minimal iCal data
		modelEvent.ICalData = wrapVEvent(generateVEvent(modelEvent))
	}

	return modelEvent, nil
}

// eventTZIDs lists every zone the event's properties reference, sorted. The
// order matters: ical_data is hashed into the ETag, and Props is a map, so an
// unsorted walk would reshuffle the stored form on every parse and make every
// event look changed on every sync.
func eventTZIDs(event *ical.Event) []string {
	var ids []string
	seen := make(map[string]bool)

	for _, props := range event.Props {
		for _, prop := range props {
			id := prop.Params.Get(ical.ParamTimezoneID)
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			ids = append(ids, id)
		}
	}

	sort.Strings(ids)
	return ids
}

// isDateValue reports whether a property value is a DATE rather than a
// DATE-TIME — either because VALUE says so, or because it is eight digits long.
func isDateValue(valueParam, value string) bool {
	if strings.EqualFold(valueParam, "DATE") {
		return true
	}
	return len(strings.TrimSpace(value)) == len(icalDate)
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

// parseVEvent parses a VEVENT string into a CalendarEvent. This is the fallback
// for payloads go-ical refuses outright; it shares the zone resolver with the
// decoder path so that the same TZID cannot mean two different things depending
// on which parser happened to run.
func parseVEvent(vevent string, calendarID int64, tz *zoneResolver, fallback *time.Location) *models.CalendarEvent {
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
			t, allDay, err := parseDateTimeSimple(value, line, tz, fallback)
			if err != nil {
				// Without a start there is nothing to show and nothing to
				// remind about, so drop the event rather than store it at the
				// epoch.
				log.Printf("importer: skipping event with unusable DTSTART %q: %v", value, err)
				return nil
			}
			event.DTStart = timeutil.ToMs(t)
			event.AllDay = allDay
		} else if strings.HasPrefix(line, "DTEND") {
			value := extractValue(line)
			dtend, _, err := parseDateTimeSimple(value, line, tz, fallback)
			if err != nil {
				log.Printf("importer: ignoring unusable DTEND %q: %v", value, err)
			} else {
				ms := timeutil.ToMs(dtend)
				event.DTEnd = &ms
			}
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

// parseDateTimeSimple parses one DTSTART/DTEND content line from the text
// fallback path. The zone question is delegated to the resolver, so a bare local
// time here is treated as floating rather than silently assumed to be UTC — the
// assumption that shifted an entire feed by the difference between its zone and
// ours.
func parseDateTimeSimple(value, line string, tz *zoneResolver, fallback *time.Location) (time.Time, bool, error) {
	isDate := isDateValue(icalParam(line, "VALUE"), value)
	t, err := tz.parseProp(value, icalParam(line, "TZID"), isDate, fallback)
	return t, isDate, err
}

// wrapVEvent wraps a VEVENT in a VCALENDAR
func wrapVEvent(vevent string) string {
	return fmt.Sprintf("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//DDMailServer//Calendar//EN\r\n%sEND:VCALENDAR\r\n", vevent)
}

// generateVEvent generates a VEVENT from a CalendarEvent
func generateVEvent(event *models.CalendarEvent) string {
	dtstart := timeutil.FromMs(event.DTStart).Format("20060102T150405Z")
	var dtend string
	if event.DTEnd != nil && *event.DTEnd != 0 {
		dtend = timeutil.FromMs(*event.DTEnd).Format("20060102T150405Z")
	} else {
		dtend = timeutil.FromMs(event.DTStart + 3600*1000).Format("20060102T150405Z")
	}

	vevent := "BEGIN:VEVENT\r\n"
	vevent += "UID:" + event.UID + "\r\n"
	// REQUIRED by RFC 5545, and go-ical refuses to encode a VEVENT without it —
	// which is how this fallback gets used in the first place.
	vevent += "DTSTAMP:" + timeutil.FromMs(timeutil.Now()).Format("20060102T150405Z") + "\r\n"
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
