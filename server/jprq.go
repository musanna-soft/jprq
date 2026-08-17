package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/azimjohn/jprq/server/config"
	"github.com/azimjohn/jprq/server/events"
	"github.com/azimjohn/jprq/server/moderation"
	"github.com/azimjohn/jprq/server/musanna"
	"github.com/azimjohn/jprq/server/server"
	"github.com/azimjohn/jprq/server/tunnel"
)

const dateFormat = "2006/01/02 15:04:05"

type Jprq struct {
	config          config.Config
	eventServer     server.TCPServer
	publicServer    server.TCPServer
	publicServerTLS server.TCPServer
	authenticator   musanna.Authenticator
	moderation      moderation.Guard
	mu              sync.RWMutex
	cnameMap        map[string]string
	tcpTunnels      map[uint16]tunnel.Tunnel
	httpTunnels     map[string]tunnel.Tunnel
	userTunnels     map[string]map[string]tunnel.Tunnel
}

func (j *Jprq) Init(conf config.Config, auth musanna.Authenticator, mod moderation.Guard) error {
	j.config = conf
	j.authenticator = auth
	j.moderation = mod
	j.cnameMap = make(map[string]string)
	j.tcpTunnels = make(map[uint16]tunnel.Tunnel)
	j.httpTunnels = make(map[string]tunnel.Tunnel)
	j.userTunnels = make(map[string]map[string]tunnel.Tunnel)

	if err := j.eventServer.Init(conf.EventServerPort, "jprq_event_server"); err != nil {
		return err
	}
	if err := j.publicServer.Init(conf.PublicServerPort, "jprq_public_server"); err != nil {
		return err
	}
	// FORK PATCH (musanna-soft): TLS port=0 bo'lsa, TLS server o'chiq
	// (ingress orqali terminate qilinadi).
	if conf.PublicServerTLSPort == 0 {
		return nil
	}
	return j.publicServerTLS.InitTLS(conf.PublicServerTLSPort, "jprq_public_server_tls", conf.TLSCertFile, conf.TLSKeyFile)
}

func (j *Jprq) Start() {
	go j.eventServer.Start(j.serveEventConn)
	go j.publicServer.Start(j.servePublicConn)
	if j.config.PublicServerTLSPort != 0 {
		go j.publicServerTLS.Start(j.servePublicConn)
	}
}

func (j *Jprq) Stop() error {
	if err := j.eventServer.Stop(); err != nil {
		return err
	}
	if err := j.publicServer.Stop(); err != nil {
		return err
	}
	if j.config.PublicServerTLSPort != 0 {
		return j.publicServerTLS.Stop()
	}
	return nil
}

func (j *Jprq) servePublicConn(conn net.Conn) error {
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	host, buffer, err := parseHost(conn)
	if err != nil || host == "" {
		writeResponse(conn, 400, "Bad Request", "Bad Request")
		return nil
	}
	// FORK PATCH (musanna-soft): kubelet readinessProbe — `/healthz` chaqirig'i
	// Host'dan qat'iy nazar darrov 200 qaytaradi.
	if isHealthCheckRequest(buffer) {
		writeResponse(conn, 200, "OK", "ok")
		return nil
	}
	j.mu.RLock()
	if tunnelHost, ok := j.cnameMap[host]; ok && tunnelHost != "" {
		host = tunnelHost
	}
	j.mu.RUnlock()
	host = strings.ToLower(host)

	// FORK PATCH (musanna-soft): bazaviy domen → embedded website.
	if host == j.config.DomainName || host == "www."+j.config.DomainName {
		return j.proxyToWebsite(conn, buffer)
	}

	j.mu.RLock()
	t, found := j.httpTunnels[host]
	j.mu.RUnlock()
	if !found {
		writeResponse(conn, 404, "Not Found", fmt.Sprintf("tunnel not found. create one at https://%s/", j.config.DomainName))
		return fmt.Errorf("unknown host requested %s", host)
	}
	conn.SetReadDeadline(time.Time{})
	return t.PublicConnectionHandler(conn, buffer)
}

// proxyToWebsite — bazaviy domen so'rovini ichki website serverga uzatadi
// (TCP forward). Website server localhost:3300 da ishlaydi.
func (j *Jprq) proxyToWebsite(conn net.Conn, buffer []byte) error {
	defer conn.Close()
	port := os.Getenv("JPRQ_WEBSITE_PORT")
	if port == "" {
		port = "3300"
	}
	upstream, err := net.DialTimeout("tcp", "127.0.0.1:"+port, 3*time.Second)
	if err != nil {
		writeResponse(conn, 502, "Bad Gateway", "website server unreachable")
		return err
	}
	defer upstream.Close()
	conn.SetReadDeadline(time.Time{})
	if _, err := upstream.Write(buffer); err != nil {
		return err
	}
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstream, conn)
		_ = upstream.(*net.TCPConn).CloseWrite()
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(conn, upstream)
		if tc, ok := conn.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		done <- struct{}{}
	}()
	<-done
	return nil
}

func (j *Jprq) serveEventConn(conn net.Conn) error {
	defer conn.Close()
	// Long-lived control channel — without TCP keepalive a stateful NAT
	// closes the conn after a few seconds of idleness, which the user sees
	// as `tunnel-closed` every 10–20 s.
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(15 * time.Second)
	}
	framed := events.NewFramedConn(conn)

	// Expect MsgTunnelRequested first.
	var first events.Message
	if err := framed.Recv(&first); err != nil {
		return err
	}
	req, err := events.DecodeTunnelRequested(&first)
	if err != nil {
		return err
	}

	if req.Protocol != events.HTTP && req.Protocol != events.TCP {
		return events.WriteError(framed, "invalid protocol %s", req.Protocol)
	}

	user, err := j.authenticator.Authenticate(req.AuthToken)
	if err != nil {
		return events.WriteError(framed, "authentication failed%s", "\n\tobtain auth token from https://me.musanna.uz/api-keys\n")
	}

	j.mu.Lock()
	if len(j.userTunnels[user.Login]) >= j.config.MaxTunnelsPerUser {
		j.mu.Unlock()
		return events.WriteError(framed, "tunnels limit reached for %s", user.Login)
	}
	j.mu.Unlock()

	if req.Subdomain == "" {
		req.Subdomain = user.Login
	}
	if err := validate(&req.Subdomain); err != nil {
		return events.WriteError(framed, "invalid subdomain %s: %s", req.Subdomain, err.Error())
	}
	if j.moderation.IsProfane(req.Subdomain) {
		return events.WriteError(framed, "subdomain not allowed: %s, choose another one", req.Subdomain)
	}
	hostname := fmt.Sprintf("%s.%s", req.Subdomain, j.config.DomainName)
	j.mu.Lock()
	if _, ok := j.httpTunnels[hostname]; ok {
		j.mu.Unlock()
		return events.WriteError(framed, "subdomain is busy: %s, try another one", req.Subdomain)
	}
	if _, ok := j.cnameMap[req.CanonName]; ok && req.CanonName != "" {
		j.mu.Unlock()
		return events.WriteError(framed, "cname is busy: %s, try another one", req.CanonName)
	}
	j.mu.Unlock()

	var t tunnel.Tunnel
	maxConsLimit := j.config.MaxConsPerTunnel

	switch req.Protocol {
	case events.HTTP:
		tn, err := tunnel.NewHTTP(hostname, framed, maxConsLimit)
		if err != nil {
			return events.WriteError(framed, "failed to create http tunnel: %s", err.Error())
		}
		j.mu.Lock()
		j.cnameMap[req.CanonName] = hostname
		j.httpTunnels[hostname] = tn
		j.mu.Unlock()
		defer func() {
			j.mu.Lock()
			delete(j.cnameMap, req.CanonName)
			delete(j.httpTunnels, hostname)
			j.mu.Unlock()
		}()
		t = tn
	case events.TCP:
		// Custom public port is only honoured inside the reserved range so
		// users can't squat on 22 / 80 / 443 / 4321 or anything outside what
		// the hostPort range exposes (33000-33099 by deploy config).
		requested := req.PublicPort
		if requested != 0 && (requested < 33000 || requested > 33009) {
			return events.WriteError(framed, "public port out of allowed range 33000-33009%s", "")
		}
		j.mu.Lock()
		if requested != 0 {
			if _, busy := j.tcpTunnels[requested]; busy {
				j.mu.Unlock()
				return events.WriteError(framed, "public port is busy, try another one%s", "")
			}
		}
		j.mu.Unlock()
		tn, err := tunnel.NewTCP(hostname, framed, maxConsLimit, requested)
		if err != nil {
			return events.WriteError(framed, "failed to create tcp tunnel: %s", err.Error())
		}
		j.mu.Lock()
		j.tcpTunnels[tn.PublicServerPort()] = tn
		j.mu.Unlock()
		defer func() {
			j.mu.Lock()
			delete(j.tcpTunnels, tn.PublicServerPort())
			j.mu.Unlock()
		}()
		t = tn
	}

	j.mu.Lock()
	if _, ok := j.userTunnels[user.ID]; !ok {
		j.userTunnels[user.ID] = make(map[string]tunnel.Tunnel)
	}
	tunnelId := fmt.Sprintf("%s:%d", t.Hostname(), t.PublicServerPort())
	j.userTunnels[user.ID][tunnelId] = t
	j.mu.Unlock()
	defer func() {
		j.mu.Lock()
		delete(j.userTunnels[user.ID], tunnelId)
		j.mu.Unlock()
	}()

	t.Open()
	defer t.Close()

	if err := framed.SendControl(events.MsgTunnelOpened, &events.TunnelOpened{
		Hostname:     t.Hostname(),
		Protocol:     t.Protocol(),
		PublicServer: t.PublicServerPort(),
	}); err != nil {
		return err
	}

	fmt.Printf("%s [tunnel-opened] %s: %s\n", time.Now().Format(dateFormat), user.Login, tunnelId)

	// App-level keepalive: in addition to SO_KEEPALIVE, push a Ping every
	// 10 s so even a NAT that ignores TCP keepalive packets still sees real
	// traffic and refuses to evict the conntrack entry.
	pingDone := make(chan struct{})
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-pingDone:
				return
			case <-t.C:
				if err := framed.Send(&events.Message{Type: events.MsgPing}); err != nil {
					return
				}
			}
		}
	}()
	defer close(pingDone)

	// Read multiplexed frames from CLI until the connection drops.
	for {
		var m events.Message
		if err := framed.Recv(&m); err != nil {
			break
		}
		switch m.Type {
		case events.MsgConnectionData:
			if err := t.DispatchData(m.StreamID, m.Payload); err != nil {
				// stream may have already closed — silently ignore
			}
		case events.MsgConnectionClose:
			t.DispatchClose(m.StreamID)
		case events.MsgPing:
			// no-op
		}
	}

	fmt.Printf("%s [tunnel-closed] %s: %s\n", time.Now().Format(dateFormat), user.Login, tunnelId)
	return nil
}
