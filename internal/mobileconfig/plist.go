package mobileconfig

import (
	"bytes"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Value is a decoded plist node. Only the subset Apple configuration profiles
// actually use is modelled: dictionaries, arrays, strings, numbers, booleans
// and data. Dates are decoded as strings — no payload key we read is a date,
// and inventing a time type for a field nobody consults would be dead weight.
type Value struct {
	Kind   Kind
	Str    string
	Num    float64
	Flag   bool // <true/> or <false/>
	Data   []byte
	Array  []*Value
	Dict   map[string]*Value
	keys   []string // insertion order, for stable iteration
}

// Kind enumerates the plist node types.
type Kind int

const (
	KindString Kind = iota
	KindInteger
	KindReal
	KindBool
	KindData
	KindArray
	KindDict
)

// ErrSignedProfile is returned for a profile wrapped in CMS/PKCS#7 rather than
// served as XML. Such a file is a valid .mobileconfig and a perfectly normal
// thing to be handed; it simply cannot be read without unwrapping the
// signature first, and saying so beats an XML syntax error.
var ErrSignedProfile = fmt.Errorf("profile is signed (CMS/PKCS#7); export or unwrap it as plain XML first")

// ParsePlist decodes an XML property list.
//
// Written against encoding/xml rather than pulling in a plist library: the
// subset a configuration profile uses is a handful of element names, and the
// dependency would be carried for the whole project to read fifteen keys.
func ParsePlist(data []byte) (*Value, error) {
	if looksBinary(data) {
		return nil, ErrSignedProfile
	}

	decoder := xml.NewDecoder(bytes.NewReader(data))
	// Profiles in the wild reference the Apple DTD by URL. Resolving it would
	// mean a network fetch during an upload; Strict=false lets the reference
	// pass unresolved.
	decoder.Strict = false

	for {
		tok, err := decoder.Token()
		if err == io.EOF {
			return nil, fmt.Errorf("no plist element found")
		}
		if err != nil {
			return nil, fmt.Errorf("malformed XML: %w", err)
		}

		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}

		switch start.Name.Local {
		case "plist":
			return parsePlistBody(decoder)
		case "dict", "array":
			// Some generators emit a bare root without the <plist> wrapper.
			return parseValue(decoder, start)
		}
	}
}

// looksBinary reports whether the payload is a binary plist or a DER-wrapped
// signed profile rather than XML.
func looksBinary(data []byte) bool {
	trimmed := bytes.TrimLeft(data, " \t\r\n")
	if len(trimmed) == 0 {
		return false
	}
	// Binary plists start with "bplist"; DER SEQUENCE (a signed profile)
	// starts with 0x30.
	return bytes.HasPrefix(trimmed, []byte("bplist")) || trimmed[0] == 0x30
}

// parsePlistBody reads the single value inside <plist>…</plist>.
func parsePlistBody(decoder *xml.Decoder) (*Value, error) {
	for {
		tok, err := decoder.Token()
		if err == io.EOF {
			return nil, fmt.Errorf("plist element is empty")
		}
		if err != nil {
			return nil, fmt.Errorf("malformed XML: %w", err)
		}

		switch t := tok.(type) {
		case xml.StartElement:
			return parseValue(decoder, t)
		case xml.EndElement:
			if t.Name.Local == "plist" {
				return nil, fmt.Errorf("plist element is empty")
			}
		}
	}
}

// parseValue decodes the element that has just been opened.
func parseValue(decoder *xml.Decoder, start xml.StartElement) (*Value, error) {
	switch start.Name.Local {
	case "dict":
		return parseDict(decoder)
	case "array":
		return parseArray(decoder)
	case "string":
		s, err := readText(decoder, start)
		return &Value{Kind: KindString, Str: s}, err
	case "integer":
		s, err := readText(decoder, start)
		if err != nil {
			return nil, err
		}
		n, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil {
			return nil, fmt.Errorf("bad integer %q: %w", s, err)
		}
		return &Value{Kind: KindInteger, Num: n, Str: s}, nil
	case "real":
		s, err := readText(decoder, start)
		if err != nil {
			return nil, err
		}
		n, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil {
			return nil, fmt.Errorf("bad real %q: %w", s, err)
		}
		return &Value{Kind: KindReal, Num: n, Str: s}, nil
	case "true":
		return &Value{Kind: KindBool, Flag: true}, skipElement(decoder, start)
	case "false":
		return &Value{Kind: KindBool, Flag: false}, skipElement(decoder, start)
	case "data":
		s, err := readText(decoder, start)
		if err != nil {
			return nil, err
		}
		// Base64 in a plist is conventionally wrapped across lines.
		clean := strings.Map(func(r rune) rune {
			if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
				return -1
			}
			return r
		}, s)
		raw, err := base64.StdEncoding.DecodeString(clean)
		if err != nil {
			return nil, fmt.Errorf("bad base64 data: %w", err)
		}
		return &Value{Kind: KindData, Data: raw}, nil
	case "date":
		// Kept as text; see the note on Value.
		s, err := readText(decoder, start)
		return &Value{Kind: KindString, Str: s}, err
	default:
		// An element we do not model. Skip it whole rather than fail: profiles
		// carry vendor extensions and refusing the file over one is needless.
		return nil, skipElement(decoder, start)
	}
}

func parseDict(decoder *xml.Decoder) (*Value, error) {
	out := &Value{Kind: KindDict, Dict: make(map[string]*Value)}

	var pendingKey string
	haveKey := false

	for {
		tok, err := decoder.Token()
		if err == io.EOF {
			return nil, fmt.Errorf("unterminated dict")
		}
		if err != nil {
			return nil, fmt.Errorf("malformed XML: %w", err)
		}

		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "key" {
				s, err := readText(decoder, t)
				if err != nil {
					return nil, err
				}
				pendingKey = strings.TrimSpace(s)
				haveKey = true
				continue
			}

			value, err := parseValue(decoder, t)
			if err != nil {
				return nil, err
			}
			// A value with no preceding <key> is malformed; drop it rather
			// than key the dict on "".
			if !haveKey || value == nil {
				haveKey = false
				continue
			}
			if _, exists := out.Dict[pendingKey]; !exists {
				out.keys = append(out.keys, pendingKey)
			}
			out.Dict[pendingKey] = value
			haveKey = false

		case xml.EndElement:
			if t.Name.Local == "dict" {
				return out, nil
			}
		}
	}
}

func parseArray(decoder *xml.Decoder) (*Value, error) {
	out := &Value{Kind: KindArray}

	for {
		tok, err := decoder.Token()
		if err == io.EOF {
			return nil, fmt.Errorf("unterminated array")
		}
		if err != nil {
			return nil, fmt.Errorf("malformed XML: %w", err)
		}

		switch t := tok.(type) {
		case xml.StartElement:
			value, err := parseValue(decoder, t)
			if err != nil {
				return nil, err
			}
			if value != nil {
				out.Array = append(out.Array, value)
			}
		case xml.EndElement:
			if t.Name.Local == "array" {
				return out, nil
			}
		}
	}
}

// readText consumes an element and returns its character data.
func readText(decoder *xml.Decoder, start xml.StartElement) (string, error) {
	var sb strings.Builder
	depth := 1

	for depth > 0 {
		tok, err := decoder.Token()
		if err != nil {
			return "", fmt.Errorf("unterminated <%s>: %w", start.Name.Local, err)
		}
		switch t := tok.(type) {
		case xml.CharData:
			sb.Write(t)
		case xml.StartElement:
			depth++
		case xml.EndElement:
			depth--
		}
	}

	return sb.String(), nil
}

// skipElement discards an element and everything inside it.
func skipElement(decoder *xml.Decoder, start xml.StartElement) error {
	depth := 1
	for depth > 0 {
		tok, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("unterminated <%s>: %w", start.Name.Local, err)
		}
		switch tok.(type) {
		case xml.StartElement:
			depth++
		case xml.EndElement:
			depth--
		}
	}
	return nil
}

// --- accessors -------------------------------------------------------------

// String returns the value's text, and whether the key held one.
func (v *Value) String(key string) (string, bool) {
	child := v.child(key)
	if child == nil {
		return "", false
	}
	switch child.Kind {
	case KindString:
		return child.Str, true
	case KindInteger, KindReal:
		return child.Str, true
	default:
		return "", false
	}
}

// StringOr returns the value's text, or fallback when absent.
func (v *Value) StringOr(key, fallback string) string {
	if s, ok := v.String(key); ok && s != "" {
		return s
	}
	return fallback
}

// Int returns a numeric value as an int.
//
// Accepts <integer>, <real> and a numeric <string> alike. This is not
// permissiveness for its own sake: real profiles put the same port number in
// different types — one file seen in the wild carries CalDAVPort as
// <real>443</real> and CardDAVPort as <integer>443</integer>, in the same
// document. A reader that insists on <integer> rejects half the profiles it
// meets.
func (v *Value) Int(key string) (int, bool) {
	child := v.child(key)
	if child == nil {
		return 0, false
	}
	switch child.Kind {
	case KindInteger, KindReal:
		return int(child.Num), true
	case KindString:
		n, err := strconv.ParseFloat(strings.TrimSpace(child.Str), 64)
		if err != nil {
			return 0, false
		}
		return int(n), true
	default:
		return 0, false
	}
}

// IntOr returns a numeric value, or fallback when absent or unparseable.
func (v *Value) IntOr(key string, fallback int) int {
	if n, ok := v.Int(key); ok {
		return n
	}
	return fallback
}

// Bool returns a boolean value.
//
// <true/> and <false/> are the canonical spellings, but profiles also carry
// booleans as <integer>1</integer> and as the strings "true"/"YES".
func (v *Value) Bool(key string) (bool, bool) {
	child := v.child(key)
	if child == nil {
		return false, false
	}
	switch child.Kind {
	case KindBool:
		return child.Flag, true
	case KindInteger, KindReal:
		return child.Num != 0, true
	case KindString:
		switch strings.ToLower(strings.TrimSpace(child.Str)) {
		case "true", "yes", "1":
			return true, true
		case "false", "no", "0":
			return false, true
		}
		return false, false
	default:
		return false, false
	}
}

// BoolOr returns a boolean value, or fallback when absent.
func (v *Value) BoolOr(key string, fallback bool) bool {
	if b, ok := v.Bool(key); ok {
		return b
	}
	return fallback
}

// Children returns an array's elements, or nil for any other kind.
func (v *Value) Children() []*Value {
	if v == nil || v.Kind != KindArray {
		return nil
	}
	return v.Array
}

// Get returns a nested value by key.
func (v *Value) Get(key string) *Value {
	return v.child(key)
}

// Keys returns a dictionary's keys in document order.
func (v *Value) Keys() []string {
	if v == nil || v.Kind != KindDict {
		return nil
	}
	return v.keys
}

func (v *Value) child(key string) *Value {
	if v == nil || v.Kind != KindDict {
		return nil
	}
	return v.Dict[key]
}
