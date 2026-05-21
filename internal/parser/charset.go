package parser

import (
	"bytes"
	"encoding/base64"
	"io"
	"mime"
	"mime/quotedprintable"
	"regexp"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
	"golang.org/x/text/encoding/unicode"
)

// DecodeMIMEHeader unwraps RFC 2047 encoded-words (`=?charset?B?...?=`
// / `=?charset?Q?...?=`) and returns plain UTF-8. Subjects, display
// names in From/To/Cc, and any other header values that come back
// from go-imap's ENVELOPE response are typically still in their
// wire-format encoded form — the IMAP library doesn't decode them.
//
// We first try the stdlib `mime.WordDecoder` (strict, RFC-compliant)
// for the happy path; when that rejects an encoded-word — e.g.
// because some sender double-padded the base64 with `===` instead of
// `==` — we fall back to a lenient regex decoder that fixes up the
// padding before handing the bytes off to the charset converter.
// Unknown charsets fall back to a passthrough so an unrecognised
// label never poisons the whole header.
func DecodeMIMEHeader(s string) string {
	if s == "" || !strings.Contains(s, "=?") {
		return s
	}
	// Try stdlib first — it has a slightly broader tokenizer for
	// pathological inputs. Only accept the result if it actually
	// changed something: when the input is malformed, stdlib often
	// returns the original string unchanged AND nil error, which
	// would otherwise short-circuit our lenient fallback.
	dec := &mime.WordDecoder{
		CharsetReader: func(label string, input io.Reader) (io.Reader, error) {
			return CharsetReader(label, input)
		},
	}
	if out, err := dec.DecodeHeader(s); err == nil && out != s {
		return SanitizeUTF8(out)
	}
	return decodeMIMEHeaderLenient(s)
}

// encodedWordRe matches a single RFC 2047 encoded-word. Non-greedy on
// the encoded-text so a `?=` inside the payload doesn't get matched
// as the terminator.
var encodedWordRe = regexp.MustCompile(`=\?([^?]+)\?([BbQq])\?([^?]*)\?=`)

func decodeMIMEHeaderLenient(s string) string {
	return encodedWordRe.ReplaceAllStringFunc(s, func(match string) string {
		parts := encodedWordRe.FindStringSubmatch(match)
		if len(parts) != 4 {
			return match
		}
		charsetName := parts[1]
		enc := strings.ToUpper(parts[2])
		payload := parts[3]

		var raw []byte
		switch enc {
		case "B":
			// Strip every trailing '=' and re-pad to a length that's a
			// multiple of 4. Some senders emit one too many padding
			// chars; stripping + repadding is harmless when the input
			// is already correct.
			body := strings.TrimRight(payload, "=")
			for len(body)%4 != 0 {
				body += "="
			}
			decoded, err := base64.StdEncoding.DecodeString(body)
			if err != nil {
				return match
			}
			raw = decoded
		case "Q":
			// quoted-printable: '_' represents space (RFC 2047 quirk),
			// and we feed the rest to the standard QP reader.
			body := strings.ReplaceAll(payload, "_", " ")
			decoded, err := io.ReadAll(quotedprintable.NewReader(strings.NewReader(body)))
			if err != nil {
				return match
			}
			raw = decoded
		default:
			return match
		}

		reader, err := CharsetReader(charsetName, bytes.NewReader(raw))
		if err != nil {
			return match
		}
		out, err := io.ReadAll(reader)
		if err != nil {
			return match
		}
		return SanitizeUTF8(string(out))
	})
}

// CharsetReader returns a reader that converts from the given charset to UTF-8
func CharsetReader(charset string, input io.Reader) (io.Reader, error) {
	charset = strings.ToLower(charset)

	var decoder *encoding.Decoder

	switch charset {
	// UTF-8 - no conversion needed
	case "utf-8", "utf8", "":
		return input, nil

	// Russian encodings
	case "windows-1251", "cp1251":
		decoder = charmap.Windows1251.NewDecoder()
	case "koi8-r":
		decoder = charmap.KOI8R.NewDecoder()
	case "koi8-u":
		decoder = charmap.KOI8U.NewDecoder()
	case "iso-8859-5":
		decoder = charmap.ISO8859_5.NewDecoder()

	// Western European
	case "windows-1252", "cp1252":
		decoder = charmap.Windows1252.NewDecoder()
	case "iso-8859-1", "latin1", "latin-1":
		decoder = charmap.ISO8859_1.NewDecoder()
	case "iso-8859-2":
		decoder = charmap.ISO8859_2.NewDecoder()
	case "iso-8859-15":
		decoder = charmap.ISO8859_15.NewDecoder()

	// Japanese
	case "iso-2022-jp":
		decoder = japanese.ISO2022JP.NewDecoder()
	case "shift_jis", "shift-jis", "sjis":
		decoder = japanese.ShiftJIS.NewDecoder()
	case "euc-jp":
		decoder = japanese.EUCJP.NewDecoder()

	// Chinese
	case "gb2312", "gbk", "gb18030":
		decoder = simplifiedchinese.GBK.NewDecoder()
	case "big5":
		decoder = traditionalchinese.Big5.NewDecoder()

	// Korean
	case "euc-kr":
		decoder = korean.EUCKR.NewDecoder()

	// Unicode
	case "utf-16", "utf-16le":
		decoder = unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM).NewDecoder()
	case "utf-16be":
		decoder = unicode.UTF16(unicode.BigEndian, unicode.IgnoreBOM).NewDecoder()

	default:
		// If charset is not recognized, return the input as-is
		return input, nil
	}

	// Read all input and decode
	data, err := io.ReadAll(input)
	if err != nil {
		return nil, err
	}

	decoded, err := decoder.Bytes(data)
	if err != nil {
		// If decoding fails, return original data
		return bytes.NewReader(data), nil
	}

	return bytes.NewReader(decoded), nil
}

// DecodeCharset decodes bytes from the given charset to UTF-8 string
func DecodeCharset(charset string, data []byte) (string, error) {
	reader, err := CharsetReader(charset, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	decoded, err := io.ReadAll(reader)
	if err != nil {
		return "", err
	}
	// Sanitize to ensure valid UTF-8
	return SanitizeUTF8(string(decoded)), nil
}

// SanitizeUTF8 replaces invalid UTF-8 sequences with the replacement character
func SanitizeUTF8(s string) string {
	if s == "" {
		return s
	}

	// Check if already valid UTF-8
	valid := true
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			valid = false
			break
		}
		i += size
	}

	if valid {
		return s
	}

	// Build a new string with invalid bytes replaced
	var result strings.Builder
	result.Grow(len(s))

	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			// Invalid byte - replace with replacement character
			result.WriteRune('\uFFFD')
			i++
		} else {
			result.WriteRune(r)
			i += size
		}
	}

	return result.String()
}
