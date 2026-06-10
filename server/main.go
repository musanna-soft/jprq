package main

import (
	_ "embed"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"

	"github.com/azimjohn/jprq/server/config"
	"github.com/azimjohn/jprq/server/github"
)

// FORK PATCH (musanna-soft): website static fayllarini embed qilamiz —
// jprq-server bitta binar sifatida website handlerlarini ham ishga tushiradi
// (alohida `website/main.go` jarayonga ehtiyoj yo'q).

//go:embed static/index.html
var indexHTML string

//go:embed static/token.html
var tokenHTML string

//go:embed static/install.sh
var installerSH string

var oauth github.Authenticator

func main() {
	var (
		conf config.Config
		jprq Jprq
	)

	err := conf.Load()
	if err != nil {
		log.Fatalf("failed to load conf: %v", err)
	}

	oauth = github.New(conf.GithubClientID, conf.GithubClientSecret)

	err = jprq.Init(conf, oauth)
	if err != nil {
		log.Fatalf("failed to init jprq %v", err)
	}

	// Website handlerlar — localhost:3300 (yoki JPRQ_WEBSITE_PORT)
	go startWebsite(conf.DomainName)

	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, os.Interrupt)

	jprq.Start()
	defer jprq.Stop()

	<-signalChan
}

func startWebsite(domain string) {
	port := os.Getenv("JPRQ_WEBSITE_PORT")
	if port == "" {
		port = "3300"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", contentHandler(indexHTML, "text/html"))
	mux.HandleFunc("/config.json", configHandler(domain))
	mux.HandleFunc("/install.sh", contentHandler(installerSH, "text/x-shellscript"))
	mux.HandleFunc("/auth", authHandler)
	mux.HandleFunc("/oauth-callback", oauthCallback)

	addr := "127.0.0.1:" + port
	log.Printf("website: listening on %s (proxied via %s)", addr, domain)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Printf("website server error: %v", err)
	}
}

func contentHandler(body, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Write([]byte(body))
	}
}

func configHandler(domain string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"domain":"%s","events":"event.%s:4321"}`, domain, domain)
	}
}

func authHandler(w http.ResponseWriter, r *http.Request) {
	app := r.URL.Query().Get("app")
	callback := r.URL.Query().Get("callback")
	if app != "" {
		http.SetCookie(w, &http.Cookie{
			Name: "jprq_app", Value: app, Path: "/",
			MaxAge: 300, HttpOnly: true, SameSite: http.SameSiteLaxMode,
		})
	}
	if callback != "" {
		http.SetCookie(w, &http.Cookie{
			Name: "jprq_callback", Value: callback, Path: "/",
			MaxAge: 300, HttpOnly: true, SameSite: http.SameSiteLaxMode,
		})
	}
	http.Redirect(w, r, oauth.OAuthUrl(), http.StatusFound)
}

func oauthCallback(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil || r.FormValue("code") == "" {
		http.Redirect(w, r, "/auth", http.StatusTemporaryRedirect)
		return
	}
	token, err := oauth.ObtainToken(r.FormValue("code"))
	if err != nil || token == "" {
		log.Printf("error obtaining token: %v", err)
		http.Redirect(w, r, "/auth", http.StatusTemporaryRedirect)
		return
	}

	appCookie, _ := r.Cookie("jprq_app")
	callbackCookie, _ := r.Cookie("jprq_callback")
	// Tozalaymiz
	http.SetCookie(w, &http.Cookie{Name: "jprq_app", Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	http.SetCookie(w, &http.Cookie{Name: "jprq_callback", Value: "", Path: "/", MaxAge: -1, HttpOnly: true})

	if callbackCookie != nil && callbackCookie.Value != "" {
		parsed, perr := url.Parse(callbackCookie.Value)
		if perr == nil && parsed.Scheme != "" && parsed.Host != "" {
			q := parsed.Query()
			q.Set("token", token)
			parsed.RawQuery = q.Encode()
			http.Redirect(w, r, parsed.String(), http.StatusFound)
			return
		}
	}

	if appCookie != nil && appCookie.Value != "" {
		switch strings.ToLower(appCookie.Value) {
		case "mac", "windows", "linux":
			http.Redirect(w, r, fmt.Sprintf("jprq://auth/callback?token=%s", token), http.StatusFound)
			return
		}
	}
	w.Header().Set("Content-Type", "text/html")
	fmt.Fprintf(w, tokenHTML, token)
}
