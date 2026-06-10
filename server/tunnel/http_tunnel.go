package tunnel

import (
	"net"

	"github.com/azimjohn/jprq/server/events"
)

const DefaultHttpPort = 80

// HTTPTunnel is a multiplexed HTTP tunnel — every public connection
// becomes a stream over the shared event channel.
type HTTPTunnel struct {
	tunnel
}

func NewHTTP(hostname string, event *events.FramedConn, maxConsLimit int) (*HTTPTunnel, error) {
	return &HTTPTunnel{tunnel: newTunnel(hostname, event, maxConsLimit)}, nil
}

func (t *HTTPTunnel) Protocol() string         { return "http" }
func (t *HTTPTunnel) PublicServerPort() uint16 { return DefaultHttpPort }

// Open is a no-op: there is no per-tunnel listener anymore. The shared
// `publicServer` in jprq.go demuxes incoming connections by Host header.
func (t *HTTPTunnel) Open() {}

// PublicConnectionHandler is invoked by the demuxer for each public
// connection whose Host matches this tunnel.
func (t *HTTPTunnel) PublicConnectionHandler(publicCon net.Conn, initialBuffer []byte) error {
	return t.handlePublicConn(publicCon, initialBuffer)
}
