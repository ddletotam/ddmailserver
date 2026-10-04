package parser

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
)

// fakeResolver is an in-memory DNS for SPF tests.
type fakeResolver struct {
	mu      sync.Mutex
	txt     map[string][]string
	ip      map[string][]net.IP
	mx      map[string][]*net.MX
	ptr     map[string][]string
	fail    map[string]bool // names whose lookups fail transiently
	queries int
}

func newFakeResolver() *fakeResolver {
	return &fakeResolver{
		txt:  map[string][]string{},
		ip:   map[string][]net.IP{},
		mx:   map[string][]*net.MX{},
		ptr:  map[string][]string{},
		fail: map[string]bool{},
	}
}

func notFound(name string) error {
	return &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

func (f *fakeResolver) hit(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries++
	if f.fail[name] {
		return &net.DNSError{Err: "server misbehaving", Name: name, IsTemporary: true}
	}
	return nil
}

func (f *fakeResolver) LookupTXT(_ context.Context, name string) ([]string, error) {
	if err := f.hit(name); err != nil {
		return nil, err
	}
	if r, ok := f.txt[name]; ok {
		return r, nil
	}
	return nil, notFound(name)
}

func (f *fakeResolver) LookupIP(_ context.Context, network, host string) ([]net.IP, error) {
	if err := f.hit(host); err != nil {
		return nil, err
	}
	var out []net.IP
	for _, a := range f.ip[host] {
		if (a.To4() != nil) == (network == "ip4") {
			out = append(out, a)
		}
	}
	if len(out) == 0 {
		return nil, notFound(host)
	}
	return out, nil
}

func (f *fakeResolver) LookupMX(_ context.Context, name string) ([]*net.MX, error) {
	if err := f.hit(name); err != nil {
		return nil, err
	}
	if r, ok := f.mx[name]; ok {
		return r, nil
	}
	return nil, notFound(name)
}

func (f *fakeResolver) LookupAddr(_ context.Context, addr string) ([]string, error) {
	if err := f.hit(addr); err != nil {
		return nil, err
	}
	if r, ok := f.ptr[addr]; ok {
		return r, nil
	}
	return nil, notFound(addr)
}

func checkSPF(t *testing.T, r *fakeResolver, ip, domain string, want AuthResult) string {
	t.Helper()
	c := NewSPFCheckerWithResolver(r)
	got, detail := c.CheckSPF(ip, domain)
	if got != want {
		t.Fatalf("CheckSPF(%s, %s) = %s (%s), want %s", ip, domain, got, detail, want)
	}
	return detail
}

func TestSPFBasicPassFail(t *testing.T) {
	r := newFakeResolver()
	r.txt["example.com"] = []string{"some-other=txt", "v=spf1 ip4:192.0.2.0/24 ip6:2001:db8::/32 a mx -all"}
	r.ip["example.com"] = []net.IP{net.ParseIP("198.51.100.7")}
	r.mx["example.com"] = []*net.MX{{Host: "mx.example.com.", Pref: 10}}
	r.ip["mx.example.com"] = []net.IP{net.ParseIP("203.0.113.5"), net.ParseIP("2001:db9::25")}

	checkSPF(t, r, "192.0.2.55", "example.com", AuthResultPass)
	checkSPF(t, r, "2001:db8::1", "Example.COM.", AuthResultPass)
	checkSPF(t, r, "198.51.100.7", "example.com", AuthResultPass)
	checkSPF(t, r, "203.0.113.5", "example.com", AuthResultPass)
	checkSPF(t, r, "2001:db9::25", "example.com", AuthResultPass)
	checkSPF(t, r, "10.1.1.1", "example.com", AuthResultFail)
}

func TestSPFQualifiersAndNone(t *testing.T) {
	r := newFakeResolver()
	r.txt["soft.test"] = []string{"v=spf1 ip4:192.0.2.1 ~all"}
	r.txt["neutral.test"] = []string{"v=spf1 ?all"}
	r.txt["empty.test"] = []string{"v=spf1"}
	r.txt["nospf.test"] = []string{"google-site-verification=x"}
	r.txt["two.test"] = []string{"v=spf1 -all", "v=spf1 +all"}
	r.txt["bad.test"] = []string{"v=spf1 frobnicate:x -all"}

	checkSPF(t, r, "10.0.0.1", "soft.test", AuthResultSoftfail)
	checkSPF(t, r, "10.0.0.1", "neutral.test", AuthResultNeutral)
	checkSPF(t, r, "10.0.0.1", "empty.test", AuthResultNeutral)
	checkSPF(t, r, "10.0.0.1", "nospf.test", AuthResultNone)
	checkSPF(t, r, "10.0.0.1", "nxdomain.test", AuthResultNone)
	checkSPF(t, r, "10.0.0.1", "two.test", AuthResultPermError)
	checkSPF(t, r, "10.0.0.1", "bad.test", AuthResultPermError)
}

func TestSPFIncludeSelfLoop(t *testing.T) {
	r := newFakeResolver()
	r.txt["loop.test"] = []string{"v=spf1 include:loop.test -all"}
	detail := checkSPF(t, r, "192.0.2.1", "loop.test", AuthResultPermError)
	if !strings.Contains(detail, "loop") {
		t.Errorf("detail %q does not mention loop", detail)
	}
	if r.queries > 5 {
		t.Errorf("self-include made %d DNS queries", r.queries)
	}
}

func TestSPFMutualIncludeLoop(t *testing.T) {
	r := newFakeResolver()
	r.txt["a.test"] = []string{"v=spf1 include:b.test -all"}
	r.txt["b.test"] = []string{"v=spf1 include:a.test -all"}
	checkSPF(t, r, "192.0.2.1", "a.test", AuthResultPermError)
}

func TestSPFRedirectLoop(t *testing.T) {
	r := newFakeResolver()
	r.txt["r.test"] = []string{"v=spf1 redirect=r.test"}
	checkSPF(t, r, "192.0.2.1", "r.test", AuthResultPermError)
}

func TestSPFTooManyIncludes(t *testing.T) {
	r := newFakeResolver()
	// Chain of 11 includes: d0 -> d1 -> ... -> d11; d11 would pass.
	for i := 0; i < 11; i++ {
		r.txt[fmt.Sprintf("d%d.test", i)] = []string{fmt.Sprintf("v=spf1 include:d%d.test -all", i+1)}
	}
	r.txt["d11.test"] = []string{"v=spf1 +all"}
	detail := checkSPF(t, r, "192.0.2.1", "d0.test", AuthResultPermError)
	if !strings.Contains(detail, "too many DNS lookups") {
		t.Errorf("detail %q does not mention lookup limit", detail)
	}

	// Ten includes are still within the limit.
	r2 := newFakeResolver()
	for i := 0; i < 10; i++ {
		r2.txt[fmt.Sprintf("d%d.test", i)] = []string{fmt.Sprintf("v=spf1 include:d%d.test -all", i+1)}
	}
	r2.txt["d10.test"] = []string{"v=spf1 +all"}
	checkSPF(t, r2, "192.0.2.1", "d0.test", AuthResultPass)
}

func TestSPFTooManyFlatLookups(t *testing.T) {
	r := newFakeResolver()
	var terms []string
	for i := 0; i < 11; i++ {
		terms = append(terms, fmt.Sprintf("a:h%d.test", i))
		r.ip[fmt.Sprintf("h%d.test", i)] = []net.IP{net.ParseIP("198.51.100.1")}
	}
	r.txt["flat.test"] = []string{"v=spf1 " + strings.Join(terms, " ") + " -all"}
	checkSPF(t, r, "192.0.2.1", "flat.test", AuthResultPermError)
}

func TestSPFVoidLookupLimit(t *testing.T) {
	r := newFakeResolver()
	r.txt["void.test"] = []string{"v=spf1 a:n1.test a:n2.test a:n3.test -all"}
	checkSPF(t, r, "192.0.2.1", "void.test", AuthResultPermError)

	r.txt["void2.test"] = []string{"v=spf1 a:n1.test a:n2.test -all"}
	checkSPF(t, r, "192.0.2.1", "void2.test", AuthResultFail)
}

func TestSPFRedirectAppliesOnlyWithoutMatch(t *testing.T) {
	r := newFakeResolver()
	r.txt["target.test"] = []string{"v=spf1 ip4:203.0.113.0/24 -all"}
	// redirect= listed first must still be evaluated last.
	r.txt["redir.test"] = []string{"v=spf1 redirect=target.test ip4:192.0.2.1"}
	checkSPF(t, r, "192.0.2.1", "redir.test", AuthResultPass)
	checkSPF(t, r, "203.0.113.9", "redir.test", AuthResultPass)
	checkSPF(t, r, "10.0.0.1", "redir.test", AuthResultFail)

	// An explicit "all" means redirect is never used.
	r.txt["allfirst.test"] = []string{"v=spf1 ?all redirect=target.test"}
	checkSPF(t, r, "203.0.113.9", "allfirst.test", AuthResultNeutral)

	// Redirect to a domain without SPF is a permerror.
	r.txt["redirnone.test"] = []string{"v=spf1 redirect=nothing.test"}
	checkSPF(t, r, "10.0.0.1", "redirnone.test", AuthResultPermError)

	// Duplicate redirect is a permerror.
	r.txt["dup.test"] = []string{"v=spf1 redirect=target.test redirect=target.test"}
	checkSPF(t, r, "10.0.0.1", "dup.test", AuthResultPermError)
}

func TestSPFIncludeSemantics(t *testing.T) {
	r := newFakeResolver()
	r.txt["inc.test"] = []string{"v=spf1 include:provider.test ~all"}
	r.txt["provider.test"] = []string{"v=spf1 ip4:192.0.2.0/24 -all"}
	checkSPF(t, r, "192.0.2.3", "inc.test", AuthResultPass)
	// provider's -all does not leak: include simply does not match.
	checkSPF(t, r, "10.0.0.1", "inc.test", AuthResultSoftfail)

	// The same domain included twice in sibling branches is not a loop.
	r.txt["twice.test"] = []string{"v=spf1 include:provider.test include:provider.test -all"}
	checkSPF(t, r, "10.0.0.1", "twice.test", AuthResultFail)

	r.txt["incnone.test"] = []string{"v=spf1 include:nothing.test -all"}
	checkSPF(t, r, "10.0.0.1", "incnone.test", AuthResultPermError)
}

func TestSPFTempErrorNotCached(t *testing.T) {
	r := newFakeResolver()
	r.txt["flaky.test"] = []string{"v=spf1 ip4:192.0.2.1 -all"}
	r.fail["flaky.test"] = true
	c := NewSPFCheckerWithResolver(r)
	if got, _ := c.CheckSPF("192.0.2.1", "flaky.test"); got != AuthResultTempError {
		t.Fatalf("got %s, want temperror", got)
	}
	r.fail["flaky.test"] = false
	if got, _ := c.CheckSPF("192.0.2.1", "flaky.test"); got != AuthResultPass {
		t.Fatalf("got %s after recovery, want pass", got)
	}
}

func TestSPFCIDRSuffixes(t *testing.T) {
	r := newFakeResolver()
	r.txt["cidr.test"] = []string{"v=spf1 a:hosts.test/24//64 -all"}
	r.ip["hosts.test"] = []net.IP{net.ParseIP("192.0.2.10"), net.ParseIP("2001:db8:1:2::1")}
	checkSPF(t, r, "192.0.2.200", "cidr.test", AuthResultPass)
	checkSPF(t, r, "2001:db8:1:2::ffff", "cidr.test", AuthResultPass)
	checkSPF(t, r, "192.0.3.1", "cidr.test", AuthResultFail)
}

func TestSPFErrorTypes(t *testing.T) {
	err := permErr("x")
	var se *spfError
	if !errors.As(err, &se) || se.result != AuthResultPermError {
		t.Fatalf("permErr not recognised")
	}
}
