package parser

import (
	"bytes"
	"strings"

	"github.com/emersion/go-msgauth/dkim"
)

// DKIMChecker performs DKIM signature verification
type DKIMChecker struct{}

// NewDKIMChecker creates a new DKIM checker
func NewDKIMChecker() *DKIMChecker {
	return &DKIMChecker{}
}

// DKIMResult contains the result of a DKIM verification
type DKIMResult struct {
	Result AuthResult
	Domain string
	Reason string
}

// CheckDKIM verifies DKIM signatures in a raw email message
// Returns AuthResult and details about the verification
func (c *DKIMChecker) CheckDKIM(rawMessage []byte) (AuthResult, string) {
	verifications, err := dkim.Verify(bytes.NewReader(rawMessage))
	if err != nil {
		return AuthResultNone, "DKIM verification error: " + err.Error()
	}

	if len(verifications) == 0 {
		return AuthResultNone, "no DKIM signature found"
	}

	// Check all signatures
	var passCount, failCount int
	var domains []string
	var failReasons []string

	for _, v := range verifications {
		domain := v.Domain
		domains = append(domains, domain)

		if v.Err == nil {
			passCount++
		} else {
			failCount++
			failReasons = append(failReasons, domain+": "+v.Err.Error())
		}
	}

	// Determine overall result
	if passCount > 0 && failCount == 0 {
		return AuthResultPass, "verified: " + strings.Join(domains, ", ")
	}

	if failCount > 0 && passCount == 0 {
		return AuthResultFail, "failed: " + strings.Join(failReasons, "; ")
	}

	if passCount > 0 && failCount > 0 {
		// Mixed results - some passed, some failed
		return AuthResultPass, "partial: " + strings.Join(domains, ", ")
	}

	return AuthResultNone, "no valid signatures"
}

// CheckDKIMDetailed returns detailed results for each signature
func (c *DKIMChecker) CheckDKIMDetailed(rawMessage []byte) []DKIMResult {
	verifications, err := dkim.Verify(bytes.NewReader(rawMessage))
	if err != nil {
		return []DKIMResult{{
			Result: AuthResultNone,
			Reason: "verification error: " + err.Error(),
		}}
	}

	if len(verifications) == 0 {
		return []DKIMResult{{
			Result: AuthResultNone,
			Reason: "no DKIM signature",
		}}
	}

	var results []DKIMResult
	for _, v := range verifications {
		result := DKIMResult{
			Domain: v.Domain,
		}

		if v.Err == nil {
			result.Result = AuthResultPass
			result.Reason = "signature valid"
		} else {
			result.Result = AuthResultFail
			result.Reason = v.Err.Error()
		}

		results = append(results, result)
	}

	return results
}

// HasDKIMSignature checks if a message has a DKIM-Signature header
func HasDKIMSignature(rawHeaders map[string][]string) bool {
	_, ok := rawHeaders["Dkim-Signature"]
	if ok {
		return true
	}
	// Try lowercase
	_, ok = rawHeaders["dkim-signature"]
	return ok
}
