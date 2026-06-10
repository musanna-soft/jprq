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
	streams  map[uint32]net.Conn
}

func (j *jprqClient) Start(port int, debug bool) {
	eventCon, err := net.Dial("tcp", j.config.Remote.Events)
	if err != nil {
		log.Fatalf("failed to connect to event server: %s\n", err)
	}
	defer eventCon.Close()
	j.framed = events.NewFramedConn(eventCon)
	j.streams = make(map[uint32]net.Conn)

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

	// Multiplexed event loop: dispatch incoming MsgConnectionOpened / Data /
	// Close frames to local connections, each routed by stream id.
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
			go j.handleNewStream(m.StreamID)
		case events.MsgConnectionData:
			j.dispatchData(m.StreamID, m.Payload)
		case events.MsgConnectionClose:
			j.closeStream(m.StreamID)
		case events.MsgConnectionLimit:
			// optional — could surface rate-limit info; ignore for now
		}
	}
}

// handleNewStream dials the local server for a new public connection and
// starts pumping bytes from local → event channel as MsgConnectionData.
func (j *jprqClient) handleNewStream(streamID uint32) {
	localCon, err := net.Dial("tcp", j.localServer)
	if err != nil {
		log.Printf("failed to connect to local server: %s\n", err)
		_ = j.framed.Send(&events.Message{Type: events.MsgConnectionClose, StreamID: streamID})
		return
	}

	j.streamMu.Lock()
	j.streams[streamID] = localCon
	j.streamMu.Unlock()

	defer j.closeStream(streamID)

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
				return
			}
		}
		if err != nil {
			break
		}
	}
	_ = j.framed.Send(&events.Message{Type: events.MsgConnectionClose, StreamID: streamID})
}

func (j *jprqClient) dispatchData(streamID uint32, data []byte) {
	j.streamMu.Lock()
	c, ok := j.streams[streamID]
	j.streamMu.Unlock()
	if !ok {
		return
	}
	if _, err := c.Write(data); err != nil {
		j.closeStream(streamID)
	}
}

func (j *jprqClient) closeStream(streamID uint32) {
	j.streamMu.Lock()
	c, ok := j.streams[streamID]
	if ok {
		delete(j.streams, streamID)
	}
	j.streamMu.Unlock()
	if ok && c != nil {
		_ = c.Close()
	}
}
