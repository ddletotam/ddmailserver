// Package migrations carries the SQL schema migrations inside the server
// binary, so the server applies them itself at startup (see db.RunMigrations)
// instead of an operator running psql by hand.
//
// File names are NNN[x]_name.sql: a three-digit number, an optional
// lower-case letter for files that had to be slotted in between existing
// numbers, then a snake_case name. The version is the part before the first
// underscore ("020", "020a"); versions sort by number, then by letter.
package migrations

import (
	"embed"
	"io/fs"
)

//go:embed *.sql
var files embed.FS

// FS returns the embedded migration files.
func FS() fs.FS {
	return files
}
