package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

var regex = regexp.MustCompile(`^[a-z\d](?:[a-z\d]|-[a-z\d]){0,38}$`)
var blockList = map[string]bool{"www": true, "jprq": true}

func sanitize(subdomain string) string {
	sanitized := strings.ToLower(subdomain)
	reg := regexp.MustCompile(`[^a-z0-9-]+`)
	sanitized = reg.ReplaceAllString(sanitized, "-")
	reg2 := regexp.MustCompile(`-+`)
	sanitized = reg2.ReplaceAllString(sanitized, "-")
	sanitized = strings.Trim(sanitized, "-")
	return sanitized
}

func validate(subdomain *string) error {
	if len(*subdomain) > 38 || len(*subdomain) < 3 {
		return errors.New("subdomain length must be between 3 and 42")
	}
	if blockList[*subdomain] {
		return errors.New("subdomain is in deny list")
	}
	if !regex.MatchString(*subdomain) {
		*subdomain = sanitize(*subdomain)
		if len(*subdomain) > 38 || len(*subdomain) < 3 {
			return errors.New("subdomain length must be between 3 and 42")
		}
		if blockList[*subdomain] {
			return errors.New("subdomain is in deny list")
		}
		if !regex.MatchString(*subdomain) {
			return errors.New("subdomain must be lowercase & alphanumeric")
		}
	}
	return nil
}

// maxHeaderPeek caps how much of an inbound request parseHost will buffer
// while hunting for the Host header, bounding memory for a client that never
// sends one. The read deadline set by the caller bounds it in time.
const maxHeaderPeek = 64 * 1024

// parseHost reads the beginning of an HTTP request from r until it has seen a
// complete `Host:` header line (or the end of the header block), and returns
// the host value together with every byte it consumed so the caller can
// forward them on to the tunnel.
//
// A single Read is NOT enough. TCP is a stream: a request whose request-line
// is long — e.g. a Keycloak/OIDC callback carrying a big `state`/`code` query
// (`GET /signin-oidc?state=…&code=… HTTP/1.1`) — can arrive split across
// segments, with the `Host:` header landing in a later segment than the first
// Read returns. The old single-Read code then reported "no host detected",
// answered 400 and closed the connection mid-request; an ingress (nginx) still
// writing the request saw the reset and surfaced it to the user as a 502.
func parseHost(r io.Reader) (string, []byte, error) {
	buffer := make([]byte, 0, 2048)
	tmp := make([]byte, 2048)
	for {
		n, err := r.Read(tmp)
		if n > 0 {
			buffer = append(buffer, tmp[:n]...)
			if host, ok := findHost(buffer); ok {
				return host, buffer, nil
			}
			// Full header block received without a Host — genuinely absent.
			if bytes.Contains(buffer, []byte("\r\n\r\n")) || bytes.Contains(buffer, []byte("\n\n")) {
				return "", buffer, fmt.Errorf("no host detected")
			}
			if len(buffer) >= maxHeaderPeek {
				return "", buffer, fmt.Errorf("request header too large")
			}
		}
		if err != nil {
			// A partial Host line may still be complete enough to parse.
			if host, ok := findHost(buffer); ok {
				return host, buffer, nil
			}
			return "", buffer, err
		}
	}
}

// findHost extracts the Host header value from a (possibly partial) request
// header buffer. ok is false until a complete `Host:` line — terminated by a
// newline — is present, so the caller keeps reading rather than acting on a
// truncated value.
func findHost(buffer []byte) (string, bool) {
	text := string(buffer)
	left := strings.Index(text, "Host: ")
	if left < 0 {
		left = strings.Index(text, "host: ")
	}
	if left < 0 {
		return "", false
	}
	text = text[left+6:] // drops chars "Host: "
	right := strings.Index(text, "\n")
	if right < 0 {
		return "", false // Host header not fully received yet
	}
	return strings.TrimSpace(text[:right]), true
}

// isHealthCheckRequest checks the first line of an HTTP request buffer for
// `GET /healthz`. Used by servePublicConn to short-circuit kubelet probes.
func isHealthCheckRequest(buffer []byte) bool {
	if len(buffer) < 12 {
		return false
	}
	nl := bytes.IndexByte(buffer, '\n')
	if nl < 0 {
		nl = len(buffer)
	}
	line := string(buffer[:nl])
	return strings.HasPrefix(line, "GET /healthz") || strings.HasPrefix(line, "HEAD /healthz")
}

func writeResponse(conn io.WriteCloser, statusCode int, status string, message string) {
	response := fmt.Sprintf(
		"HTTP/1.1 %d %s\r\nContent-Length: %d\r\n\r\n%s", statusCode, status, len(message), message)
	conn.Write([]byte(response))
	conn.Close()
}
