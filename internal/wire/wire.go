// SPDX-License-Identifier: AGPL-3.0-only

// Package wire holds what both sides share: keys, the pairing handshake,
// and the stream header.
package wire

import (
	"bufio"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"
)

// LoadOrCreateKey reads an ed25519 private key (base64 seed) or makes one.
func LoadOrCreateKey(path string) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
		if err != nil || len(seed) != ed25519.SeedSize {
			return nil, fmt.Errorf("bad key file %s", path)
		}
		return ed25519.NewKeyFromSeed(seed), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	enc := base64.StdEncoding.EncodeToString(priv.Seed())
	if err := os.WriteFile(path, []byte(enc+"\n"), 0o600); err != nil {
		return nil, err
	}
	return priv, nil
}

// PubString is the printable form of a public key.
func PubString(k ed25519.PublicKey) string { return base64.StdEncoding.EncodeToString(k) }

// ParsePub reads the printable form back.
func ParsePub(s string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("bad public key")
	}
	return ed25519.PublicKey(b), nil
}

const (
	labelSite = "artisanhub-link site v0"
	labelHub  = "artisanhub-link hub v0"
)

func nonce() []byte {
	n := make([]byte, 32)
	if _, err := rand.Read(n); err != nil {
		panic(err)
	}
	return n
}

func signed(label string, n []byte) []byte { return append([]byte(label), n...) }

// HubHandshake runs on the hub. allowed reports whether a site key is paired.
// It returns the site's public key.
func HubHandshake(c net.Conn, priv ed25519.PrivateKey, allowed func(ed25519.PublicKey) bool) (ed25519.PublicKey, error) {
	_ = c.SetDeadline(time.Now().Add(15 * time.Second))
	defer c.SetDeadline(time.Time{})
	nh := nonce()
	if _, err := c.Write(append(append([]byte{}, priv.Public().(ed25519.PublicKey)...), nh...)); err != nil {
		return nil, err
	}
	buf := make([]byte, 32+32+64)
	if _, err := io.ReadFull(c, buf); err != nil {
		return nil, err
	}
	sitePub := ed25519.PublicKey(append([]byte{}, buf[:32]...))
	ns, sig := buf[32:64], buf[64:]
	if !allowed(sitePub) {
		return nil, errors.New("site key is not paired")
	}
	if !ed25519.Verify(sitePub, signed(labelSite, nh), sig) {
		return nil, errors.New("site signature is wrong")
	}
	_, err := c.Write(ed25519.Sign(priv, signed(labelHub, ns)))
	return sitePub, err
}

// SiteHandshake runs on the site. hubPub is the key pinned at pairing.
func SiteHandshake(c net.Conn, priv ed25519.PrivateKey, hubPub ed25519.PublicKey) error {
	_ = c.SetDeadline(time.Now().Add(15 * time.Second))
	defer c.SetDeadline(time.Time{})
	buf := make([]byte, 64)
	if _, err := io.ReadFull(c, buf); err != nil {
		return err
	}
	if !ed25519.PublicKey(buf[:32]).Equal(hubPub) {
		return errors.New("hub key does not match the paired hub")
	}
	nh := buf[32:]
	ns := nonce()
	out := append(append(append([]byte{}, priv.Public().(ed25519.PublicKey)...), ns...), ed25519.Sign(priv, signed(labelSite, nh))...)
	if _, err := c.Write(out); err != nil {
		return err
	}
	sig := make([]byte, 64)
	if _, err := io.ReadFull(c, sig); err != nil {
		return err
	}
	if !ed25519.Verify(hubPub, signed(labelHub, ns), sig) {
		return errors.New("hub signature is wrong")
	}
	return nil
}

// Printer is one printer the site found.
type Printer struct {
	IP     string `json:"ip"`
	Family string `json:"family"` // bambu, moonraker, octoprint, prusalink, elegoo
	Name   string `json:"name,omitempty"`
	Model  string `json:"model,omitempty"`
	Serial string `json:"serial,omitempty"`
	Via    string `json:"via"` // ssdp, mdns, static
}

// Open is the first line of every data stream the hub opens.
type Open struct {
	IP   string `json:"ip"`
	Port int    `json:"port"`
}

// Stream status bytes sent by the site after an Open.
const (
	StatusOK     byte = 0
	StatusDenied byte = 1
	StatusFailed byte = 2
)

// Stream kinds, the first byte of a stream.
const (
	KindData    byte = 'D'
	KindPrinter byte = 'P'
)

// WriteJSONLine writes v as one JSON line.
func WriteJSONLine(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// ReadJSONLine reads one JSON line (max 64 KB) into v.
func ReadJSONLine(r *bufio.Reader, v any) error {
	line, err := r.ReadSlice('\n')
	if err != nil {
		return err
	}
	return json.Unmarshal(line, v)
}

// Pipe copies both ways until either side closes.
func Pipe(a, b io.ReadWriteCloser) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(a, b); done <- struct{}{} }()
	go func() { _, _ = io.Copy(b, a); done <- struct{}{} }()
	<-done
	_ = a.Close()
	_ = b.Close()
	<-done
}
