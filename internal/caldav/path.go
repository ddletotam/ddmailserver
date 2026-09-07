package caldav

import "strings"

// ObjectPath builds the path of a calendar object inside a collection.
//
// It exists because the obvious `collection + uid + ".ics"` silently produces
// garbage when the collection path carries no trailing slash: a SOGo home of
// "/SOGo/dav/user@example.org" and a UID of "1a2b" concatenated to
// "/SOGo/dav/user@example.org1a2b.ics", which the server answered with 403 —
// the request never named a collection it could write into, so the failure
// looked like a permission problem rather than a malformed path.
//
// The UID is used verbatim as the resource name. That is deliberate: an
// escaped UID would no longer match the name the remote assigned, and every
// server we talk to (Yandex, iCloud, SOGo) names objects after the raw UID.
func ObjectPath(collection, uid string) string {
	if collection == "" {
		return uid + ".ics"
	}
	return strings.TrimSuffix(collection, "/") + "/" + uid + ".ics"
}
