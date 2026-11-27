package client

import (
	"net"
	"testing"
	"time"
)

// The dial goes out from the configured source address.
func TestDialFromSourceIP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	seen := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		seen <- c.RemoteAddr().(*net.TCPAddr).IP.String()
		_, _ = c.Write([]byte("220 test ESMTP\r\n"))
		time.Sleep(100 * time.Millisecond)
		c.Close()
	}()
	tm := defaultTimeouts()
	tm.local = &net.TCPAddr{IP: net.ParseIP("127.0.0.2")}
	cl, err := dialSMTP(ln.Addr().String(), "test", nil, tm)
	if err != nil {
		t.Skipf("127.0.0.2 not usable as a source here: %v", err)
	}
	defer cl.Close()
	if got := <-seen; got != "127.0.0.2" {
		t.Fatalf("connection came from %s, want 127.0.0.2", got)
	}
}
