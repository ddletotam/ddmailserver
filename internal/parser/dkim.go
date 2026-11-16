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
	result, detail, _ := c.CheckDKIMDomains(rawMessage)
	return result, detail
}

// CheckDKIMDomains is CheckDKIM plus the d= domains of the signatures that
// verified — DMARC alignment needs them. One verification pass.
func (c *DKIMChecker) CheckDKIMDomains(rawMessage []byte) (AuthResult, string, []string) {
	verifications, err := dkim.Verify(bytes.NewReader(rawMessage))
	if err != nil {
		return AuthResultNone, "DKIM verification error: " + err.Error(), nil
	}

	if len(verifications) == 0 {
		return AuthResultNone, "no DKIM signature found", nil
	}

	var domains, passed, failReasons []string
	for _, v := range verifications {
		domains = append(domains, v.Domain)
		if v.Err == nil {
			passed = append(passed, v.Domain)
		} else {
			failReasons = append(failReasons, v.Domain+": "+v.Err.Error())
		}
	}

	switch {
	case len(passed) > 0 && len(failReasons) == 0:
		return AuthResultPass, "verified: " + strings.Join(domains, ", "), passed
	case len(failReasons) > 0 && len(passed) == 0:
		return AuthResultFail, "failed: " + strings.Join(failReasons, "; "), passed
	case len(passed) > 0:
		// Mixed results - some passed, some failed
		return AuthResultPass, "partial: " + strings.Join(domains, ", "), passed
	}
	return AuthResultNone, "no valid signatures", passed
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
