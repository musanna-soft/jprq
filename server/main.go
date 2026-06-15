package main

import (
	_ "embed"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"

	"github.com/azimjohn/jprq/server/config"
	"github.com/azimjohn/jprq/server/moderation"
	"github.com/azimjohn/jprq/server/musanna"
)

// FORK PATCH (musanna-soft): website static fayllarini embed qilamiz —
// jprq-server bitta binar sifatida minimal website handlerlarini
// (index, config.json, install.sh) ham ishga tushiradi. Auth UI'i
// alohida frontend (me.musanna.uz) tomonidan ko'rsatiladi.

//go:embed static/index.html
var indexHTML string

//go:embed static/install.sh
var installerSH string

//go:embed static/install.ps1
var installerPS1 string

func main() {
	var (
		conf config.Config
		jprq Jprq
	)

	if err := conf.Load(); err != nil {
		log.Fatalf("failed to load conf: %v", err)
	}

	auth := musanna.New()
	mod := moderation.New()

	if err := jprq.Init(conf, auth, mod); err != nil {
		log.Fatalf("failed to init jprq %v", err)
	}

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
	mux.HandleFunc("/install.ps1", contentHandler(installerPS1, "text/plain; charset=utf-8"))

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
