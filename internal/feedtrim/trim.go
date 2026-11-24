package feedtrim

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Ancestor is an earlier message of the same thread that the user has.
type Ancestor struct {
	From    string // "Name <addr>" or bare address
	Subject string
	Date    time.Time
	Body    *Body // full text, with its own quotes — a reply quotes it whole
}

// Result says where the feed version of a body ends. -1 — no such cut.
type Result struct {
	History   int // first line of the quoted correspondence
	Signature int // first line of the signature block
	// Own is the sender's own lines (before the history cut), lower case: the
	// material later messages of the same sender are compared with.
	Own []string
}

// Cut is the first line the feed drops, or -1 if the whole body stays.
func (r Result) Cut() int {
	switch {
	case r.Signature >= 0:
		return r.Signature
	default:
		return r.History
	}
}

const (
	coveredShare   = 0.8 // a line is quoted if this share of its 3-word shingles is in the ancestors
	minCoveredWord = 5   // the quoted tail must carry at least this many words
	ancestorShare  = 0.5 // without a header the tail must cover this share of an ancestor
	headerReach    = 8   // lines above the quote searched for the header
	sigMinSenders  = 3   // a repeated tail is a signature only in this many messages
	delimReach     = 40  // a lone "-- " counts only this close to the end
	ownTailLines   = 40  // own lines kept for comparison with later messages
)

var (
	separatorRe = regexp.MustCompile(`^[\pP\pS\s]{3,}$`) // «-----», «_____», «*****»
	sigDelimRe  = regexp.MustCompile(`^(--|—|__)$`)
	headerWord  = regexp.MustCompile(`(?i)(wrote|писал|пишет|написал|from:|от:|sent:|отправлено|кому:|to:|date:|дата:|subject:|тема:|original message|исходное сообщение|пересылаемое|внимание|external|внешн)`)
	subjPrefix  = regexp.MustCompile(`(?i)^\s*((re|fwd?|ответ|пересл|на)\s*(\[\d+\])?\s*:\s*)+`)
	addrRe      = regexp.MustCompile(`[\w.+-]+@[\w.-]+`)
	nameRe      = regexp.MustCompile(`^\s*"?([^"<]+?)"?\s*<`)
)

// Peer is another message of the same sender: its subject and its own lines
// (Result.Own of its earlier computation).
type Peer struct {
	Subject string
	Own     []string
}

// Trim finds the cuts of a body with the given subject. anc are the thread's
// earlier messages the user has, peers — the sender's other messages.
func Trim(b *Body, subject string, anc []Ancestor, peers []Peer) Result {
	r := Result{History: -1, Signature: -1}
	if b == nil {
		return r
	}
	r.History = historyCut(b, anc)
	end := len(b.Lines)
	if r.History >= 0 {
		end = r.History
	}
	own := make([]string, 0, end)
	for _, l := range b.Lines[:end] {
		own = append(own, strings.ToLower(l.Text))
	}
	r.Signature = signatureCut(b, own, normSubject(subject), peers)
	if len(own) > ownTailLines {
		own = own[len(own)-ownTailLines:]
	}
	r.Own = own
	return r
}

type shingle [3]string

func shingles(ts []string) map[shingle]struct{} {
	m := make(map[shingle]struct{})
	for i := 0; i+2 < len(ts); i++ {
		m[shingle{ts[i], ts[i+1], ts[i+2]}] = struct{}{}
	}
	return m
}

func allTokens(b *Body) []string {
	var ts []string
	for _, l := range b.Lines {
		ts = append(ts, l.Tokens...)
	}
	return ts
}

func historyCut(b *Body, anc []Ancestor) int {
	if len(anc) == 0 {
		return -1
	}
	ancSh := make(map[shingle]struct{})
	ancW := make(map[string]struct{})
	perAnc := make([]map[shingle]struct{}, 0, len(anc))
	for _, a := range anc {
		if a.Body == nil {
			continue
		}
		ts := allTokens(a.Body)
		s := shingles(ts)
		perAnc = append(perAnc, s)
		for k := range s {
			ancSh[k] = struct{}{}
		}
		for _, w := range ts {
			ancW[w] = struct{}{}
		}
	}
	if len(ancSh) == 0 {
		return -1
	}

	// C — quoted, W — short line whose words all occur in the ancestors,
	// U — the sender's own.
	n := len(b.Lines)
	st := make([]byte, n)
	for i, l := range b.Lines {
		switch {
		case len(l.Tokens) >= 3:
			s := shingles(l.Tokens)
			hit := 0
			for k := range s {
				if _, ok := ancSh[k]; ok {
					hit++
				}
			}
			if float64(hit) >= coveredShare*float64(len(s)) {
				st[i] = 'C'
			} else {
				st[i] = 'U'
			}
		case len(l.Tokens) == 0:
			st[i] = 'W'
		default:
			st[i] = 'U'
			all := true
			for _, w := range l.Tokens {
				if _, ok := ancW[w]; !ok {
					all = false
					break
				}
			}
			if all {
				st[i] = 'W'
			}
		}
	}

	// The quoted tail: from the end up while lines are quoted, then forward to
	// its first truly quoted line — a short own line ("Спасибо!") whose words
	// happen to be in the quote stays the sender's.
	i := n
	for i > 0 && st[i-1] != 'U' {
		i--
	}
	for i < n && st[i] != 'C' {
		i++
	}
	if i >= n {
		return -1
	}

	// The header above the quote: lines that name an ancestor (address, name,
	// time in any zone, subject), separators, and the usual header words.
	sig := headerSignals(anc)
	j := i   // walks up over header-ish lines
	top := i // topmost line that is header proper, not just a short line
	found := map[string]bool{}
	sep := false
	for j > 0 && i-j < headerReach {
		l := b.Lines[j-1]
		low := strings.ToLower(l.Text)
		hit := sig.match(l.Text, low, found)
		isSep := separatorRe.MatchString(l.Text)
		word := len([]rune(l.Text)) <= 120 && headerWord.MatchString(low)
		if hit || isSep || word || st[j-1] == 'W' {
			if isSep {
				sep = true
			}
			if hit || isSep || word {
				top = j - 1
			}
			j--
			continue
		}
		break
	}
	j = top // short lines above the header stay the sender's («Спасибо!»)
	headerOK := len(found) >= 2 || (found["addr"] || found["name"]) && sep

	covered := 0
	tail := make(map[shingle]struct{})
	for k := i; k < n; k++ {
		if st[k] == 'C' {
			covered += len(b.Lines[k].Tokens)
		}
		for s := range shingles(b.Lines[k].Tokens) {
			tail[s] = struct{}{}
		}
	}
	if covered < minCoveredWord {
		return -1
	}
	if !headerOK {
		best := 0.0
		for _, a := range perAnc {
			if len(a) == 0 {
				continue
			}
			hit := 0
			for k := range a {
				if _, ok := tail[k]; ok {
					hit++
				}
			}
			if f := float64(hit) / float64(len(a)); f > best {
				best = f
			}
		}
		if best < ancestorShare {
			return -1
		}
	}
	if headerOK {
		if !hasWords(b.Lines[:j]) {
			return -1 // nothing of the sender's own would remain
		}
		return j
	}
	// Without a header only a line that is not in the ancestors proves that
	// the sender wrote something: «Привет!» above a quote that starts with
	// «Привет!» may be the quote itself.
	for k := 0; k < i; k++ {
		if st[k] == 'U' {
			return i
		}
	}
	return -1
}

func hasWords(ls []Line) bool {
	for _, l := range ls {
		if len(l.Tokens) > 0 {
			return true
		}
	}
	return false
}

type signals struct {
	addrs, names, subjects []string
	times                  map[string]struct{}
}

func headerSignals(anc []Ancestor) signals {
	s := signals{times: map[string]struct{}{}}
	for _, a := range anc {
		low := strings.ToLower(a.From)
		if m := addrRe.FindString(low); m != "" {
			s.addrs = append(s.addrs, m)
		}
		if m := nameRe.FindStringSubmatch(a.From); m != nil && len([]rune(m[1])) > 3 {
			s.names = append(s.names, strings.ToLower(strings.TrimSpace(m[1])))
		}
		if sb := normSubject(a.Subject); len([]rune(sb)) > 3 {
			s.subjects = append(s.subjects, sb)
		}
		if !a.Date.IsZero() {
			// The header shows the time in the replier's zone, which we do not know.
			for off := -12; off <= 14; off++ {
				t := a.Date.UTC().Add(time.Duration(off) * time.Hour)
				s.times[fmt.Sprintf("%02d:%02d", t.Hour(), t.Minute())] = struct{}{}
				s.times[fmt.Sprintf("%d:%02d", t.Hour(), t.Minute())] = struct{}{}
			}
		}
	}
	return s
}

var clockRe = regexp.MustCompile(`(?:^|[^\d])(\d{1,2}:\d{2})(?:[^\d]|$)`)

func (s signals) match(text, low string, found map[string]bool) bool {
	hit := false
	for _, a := range s.addrs {
		if strings.Contains(low, a) {
			found["addr"], hit = true, true
			break
		}
	}
	for _, n := range s.names {
		if strings.Contains(low, n) {
			found["name"], hit = true, true
			break
		}
	}
	for _, sb := range s.subjects {
		if strings.Contains(low, sb) {
			found["subj"], hit = true, true
			break
		}
	}
	for _, m := range clockRe.FindAllStringSubmatch(text, -1) {
		if _, ok := s.times[m[1]]; ok {
			found["time"], hit = true, true
			break
		}
	}
	return hit
}

func normSubject(s string) string {
	return strings.TrimSpace(subjPrefix.ReplaceAllString(strings.ToLower(s), ""))
}

func signatureCut(b *Body, own []string, subject string, peers []Peer) int {
	if len(own) == 0 {
		return -1
	}
	// The tail shared by most of the sender's threads, this one included.
	// Threads, not messages: updates of one meeting or one ticket repeat
	// their content, and that content is not a signature.
	threads := map[string]struct{}{subject: {}}
	for _, p := range peers {
		threads[normSubject(p.Subject)] = struct{}{}
	}
	need := (len(threads) + 1) / 2
	if need < sigMinSenders {
		need = sigMinSenders
	}
	best := 0
	if len(threads) >= sigMinSenders {
		for k := 1; k <= len(own); k++ {
			suf := own[len(own)-k:]
			sup := map[string]struct{}{subject: {}}
			for _, p := range peers {
				if hasSuffix(p.Own, suf) {
					sup[normSubject(p.Subject)] = struct{}{}
				}
			}
			if len(sup) < need {
				break
			}
			best = k
		}
	}
	start := -1
	if best > 0 {
		from := len(own) - best
		for k := from; k < len(own); k++ {
			if sigDelimRe.MatchString(strings.TrimSpace(own[k])) {
				start = k
				break
			}
		}
		if start < 0 {
			words := 0
			for k := from; k < len(own); k++ {
				words += len(b.Lines[k].Tokens)
			}
			if words >= 3 {
				start = from
			}
		}
	}
	if start < 0 {
		for k := len(own) - 1; k >= 0 && len(own)-k <= delimReach; k-- {
			if sigDelimRe.MatchString(strings.TrimSpace(own[k])) {
				start = k
				break
			}
		}
	}
	if start < 0 || !hasWords(b.Lines[:start]) {
		return -1
	}
	return start
}

func hasSuffix(s, suf []string) bool {
	if len(s) < len(suf) {
		return false
	}
	off := len(s) - len(suf)
	for i := range suf {
		if s[off+i] != suf[i] {
			return false
		}
	}
	return true
}
