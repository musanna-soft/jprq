// Package moderation screens user-chosen subdomains against the shirinsoz
// moderation service so a tunnel can't be opened on an offensive name
// (e.g. a profane custom subdomain on the public domain).
//
// FORK PATCH (musanna-soft): not present upstream.
//
// Fail-open by design: shirinsoz is a third-party dependency. When moderation is
// disabled, unconfigured, unreachable, or slow, IsProfane returns false (allow)
// so an outage never blocks tunnel creation. Only a positive, confirmed match
// rejects a subdomain.
package moderation

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Guard reports whether a piece of text carries a badword.
type Guard interface {
	// IsProfane returns true ONLY when shirinsoz positively confirms a badword.
	// Empty input, disabled/unconfigured moderation, or any error → false (allow).
	IsProfane(text string) bool
}

type guard struct {
	enabled      bool
	scanURL      string
	tokenURL     string
	clientID     string
	clientSecret string
	scope        string
	httpClient   *http.Client

	mu          sync.Mutex
	cachedToken string
	expiresAt   time.Time
}

// New builds a Guard from the environment:
//
//	MODERATION_ENABLED        "true" to turn it on (default off)
//	SHIRINSOZ_BASE_URL        default https://shirinsoz.musanna.uz
//	MODERATION_TOKEN_URL      OIDC token endpoint; default MUSANNA_BASE_URL + /connect/token
//	                          (falls back to https://platform.musanna.uz/connect/token)
//	MODERATION_CLIENT_ID      confidential client_credentials client (e.g. jprq-svc)
//	MODERATION_CLIENT_SECRET  its secret
//	MODERATION_SCOPE          default shirinsoz.internal
func New() Guard {
	base := strings.TrimRight(os.Getenv("SHIRINSOZ_BASE_URL"), "/")
	if base == "" {
		base = "https://shirinsoz.musanna.uz"
	}

	tokenURL := strings.TrimRight(os.Getenv("MODERATION_TOKEN_URL"), "/")
	if tokenURL == "" {
		platform := strings.TrimRight(os.Getenv("MUSANNA_BASE_URL"), "/")
		if platform == "" {
			platform = "https://platform.musanna.uz"
		}
		tokenURL = platform + "/connect/token"
	}

	scope := os.Getenv("MODERATION_SCOPE")
	if scope == "" {
		scope = "shirinsoz.internal"
	}

	return &guard{
		enabled:      strings.EqualFold(os.Getenv("MODERATION_ENABLED"), "true"),
		scanURL:      base + "/api/shirinsoz/scan",
		tokenURL:     tokenURL,
		clientID:     os.Getenv("MODERATION_CLIENT_ID"),
		clientSecret: os.Getenv("MODERATION_CLIENT_SECRET"),
		scope:        scope,
		httpClient:   &http.Client{Timeout: 3 * time.Second},
	}
}

func (g *guard) IsProfane(text string) bool {
	if !g.enabled || strings.TrimSpace(text) == "" {
		return false
	}

	token := g.token()
	if token == "" {
		return false // couldn't authenticate → fail-open
	}

	body, err := json.Marshal(scanRequest{
		Text:          strings.TrimSpace(text),
		Categories:    []string{"profanity", "sexual", "hate", "slur"},
		CrossLanguage: true,
	})
	if err != nil {
		return false
	}

	req, err := http.NewRequest("POST", g.scanURL, bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := g.httpClient.Do(req)
	if err != nil {
		log.Printf("moderation: scan failed, allowing (fail-open): %v", err)
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("moderation: scan http %d, allowing (fail-open)", resp.StatusCode)
		return false
	}

	var out scanResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false
	}
	return out.Found
}

// token returns a cached client_credentials access token, refreshing it shortly
// before expiry. Returns "" on any failure (caller fails open).
func (g *guard) token() string {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.cachedToken != "" && time.Now().Before(g.expiresAt.Add(-30*time.Second)) {
		return g.cachedToken
	}
	if g.clientID == "" {
		return ""
	}

	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {g.clientID},
		"client_secret": {g.clientSecret},
		"scope":         {g.scope},
	}

	resp, err := g.httpClient.PostForm(g.tokenURL, form)
	if err != nil {
		log.Printf("moderation: token request failed: %v", err)
		return ""
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("moderation: token endpoint http %d", resp.StatusCode)
		return ""
	}

	var tok tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil || tok.AccessToken == "" {
		return ""
	}

	expiresIn := tok.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	g.cachedToken = tok.AccessToken
	g.expiresAt = time.Now().Add(time.Duration(expiresIn) * time.Second)
	return g.cachedToken
}

type scanRequest struct {
	Text          string   `json:"text"`
	Categories    []string `json:"categories"`
	CrossLanguage bool     `json:"crossLanguage"`
}

type scanResponse struct {
	Found bool `json:"found"`
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
}
