// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/flynn/noise"
	"github.com/norjms/openprinthq-link/internal/wire"
)

// pairing is what the site keeps once paired: the hub, the hub's key to pin, and who it is there.
type pairing struct {
	Hub         string    `json:"hub"`
	HubKey      string    `json:"hub_public_key"`
	SiteID      string    `json:"site_id"`
	Fingerprint string    `json:"fingerprint"`
	PairedAt    time.Time `json:"paired_at"`
}

// hubBase turns what a person typed (hub.example.com, https://hub.example.com/) into https://host.
func hubBase(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("no hub address")
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", fmt.Errorf("%q is not a hub address", s)
	}
	return u.Scheme + "://" + u.Host, nil
}

func linkAddress(hub string) (string, error) {
	base, err := hubBase(hub)
	if err != nil {
		return "", err
	}
	return "ws" + strings.TrimPrefix(base, "http") + "/link", nil
}

// ensurePaired returns the pairing kept in the data folder, or pairs with the code given. A new code for
// another hub pairs again.
func ensurePaired(ctx context.Context, st settings, key noise.DHKey) (*pairing, error) {
	path := filepath.Join(st.dataDir, "site.json")
	var kept pairing
	if b, err := os.ReadFile(path); err == nil && json.Unmarshal(b, &kept) == nil && kept.HubKey != "" {
		base, _ := hubBase(st.hub)
		if st.hub == "" || base == kept.Hub || st.code == "" {
			return &kept, nil
		}
	}
	if st.hub == "" || st.code == "" {
		return nil, errors.New("not paired yet: give the hub's address and the pairing code it shows (LINK_HUB, LINK_PAIR_CODE)")
	}
	base, err := hubBase(st.hub)
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(wire.PairRequest{Code: st.code, Name: st.name, PublicKey: wire.PubString(key.Public), Version: version})
	rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(rctx, http.MethodPost, base+"/link/pair", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach the hub at %s: %w", base, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return nil, fmt.Errorf("the hub refused pairing: %s", e.Error)
	}
	var pr wire.PairResponse
	if err := json.Unmarshal(raw, &pr); err != nil {
		return nil, fmt.Errorf("the hub's answer was not understood: %w", err)
	}
	hubPub, err := wire.ParsePub(pr.HubKey)
	if err != nil {
		return nil, err
	}
	// Both sides work the fingerprint out from the two keys; the site checks the hub's own sum matches.
	if fp := wire.Fingerprint(hubPub, key.Public); fp != pr.Fingerprint {
		return nil, fmt.Errorf("fingerprints differ (%s here, %s at the hub); not paired", fp, pr.Fingerprint)
	}
	p := pairing{Hub: base, HubKey: pr.HubKey, SiteID: pr.SiteID, Fingerprint: pr.Fingerprint, PairedAt: time.Now().UTC()}
	out, _ := json.MarshalIndent(p, "", "  ")
	if err := os.MkdirAll(st.dataDir, 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, append(out, '\n'), 0o600); err != nil {
		return nil, err
	}
	return &p, nil
}
