// Package musanna validates jprq CLI credentials against musanna-platform.
//
// The CLI ships an API key (created on console.musanna.uz); the server
// introspects it with GET /api/apikeys/whoami, forwarding the key in the
// X-Api-Key header, and the platform replies with the key's owner, the app it
// belongs to and its scopes.
//
// WHAT CHANGED AND WHY. Keys used to be scoped to a PROJECT and the quota lived
// on a project × service × meter row. Both concepts are gone: a key now belongs
// to a PERSON and to exactly one APP, and the limit lives in the tariff the
// owner picked for that app. The app is no longer sent by the caller either —
// the platform reads it off the key, so one app's key cannot eat another's
// allowance.
//
// WHO OWNS A TUNNEL. The platform answers with an OWNER PAIR — a kind (tenant or
// user) and an id — and jprq keys its per-owner tunnel limit off both. The kind
// cannot be dropped: the same GUID may name an organisation in one place and a
// person in another, so an id alone would let two different accounts collide in
// one bucket.
//
// Today every key is personal, so the pair is always "User:<id>". The kind is
// still carried and still part of the bucket key: organisation-owned keys are a
// planned shape, and a limit keyed off the id alone would silently merge two
// different accounts the day they appear.
//
// FORK PATCH (musanna-soft): replaces the upstream GitHub OAuth authenticator.
package musanna

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// User mirrors the shape jprq.go consumed from the GitHub authenticator —
// keeping the same field names lets the call site stay unchanged.
type User struct {
	// ID identifies the OWNER — "<kind>:<id>", e.g. "User:0193…" — and the
	// tunnel limit is keyed off it. The kind is part of the key on purpose: an
	// id on its own is ambiguous across the two kinds of account.
	ID string

	// OwnerKind is "Tenant" or "User": whose account this key bills to.
	OwnerKind string

	// OwnerID is the raw platform id, without the kind prefix. Kept for logs
	// and for talking back to the platform, which expects the pair split.
	OwnerID string

	// Login is the default subdomain. It is derived from the key's NAME (the
	// label its owner typed in the console), slugified. The platform has no
	// DNS-safe handle for a person — usernames there are phone numbers or
	// e-mail addresses — so the key label is the closest thing to a name the
	// owner actually chose. Collisions are not fatal: the caller falls back to
	// "subdomain is busy, try another one" exactly as before.
	Login string

	KeyID string

	// AppCode is the app the key was issued for — jprq keys always say "jprq".
	// It comes from the platform, never from the caller.
	AppCode string

	Scopes []string
}

// HasScope reports whether the key carries the given scope.
func (u User) HasScope(scope string) bool {
	for _, s := range u.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

type Authenticator interface {
	Authenticate(token string) (User, error)
}

// RequiredScope is what a key must carry to open a tunnel. Without it any
// platform key would reach jprq — being scoped to a tenant does not mean the
// key is entitled to THIS service.
const RequiredScope = "jprq.tunnel"

// Meter names what the platform counts for jprq. The limit for it lives in the
// tariff the key's owner picked, and the window runs from their subscription
// day — not from the first of the month.
const Meter = "tunnels"

type musanna struct {
	baseURL    string
	httpClient *http.Client
}

// New returns a validator pointed at musanna-platform.
//
// Endpoint precedence:
//  1. MUSANNA_BASE_URL (host root, e.g. https://platform.musanna.uz)
//  2. https://platform.musanna.uz
//
// MUSANNA_VALIDATE_URL is still read for backwards compatibility, but only its
// host part matters now — the paths are fixed by the platform's contract.
func New() Authenticator {
	base := strings.TrimRight(os.Getenv("MUSANNA_BASE_URL"), "/")
	if base == "" {
		if legacy := strings.TrimRight(os.Getenv("MUSANNA_VALIDATE_URL"), "/"); legacy != "" {
			base = strings.TrimSuffix(legacy, "/api/keys/validate")
		}
	}
	if base == "" {
		base = "https://platform.musanna.uz"
	}

	return musanna{
		baseURL:    base,
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}
}

// whoami is the platform's ApiKeyScopesView.
type whoami struct {
	Valid     bool     `json:"valid"`
	KeyId     string   `json:"keyId"`
	Name      string   `json:"name"`
	OwnerKind string   `json:"ownerKind"`
	OwnerId   string   `json:"ownerId"`
	AppCode   string   `json:"appCode"`
	Scopes    []string `json:"scopes"`
}

func (m musanna) Authenticate(token string) (User, error) {
	user := User{}
	if strings.TrimSpace(token) == "" {
		return user, fmt.Errorf("empty api key")
	}

	req, err := http.NewRequest("GET", m.baseURL+"/api/apikeys/whoami", nil)
	if err != nil {
		return user, fmt.Errorf("build whoami request: %v", err)
	}
	req.Header.Set("X-Api-Key", strings.TrimSpace(token))
	req.Header.Set("Accept", "application/json")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return user, fmt.Errorf("whoami request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return user, fmt.Errorf("invalid api key (http %d)", resp.StatusCode)
	}

	var body whoami
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return user, fmt.Errorf("decode whoami response: %v", err)
	}
	if !body.Valid {
		return user, fmt.Errorf("api key is not valid")
	}

	user = User{
		Login:     slugify(body.Name),
		KeyID:     body.KeyId,
		OwnerKind: body.OwnerKind,
		OwnerID:   body.OwnerId,
		AppCode:   body.AppCode,
		Scopes:    body.Scopes,
	}

	// The bucket key carries BOTH halves. Concatenating them is what makes the
	// limit correct across the two kinds of account.
	if body.OwnerKind != "" && body.OwnerId != "" {
		user.ID = body.OwnerKind + ":" + body.OwnerId
	}

	if !user.HasScope(RequiredScope) {
		return User{}, fmt.Errorf("api key is missing the %s scope", RequiredScope)
	}

	// A key with no owner cannot be limited: jprq refuses it rather than
	// silently sharing one bucket between everybody. The platform always stamps
	// an owner now, so this only fires on a malformed response — and failing
	// closed is the right answer there.
	if user.ID == "" {
		return User{}, fmt.Errorf("api key has no owner; create a new key in console.musanna.uz")
	}

	// Fall back to the key id when the label slugifies to nothing (e.g. it was
	// written entirely in Cyrillic). A GUID is an ugly subdomain, but an empty
	// one is an invalid hostname.
	if user.Login == "" {
		user.Login = strings.ToLower(user.KeyID)
	}

	return user, nil
}

// ReportUsage records one opened tunnel against the owner's tariff. It returns
// an error when the tariff is exhausted (HTTP 429) so the caller can refuse the
// tunnel — this is where the old per-user tunnel counter moved to.
//
// The app is NOT in the payload: the platform reads it from the key. Sending it
// would let a key issued for one app spend another app's allowance.
//
// Any other failure is reported to the caller as nil: losing a usage row is bad,
// but taking the whole tunnel server down with the metering endpoint is worse.
func ReportUsage(baseURL, apiKey string) error {
	payload, err := json.Marshal(map[string]any{
		"meter":    Meter,
		"quantity": 1,
	})
	if err != nil {
		return nil
	}

	req, err := http.NewRequest("POST", strings.TrimRight(baseURL, "/")+"/api/integration/usage", bytes.NewReader(payload))
	if err != nil {
		return nil
	}
	req.Header.Set("X-Api-Key", strings.TrimSpace(apiKey))
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return fmt.Errorf("tunnel limit for your tariff is used up")
	}
	return nil
}

// slugify turns a key label into a DNS label: lowercase, [a-z0-9-], no leading
// or trailing dash, at most 63 characters.
func slugify(s string) string {
	var b strings.Builder
	lastDash := true // suppresses a leading dash
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastDash = false
		case r == '-' || r == ' ' || r == '_' || r == '.':
			if !lastDash {
				b.WriteRune('-')
				lastDash = true
			}
		}
		if b.Len() >= 63 {
			break
		}
	}
	return strings.Trim(b.String(), "-")
}
