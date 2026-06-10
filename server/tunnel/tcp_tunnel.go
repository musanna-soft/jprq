package tunnel

import (
	"fmt"
	"net"

	"github.com/azimjohn/jprq/server/events"
	"github.com/azimjohn/jprq/server/server"
)

// TCPTunnel still owns a per-tunnel public port (the whole point of a TCP
// tunnel is to expose a port to the outside world), but private routing
// to the CLI is multiplexed onto the shared event channel like HTTPTunnel.
type TCPTunnel struct {
	tunnel
	publicServer server.TCPServer
}

func NewTCP(hostname string, event *events.FramedConn, maxConsLimit int) (*TCPTunnel, error) {
	t := &TCPTunnel{tunnel: newTunnel(hostname, event, maxConsLimit)}
	if err := t.publicServer.Init(0, "tcp_tunnel_public_server"); err != nil {
		return t, fmt.Errorf("error init public server: %w", err)
	}
	return t, nil
}

func (t *TCPTunnel) Protocol() string         { return "tcp" }
func (t *TCPTunnel) PublicServerPort() uint16 { return t.publicServer.Port() }

func (t *TCPTunnel) Open() {
	go t.publicServer.Start(func(c net.Conn) error {
		return t.handlePublicConn(c, nil)
	})
}

func (t *TCPTunnel) Close() {
	_ = t.publicServer.Stop()
	t.tunnel.Close()
}

// PublicConnectionHandler is unused for TCPTunnel (handled by its own
// listener) but kept to satisfy the Tunnel interface.
func (t *TCPTunnel) PublicConnectionHandler(net.Conn, []byte) error {
	return fmt.Errorf("tcp tunnel does not accept demuxed public connections")
}
