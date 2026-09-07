package caldav

import "testing"

func TestObjectPath(t *testing.T) {
	tests := []struct {
		name       string
		collection string
		uid        string
		want       string
	}{
		{
			// The SOGo home that produced the 403: no trailing slash.
			name:       "collection without trailing slash",
			collection: "https://mail.example.org/SOGo/dav/user@example.org",
			uid:        "141zhiglw734ojlzps2f1n64yyandex.ru",
			want:       "https://mail.example.org/SOGo/dav/user@example.org/141zhiglw734ojlzps2f1n64yyandex.ru.ics",
		},
		{
			name:       "collection with trailing slash",
			collection: "/1287640055/calendars/6E649B61/",
			uid:        "C374DBD3-1343-4685-B5C3-94697E56519A",
			want:       "/1287640055/calendars/6E649B61/C374DBD3-1343-4685-B5C3-94697E56519A.ics",
		},
		{
			// An empty collection means "the client base URL already is the
			// collection" — the object name stands alone.
			name:       "empty collection",
			collection: "",
			uid:        "abc",
			want:       "abc.ics",
		},
		{
			name:       "uid keeps its at-sign",
			collection: "/calendars/user/events/",
			uid:        "58f539e06a7651389497e53ac5d9bf95@ddmailserver",
			want:       "/calendars/user/events/58f539e06a7651389497e53ac5d9bf95@ddmailserver.ics",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ObjectPath(tt.collection, tt.uid); got != tt.want {
				t.Errorf("ObjectPath(%q, %q) = %q, want %q", tt.collection, tt.uid, got, tt.want)
			}
		})
	}
}
