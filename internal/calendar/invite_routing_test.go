package calendar

import (
	"errors"
	"testing"

	"github.com/yourusername/mailserver/internal/models"
)

func acct(id int64) *int64 { return &id }

// realWorldSources mirrors the configuration that misrouted a work invite:
// five CalDAV sources sharing one defaulted identity_email, exactly one of
// them bound to an account, and the newest calendar belonging to an unrelated
// account.
func realWorldSources() ([]*models.Calendar, []*models.CalendarSource) {
	sources := []*models.CalendarSource{
		{ID: 5, UserID: 22, Name: "Yandex", SourceType: "caldav", CalDAVUsername: "deniskr@yandex.ru", IdentityEmail: "info@help-it-audit.ru"},
		{ID: 6, UserID: 22, Name: "AppSec", SourceType: "caldav", CalDAVUsername: "ddanilin@appsec.global", IdentityEmail: "info@help-it-audit.ru"},
		{ID: 9, UserID: 22, Name: "Apple", SourceType: "caldav", CalDAVUsername: "dd@letotam.ru", IdentityEmail: "info@help-it-audit.ru"},
		{ID: 26, UserID: 22, Name: "SmallKZZZ", SourceType: "caldav", CalDAVUsername: "d.danilin@small.kz", IdentityEmail: "d.danilin@small.kz", AccountID: acct(16)},
		{ID: 27, UserID: 22, Name: "SKZ", SourceType: "ics_url", IdentityEmail: "info@help-it-audit.ru"},
	}
	// created_at descending, the order the database hands them over: the
	// newest calendar first. Calendar 52 is the placeholder row.
	cals := []*models.Calendar{
		{ID: 52, SourceID: 26, Name: "SmallKZZZ", CanWrite: true, Enabled: true, CreatedAt: 900},
		{ID: 51, SourceID: 27, Name: "SKZ", CanWrite: true, Enabled: true, CreatedAt: 800},
		{ID: 50, SourceID: 26, Name: "Персональный календарь", CanWrite: true, Enabled: true, CreatedAt: 700},
		{ID: 17, SourceID: 9, Name: "iOS", CanWrite: true, Enabled: true, CreatedAt: 200},
		{ID: 7, SourceID: 6, Name: "AppSec", CanWrite: true, Enabled: true, CreatedAt: 100},
	}
	return cals, sources
}

// The regression: an invite delivered to ddanilin@appsec.global used to land
// in calendar 52 ("SmallKZZZ") because it was the most recently created one.
func TestChooseInviteCalendarUsesInvitedIdentityNotRecency(t *testing.T) {
	cals, sources := realWorldSources()

	// account_id is unbound for source 6 (as it was on disk), so only the
	// address from the ICS can decide.
	invited := map[string]struct{}{"ddanilin@appsec.global": {}}

	cal, err := chooseInviteCalendar(cals, sources, 8, invited)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cal.ID != 7 {
		t.Errorf("invite for ddanilin@appsec.global routed to calendar %d (%q), want 7 (AppSec)", cal.ID, cal.Name)
	}
}

func TestChooseInviteCalendarPrefersBoundAccount(t *testing.T) {
	cals, sources := realWorldSources()
	// Same source list, but AppSec is now bound to the delivering account —
	// which is what migration 049 backfills.
	for _, s := range sources {
		if s.ID == 6 {
			s.AccountID = acct(8)
		}
	}

	cal, err := chooseInviteCalendar(cals, sources, 8, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cal.ID != 7 {
		t.Errorf("bound account routed to calendar %d, want 7", cal.ID)
	}
}

// Within one source the oldest collection wins, not the newest: the
// destination must not move when the user subscribes to something new.
func TestChooseInviteCalendarPicksOldestCollectionOfSource(t *testing.T) {
	cals, sources := realWorldSources()

	cal, err := chooseInviteCalendar(cals, sources, 16, map[string]struct{}{"d.danilin@small.kz": {}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cal.ID != 50 {
		t.Errorf("routed to calendar %d, want 50 (the real collection, not the newer placeholder)", cal.ID)
	}
}

// A read-only calendar is never a destination — that is where the placeholder
// row ends up after migration 049 and DemoteDirectURLCalendar.
func TestChooseInviteCalendarSkipsReadOnly(t *testing.T) {
	cals, sources := realWorldSources()
	for _, c := range cals {
		if c.ID == 50 {
			c.CanWrite = false // suppose the real collection is read-only
		}
		if c.ID == 52 {
			c.CanWrite = false // demoted placeholder
		}
	}

	_, err := chooseInviteCalendar(cals, sources, 16, map[string]struct{}{"d.danilin@small.kz": {}})
	if !errors.Is(err, ErrNoInviteCalendar) {
		t.Errorf("err = %v, want ErrNoInviteCalendar — no writable calendar for that account", err)
	}
}

// The whole point of the change: when nothing matches, refuse instead of
// filing the invite into an arbitrary calendar.
func TestChooseInviteCalendarRefusesToGuess(t *testing.T) {
	cals, sources := realWorldSources()

	// Delivered to a local mailbox: no source authenticates as it, no local
	// calendar exists, and five sources share identity_email — so the
	// identity pass is ambiguous too.
	_, err := chooseInviteCalendar(cals, sources, 0, map[string]struct{}{"info@help-it-audit.ru": {}})
	if !errors.Is(err, ErrNoInviteCalendar) {
		t.Errorf("err = %v, want ErrNoInviteCalendar", err)
	}
}

func TestChooseInviteCalendarUsesLocalCalendar(t *testing.T) {
	cals, sources := realWorldSources()
	sources = append(sources, &models.CalendarSource{
		ID: 30, UserID: 22, Name: "Local", SourceType: "local", IdentityEmail: "info@help-it-audit.ru",
	})
	cals = append([]*models.Calendar{
		{ID: 60, SourceID: 30, Name: "Мой календарь", CanWrite: true, Enabled: true, CreatedAt: 1000},
	}, cals...)

	cal, err := chooseInviteCalendar(cals, sources, 0, map[string]struct{}{"info@help-it-audit.ru": {}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cal.ID != 60 {
		t.Errorf("routed to calendar %d, want 60 (local)", cal.ID)
	}
}

// A single source claiming the identity is enough to decide.
func TestChooseInviteCalendarUniqueIdentityDecides(t *testing.T) {
	sources := []*models.CalendarSource{
		{ID: 5, UserID: 22, Name: "Yandex", SourceType: "caldav", CalDAVUsername: "other@yandex.ru", IdentityEmail: "me@example.org"},
		{ID: 6, UserID: 22, Name: "Work", SourceType: "caldav", CalDAVUsername: "login-not-an-email", IdentityEmail: "work@example.org"},
	}
	cals := []*models.Calendar{
		{ID: 2, SourceID: 6, Name: "Work", CanWrite: true, Enabled: true},
		{ID: 1, SourceID: 5, Name: "Yandex", CanWrite: true, Enabled: true},
	}

	cal, err := chooseInviteCalendar(cals, sources, 0, map[string]struct{}{"work@example.org": {}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cal.ID != 2 {
		t.Errorf("routed to calendar %d, want 2", cal.ID)
	}
}

func TestChooseInviteCalendarNoCalendarsAtAll(t *testing.T) {
	_, err := chooseInviteCalendar(nil, nil, 8, map[string]struct{}{"a@b.c": {}})
	if !errors.Is(err, ErrNoInviteCalendar) {
		t.Errorf("err = %v, want ErrNoInviteCalendar", err)
	}
}

func TestInvitedIdentities(t *testing.T) {
	attendees := []models.CalendarAttendee{
		{Email: "szotov@appsec.global"},
		{Email: "DDanilin@AppSec.Global"}, // clients send whatever case they like
		{Email: "iodintsov@appsec.global"},
	}
	idSet := map[string]struct{}{
		"ddanilin@appsec.global": {},
		"dd@letotam.ru":          {},
	}

	got := invitedIdentities(attendees, idSet)
	if len(got) != 1 {
		t.Fatalf("got %d invited identities, want 1: %v", len(got), got)
	}
	if _, ok := got["ddanilin@appsec.global"]; !ok {
		t.Errorf("invited identities = %v, want ddanilin@appsec.global", got)
	}
}

func TestInvitedIdentitiesEmptyWhenNotAddressed(t *testing.T) {
	attendees := []models.CalendarAttendee{{Email: "someone@else.org"}}
	idSet := map[string]struct{}{"me@example.org": {}}

	if got := invitedIdentities(attendees, idSet); len(got) != 0 {
		t.Errorf("invited identities = %v, want empty", got)
	}
}
