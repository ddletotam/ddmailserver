package parser

import (
	"bytes"
	"io"
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
