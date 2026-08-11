package db

import "testing"

// TestSetCalendarEnabled_FlipsAndFilters covers the master switch end to end:
// flipping it must both persist and take the calendar out of the enabled-only
// query, which is what every client-facing path is gated on.
func TestSetCalendarEnabled_FlipsAndFilters(t *testing.T) {
	db := requireTestDB(t)

	var calID, userID int64
	err := db.DB.QueryRow(`
		SELECT id, user_id FROM calendars WHERE COALESCE(enabled, true) = true LIMIT 1
	`).Scan(&calID, &userID)
	if err != nil {
		t.Skipf("no enabled calendar in the test DB: %v", err)
	}

	// Whatever happens, put it back: this runs against a live schema.
	t.Cleanup(func() {
		if err := db.SetCalendarEnabled(calID, userID, true); err != nil {
			t.Errorf("failed to re-enable calendar %d: %v", calID, err)
		}
	})

	if err := db.SetCalendarEnabled(calID, userID, false); err != nil {
		t.Fatalf("SetCalendarEnabled(false): %v", err)
	}

	cal, err := db.GetCalendarByID(calID)
	if err != nil {
		t.Fatalf("GetCalendarByID: %v", err)
	}
	if cal.Enabled {
		t.Error("calendar still reports enabled after being switched off")
	}

	// The enabled-only query is the gate the desktop payload, the CalDAV server
	// and the event feed all sit behind.
	enabled, err := db.GetEnabledCalendarsByUserID(userID)
	if err != nil {
		t.Fatalf("GetEnabledCalendarsByUserID: %v", err)
	}
	for _, c := range enabled {
		if c.ID == calID {
			t.Error("a disabled calendar is still returned by GetEnabledCalendarsByUserID")
		}
	}

	if err := db.SetCalendarEnabled(calID, userID, true); err != nil {
		t.Fatalf("SetCalendarEnabled(true): %v", err)
	}
	cal, err = db.GetCalendarByID(calID)
	if err != nil {
		t.Fatalf("GetCalendarByID after re-enable: %v", err)
	}
	if !cal.Enabled {
		t.Error("calendar did not come back on")
	}
}

// TestSetCalendarEnabled_WrongUserIsRejected: the id comes from a URL, so the
// query must be scoped by user rather than trusting it.
func TestSetCalendarEnabled_WrongUserIsRejected(t *testing.T) {
	db := requireTestDB(t)

	var calID, userID int64
	err := db.DB.QueryRow(`SELECT id, user_id FROM calendars LIMIT 1`).Scan(&calID, &userID)
	if err != nil {
		t.Skipf("no calendar in the test DB: %v", err)
	}

	if err := db.SetCalendarEnabled(calID, userID+100000, false); err == nil {
		// Undo just in case the guard is missing, so the test does not leave a
		// calendar switched off.
		db.SetCalendarEnabled(calID, userID, true)
		t.Error("SetCalendarEnabled accepted a calendar that does not belong to the user")
	}
}
