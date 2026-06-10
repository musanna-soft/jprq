// Package musanna validates jprq CLI auth tokens against musanna-platform's
// ApiKeys module. The CLI ships a personal access token (created on
// me.musanna.uz), the server POSTs it to /api/keys/validate?appId=<jprq-app-id>,
// and the platform replies with the owning user's identity. The appId
// parameter scopes the validation: a key issued for one app cannot be
// replayed against another.
//
// FORK PATCH (musanna-soft): replaces the upstream GitHub OAuth authenticator.
package musanna

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// User mirrors the shape jprq.go consumed from the GitHub authenticator —
// keeping the same field names lets the call site stay unchanged. The
// PascalCase JSON tags follow musanna-platform's MVC controllers.
type User struct {
	ID            string `json:"UserId"`
	Email         string `json:"Email"`
	Login         string `json:"Login"`
	IsVip         bool   `json:"IsVip"`
	ApplicationId string `json:"ApplicationId"`
	AppCode       string `json:"AppCode"`
}

type Authenticator interface {
	Authenticate(token string) (User, error)
}

type musanna struct {
	validateURL string
	appId       string
	httpClient  *http.Client
}

// New returns a validator pointed at musanna-platform.
//
// Endpoint precedence:
//  1. MUSANNA_VALIDATE_URL (full URL, e.g. https://platform.musanna.uz/api/keys/validate)
//  2. MUSANNA_BASE_URL + /api/keys/validate
//  3. https://platform.musanna.uz/api/keys/validate
//
// MUSANNA_APP_ID (required) is the platform Application GUID this server
// validates against. The server appends ?appId=<guid> so the platform
// refuses tokens issued for any other app.
func New() Authenticator {
	u := strings.TrimRight(os.Getenv("MUSANNA_VALIDATE_URL"), "/")
	if u == "" {
		base := strings.TrimRight(os.Getenv("MUSANNA_BASE_URL"), "/")
		if base == "" {
			base = "https://platform.musanna.uz"
		}
		u = base + "/api/keys/validate"
	}
	return musanna{
		validateURL: u,
		appId:       os.Getenv("MUSANNA_APP_ID"),
		httpClient:  &http.Client{Timeout: 5 * time.Second},
	}
}

func (m musanna) Authenticate(token string) (User, error) {
	user := User{}
	if strings.TrimSpace(token) == "" {
		return user, fmt.Errorf("empty token")
	}

	body, err := json.Marshal(map[string]string{"token": token})
	if err != nil {
		return user, err
	}

	target := m.validateURL
	if m.appId != "" {
		sep := "?"
		if strings.Contains(target, "?") {
			sep = "&"
		}
		target = target + sep + "appId=" + url.QueryEscape(m.appId)
	}

	req, err := http.NewRequest("POST", target, bytes.NewReader(body))
	if err != nil {
		return user, fmt.Errorf("build validate request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return user, fmt.Errorf("validate request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return user, fmt.Errorf("invalid token (http %d)", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&user); err != nil {
		return user, fmt.Errorf("decode validate response: %v", err)
	}
	user.Login = strings.ToLower(user.Login)
	return user, nil
}
