package main

import (
	"net"
	"strings"
	"testing"
	"time"

	"github.com/azimjohn/jprq/server/config"
	"github.com/azimjohn/jprq/server/events"
	"github.com/azimjohn/jprq/server/musanna"
	"github.com/azimjohn/jprq/server/tunnel"
)

// Iste'mol tunnel ochilganda YOZILISHI shart.
//
// Bu bir marta butunlay tushib qolgan: `ReportUsage` yozilgan, lekin hech
// qayerdan chaqirilmagan edi. Tunnel ochilar, console'dagi iste'mol esa nol
// bo'lib turar, tarif chegarasi umuman qo'llanmasdi — va buni server tomonda
// hech qanday xato ko'rsatmasdi.

type fakeAuth struct {
	reports   []string
	reportErr error
}

func (f *fakeAuth) Authenticate(token string) (musanna.User, error) {
	return musanna.User{
		ID:        "User:test",
		OwnerKind: "User",
		OwnerID:   "test",
		Login:     "tester",
		KeyID:     "k1",
		AppCode:   "jprq",
		Scopes:    []string{musanna.RequiredScope},
	}, nil
}

func (f *fakeAuth) ReportUsage(token string) error {
	f.reports = append(f.reports, token)
	return f.reportErr
}

type allowAll struct{}

func (allowAll) IsProfane(string) bool { return false }

func newTestJprq(auth musanna.Authenticator) *Jprq {
	return &Jprq{
		config: config.Config{
			DomainName:        "test.local",
			MaxTunnelsPerUser: 4,
			MaxConsPerTunnel:  4,
		},
		authenticator: auth,
		moderation:    allowAll{},
		cnameMap:      make(map[string]string),
		tcpTunnels:    make(map[uint16]tunnel.Tunnel),
		httpTunnels:   make(map[string]tunnel.Tunnel),
		userTunnels:   make(map[string]map[string]tunnel.Tunnel),
	}
}

// requestTunnel drives one MsgTunnelRequested through serveEventConn and
// returns the CLI-visible answer.
func requestTunnel(t *testing.T, j *Jprq, token string) *events.TunnelOpened {
	t.Helper()

	client, srv := net.Pipe()
	defer client.Close()

	go func() { _ = j.serveEventConn(srv) }()

	framed := events.NewFramedConn(client)
	if err := framed.SendControl(events.MsgTunnelRequested, &events.TunnelRequested{
		Protocol:  events.HTTP,
		Subdomain: "demo",
		AuthToken: token,
	}); err != nil {
		t.Fatalf("send: %v", err)
	}

	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	var reply events.Message
	if err := framed.Recv(&reply); err != nil {
		t.Fatalf("recv: %v", err)
	}
	opened, err := events.DecodeTunnelOpened(&reply)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return opened
}

func TestTunnelOpen_ReportsUsage(t *testing.T) {
	auth := &fakeAuth{}
	opened := requestTunnel(t, newTestJprq(auth), "key-abc")

	if opened.ErrorMessage != "" {
		t.Fatalf("tunnel ochilishi kerak edi, xato: %s", opened.ErrorMessage)
	}
	if len(auth.reports) != 1 {
		t.Fatalf("iste'mol bir marta yozilishi kerak, yozilgani: %d", len(auth.reports))
	}
	if auth.reports[0] != "key-abc" {
		t.Fatalf("kalit uzatilmadi: %q", auth.reports[0])
	}
}

// Chegara tugaganda (platforma 429) tunnel RAD ETILADI — aks holda tarif
// shunchaki bezak bo'lardi.
func TestTunnelOpen_RefusedWhenTariffExhausted(t *testing.T) {
	auth := &fakeAuth{reportErr: errTariff}
	opened := requestTunnel(t, newTestJprq(auth), "key-abc")

	if opened.ErrorMessage == "" {
		t.Fatal("chegara tugagan, tunnel rad etilishi kerak edi")
	}
	if !strings.Contains(opened.ErrorMessage, "limit") {
		t.Fatalf("xato sababi ko'rinmadi: %q", opened.ErrorMessage)
	}
}

type tariffError struct{}

func (tariffError) Error() string { return "tunnel limit for your tariff is used up" }

var errTariff = tariffError{}
