package parser

import (
	"fmt"
	"regexp"
	"testing"
)

func TestDecodeMIMEHeaderFromAddress(t *testing.T) {
	in := "=?windows-1251?B?0ODx8fvr6uAg6uLo8uDt9ujpp?= <no-reply@yarrg.yaroslavl.ru>"

	// Direct regex test.
	re := regexp.MustCompile(`=\?([^?]+)\?([BbQq])\?([^?]*)\?=`)
	matches := re.FindStringSubmatch(in)
	fmt.Printf("regex matches: %#v\n", matches)

	final := DecodeMIMEHeader(in)
	fmt.Printf("final: %q\n", final)
	if final == in {
		t.Fatalf("unchanged")
	}
}
