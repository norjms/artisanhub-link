// SPDX-License-Identifier: AGPL-3.0-only

package pairing

import (
	"strings"
	"testing"
	"time"
)

func TestACodeWorksOnceAndIsFormattedInGroups(t *testing.T) {
	c := New()
	code, _ := c.Make("Home")
	if len(code) != 14 || strings.Count(code, "-") != 2 {
		t.Fatalf("code %q is not three groups of four", code)
	}
	name, ok := c.Use(strings.ToLower(strings.ReplaceAll(code, "-", " ")))
	if !ok || name != "Home" {
		t.Fatalf("typed in lower case with spaces, the code should work: %v %q", ok, name)
	}
	if _, ok := c.Use(code); ok {
		t.Fatal("a code must not work twice")
	}
}

func TestACodeExpires(t *testing.T) {
	now := time.Now()
	c := New()
	c.now = func() time.Time { return now }
	code, exp := c.Make("Shop")
	if !exp.Equal(now.Add(TTL)) {
		t.Fatalf("expiry %v", exp)
	}
	c.now = func() time.Time { return now.Add(TTL + time.Second) }
	if _, ok := c.Use(code); ok {
		t.Fatal("an expired code must not work")
	}
	if c.Count() != 0 {
		t.Fatal("expired codes are dropped")
	}
}

func TestAWrongCodeFails(t *testing.T) {
	c := New()
	c.Make("Home")
	if _, ok := c.Use("AAAA-AAAA-AAAA"); ok {
		t.Fatal("a code nobody made must not work")
	}
	if c.Count() != 1 {
		t.Fatal("a wrong code spends nothing")
	}
}
