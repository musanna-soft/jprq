package main

import (
	"context"
	"strings"
	"sync"

	"github.com/azimjohn/jprq/client"
	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// App is the Wails-bound backend. Frontend calls its exported methods; it pushes
// progress back via Wails events: "status", "url", "log".
type App struct {
	ctx context.Context

	mu      sync.Mutex
	cancel  context.CancelFunc
	running bool
}

func NewApp() *App { return &App{} }

func (a *App) startup(ctx context.Context) { a.ctx = ctx }

func (a *App) emit(event, payload string) {
	if a.ctx != nil {
		wruntime.EventsEmit(a.ctx, event, payload)
	}
}

// GetToken returns the saved auth token (shared with the CLI), or "".
func (a *App) GetToken() string { return loadToken() }

// SaveToken persists the auth token. Returns "" on success, else the error text.
func (a *App) SaveToken(token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return "token is empty"
	}
	if err := saveToken(token); err != nil {
		return err.Error()
	}
	return ""
}

// StartTunnel opens a tunnel for the given protocol ("http"/"tcp"), local port,
// and optional custom subdomain. Returns "" on launch, else the error text.
// Progress arrives asynchronously through the "status"/"url"/"log" events.
func (a *App) StartTunnel(protocol string, port int, subdomain string) string {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.running {
		return "a tunnel is already running"
	}
	if protocol != "http" && protocol != "tcp" {
		return "protocol must be http or tcp"
	}
	if port <= 0 || port > 65535 {
		return "port must be 1–65535"
	}
	token := loadToken()
	if token == "" {
		return "no api key — set it first (console.musanna.uz)"
	}
	rc, err := fetchRemote()
	if err != nil {
		return err.Error()
	}

	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	a.running = true

	cl := client.New(client.Options{
		EventsAddr: rc.Events,
		Domain:     rc.Domain,
		AuthToken:  token,
		Protocol:   protocol,
		LocalPort:  port,
		Subdomain:  strings.TrimSpace(subdomain),
		Version:    version,
	}, client.Handlers{
		OnStatus: func(s string) { a.emit("status", s) },
		OnURL:    func(u string) { a.emit("url", u) },
		OnLog:    func(l string) { a.emit("log", l) },
	})

	go func() {
		runErr := cl.Run(ctx)
		a.mu.Lock()
		a.running = false
		a.cancel = nil
		a.mu.Unlock()
		if runErr != nil {
			a.emit("log", "error: "+runErr.Error())
		}
		a.emit("status", "offline")
	}()

	return ""
}

// StopTunnel cancels the running tunnel (no-op if none).
func (a *App) StopTunnel() {
	a.mu.Lock()
	cancel := a.cancel
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// IsRunning reports whether a tunnel is currently active.
func (a *App) IsRunning() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.running
}
