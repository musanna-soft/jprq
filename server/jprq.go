package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/azimjohn/jprq/server/config"
	"github.com/azimjohn/jprq/server/events"
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
	cnameMap        map[string]string
	tcpTunnels      map[uint16]*tunnel.TCPTunnel
	httpTunnels     map[string]*tunnel.HTTPTunnel
	userTunnels     map[string]map[string]tunnel.Tunnel
}

func (j *Jprq) Init(conf config.Config, auth musanna.Authenticator) error {
	j.config = conf
	j.authenticator = auth
	j.cnameMap = make(map[string]string)
	j.tcpTunnels = make(map[uint16]*tunnel.TCPTunnel)
	j.httpTunnels = make(map[string]*tunnel.HTTPTunnel)
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
	// Host'dan qat'iy nazar darrov 200 qaytaradi (so'rovni website yoki
	// tunnellarga forward qilmasdan).
	if isHealthCheckRequest(buffer) {
		writeResponse(conn, 200, "OK", "ok")
		return nil
	}
	if tunnelHost, ok := j.cnameMap[host]; ok && tunnelHost != "" {
		host = tunnelHost
	}
	host = strings.ToLower(host)

	// FORK PATCH (musanna-soft): bazaviy domen (tulki.uz, www.tulki.uz) → ichki
	// website serverga proxy. Tunnellardan farqli o'laroq, bu domenga (subdomain'siz)
	// kirgan foydalanuvchilar /, /auth, /oauth-callback va h.k. endpointlarga
	// kirishadi (OAuth flow, token olish).
	if host == j.config.DomainName || host == "www."+j.config.DomainName {
		return j.proxyToWebsite(conn, buffer)
	}

	t, found := j.httpTunnels[host]
	if !found {
		writeResponse(conn, 404, "Not Found", fmt.Sprintf("tunnel not found. create one at https://%s/auth", j.config.DomainName))
		return fmt.Errorf("unknown host requested %s", host)
	}
	return t.PublicConnectionHandler(conn, buffer)
}

// proxyToWebsite — bazaviy domen so'rovini ichki website serverga uzatadi
// (TCP forward). Website server localhost:3300 da ishlaydi (server/main.go da
// goroutine sifatida ishga tushadi).
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
	conn.SetReadDeadline(time.Time{}) // o'chirib qo'yamiz — proxy ishlash davomida deadline kerakmas
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

	var event events.Event[events.TunnelRequested]
	if err := event.Read(conn); err != nil {
		return err
	}

	request := event.Data
	if request.Protocol != events.HTTP && request.Protocol != events.TCP {
		return events.WriteError(conn, "invalid protocol %s", request.Protocol)
	}
	user, err := j.authenticator.Authenticate(request.AuthToken)
	if err != nil {
		return events.WriteError(conn, "authentication failed %s", "\n\tobtain auth token from https://me.musanna.uz/keys\n")
	}

	// FORK PATCH (musanna-soft): allowed-users.csv tekshirivi olib tashlandi —
	// GitHub OAuth orqali kirgan har bir foydalanuvchi ruxsat oladi (tulki.uz open service).
	if len(j.userTunnels[user.Login]) >= j.config.MaxTunnelsPerUser {
		return events.WriteError(conn, "tunnels limit reached for %s", user.Login)
	}
	if request.Subdomain == "" {
		request.Subdomain = user.Login
	}
	if err := validate(&request.Subdomain); err != nil {
		return events.WriteError(conn, "invalid subdomain %s: %s", request.Subdomain, err.Error())
	}
	hostname := fmt.Sprintf("%s.%s", request.Subdomain, j.config.DomainName)
	if _, ok := j.httpTunnels[hostname]; ok {
		return events.WriteError(conn, "subdomain is busy: %s, try another one", request.Subdomain)
	}
	cname := request.CanonName
	if _, ok := j.cnameMap[cname]; ok && cname != "" {
		return events.WriteError(conn, "cname is busy: %s, try another one", request.CanonName)
	}

	var t tunnel.Tunnel
	var maxConsLimit = j.config.MaxConsPerTunnel

	switch request.Protocol {
	case events.HTTP:
		tn, err := tunnel.NewHTTP(hostname, conn, maxConsLimit)
		if err != nil {
			return events.WriteError(conn, "failed to create http tunnel", err.Error())
		}
		j.cnameMap[cname] = hostname
		j.httpTunnels[hostname] = tn
		defer delete(j.cnameMap, cname)
		defer delete(j.httpTunnels, hostname)
		t = tn
	case events.TCP:
		tn, err := tunnel.NewTCP(hostname, conn, maxConsLimit)
		if err != nil {
			return events.WriteError(conn, "failed to create tcp tunnel", err.Error())
		}
		j.tcpTunnels[tn.PublicServerPort()] = tn
		defer delete(j.tcpTunnels, tn.PublicServerPort())
		t = tn
	}

	if len(j.userTunnels[user.Login]) == 0 {
		j.userTunnels[user.Login] = make(map[string]tunnel.Tunnel)
	}
	tunnelId := fmt.Sprintf("%s:%d", t.Hostname(), t.PublicServerPort())
	j.userTunnels[user.Login][tunnelId] = t
	defer delete(j.userTunnels[user.Login], tunnelId)

	t.Open()
	defer t.Close()
	opened := events.Event[events.TunnelOpened]{
		Data: &events.TunnelOpened{
			Hostname:      t.Hostname(),
			Protocol:      t.Protocol(),
			PublicServer:  t.PublicServerPort(),
			PrivateServer: t.PrivateServerPort(),
		},
	}
	if err := opened.Write(conn); err != nil {
		return err
	}

	fmt.Printf("%s [tunnel-opened] %s: %s\n", time.Now().Format(dateFormat), user.Login, tunnelId)

	buffer := make([]byte, 8) // wait until connection is closed
	for {
		_ = conn.SetReadDeadline(time.Now().Add(time.Minute))
		if _, err := conn.Read(buffer); err == io.EOF {
			break
		}
		// FORK PATCH (musanna-soft): allowed-users tekshiruvi olib tashlandi.
	}
	fmt.Printf("%s [tunnel-closed] %s: %s\n", time.Now().Format(dateFormat), user.Login, tunnelId)
	return nil
}

