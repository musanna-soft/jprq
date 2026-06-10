package config

import (
	"errors"
	"os"
	"strconv"
)

type Config struct {
	DomainName          string
	MaxTunnelsPerUser   int
	MaxConsPerTunnel    int
	EventServerPort     uint16
	PublicServerPort    uint16
	PublicServerTLSPort uint16 // 0 → TLS server o'chiq (ingress terminate qiladi)
	TLSCertFile         string
	TLSKeyFile          string
}

func envPort(name string, def uint16) uint16 {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.ParseUint(v, 10, 16)
	if err != nil {
		return def
	}
	return uint16(n)
}

func (c *Config) Load() error {
	c.MaxTunnelsPerUser = 4
	c.MaxConsPerTunnel = 24
	// FORK PATCH (musanna-soft): portlar env'dan o'qiladi.
	// PublicServerTLSPort=0 → TLS server o'chiq (ingress orqali terminate).
	c.PublicServerPort = envPort("JPRQ_PUBLIC_PORT", 80)
	c.EventServerPort = envPort("JPRQ_EVENT_PORT", 4321)
	c.PublicServerTLSPort = envPort("JPRQ_PUBLIC_TLS_PORT", 443)
	c.DomainName = os.Getenv("JPRQ_DOMAIN")
	c.TLSKeyFile = os.Getenv("JPRQ_TLS_KEY")
	c.TLSCertFile = os.Getenv("JPRQ_TLS_CERT")

	if c.DomainName == "" {
		return errors.New("jprq domain env is not set")
	}
	// TLS port=0 bo'lsa TLS o'chiq — cert/key talab qilinmaydi
	if c.PublicServerTLSPort != 0 && (c.TLSKeyFile == "" || c.TLSCertFile == "") {
		return errors.New("TLS key/cert file is missing")
	}
	return nil
}
