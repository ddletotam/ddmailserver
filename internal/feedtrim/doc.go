// Package feedtrim makes the conversation-feed version of a message: only what
// the sender wrote this time, without the quoted earlier correspondence (the
// separator, the "who/when/subject" header and the quote) and without the
// signature block (name, title, banner, disclaimer, robot footers).
//
// Nothing is guessed from the shape of the text. Both cuts are proven against
// mail the user already has:
//
//   - history — the tail of the message matches the text of its ancestors
//     (In-Reply-To / References) stored in the same mailbox, and the header
//     above it names an ancestor's sender, time or subject. No ancestor in the
//     mailbox — no cut: the feed would otherwise hide something it never shows.
//   - signature — the tail of the sender's own text repeats in most of the
//     sender's messages (at least three). A first-time sender keeps the
//     signature unless it is set off by the standard "-- " line.
//
// The original message is never touched: the result is a derived copy the feed
// shows, and the client opens the original behind "•••".
package feedtrim

// Version of the algorithm. Raise it whenever a rule changes: rows computed by
// an older version are recomputed in the background.
const Version = 1
