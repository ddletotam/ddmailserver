package feedtrim

import (
	"bytes"
	"regexp"
	"strings"

	"golang.org/x/net/html"
)

// Line is one visual line of a message: the text between two block
// boundaries. Empty lines are not kept.
type Line struct {
	Text   string   // whitespace collapsed
	Tokens []string // lower-case words, ё → е
	start  *html.Node
	raw    int // index of the raw text line (text bodies)
}

// Body is a parsed message body that can be cut at a line.
type Body struct {
	Lines []Line
	doc   *html.Node // HTML bodies
	raw   []string   // text bodies
}

var blockTags = map[string]bool{
	"p": true, "div": true, "br": true, "tr": true, "td": true, "th": true, "li": true,
	"ul": true, "ol": true, "table": true, "tbody": true, "thead": true, "tfoot": true,
	"blockquote": true, "h1": true, "h2": true, "h3": true, "h4": true, "h5": true,
	"h6": true, "hr": true, "pre": true, "section": true, "article": true,
	"header": true, "footer": true, "center": true, "dl": true, "dt": true, "dd": true,
}

var skipTags = map[string]bool{"style": true, "script": true, "head": true, "title": true, "xml": true}

var wordRe = regexp.MustCompile(`[\p{L}\p{N}_]+`)

func tokens(s string) []string {
	ws := wordRe.FindAllString(strings.ToLower(s), -1)
	for i, w := range ws {
		ws[i] = strings.ReplaceAll(w, "ё", "е")
	}
	return ws
}

func collapse(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, " ", " ")), " ")
}

// ParseHTML splits an HTML body into lines. It returns nil if the HTML has no
// text at all.
func ParseHTML(s string) *Body {
	doc, err := html.Parse(strings.NewReader(s))
	if err != nil {
		return nil
	}
	b := &Body{doc: doc}
	var cur strings.Builder
	var start *html.Node
	flush := func() {
		t := collapse(cur.String())
		if t != "" {
			b.Lines = append(b.Lines, Line{Text: t, Tokens: tokens(t), start: start})
		}
		cur.Reset()
		start = nil
	}
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		switch n.Type {
		case html.TextNode:
			if strings.TrimSpace(strings.ReplaceAll(n.Data, " ", " ")) != "" && start == nil {
				start = n
			}
			cur.WriteString(n.Data)
			return
		case html.ElementNode:
			if skipTags[n.Data] {
				return
			}
			if blockTags[n.Data] {
				flush()
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		if n.Type == html.ElementNode && blockTags[n.Data] {
			flush()
		}
	}
	walk(doc)
	flush()
	if len(b.Lines) == 0 {
		return nil
	}
	return b
}

var quotePrefix = regexp.MustCompile(`^\s*(>\s*)+`)

// ParseText splits a plain-text body into lines; leading ">" quote marks do
// not take part in matching.
func ParseText(s string) *Body {
	b := &Body{raw: strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")}
	for i, l := range b.raw {
		t := collapse(quotePrefix.ReplaceAllString(l, ""))
		if t != "" {
			b.Lines = append(b.Lines, Line{Text: t, Tokens: tokens(t), raw: i})
		}
	}
	if len(b.Lines) == 0 {
		return nil
	}
	return b
}

// CutAt returns the body without line i and everything after it. For HTML the
// document is cut at the node that starts line i, and containers left empty
// behind the cut are removed, so no half of a table or quote remains.
func (b *Body) CutAt(i int) string {
	if b.doc == nil {
		return strings.TrimRight(strings.Join(b.raw[:b.Lines[i].raw], "\n"), " \t\n")
	}
	doc := cloneTree(b.doc, b.Lines[i].start)
	truncateFrom(doc.mark)
	pruneTrailingEmpty(doc.root)
	var buf bytes.Buffer
	if err := html.Render(&buf, doc.root); err != nil {
		return ""
	}
	return buf.String()
}

type cloned struct {
	root, mark *html.Node
}

// cloneTree deep-copies the document so a Body can be cut more than once;
// mark is the copy of the node want.
func cloneTree(n, want *html.Node) cloned {
	var res cloned
	var cp func(n *html.Node) *html.Node
	cp = func(n *html.Node) *html.Node {
		c := &html.Node{Type: n.Type, DataAtom: n.DataAtom, Data: n.Data, Namespace: n.Namespace}
		c.Attr = append([]html.Attribute(nil), n.Attr...)
		if n == want {
			res.mark = c
		}
		for k := n.FirstChild; k != nil; k = k.NextSibling {
			c.AppendChild(cp(k))
		}
		return c
	}
	res.root = cp(n)
	return res
}

// truncateFrom removes n and everything after it in document order.
func truncateFrom(n *html.Node) {
	for cur := n; cur != nil && cur.Parent != nil; cur = cur.Parent {
		for s := cur.NextSibling; s != nil; {
			next := s.NextSibling
			cur.Parent.RemoveChild(s)
			s = next
		}
	}
	if n.Parent != nil {
		n.Parent.RemoveChild(n)
	}
}

var contentTags = map[string]bool{"img": true, "video": true, "audio": true, "iframe": true, "object": true, "embed": true, "svg": true}

func hasContent(n *html.Node) bool {
	switch n.Type {
	case html.TextNode:
		return strings.TrimSpace(strings.ReplaceAll(n.Data, " ", " ")) != ""
	case html.ElementNode:
		if skipTags[n.Data] {
			return false
		}
		if contentTags[n.Data] {
			return true
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if hasContent(c) {
			return true
		}
	}
	return false
}

// pruneTrailingEmpty removes, from the end of the document backwards, nodes
// that carry nothing visible: the shells of containers the cut emptied, line
// breaks and whitespace. It stops at the first node with content.
func pruneTrailingEmpty(n *html.Node) {
	for c := n.LastChild; c != nil; {
		prev := c.PrevSibling
		if c.Type == html.ElementNode && (c.Data == "head" || skipTags[c.Data]) {
			c = prev
			continue
		}
		if !hasContent(c) {
			if c.Type == html.TextNode || c.Type == html.ElementNode {
				n.RemoveChild(c)
			}
			c = prev
			continue
		}
		pruneTrailingEmpty(c)
		return
	}
}
