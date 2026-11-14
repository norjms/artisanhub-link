// SPDX-License-Identifier: AGPL-3.0-only

// Package pairing makes and checks the one-time codes that pair a site with a hub.
package pairing

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"strings"
	"sync"
	"time"
)

// Letters a person can read back without mixing them up (no 0/O, 1/I/L, 5/S, 8/B).
const alphabet = "ACDEFGHJKMNPQRTUVWXYZ234679"

// TTL is how long a code works.
const TTL = 30 * time.Minute

// Normalize upper-cases a typed code and drops spaces and dashes.
func Normalize(code string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(code) {
		if r != '-' && r != ' ' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Format shows a code in groups of four: KX7P-29QD-MV4A.
func Format(code string) string {
	code = Normalize(code)
	var parts []string
	for len(code) > 4 {
		parts = append(parts, code[:4])
		code = code[4:]
	}
	return strings.Join(append(parts, code), "-")
}

type entry struct {
	hash    [32]byte
	name    string
	expires time.Time
}

// Codes holds the open codes, by a hash only.
type Codes struct {
	mu   sync.Mutex
	open []entry
	now  func() time.Time
}

// New makes an empty set of codes.
func New() *Codes { return &Codes{now: time.Now} }

// Make returns a new code (12 characters, about 57 bits) for a site to be named name.
func (c *Codes) Make(name string) (string, time.Time) {
	// Rejection sampling, so every letter is equally likely.
	limit := 256 - 256%len(alphabet)
	b := make([]byte, 0, 12)
	one := make([]byte, 1)
	for len(b) < 12 {
		if _, err := rand.Read(one); err != nil {
			panic(err)
		}
		if int(one[0]) < limit {
			b = append(b, alphabet[int(one[0])%len(alphabet)])
		}
	}
	code := string(b)
	exp := c.now().Add(TTL)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prune()
	c.open = append(c.open, entry{hash: sha256.Sum256([]byte(code)), name: name, expires: exp})
	return Format(code), exp
}

// Use checks a code and spends it: it works once. It returns the site name given
// when the code was made.
func (c *Codes) Use(code string) (string, bool) {
	h := sha256.Sum256([]byte(Normalize(code)))
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prune()
	for i, e := range c.open {
		if subtle.ConstantTimeCompare(e.hash[:], h[:]) == 1 {
			c.open = append(c.open[:i], c.open[i+1:]...)
			return e.name, true
		}
	}
	return "", false
}

// Count is how many codes are still open.
func (c *Codes) Count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prune()
	return len(c.open)
}

func (c *Codes) prune() {
	now := c.now()
	kept := c.open[:0]
	for _, e := range c.open {
		if now.Before(e.expires) {
			kept = append(kept, e)
		}
	}
	c.open = kept
}
