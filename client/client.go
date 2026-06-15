// Package client is an embeddable jprq tunnel client. It speaks the same
// multiplexed event protocol as the CLI (server/events) but, unlike cli/, it
// never calls log.Fatalf / fmt.Printf — it returns errors and reports progress
// through callbacks, so a GUI (the Wails desktop app) can drive it.
package client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/azimjohn/jprq/server/events"
)

// Options configures a single tunnel run.
type Options struct {
	EventsAddr string // jprq event server, host:port (e.g. event.tulki.uz:4321)
	Domain     string // base domain, used to build the public URL for display
	AuthToken  string
	Protocol   string // "http" | "tcp"
	LocalPort  int
	Subdomain  string // optional custom subdomain (http)
	CName      string // optional CNAME (http)
	PublicPort uint16 // optional fixed public port (tcp)
	Version    string
}

// Handlers receive progress. Any may be nil.
type Handlers struct {
	OnStatus func(string) // "connecting", "online", "offline"
	OnURL    func(string) // the public URL / address once the tunnel opens
	OnLog    func(string) // human-readable log lines
}

type stream struct {
	mu       sync.Mutex
	localCon net.Conn
	pending  [][]byte
	closed   bool
}

// Client is one tunnel. Not safe for concurrent Run.
type Client struct {
	opts     Options
	h        Handlers
	framed   *events.FramedConn
	eventCon net.Conn

	streamMu sync.Mutex
	streams  map[uint32]*stream
}

func New(opts Options, h Handlers) *Client {
	return &Client{opts: opts, h: h, streams: make(map[uint32]*stream)}
}

func (c *Client) logf(format string, a ...any) {
	if c.h.OnLog != nil {
		c.h.OnLog(fmt.Sprintf(format, a...))
	}
}

func (c *Client) status(s string) {
	if c.h.OnStatus != nil {
		c.h.OnStatus(s)
	}
}

// Run opens the tunnel and pumps traffic until ctx is cancelled, the event
// channel drops, or a fatal error occurs. It returns nil on a clean shutdown
// (ctx cancelled / EOF) and an error otherwise.
func (c *Client) Run(ctx context.Context) error {
	c.status("connecting")

	eventCon, err := net.Dial("tcp", c.opts.EventsAddr)
	if err != nil {
		c.status("offline")
		return fmt.Errorf("connect to event server: %w", err)
	}
	c.eventCon = eventCon
	if tc, ok := eventCon.(*net.TCPConn); ok {
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(15 * time.Second)
	}
	c.framed = events.NewFramedConn(eventCon)

	// Cancel/close path: when ctx is done, drop the conn so Recv unblocks.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = eventCon.Close()
		case <-stop:
		}
	}()

	if err := c.framed.SendControl(events.MsgTunnelRequested, &events.TunnelRequested{
		Protocol:   c.opts.Protocol,
		Subdomain:  c.opts.Subdomain,
		CanonName:  c.opts.CName,
		AuthToken:  c.opts.AuthToken,
		CliVersion: c.opts.Version,
		PublicPort: c.opts.PublicPort,
	}); err != nil {
		c.status("offline")
		return fmt.Errorf("send tunnel request: %w", err)
	}

	var openMsg events.Message
	if err := c.framed.Recv(&openMsg); err != nil {
		c.status("offline")
		return fmt.Errorf("receive tunnel info: %w", err)
	}
	if openMsg.Type != events.MsgTunnelOpened {
		c.status("offline")
		return fmt.Errorf("unexpected first message type %d", openMsg.Type)
	}
	opened, err := events.DecodeTunnelOpened(&openMsg)
	if err != nil {
		c.status("offline")
		return fmt.Errorf("decode tunnel info: %w", err)
	}
	if opened.ErrorMessage != "" {
		c.status("offline")
		return errors.New(opened.ErrorMessage)
	}

	public := fmt.Sprintf("%s:%d", opened.Hostname, opened.PublicServer)
	if c.opts.Protocol == "http" {
		public = "https://" + opened.Hostname
	}
	if c.h.OnURL != nil {
		c.h.OnURL(public)
	}
	c.status("online")
	c.logf("tunnel online: %s -> localhost:%d", strings.TrimSuffix(public, ":80"), c.opts.LocalPort)

	for {
		var m events.Message
		if err := c.framed.Recv(&m); err != nil {
			c.status("offline")
			if ctx.Err() != nil {
				return nil // intentional stop
			}
			c.logf("event channel closed: %s", err)
			return nil
		}
		switch m.Type {
		case events.MsgConnectionOpened:
			c.openStream(m.StreamID)
		case events.MsgConnectionData:
			c.deliverData(m.StreamID, m.Payload)
		case events.MsgConnectionClose:
			c.closeStream(m.StreamID)
		case events.MsgConnectionLimit:
			c.logf("connection limit reached")
		}
	}
}

// Close stops a running tunnel; Run then returns nil.
func (c *Client) Close() {
	if c.eventCon != nil {
		_ = c.eventCon.Close()
	}
}

func (c *Client) localAddr() string {
	return fmt.Sprintf("localhost:%d", c.opts.LocalPort)
}

func (c *Client) openStream(streamID uint32) {
	s := &stream{}
	c.streamMu.Lock()
	c.streams[streamID] = s
	c.streamMu.Unlock()

	go func() {
		localCon, err := net.Dial("tcp", c.localAddr())
		if err != nil {
			c.logf("connect to local server: %s", err)
			c.closeStream(streamID)
			return
		}

		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = localCon.Close()
			return
		}
		s.localCon = localCon
		pending := s.pending
		s.pending = nil
		s.mu.Unlock()

		for _, p := range pending {
			if _, werr := localCon.Write(p); werr != nil {
				c.closeStream(streamID)
				return
			}
		}

		// Idle reads (quiet WebSockets) must NOT close the stream; keepalive
		// reaps a silently-dead local server.
		if tc, ok := localCon.(*net.TCPConn); ok {
			_ = tc.SetKeepAlive(true)
			_ = tc.SetKeepAlivePeriod(30 * time.Second)
		}
		buf := make([]byte, events.MaxPayloadLen)
		for {
			_ = localCon.SetReadDeadline(time.Now().Add(30 * time.Second))
			n, rerr := localCon.Read(buf)
			if n > 0 {
				if werr := c.framed.Send(&events.Message{
					Type:     events.MsgConnectionData,
					StreamID: streamID,
					Payload:  append([]byte{}, buf[:n]...),
				}); werr != nil {
					break
				}
			}
			if rerr != nil {
				if errors.Is(rerr, os.ErrDeadlineExceeded) {
					continue
				}
				break
			}
		}
		_ = c.framed.Send(&events.Message{Type: events.MsgConnectionClose, StreamID: streamID})
		c.closeStream(streamID)
	}()
}

func (c *Client) deliverData(streamID uint32, data []byte) {
	c.streamMu.Lock()
	s, ok := c.streams[streamID]
	c.streamMu.Unlock()
	if !ok {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	if s.localCon == nil {
		s.pending = append(s.pending, append([]byte{}, data...))
		s.mu.Unlock()
		return
	}
	con := s.localCon
	s.mu.Unlock()
	if _, err := con.Write(data); err != nil {
		c.closeStream(streamID)
	}
}

func (c *Client) closeStream(streamID uint32) {
	c.streamMu.Lock()
	s, ok := c.streams[streamID]
	if ok {
		delete(c.streams, streamID)
	}
	c.streamMu.Unlock()
	if !ok {
		return
	}
	s.mu.Lock()
	s.closed = true
	con := s.localCon
	s.localCon = nil
	s.pending = nil
	s.mu.Unlock()
	if con != nil {
		_ = con.Close()
	}
}
