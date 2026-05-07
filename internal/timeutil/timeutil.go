// Package timeutil provides helpers for the ms-since-epoch time representation
// used throughout DDMailServer. All timestamps in the database are stored as
// BIGINT milliseconds (UTC). Where the sender's timezone matters (email Date,
// calendar events) a separate SMALLINT column holds the UTC offset in minutes.
package timeutil

import "time"

// Now returns the current time as milliseconds since Unix epoch (UTC).
func Now() int64 {
	return time.Now().UnixMilli()
}

// ToMs converts a time.Time to milliseconds since epoch. Returns 0 for zero time.
func ToMs(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// FromMs converts milliseconds since epoch to time.Time (UTC). Returns zero time for 0.
func FromMs(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

// NullToMs converts a nullable time.Time to a nullable int64 (ms).
func NullToMs(t *time.Time) *int64 {
	if t == nil || t.IsZero() {
		return nil
	}
	ms := t.UnixMilli()
	return &ms
}

// MsToNull converts a nullable int64 (ms) to a nullable time.Time.
func MsToNull(ms *int64) *time.Time {
	if ms == nil || *ms == 0 {
		return nil
	}
	t := time.UnixMilli(*ms).UTC()
	return &t
}

// TZOffsetMinutes extracts the UTC offset in minutes from a time.Time.
// For example, MSK (+03:00) returns 180.
func TZOffsetMinutes(t time.Time) int16 {
	_, offset := t.Zone()
	return int16(offset / 60)
}

// FormatWithTZ formats ms + tz offset into an RFC 2822 date string
// using the sender's original timezone.
func FormatWithTZ(ms int64, tzMinutes int16) string {
	t := FromMs(ms)
	if tzMinutes != 0 {
		loc := time.FixedZone("", int(tzMinutes)*60)
		t = t.In(loc)
	}
	return t.Format(time.RFC1123Z)
}

// FormatUTC formats ms as an RFC 2822 date string in UTC.
func FormatUTC(ms int64) string {
	return FromMs(ms).Format(time.RFC1123Z)
}
