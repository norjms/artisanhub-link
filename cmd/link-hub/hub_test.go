package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"testing"

	"github.com/norjms/artisanhub-link/internal/wire"
)

func testHub(t *testing.T, dir string) *hub {
	t.Helper()
	k, err := wire.LoadOrCreateKey(filepath.Join(dir, "hub.key"))
	if err != nil {
		t.Fatal(err)
	}
	h, err := newHub(k, dir, netip.MustParsePrefix("198.18.0.0/15"))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func pair(t *testing.T, srv *httptest.Server, code, pub string) (*http.Response, wire.PairResponse) {
	t.Helper()
	b, _ := json.Marshal(wire.PairRequest{Code: code, Name: "Garage", PublicKey: pub, Version: "test"})
	resp, err := http.Post(srv.URL+"/link/pair", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var pr wire.PairResponse
	_ = json.NewDecoder(resp.Body).Decode(&pr)
	return resp, pr
}

func TestPairingCodeWorksOnce(t *testing.T) {
	dir := t.TempDir()
	h := testHub(t, dir)
	srv := httptest.NewServer(h.publicHandler())
	defer srv.Close()
	site, _ := wire.LoadOrCreateKey(filepath.Join(dir, "site.key"))
	code, _ := h.codes.Make("Home")

	resp, pr := pair(t, srv, code, wire.PubString(site.Public))
	if resp.StatusCode != 200 {
		t.Fatalf("pairing failed: %d", resp.StatusCode)
	}
	if pr.HubKey != wire.PubString(h.static.Public) || pr.Fingerprint != wire.Fingerprint(h.static.Public, site.Public) {
		t.Fatal("pairing answer has the wrong hub key or fingerprint")
	}
	if s := h.siteByKey(site.Public); s == nil || s.Name != "Home" {
		t.Fatalf("site not stored with the code's name: %+v", s)
	}
	if resp, _ := pair(t, srv, code, wire.PubString(site.Public)); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a used code paired again: %d", resp.StatusCode)
	}
	// The pairing survives a restart.
	if testHub(t, dir).siteByKey(site.Public) == nil {
		t.Fatal("paired site lost on restart")
	}
}

func TestPairingIsRateLimited(t *testing.T) {
	h := testHub(t, t.TempDir())
	srv := httptest.NewServer(h.publicHandler())
	defer srv.Close()
	last := 0
	for i := 0; i < 11; i++ {
		resp, _ := pair(t, srv, "AAAA-AAAA-AAAA", "bad")
		last = resp.StatusCode
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("eleventh try got %d", last)
	}
}

func TestOverlayAddressesStayPut(t *testing.T) {
	dir := t.TempDir()
	h := testHub(t, dir)
	ps := []wire.Printer{{IP: "192.168.1.20", Family: "bambu", Serial: "01P00A1"}, {IP: "192.168.1.21", Family: "moonraker"}}
	h.report("s1", ps)
	first := map[string]string{}
	for k, v := range h.st.Overlays {
		first[k] = v
	}
	if len(first) != 2 {
		t.Fatalf("want 2 overlay addresses, got %v", first)
	}
	// A new address at the site keeps the overlay address when the serial is known.
	ps[0].IP = "192.168.1.99"
	h2 := testHub(t, dir)
	h2.report("s1", ps[:1])
	if h2.st.Overlays["s1|serial|01P00A1"] != first["s1|serial|01P00A1"] {
		t.Fatal("overlay address moved after restart or IP change")
	}
	var off int
	h2.report("s1", nil)
	for _, e := range h2.byAddr {
		if !e.Online {
			off++
		}
	}
	if off != 1 {
		t.Fatalf("missing printer should be offline, got %d offline", off)
	}
	for _, a := range first {
		if ad := netip.MustParseAddr(a).As4(); ad[3] == 0 || ad[3] == 255 {
			t.Fatalf("handed out %s", a)
		}
	}
}

func TestRevokeForgetsSite(t *testing.T) {
	dir := t.TempDir()
	h := testHub(t, dir)
	site, _ := wire.LoadOrCreateKey(filepath.Join(dir, "site.key"))
	h.st.Sites = append(h.st.Sites, Site{ID: "s1", Name: "x", PublicKey: wire.PubString(site.Public)})
	h.report("s1", []wire.Printer{{IP: "10.0.0.2", Family: "octoprint"}})
	if !h.revoke("s1") || h.revoke("s1") {
		t.Fatal("revoke should succeed once")
	}
	if len(h.st.Overlays) != 0 || len(h.byAddr) != 0 || h.siteByKey(site.Public) != nil {
		t.Fatal("revoked site left state behind")
	}
}

func TestAdminAPINeedsToken(t *testing.T) {
	h := testHub(t, t.TempDir())
	srv := httptest.NewServer(h.apiHandler("s3cret-token"))
	defer srv.Close()
	for _, tok := range []string{"", "wrong", "s3cret-tokex"} {
		req, _ := http.NewRequest("POST", srv.URL+"/api/pairing-codes", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, _ := http.DefaultClient.Do(req)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("token %q got %d", tok, resp.StatusCode)
		}
	}
	req, _ := http.NewRequest("POST", srv.URL+"/api/pairing-codes", bytes.NewReader([]byte(`{"name":"Shop"}`)))
	req.Header.Set("Authorization", "Bearer s3cret-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("good token refused: %v", err)
	}
	var out struct{ Code string }
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if len(out.Code) != 14 {
		t.Fatalf("code %q is not 12 characters in groups of 4", out.Code)
	}
	if h.codes.Count() != 1 {
		t.Fatal("code not recorded")
	}
}

func TestAPrinterKeepsItsAddressWhenItsSerialIsHeard(t *testing.T) {
	h := testHub(t, t.TempDir())
	h.report("s1", []wire.Printer{{IP: "192.168.1.60", Family: "bambu"}})
	before := h.st.Overlays["s1|bambu|192.168.1.60"]
	h.report("s1", []wire.Printer{{IP: "192.168.1.60", Family: "bambu", Serial: "31B8AP610800641"}})
	if got := h.st.Overlays["s1|serial|31B8AP610800641"]; got == "" || got != before {
		t.Fatalf("address moved from %q to %q", before, got)
	}
	if _, left := h.st.Overlays["s1|bambu|192.168.1.60"]; left || len(h.st.Overlays) != 1 {
		t.Fatalf("the old entry was kept: %v", h.st.Overlays)
	}
}

func TestAnOldEntryForTheSamePrinterIsDropped(t *testing.T) {
	h := testHub(t, t.TempDir())
	// An entry left from before the hub moved addresses by serial: the same printer at the same address.
	h.st.Overlays["s1|bambu|192.168.1.60"] = "198.18.0.2"
	h.byAddr[netip.MustParseAddr("198.18.0.2")] = &printerEntry{Overlay: "198.18.0.2", Site: "s1",
		Printer: wire.Printer{IP: "192.168.1.60", Family: "bambu"}}
	h.st.Overlays["s1|serial|31B8"] = "198.18.0.3"
	h.report("s1", []wire.Printer{{IP: "192.168.1.60", Family: "bambu", Serial: "31B8"}})
	if _, left := h.byAddr[netip.MustParseAddr("198.18.0.2")]; left {
		t.Fatal("the old entry is still listed")
	}
	if len(h.st.Overlays) != 1 || h.st.Overlays["s1|serial|31B8"] != "198.18.0.3" {
		t.Fatalf("overlays: %v", h.st.Overlays)
	}
}
