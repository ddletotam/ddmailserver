package caldav

import (
	"fmt"
	"strings"
)

// InjectDefaultAlarm adds a VALARM to iCal data if it doesn't already contain one.
// The alarm is inserted before the first END:VEVENT.
func InjectDefaultAlarm(icalData string, before int, unit string) string {
	if strings.Contains(icalData, "VALARM") {
		return icalData // already has alarm
	}
	trigger := FormatAlarmTrigger(before, unit)
	alarm := fmt.Sprintf("BEGIN:VALARM\r\nACTION:DISPLAY\r\nDESCRIPTION:Reminder\r\nTRIGGER:%s\r\nEND:VALARM\r\n", trigger)
	return strings.Replace(icalData, "END:VEVENT", alarm+"END:VEVENT", 1)
}

// FormatAlarmTrigger formats the TRIGGER value for a VALARM component.
// Examples: 15 minutes -> -PT15M, 2 hours -> -PT2H, 1 day -> -P1D
func FormatAlarmTrigger(before int, unit string) string {
	switch unit {
	case "hours":
		return fmt.Sprintf("-PT%dH", before)
	case "days":
		return fmt.Sprintf("-P%dD", before)
	default: // minutes
		return fmt.Sprintf("-PT%dM", before)
	}
}
