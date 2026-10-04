package db

import (
	"fmt"
	"strings"

	"github.com/lib/pq"
)

// folderSearchByIDsMax is the largest id list a folder search narrows to
// with id = ANY($n); past it the whole folder is scanned and the caller
// ignores rows it did not ask about.
const folderSearchByIDsMax = 5000

// searchableColumn reports whether col may be named in a folder text search.
// Column names are spliced into SQL, so only this fixed set is accepted.
func searchableColumn(col string) bool {
	switch col {
	case "subject", "from_addr", "to_addr", "cc", "bcc", "reply_to",
		"message_id", "in_reply_to", "message_references", "body", "body_html":
		return true
	}
	return false
}

// folderSearchScope returns the WHERE clause (and its arguments, starting at
// $1) that restricts a search to the live messages of a folder — the rows
// GetFolderMessageRefs returns — and, when ids is short enough, to those ids.
func folderSearchScope(folderID int64, ids []int64) (string, []interface{}) {
	where := `folder_id = $1 AND deleted = false AND (soft_deleted = false OR soft_deleted IS NULL)
	          AND (is_spam = false OR is_spam IS NULL)`
	args := []interface{}{folderID}
	if ids != nil && len(ids) <= folderSearchByIDsMax {
		where += ` AND id = ANY($2)`
		args = append(args, pq.Array(ids))
	}
	return where, args
}

// LikeContainsPattern turns s into an ILIKE pattern matching any value that
// contains s: the wildcards % and _ and the escape character itself are
// escaped (ESCAPE '\').
func LikeContainsPattern(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(s) + "%"
}

// SearchFolderTextIDs returns the ids of the folder's live messages (narrowed
// to ids when it is not nil) where any of columns contains term, ignoring
// case as PostgreSQL's ILIKE does. Whether ILIKE folds non-ASCII letters
// depends on the database's LC_CTYPE — see FoldsUnicodeCase. An empty term
// asks for the columns being non-empty (IMAP: HEADER <field> "" means the
// field is present).
func (db *DB) SearchFolderTextIDs(folderID int64, ids []int64, columns []string, term string) ([]int64, error) {
	if len(columns) == 0 {
		return nil, nil
	}
	where, args := folderSearchScope(folderID, ids)
	p := 0
	if term != "" {
		args = append(args, LikeContainsPattern(term))
		p = len(args)
	}
	var conds []string
	for _, col := range columns {
		if !searchableColumn(col) {
			return nil, fmt.Errorf("folder search: column %q is not searchable", col)
		}
		if term == "" {
			conds = append(conds, fmt.Sprintf(`COALESCE(%s, '') <> ''`, col))
		} else {
			conds = append(conds, fmt.Sprintf(`%s ILIKE $%d ESCAPE '\'`, col, p))
		}
	}
	rows, err := db.Query(`SELECT id FROM messages WHERE `+where+` AND (`+strings.Join(conds, " OR ")+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to search folder text: %w", err)
	}
	defer rows.Close()

	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("failed to scan folder search hit: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to read folder search hits: %w", err)
	}
	return out, nil
}

// ScanFolderTextColumns streams the given text columns of the folder's live
// messages (narrowed to ids when it is not nil, and possibly wider — see
// folderSearchByIDsMax) to fn one row at a time, so a search can match them
// in Go without holding the folder's bodies in memory.
func (db *DB) ScanFolderTextColumns(folderID int64, ids []int64, columns []string, fn func(id int64, values []string) error) error {
	for _, col := range columns {
		if !searchableColumn(col) {
			return fmt.Errorf("folder search: column %q is not searchable", col)
		}
	}
	where, args := folderSearchScope(folderID, ids)
	sel := "id"
	for _, col := range columns {
		sel += ", COALESCE(" + col + ", '')"
	}
	rows, err := db.Query(`SELECT `+sel+` FROM messages WHERE `+where, args...)
	if err != nil {
		return fmt.Errorf("failed to scan folder text: %w", err)
	}
	defer rows.Close()

	values := make([]string, len(columns))
	dest := make([]interface{}, len(columns)+1)
	var id int64
	dest[0] = &id
	for i := range values {
		dest[i+1] = &values[i]
	}
	for rows.Next() {
		if err := rows.Scan(dest...); err != nil {
			return fmt.Errorf("failed to scan folder text row: %w", err)
		}
		if err := fn(id, values); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("failed to read folder text: %w", err)
	}
	return nil
}

// rawHeaderPrefix bounds how much of the stored RFC 822 source a header
// search reads: the header ends long before it in any real message.
const rawHeaderPrefix = 128 * 1024

// ScanFolderRawHeads streams the start of the stored RFC 822 source of the
// folder's live messages that have one (narrowed to ids as in
// ScanFolderTextColumns) to fn — enough to read their header.
func (db *DB) ScanFolderRawHeads(folderID int64, ids []int64, fn func(id int64, head []byte) error) error {
	where, args := folderSearchScope(folderID, ids)
	rows, err := db.Query(fmt.Sprintf(`SELECT id, substring(raw_email from 1 for %d) FROM messages WHERE %s AND raw_email IS NOT NULL`,
		rawHeaderPrefix, where), args...)
	if err != nil {
		return fmt.Errorf("failed to scan raw headers: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id int64
		var head []byte
		if err := rows.Scan(&id, &head); err != nil {
			return fmt.Errorf("failed to scan raw header row: %w", err)
		}
		if err := fn(id, head); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("failed to read raw headers: %w", err)
	}
	return nil
}

// FoldsUnicodeCase reports whether the database's case folding (lower(),
// ILIKE) covers non-ASCII letters. Under LC_CTYPE=C it does not, and an
// ILIKE search for Cyrillic text would silently miss differently-cased hits.
func (db *DB) FoldsUnicodeCase() (bool, error) {
	var ok bool
	if err := db.QueryRow(`SELECT lower('ЁЖÀ') = 'ёжà' AND 'Привет' ILIKE '%прив%'`).Scan(&ok); err != nil {
		return false, fmt.Errorf("failed to probe case folding: %w", err)
	}
	return ok, nil
}
