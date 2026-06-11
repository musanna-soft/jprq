package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/azimjohn/jprq/cli/debugger"
	"github.com/azimjohn/jprq/server/events"
)

// stream tracks a single multiplexed public connection in flight.
// Data frames may arrive before the local dial completes — they're held
// in `pending` until the goroutine attaches the local conn.
type stream struct {
	mu       sync.Mutex
	localCon net.Conn
	pending  [][]byte
	closed   bool
}

type jprqClient struct {
	config       Config
	protocol     string
	subdomain    string
	cname        string
	localServer  string
	publicServer string
	httpDebugger debugger.Debugger

	framed   *events.FramedConn
	streamMu sync.Mutex
	streams  map[uint32]*stream
}

func (j *jprqClient) Start(port int, debug bool) {
	eventCon, err := net.Dial("tcp", j.config.Remote.Events)
	if err != nil {
		log.Fatalf("failed to connect to event server: %s\n", err)
	}
	defer eventCon.Close()
	// Stateful NATs on the client side drop idle TCP after as little as 10s,
	// closing the multiplex channel and surfacing as "tunnel-closed" loops.
	// TCP keepalive prods the conn so the NAT entry stays warm.
	if tc, ok := eventCon.(*net.TCPConn); ok {
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(15 * time.Second)
	}
	j.framed = events.NewFramedConn(eventCon)
	j.streams = make(map[uint32]*stream)

	if err := j.framed.SendControl(events.MsgTunnelRequested, &events.TunnelRequested{
		Protocol:   j.protocol,
		Subdomain:  j.subdomain,
		CanonName:  j.cname,
		AuthToken:  j.config.Local.AuthToken,
		CliVersion: version,
	}); err != nil {
		log.Fatalf("failed to send request: %s\n", err)
	}

	var openMsg events.Message
	if err := j.framed.Recv(&openMsg); err != nil {
		log.Fatalf("failed to receive tunnel info: %s\n", err)
	}
	if openMsg.Type != events.MsgTunnelOpened {
		log.Fatalf("unexpected first message type %d", openMsg.Type)
	}
	opened, err := events.DecodeTunnelOpened(&openMsg)
	if err != nil {
		log.Fatalf("failed to decode tunnel info: %s\n", err)
	}
	if opened.ErrorMessage != "" {
		log.Fatalf(opened.ErrorMessage)
	}

	j.localServer = fmt.Sprintf("localhost:%d", port)
	j.publicServer = fmt.Sprintf("%s:%d", opened.Hostname, opened.PublicServer)
	if j.protocol == "http" {
		j.publicServer = fmt.Sprintf("https://%s", opened.Hostname)
	}

	fmt.Printf("Status: \t Online \n")
	fmt.Printf("Protocol: \t %s \n", strings.ToUpper(j.protocol))
	fmt.Printf("Forwarded: \t %s -> %s \n", strings.TrimSuffix(j.publicServer, ":80"), j.localServer)

	if j.protocol == "http" && debug {
		j.httpDebugger = debugger.New()
		if dport, err := j.httpDebugger.Run(0); err == nil {
			fmt.Printf("Http Debugger: \t http://127.0.0.1:%d \n", dport)
		}
	}

	for {
		var m events.Message
		if err := j.framed.Recv(&m); err != nil {
			if err == io.EOF {
				return
			}
			log.Printf("event channel closed: %s\n", err)
			return
		}
		switch m.Type {
		case events.MsgConnectionOpened:
			j.openStream(m.StreamID)
		case events.MsgConnectionData:
			j.deliverData(m.StreamID, m.Payload)
		case events.MsgConnectionClose:
			j.closeStream(m.StreamID)
		case events.MsgConnectionLimit:
			// ignore for now
		}
	}
}

// openStream allocates the stream synchronously (so subsequent data frames
// always find it) and dials the local server in the background, draining
// any pending payloads as soon as the local connection is ready.
func (j *jprqClient) openStream(streamID uint32) {
	s := &stream{}
	j.streamMu.Lock()
	j.streams[streamID] = s
	j.streamMu.Unlock()

	go func() {
		localCon, err := net.Dial("tcp", j.localServer)
		if err != nil {
			log.Printf("failed to connect to local server: %s\n", err)
			j.closeStream(streamID)
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

		// Replay anything that arrived before the local dial finished.
		for _, p := range pending {
			if _, werr := localCon.Write(p); werr != nil {
				j.closeStream(streamID)
				return
			}
		}

		// Pump local → event channel as MsgConnectionData frames.
		buf := make([]byte, events.MaxPayloadLen)
		for {
			_ = localCon.SetReadDeadline(time.Now().Add(30 * time.Second))
			n, err := localCon.Read(buf)
			if n > 0 {
				if werr := j.framed.Send(&events.Message{
					Type:     events.MsgConnectionData,
					StreamID: streamID,
					Payload:  append([]byte{}, buf[:n]...),
				}); werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		_ = j.framed.Send(&events.Message{Type: events.MsgConnectionClose, StreamID: streamID})
		j.closeStream(streamID)
	}()
}

// deliverData writes incoming bytes to the local connection if it's
// ready, otherwise buffers them until openStream's goroutine attaches.
func (j *jprqClient) deliverData(streamID uint32, data []byte) {
	j.streamMu.Lock()
	s, ok := j.streams[streamID]
	j.streamMu.Unlock()
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
	c := s.localCon
	s.mu.Unlock()
	if _, err := c.Write(data); err != nil {
		j.closeStream(streamID)
	}
}

func (j *jprqClient) closeStream(streamID uint32) {
	j.streamMu.Lock()
	s, ok := j.streams[streamID]
	if ok {
		delete(j.streams, streamID)
	}
	j.streamMu.Unlock()
	if !ok {
		return
	}
	s.mu.Lock()
	s.closed = true
	c := s.localCon
	s.localCon = nil
	s.pending = nil
	s.mu.Unlock()
	if c != nil {
		_ = c.Close()
	}
}
