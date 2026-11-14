// SPDX-License-Identifier: AGPL-3.0-only

package wire

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/flynn/noise"
)

// Keys are Curve25519 static keys, the ones the Noise handshake proves. Each side
// keeps its private key in a file only it can read; the other side's public key is
// pinned at pairing.

var suite = noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashBLAKE2s)

// LoadOrCreateKey reads a private key (base64, 32 bytes) or makes one, readable by
// the owner only.
func LoadOrCreateKey(path string) (noise.DHKey, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		priv, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
		if err != nil || len(priv) != 32 {
			return noise.DHKey{}, fmt.Errorf("bad key file %s", path)
		}
		return keyFromPrivate(priv)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return noise.DHKey{}, err
	}
	k, err := suite.GenerateKeypair(rand.Reader)
	if err != nil {
		return noise.DHKey{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return noise.DHKey{}, err
	}
	enc := base64.StdEncoding.EncodeToString(k.Private)
	if err := os.WriteFile(path, []byte(enc+"\n"), 0o600); err != nil {
		return noise.DHKey{}, err
	}
	return k, nil
}

func keyFromPrivate(priv []byte) (noise.DHKey, error) {
	pub, err := noise.DH25519.DH(priv, basePoint[:])
	if err != nil {
		return noise.DHKey{}, err
	}
	return noise.DHKey{Private: append([]byte{}, priv...), Public: pub}, nil
}

// basePoint is the Curve25519 generator: DH with it gives the public key.
var basePoint = [32]byte{9}

// PubString is the printable form of a public key.
func PubString(pub []byte) string { return base64.StdEncoding.EncodeToString(pub) }

// ParsePub reads the printable form back.
func ParsePub(s string) ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != 32 {
		return nil, fmt.Errorf("bad public key")
	}
	return b, nil
}

// Fingerprint is what both sides show after pairing, so a careful owner can compare
// them: four groups from a hash of the two keys, hub first.
func Fingerprint(hubPub, sitePub []byte) string {
	h := sha256.Sum256(append(append([]byte("artisanhub-link fingerprint"), hubPub...), sitePub...))
	s := strings.ToUpper(hex.EncodeToString(h[:8]))
	return s[0:4] + "-" + s[4:8] + "-" + s[8:12] + "-" + s[12:16]
}
