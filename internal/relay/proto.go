// Package relay carries traffic between nodes that cannot dial each other.
//
// A leaf never listens. It keeps ONE outbound session to each relay it is
// allowed to use, and the relay opens streams back down that same session --
// so leaf-to-leaf works without giving any leaf a public port.
//
// The session itself rides inside the Reality tunnel sing-box already
// provides: the leaf dials the relay's mesh address, sing-box wraps it in
// Reality, and the relay's sing-box hands the plaintext to us. That means
// nothing here has to know about TLS.
package relay

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Frame types. The wire format is deliberately tiny: this sits inside yamux,
// which already handles framing, ordering and flow control.
const (
	// hello is the first message on a new session: the leaf identifying itself.
	msgHello = 1
	// open is the first message on a new stream: where to connect.
	msgOpen = 2
	// ok / fail answer an open.
	msgOK   = 3
	// failure carries a reason so the caller can log something useful instead
	// of a bare connection reset.
	msgFail = 4
)

const maxPayload = 4096

// Hello identifies a node when it establishes a session.
type Hello struct {
	NodeID string
	Key    string // the node's head-issued credential; the relay verifies it
}

// Open asks the far side to connect somewhere.
//
// DstNode lets a relay route without parsing addresses: it looks up the
// session by node ID and forwards. DstAddr is what the final hop dials.
type Open struct {
	DstNode string
	DstAddr string // host:port, already resolved to a mesh address
}

func writeMsg(w io.Writer, typ byte, fields ...string) error {
	buf := []byte{typ}
	for _, f := range fields {
		if len(f) > 65535 {
			return fmt.Errorf("relay: field too long")
		}
		buf = binary.BigEndian.AppendUint16(buf, uint16(len(f)))
		buf = append(buf, f...)
	}
	if len(buf) > maxPayload {
		return fmt.Errorf("relay: message too long")
	}
	var hdr [2]byte
	binary.BigEndian.PutUint16(hdr[:], uint16(len(buf)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(buf)
	return err
}

func readMsg(r io.Reader, want int) (byte, []string, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint16(hdr[:])
	if n == 0 || int(n) > maxPayload {
		return 0, nil, fmt.Errorf("relay: bad message length %d", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, nil, err
	}
	typ := buf[0]
	rest := buf[1:]
	fields := make([]string, 0, want)
	for len(rest) > 0 {
		if len(rest) < 2 {
			return 0, nil, fmt.Errorf("relay: truncated field")
		}
		l := int(binary.BigEndian.Uint16(rest))
		rest = rest[2:]
		if len(rest) < l {
			return 0, nil, fmt.Errorf("relay: truncated field body")
		}
		fields = append(fields, string(rest[:l]))
		rest = rest[l:]
	}
	return typ, fields, nil
}

func WriteHello(w io.Writer, h Hello) error {
	return writeMsg(w, msgHello, h.NodeID, h.Key)
}

func ReadHello(r io.Reader) (Hello, error) {
	typ, f, err := readMsg(r, 2)
	if err != nil {
		return Hello{}, err
	}
	if typ != msgHello || len(f) != 2 {
		return Hello{}, fmt.Errorf("relay: expected hello, got type %d", typ)
	}
	return Hello{NodeID: f[0], Key: f[1]}, nil
}

func WriteOpen(w io.Writer, o Open) error {
	return writeMsg(w, msgOpen, o.DstNode, o.DstAddr)
}

func ReadOpen(r io.Reader) (Open, error) {
	typ, f, err := readMsg(r, 2)
	if err != nil {
		return Open{}, err
	}
	if typ != msgOpen || len(f) != 2 {
		return Open{}, fmt.Errorf("relay: expected open, got type %d", typ)
	}
	return Open{DstNode: f[0], DstAddr: f[1]}, nil
}

// WriteResult answers an Open. Sending a reason on failure is what makes a
// dead peer distinguishable from a refused port at the far end.
func WriteResult(w io.Writer, err error) error {
	if err == nil {
		return writeMsg(w, msgOK)
	}
	return writeMsg(w, msgFail, err.Error())
}

func ReadResult(r io.Reader) error {
	typ, f, err := readMsg(r, 1)
	if err != nil {
		return err
	}
	switch typ {
	case msgOK:
		return nil
	case msgFail:
		if len(f) > 0 {
			return fmt.Errorf("relay: remote refused: %s", f[0])
		}
		return fmt.Errorf("relay: remote refused")
	default:
		return fmt.Errorf("relay: unexpected result type %d", typ)
	}
}
