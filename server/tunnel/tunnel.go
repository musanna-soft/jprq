// Package tunnel wires public TCP connections into the CLI through the
// single multiplexed event channel. Each public connection is given a
// stream id; bytes flow as MsgConnectionData frames keyed by that id.
// No per-tunnel random port is allocated.
//
// FORK PATCH (musanna-soft): private-port handshake removed.
package tunnel

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/azimjohn/jprq/server/events"
)

type Tunnel interface {
	Open()
	Close()
	Hostname() string
	Protocol() string
	PublicServerPort() uint16
	// PublicConnectionHandler accepts a freshly received public connection
	// and an initial buffer (the first bytes already read from it, e.g. the
	// HTTP request line + headers).
	PublicConnectionHandler(publicCon net.Conn, initialBuffer []byte) error
	// DispatchData routes a MsgConnectionData frame coming back from the CLI
	// to the public connection with the matching stream id.
	DispatchData(streamID uint32, data []byte) error
	// DispatchClose closes the public connection with the given stream id.
	DispatchClose(streamID uint32)
}

// tunnel holds the per-tunnel multiplex bookkeeping.
type tunnel struct {
	hostname     string
	maxConsLimit int
	event        *events.FramedConn
	nextStream   uint32
	streamsMu    sync.Mutex
	streams      map[uint32]net.Conn
}

func newTunnel(hostname string, event *events.FramedConn, maxConsLimit int) tunnel {
	return tunnel{
		hostname:     hostname,
		maxConsLimit: maxConsLimit,
		event:        event,
		streams:      make(map[uint32]net.Conn),
	}
}

func (t *tunnel) Hostname() string { return t.hostname }

func (t *tunnel) registerStream(c net.Conn) uint32 {
	t.streamsMu.Lock()
	defer t.streamsMu.Unlock()
	t.nextStream++
	id := t.nextStream
	t.streams[id] = c
	return id
}

func (t *tunnel) deregisterStream(id uint32) net.Conn {
	t.streamsMu.Lock()
	defer t.streamsMu.Unlock()
	c, ok := t.streams[id]
	if ok {
		delete(t.streams, id)
	}
	return c
}

func (t *tunnel) getStream(id uint32) net.Conn {
	t.streamsMu.Lock()
	defer t.streamsMu.Unlock()
	return t.streams[id]
}

func (t *tunnel) streamCount() int {
	t.streamsMu.Lock()
	defer t.streamsMu.Unlock()
	return len(t.streams)
}

// Close ends every active public connection of this tunnel.
func (t *tunnel) Close() {
	t.streamsMu.Lock()
	defer t.streamsMu.Unlock()
	for id, c := range t.streams {
		_ = c.Close()
		delete(t.streams, id)
	}
}

func (t *tunnel) DispatchData(streamID uint32, data []byte) error {
	c := t.getStream(streamID)
	if c == nil {
		return fmt.Errorf("stream %d not found", streamID)
	}
	_, err := c.Write(data)
	return err
}

func (t *tunnel) DispatchClose(streamID uint32) {
	if c := t.deregisterStream(streamID); c != nil {
		_ = c.Close()
	}
}

// handlePublicConn frames the public connection's bytes into
// MsgConnectionData messages on the event channel until EOF, then sends a
// final MsgConnectionClose. initialBuffer (whatever was already read from
// the wire before the dispatch) is sent as the first data frame.
func (t *tunnel) handlePublicConn(publicCon net.Conn, initialBuffer []byte) error {
	ip := publicCon.RemoteAddr().(*net.TCPAddr).IP
	port := uint16(publicCon.RemoteAddr().(*net.TCPAddr).Port)

	if t.streamCount() >= t.maxConsLimit {
		_ = t.event.SendControl(events.MsgConnectionLimit, &events.ConnectionLimit{ClientIP: ip})
		_ = publicCon.Close()
		return fmt.Errorf("[connections-limit-reached]: %s", t.hostname)
	}

	streamID := t.registerStream(publicCon)
	defer func() {
		t.deregisterStream(streamID)
		_ = publicCon.Close()
	}()

	openPayload, err := events.EncodeConnectionOpened(&events.ConnectionOpened{
		ClientIP:   ip,
		ClientPort: port,
	})
	if err != nil {
		return err
	}
	if err := t.event.Send(&events.Message{
		Type:     events.MsgConnectionOpened,
		StreamID: streamID,
		Payload:  openPayload,
	}); err != nil {
		return err
	}

	if len(initialBuffer) > 0 {
		if err := t.sendData(streamID, initialBuffer); err != nil {
			return err
		}
	}

	// Keepalive so a silently-dead peer (no FIN/RST) is still detected by the OS
	// even when the stream is legitimately idle — e.g. a quiet WebSocket.
	if tc, ok := publicCon.(*net.TCPConn); ok {
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(30 * time.Second)
	}
	buf := make([]byte, events.MaxPayloadLen)
	for {
		_ = publicCon.SetReadDeadline(time.Now().Add(30 * time.Second))
		n, err := publicCon.Read(buf)
		if n > 0 {
			if werr := t.sendData(streamID, buf[:n]); werr != nil {
				return werr
			}
		}
		if err != nil {
			// An idle read deadline is NOT a close: a long-lived WebSocket can sit
			// quiet for minutes. Reset and keep reading. Real ends (EOF / RST /
			// keepalive failure) are not ErrDeadlineExceeded, so they break out.
			if errors.Is(err, os.ErrDeadlineExceeded) {
				continue
			}
			break
		}
	}

	// Tell the CLI this stream is finished so it can close its local end.
	_ = t.event.Send(&events.Message{Type: events.MsgConnectionClose, StreamID: streamID})
	return nil
}

func (t *tunnel) sendData(streamID uint32, data []byte) error {
	for len(data) > 0 {
		chunk := data
		if len(chunk) > events.MaxPayloadLen {
			chunk = chunk[:events.MaxPayloadLen]
		}
		if err := t.event.Send(&events.Message{
			Type:     events.MsgConnectionData,
			StreamID: streamID,
			Payload:  chunk,
		}); err != nil {
			return err
		}
		data = data[len(chunk):]
	}
	return nil
}

// Bind is kept as a small helper that pipes one connection into another;
// CLI uses it for HTTP-debugger fan-out.
func Bind(src net.Conn, dst net.Conn, debug io.Writer) error {
	defer src.Close()
	defer dst.Close()
	buf := make([]byte, 4096)
	for {
		_ = src.SetReadDeadline(time.Now().Add(time.Second))
		n, err := src.Read(buf)
		if err == io.EOF {
			break
		}
		_ = dst.SetWriteDeadline(time.Now().Add(time.Second))
		if _, werr := dst.Write(buf[:n]); werr != nil {
			return werr
		}
		if debug != nil {
			_, _ = debug.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
	return nil
}
