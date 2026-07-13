package main

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// helper: run parseHost against a real TCP conn fed by writeFn, returning the
// parsed host, the bytes parseHost consumed, and any error.
func parseHostOverTCP(t *testing.T, writeFn func(c net.Conn)) (string, []byte, error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			return
		}
		defer c.Close()
		writeFn(c)
		time.Sleep(200 * time.Millisecond) // keep conn open past parseHost
	}()

	srv, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	srv.SetReadDeadline(time.Now().Add(3 * time.Second))
	host, buf, err := parseHost(srv)
	return host, buf, err
}

// A Keycloak/OIDC callback has a very long request-line; over TCP the line and
// the Host header can arrive in separate segments. parseHost must still find
// the host (regression: the old single-Read version returned "no host
// detected", causing a 400+close that an ingress surfaced as 502).
func TestParseHost_SegmentedLongRequestLine(t *testing.T) {
	state := strings.Repeat("A", 560)
	code := "8460cb06-c499-f5d5-8b94-2720986a0f04.0e19ed2a-0855-45ee-8d29-94024922a040.194f4e05-25c1-4ed2-ba06-eed144ce94a3"
	reqLine := fmt.Sprintf("GET /signin-oidc?state=%s&session_state=0e19ed2a-0855-45ee-8d29-94024922a040&iss=https%%3A%%2F%%2Fauth.utc.uz%%2Frealms%%2FDotnetTest&code=%s HTTP/1.1\r\n", state, code)
	rest := "Host: fusion.tulki.uz\r\nUser-Agent: Mozilla/5.0\r\nConnection: keep-alive\r\n\r\n"

	host, buf, err := parseHostOverTCP(t, func(c net.Conn) {
		w := bufio.NewWriter(c)
		w.WriteString(reqLine) // segment 1: just the long request-line
		w.Flush()
		time.Sleep(80 * time.Millisecond)
		w.WriteString(rest) // segment 2: headers incl. Host
		w.Flush()
	})
	if err != nil {
		t.Fatalf("parseHost errored on segmented request: %v", err)
	}
	if host != "fusion.tulki.uz" {
		t.Fatalf("host = %q, want fusion.tulki.uz", host)
	}
	// Everything consumed must be preserved so it can be forwarded on.
	if want := reqLine + rest; string(buf) != want {
		t.Fatalf("consumed buffer not preserved intact:\n got %d bytes\nwant %d bytes", len(buf), len(want))
	}
}

// The Host value itself may straddle a segment boundary.
func TestParseHost_SegmentedWithinHostValue(t *testing.T) {
	host, _, err := parseHostOverTCP(t, func(c net.Conn) {
		w := bufio.NewWriter(c)
		w.WriteString("GET / HTTP/1.1\r\nHost: fus")
		w.Flush()
		time.Sleep(60 * time.Millisecond)
		w.WriteString("ion.tulki.uz\r\n\r\n")
		w.Flush()
	})
	if err != nil {
		t.Fatalf("parseHost errored: %v", err)
	}
	if host != "fusion.tulki.uz" {
		t.Fatalf("host = %q, want fusion.tulki.uz", host)
	}
}

// The common small request that arrives in a single segment still works.
func TestParseHost_SingleSegment(t *testing.T) {
	host, _, err := parseHostOverTCP(t, func(c net.Conn) {
		c.Write([]byte("GET / HTTP/1.1\r\nHost: example.tulki.uz\r\nAccept: */*\r\n\r\n"))
	})
	if err != nil {
		t.Fatalf("parseHost errored: %v", err)
	}
	if host != "example.tulki.uz" {
		t.Fatalf("host = %q, want example.tulki.uz", host)
	}
}

// A complete request that genuinely has no Host header is reported as such
// (and not hung on waiting for more bytes).
func TestParseHost_NoHostHeader(t *testing.T) {
	_, _, err := parseHostOverTCP(t, func(c net.Conn) {
		c.Write([]byte("GET / HTTP/1.0\r\nAccept: */*\r\n\r\n"))
	})
	if err == nil {
		t.Fatal("expected an error for a request without a Host header")
	}
}
