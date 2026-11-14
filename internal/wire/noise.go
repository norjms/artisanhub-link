// SPDX-License-Identifier: AGPL-3.0-only

package wire

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/flynn/noise"
)

// The link runs Noise IK inside the WebSocket: the site starts, knowing the hub's
// key from pairing; the hub learns the site's key in the first message and accepts
// it only if that site is paired. Everything after is encrypted end to end between
// the two, whatever terminates TLS on the way (a reverse proxy, Cloudflare).

const prologue = "artisanhub-link v1"

// maxFrame is Noise's message limit; plaintext per frame leaves room for the tag.
const (
	maxFrame = 65535
	maxPlain = maxFrame - 16
)

func writeFrame(w io.Writer, b []byte) error {
	if len(b) > maxFrame {
		return errors.New("frame too large")
	}
	buf := make([]byte, 2+len(b))
	binary.BigEndian.PutUint16(buf, uint16(len(b)))
	copy(buf[2:], b)
	_, err := w.Write(buf)
	return err
}

func readFrame(r io.Reader) ([]byte, error) {
	var n [2]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return nil, err
	}
	b := make([]byte, binary.BigEndian.Uint16(n[:]))
	_, err := io.ReadFull(r, b)
	return b, err
}

// SiteHandshake runs at the site: it proves the site's key and checks the hub's.
func SiteHandshake(c net.Conn, static noise.DHKey, hubPub []byte) (net.Conn, error) {
	hs, err := noise.NewHandshakeState(noise.Config{
		CipherSuite: suite, Pattern: noise.HandshakeIK, Initiator: true, Prologue: []byte(prologue),
		StaticKeypair: static, PeerStatic: hubPub,
	})
	if err != nil {
		return nil, err
	}
	_ = c.SetDeadline(time.Now().Add(15 * time.Second))
	defer c.SetDeadline(time.Time{})
	msg, _, _, err := hs.WriteMessage(nil, nil)
	if err != nil {
		return nil, err
	}
	if err := writeFrame(c, msg); err != nil {
		return nil, err
	}
	reply, err := readFrame(c)
	if err != nil {
		return nil, fmt.Errorf("the hub closed the link during the handshake (is this site still paired?): %w", err)
	}
	_, send, recv, err := hs.ReadMessage(nil, reply)
	if err != nil {
		return nil, fmt.Errorf("the hub did not prove its key: %w", err)
	}
	return &secureConn{Conn: c, send: send, recv: recv}, nil
}

// HubHandshake runs on the hub. allowed reports whether the site key is paired; the
// site's key is returned.
func HubHandshake(c net.Conn, static noise.DHKey, allowed func(sitePub []byte) bool) (net.Conn, []byte, error) {
	hs, err := noise.NewHandshakeState(noise.Config{
		CipherSuite: suite, Pattern: noise.HandshakeIK, Initiator: false, Prologue: []byte(prologue),
		StaticKeypair: static,
	})
	if err != nil {
		return nil, nil, err
	}
	_ = c.SetDeadline(time.Now().Add(15 * time.Second))
	defer c.SetDeadline(time.Time{})
	msg, err := readFrame(c)
	if err != nil {
		return nil, nil, err
	}
	if _, _, _, err := hs.ReadMessage(nil, msg); err != nil {
		return nil, nil, fmt.Errorf("site handshake: %w", err)
	}
	sitePub := append([]byte{}, hs.PeerStatic()...)
	if !allowed(sitePub) {
		return nil, sitePub, errors.New("site key is not paired")
	}
	reply, recv, send, err := hs.WriteMessage(nil, nil)
	if err != nil {
		return nil, nil, err
	}
	if err := writeFrame(c, reply); err != nil {
		return nil, nil, err
	}
	return &secureConn{Conn: c, send: send, recv: recv}, sitePub, nil
}

// secureConn encrypts every write as Noise transport messages and decrypts reads.
type secureConn struct {
	net.Conn
	wmu, rmu   sync.Mutex
	send, recv *noise.CipherState
	pending    bytes.Buffer
}

func (s *secureConn) Write(p []byte) (int, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	total := 0
	for len(p) > 0 {
		n := len(p)
		if n > maxPlain {
			n = maxPlain
		}
		ct, err := s.send.Encrypt(nil, nil, p[:n])
		if err != nil {
			return total, err
		}
		if err := writeFrame(s.Conn, ct); err != nil {
			return total, err
		}
		total += n
		p = p[n:]
	}
	return total, nil
}

func (s *secureConn) Read(p []byte) (int, error) {
	s.rmu.Lock()
	defer s.rmu.Unlock()
	for s.pending.Len() == 0 {
		ct, err := readFrame(s.Conn)
		if err != nil {
			return 0, err
		}
		pt, err := s.recv.Decrypt(nil, nil, ct)
		if err != nil {
			return 0, fmt.Errorf("link message failed to decrypt: %w", err)
		}
		s.pending.Write(pt)
	}
	return s.pending.Read(p)
}
