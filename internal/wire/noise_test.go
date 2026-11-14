package wire

import (
	"bytes"
	"io"
	"net"
	"path/filepath"
	"testing"

	"github.com/flynn/noise"
)

func newKey(t *testing.T) noise.DHKey {
	t.Helper()
	k, err := LoadOrCreateKey(filepath.Join(t.TempDir(), "k"))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestKeyFileIsStable(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "site.key")
	a, err := LoadOrCreateKey(p)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadOrCreateKey(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Public, b.Public) || !bytes.Equal(a.Private, b.Private) {
		t.Fatal("reloading the key gave a different key")
	}
	pub, err := ParsePub(PubString(a.Public))
	if err != nil || !bytes.Equal(pub, a.Public) {
		t.Fatalf("public key did not round trip: %v", err)
	}
	if _, err := ParsePub("c2hvcnQ="); err == nil {
		t.Fatal("a short key was accepted")
	}
}

func TestFingerprintDependsOnRoles(t *testing.T) {
	a, b := newKey(t), newKey(t)
	if Fingerprint(a.Public, b.Public) != Fingerprint(a.Public, b.Public) {
		t.Fatal("fingerprint is not deterministic")
	}
	if Fingerprint(a.Public, b.Public) == Fingerprint(b.Public, a.Public) {
		t.Fatal("hub and site roles should not be interchangeable")
	}
}

type result struct {
	c   net.Conn
	pub []byte
	err error
}

func handshake(t *testing.T, hub, site noise.DHKey, pinned []byte, allow func([]byte) bool) (net.Conn, net.Conn, error, error) {
	t.Helper()
	a, b := net.Pipe()
	ch := make(chan result, 1)
	go func() {
		c, pub, err := HubHandshake(a, hub, allow)
		if err != nil {
			a.Close()
		}
		ch <- result{c, pub, err}
	}()
	sc, serr := SiteHandshake(b, site, pinned)
	if serr != nil {
		b.Close()
	}
	r := <-ch
	return r.c, sc, r.err, serr
}

func TestNoiseRoundTrip(t *testing.T) {
	hub, site := newKey(t), newKey(t)
	var seen []byte
	hc, sc, herr, serr := handshake(t, hub, site, hub.Public, func(p []byte) bool { seen = p; return true })
	if herr != nil || serr != nil {
		t.Fatalf("handshake failed: hub %v, site %v", herr, serr)
	}
	if !bytes.Equal(seen, site.Public) {
		t.Fatal("hub did not learn the site key")
	}
	msg := bytes.Repeat([]byte("printer data "), 20000) // larger than one frame
	go func() { _, _ = sc.Write(msg); sc.Close() }()
	got, err := io.ReadAll(hc)
	if err != nil && err != io.EOF && err != io.ErrClosedPipe {
		t.Fatal(err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatalf("got %d bytes, want %d", len(got), len(msg))
	}
}

func TestUnknownSiteIsRefused(t *testing.T) {
	hub, site := newKey(t), newKey(t)
	_, _, herr, _ := handshake(t, hub, site, hub.Public, func([]byte) bool { return false })
	if herr == nil {
		t.Fatal("hub accepted a site it does not know")
	}
}

func TestWrongHubKeyFails(t *testing.T) {
	hub, site, other := newKey(t), newKey(t), newKey(t)
	_, _, herr, serr := handshake(t, hub, site, other.Public, func([]byte) bool { return true })
	if herr == nil && serr == nil {
		t.Fatal("a site pinned to another hub key connected")
	}
}
