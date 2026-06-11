// Package events defines the wire protocol multiplexed over the single
// TCP event-channel connection between the CLI and the server (port
// JPRQ_EVENT_PORT). Tunnel data — bytes flowing through every public
// connection a tunnel hosts — is framed into MsgConnectionData messages
// keyed by StreamID. This replaces the upstream design where each tunnel
// allocated its own private TCP server on a random ephemeral port (the
// CLI then had to dial that random port for every public connection),
// which doesn't work behind k8s ingress.
//
// FORK PATCH (musanna-soft): wire protocol redesigned for multiplexing.
package events

import (
	"bytes"
	"encoding/binary"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
)

const (
	TCP  string = "tcp"
	HTTP string = "http"
)

type MessageType uint8

const (
	MsgTunnelRequested  MessageType = 1 // CLI → server, gob(TunnelRequested)
	MsgTunnelOpened     MessageType = 2 // server → CLI, gob(TunnelOpened)
	MsgConnectionOpened MessageType = 3 // server → CLI, gob(ConnectionOpened)
	MsgConnectionData   MessageType = 4 // both ways, raw bytes
	MsgConnectionClose  MessageType = 5 // both ways, empty payload
	MsgConnectionLimit  MessageType = 6 // server → CLI, gob(ConnectionLimit)
	MsgPing             MessageType = 7 // both ways, empty payload (keepalive)
)

// Message is the single wire format carried on the event connection.
//
//	1 byte   Type
//	4 bytes  StreamID  (little-endian, 0 for tunnel-level control)
//	4 bytes  PayloadLength (little-endian)
//	N bytes  Payload
type Message struct {
	Type     MessageType
	StreamID uint32
	Payload  []byte
}

// MaxPayloadLen caps a single data frame; larger flows are split.
const MaxPayloadLen = 32 * 1024

func (m *Message) Read(r io.Reader) error {
	header := make([]byte, 9)
	if _, err := io.ReadFull(r, header); err != nil {
		return err
	}
	m.Type = MessageType(header[0])
	m.StreamID = binary.LittleEndian.Uint32(header[1:5])
	length := binary.LittleEndian.Uint32(header[5:9])
	if length > 64*1024*1024 {
		return fmt.Errorf("frame too large: %d bytes", length)
	}
	if length > 0 {
		m.Payload = make([]byte, length)
		if _, err := io.ReadFull(r, m.Payload); err != nil {
			return err
		}
	} else {
		m.Payload = nil
	}
	return nil
}

func (m *Message) Write(w io.Writer) error {
	header := make([]byte, 9)
	header[0] = byte(m.Type)
	binary.LittleEndian.PutUint32(header[1:5], m.StreamID)
	binary.LittleEndian.PutUint32(header[5:9], uint32(len(m.Payload)))
	if _, err := w.Write(header); err != nil {
		return err
	}
	if len(m.Payload) > 0 {
		if _, err := w.Write(m.Payload); err != nil {
			return err
		}
	}
	return nil
}

// FramedConn serialises writes so multiple goroutines (one per multiplexed
// public/local connection) can share the underlying event connection safely.
type FramedConn struct {
	c  net.Conn
	mu sync.Mutex
}

func NewFramedConn(c net.Conn) *FramedConn { return &FramedConn{c: c} }

func (f *FramedConn) Send(m *Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return m.Write(f.c)
}

// Recv reads the next Message; caller must serialise its invocations.
func (f *FramedConn) Recv(m *Message) error { return m.Read(f.c) }

func (f *FramedConn) Close() error  { return f.c.Close() }
func (f *FramedConn) Raw() net.Conn { return f.c }

// SendControl writes a tunnel-level (StreamID=0) gob-encoded control message.
func (f *FramedConn) SendControl(t MessageType, payload any) error {
	data, err := encodeGob(payload)
	if err != nil {
		return err
	}
	return f.Send(&Message{Type: t, StreamID: 0, Payload: data})
}

// ─── Gob-encoded payloads for control messages ────────────────────────────

type TunnelRequested struct {
	Protocol   string
	Subdomain  string
	CanonName  string
	AuthToken  string
	CliVersion string
	// PublicPort lets a TCP tunnel ask for a specific public port (e.g.
	// 33042 → ssh.tulki.uz:33042 every time) instead of a random one. 0
	// means "auto-assign". Ignored for HTTP tunnels.
	PublicPort uint16
}

type TunnelOpened struct {
	Hostname     string
	Protocol     string
	PublicServer uint16
	ErrorMessage string
}

type ConnectionOpened struct {
	ClientIP   net.IP
	ClientPort uint16
}

type ConnectionLimit struct {
	ClientIP net.IP
}

func encodeGob(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func decodeGob(data []byte, v any) error {
	return gob.NewDecoder(bytes.NewReader(data)).Decode(v)
}

// WriteError surfaces a tunnel-setup failure to the CLI as a
// MsgTunnelOpened with ErrorMessage set.
func WriteError(f *FramedConn, format string, args ...string) error {
	anyArgs := make([]any, len(args))
	for i, a := range args {
		anyArgs[i] = a
	}
	msg := fmt.Sprintf(format, anyArgs...)
	_ = f.SendControl(MsgTunnelOpened, &TunnelOpened{ErrorMessage: msg})
	return errors.New(msg)
}

func DecodeTunnelRequested(m *Message) (*TunnelRequested, error) {
	if m.Type != MsgTunnelRequested {
		return nil, fmt.Errorf("expected MsgTunnelRequested, got %d", m.Type)
	}
	var v TunnelRequested
	if err := decodeGob(m.Payload, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

func DecodeTunnelOpened(m *Message) (*TunnelOpened, error) {
	var v TunnelOpened
	if err := decodeGob(m.Payload, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

func DecodeConnectionOpened(m *Message) (*ConnectionOpened, error) {
	var v ConnectionOpened
	if err := decodeGob(m.Payload, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// EncodeConnectionOpened gob-encodes a ConnectionOpened payload for use
// inside a MsgConnectionOpened message keyed by a real StreamID.
func EncodeConnectionOpened(v *ConnectionOpened) ([]byte, error) {
	return encodeGob(v)
}
