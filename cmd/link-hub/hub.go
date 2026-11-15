// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/flynn/noise"
	"github.com/hashicorp/yamux"
	"github.com/norjms/artisanhub-link/internal/pairing"
	"github.com/norjms/artisanhub-link/internal/wire"
)

// Limits on what one site carries, so a misbehaving farm cannot use a site to flood its network.
const (
	maxStreamsPerSite    = 256
	maxStreamsPerPrinter = 32
	streamsPerSecond     = 50 // new connections per site, with a burst of the same size
)

type printerEntry struct {
	Overlay string       `json:"overlay"`
	Site    string       `json:"site_id"`
	Printer wire.Printer `json:"printer"`
	Online  bool         `json:"online"`
	streams int
}

type liveSite struct {
	sess    *yamux.Session
	since   time.Time
	remote  string
	version string
	streams int
	tokens  float64
	refill  time.Time
}

type hub struct {
	static   noise.DHKey
	dataDir  string
	prefix   netip.Prefix
	codes    *pairing.Codes
	mu       sync.Mutex
	st       *state
	live     map[string]*liveSite // site id -> its one live connection
	dupAt    map[string]time.Time // site id -> when a second copy last tried to connect
	byAddr   map[netip.Addr]*printerEntry
	pairHits map[string][]time.Time
}

func newHub(static noise.DHKey, dataDir string, prefix netip.Prefix) (*hub, error) {
	st, err := loadState(filepath.Join(dataDir, "state.json"))
	if err != nil {
		return nil, err
	}
	h := &hub{static: static, dataDir: dataDir, prefix: prefix.Masked(), codes: pairing.New(), st: st,
		live: map[string]*liveSite{}, dupAt: map[string]time.Time{}, byAddr: map[netip.Addr]*printerEntry{},
		pairHits: map[string][]time.Time{}}
	return h, nil
}

func (h *hub) saveLocked() {
	if err := h.st.save(filepath.Join(h.dataDir, "state.json")); err != nil {
		log.Printf("saving state: %v", err)
	}
}

func (h *hub) siteByKey(pub []byte) *Site {
	k := wire.PubString(pub)
	for i := range h.st.Sites {
		if h.st.Sites[i].PublicKey == k {
			return &h.st.Sites[i]
		}
	}
	return nil
}

func (h *hub) siteByID(id string) *Site {
	for i := range h.st.Sites {
		if h.st.Sites[i].ID == id {
			return &h.st.Sites[i]
		}
	}
	return nil
}

// ---- pairing ----

func clientIP(r *http.Request) string {
	if f := r.Header.Get("X-Forwarded-For"); f != "" {
		return strings.TrimSpace(strings.Split(f, ",")[0])
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

// pairAllowed limits pairing tries per address: 10 in 10 minutes. A code has about 57 bits, so this is
// belt and braces.
func (h *hub) pairAllowed(ip string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	kept := h.pairHits[ip][:0]
	for _, t := range h.pairHits[ip] {
		if now.Sub(t) < 10*time.Minute {
			kept = append(kept, t)
		}
	}
	if len(kept) >= 10 {
		h.pairHits[ip] = kept
		return false
	}
	h.pairHits[ip] = append(kept, now)
	return true
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func (h *hub) handlePair(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if !h.pairAllowed(clientIP(r)) {
		fail(w, http.StatusTooManyRequests, "too many pairing tries; wait ten minutes")
		return
	}
	var req wire.PairRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "bad request")
		return
	}
	pub, err := wire.ParsePub(req.PublicKey)
	if err != nil {
		fail(w, http.StatusBadRequest, "bad public key")
		return
	}
	given, ok := h.codes.Use(req.Code)
	if !ok {
		log.Printf("pairing refused from %s: wrong or expired code", clientIP(r))
		fail(w, http.StatusForbidden, "that pairing code is wrong, used already, or expired; make a new one in the hub")
		return
	}
	name := strings.TrimSpace(given)
	if name == "" {
		name = strings.TrimSpace(req.Name)
	}
	if name == "" {
		name = "Printer site"
	}
	if len(name) > 80 {
		name = name[:80]
	}
	h.mu.Lock()
	site := h.siteByKey(pub)
	if site == nil {
		h.st.Sites = append(h.st.Sites, Site{ID: newID(), Name: name, PublicKey: wire.PubString(pub), PairedAt: time.Now().UTC()})
		site = &h.st.Sites[len(h.st.Sites)-1]
	} else {
		site.Name = name
	}
	id := site.ID
	h.saveLocked()
	h.mu.Unlock()
	fp := wire.Fingerprint(h.static.Public, pub)
	log.Printf("site %s (%s) paired from %s, fingerprint %s", name, id, clientIP(r), fp)
	writeJSON(w, http.StatusOK, wire.PairResponse{SiteID: id, HubKey: wire.PubString(h.static.Public), Fingerprint: fp})
}

// ---- the link ----

func (h *hub) handleLink(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	ws.SetReadLimit(-1)
	raw := websocket.NetConn(r.Context(), ws, websocket.MessageBinary)
	defer raw.Close()
	var siteID string
	conn, sitePub, err := wire.HubHandshake(raw, h.static, func(pub []byte) bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		if s := h.siteByKey(pub); s != nil {
			siteID = s.ID
			return true
		}
		return false
	})
	if err != nil {
		k := ""
		if sitePub != nil {
			k = wire.PubString(sitePub)[:8] + "..."
		}
		log.Printf("refused a site from %s %s: %v", clientIP(r), k, err)
		return
	}
	cfg := yamux.DefaultConfig()
	cfg.KeepAliveInterval = 20 * time.Second
	cfg.LogOutput = os.Stderr
	sess, err := yamux.Server(conn, cfg)
	if err != nil {
		return
	}
	h.mu.Lock()
	// One live connection per site key: the first one stays. A second copy (a cloned VM, a second
	// install with the same key) is refused and shown, rather than both knocking each other off.
	if old := h.live[siteID]; old != nil && !old.sess.IsClosed() {
		h.dupAt[siteID] = time.Now().UTC()
		h.mu.Unlock()
		log.Printf("site %s connected a second time from %s; kept the first connection", siteID, clientIP(r))
		sess.Close()
		return
	}
	ls := &liveSite{sess: sess, since: time.Now().UTC(), remote: clientIP(r), tokens: streamsPerSecond, refill: time.Now()}
	h.live[siteID] = ls
	h.mu.Unlock()
	log.Printf("site %s connected from %s", siteID, ls.remote)
	defer h.siteGone(siteID, sess)
	for {
		st, err := sess.AcceptStream()
		if err != nil {
			log.Printf("site %s disconnected", siteID)
			return
		}
		go h.siteStream(siteID, ls, st)
	}
}

func (h *hub) siteGone(siteID string, sess *yamux.Session) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ls := h.live[siteID]; ls != nil && ls.sess == sess {
		delete(h.live, siteID)
		for _, e := range h.byAddr {
			if e.Site == siteID {
				e.Online = false
			}
		}
	}
}

// identity is what keeps a printer's overlay address: its serial when it has one, else its family and
// address at the site.
func identity(siteID string, p wire.Printer) string {
	if p.Serial != "" {
		return siteID + "|serial|" + p.Serial
	}
	return siteID + "|" + p.Family + "|" + p.IP
}

func (h *hub) freeAddrLocked() (netip.Addr, bool) {
	used := map[string]bool{}
	for _, a := range h.st.Overlays {
		used[a] = true
	}
	base := h.prefix.Addr().As4()
	start := binary.BigEndian.Uint32(base[:])
	size := uint32(1) << (32 - h.prefix.Bits())
	for i := uint32(1); i < size-1; i++ {
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], start+i)
		if b[3] == 0 || b[3] == 255 {
			continue
		}
		a := netip.AddrFrom4(b)
		if !used[a.String()] {
			return a, true
		}
	}
	return netip.Addr{}, false
}

func (h *hub) report(siteID string, ps []wire.Printer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	seen := map[netip.Addr]bool{}
	changed := false
	for _, p := range ps {
		key := identity(siteID, p)
		s, ok := h.st.Overlays[key]
		if ok && p.Serial != "" {
			// Kept from before its serial was heard: that address is no longer this printer's.
			if _, had := h.st.Overlays[siteID+"|"+p.Family+"|"+p.IP]; had {
				delete(h.st.Overlays, siteID+"|"+p.Family+"|"+p.IP)
				changed = true
			}
		}
		if !ok && p.Serial != "" {
			// Known first by family and address (added by hand, or before its serial was heard): it keeps
			// that address, now under its serial.
			if old, had := h.st.Overlays[siteID+"|"+p.Family+"|"+p.IP]; had {
				delete(h.st.Overlays, siteID+"|"+p.Family+"|"+p.IP)
				h.st.Overlays[key] = old
				s, ok, changed = old, true, true
			}
		}
		var a netip.Addr
		if ok {
			a, _ = netip.ParseAddr(s)
		} else {
			na, free := h.freeAddrLocked()
			if !free {
				log.Printf("overlay range %s is full", h.prefix)
				continue
			}
			a = na
			h.st.Overlays[key] = a.String()
			changed = true
			log.Printf("overlay %s -> site %s %s %s", a, siteID, p.Family, p.IP)
		}
		e := h.byAddr[a]
		if e == nil {
			e = &printerEntry{Overlay: a.String(), Site: siteID}
			h.byAddr[a] = e
		}
		e.Printer, e.Online = p, true
		seen[a] = true
	}
	// Each report is the site's full list: anything missing is offline. An entry for a printer that now
	// answers at another overlay address (the same family at the same address, its serial heard since) is
	// dropped, and its address freed.
	now := map[string]bool{}
	for a := range seen {
		if e := h.byAddr[a]; e != nil {
			now[e.Printer.Family+"|"+e.Printer.IP] = true
		}
	}
	for a, e := range h.byAddr {
		if e.Site != siteID || seen[a] {
			continue
		}
		if now[e.Printer.Family+"|"+e.Printer.IP] && e.streams == 0 {
			delete(h.byAddr, a)
			for k, v := range h.st.Overlays {
				if v == a.String() {
					delete(h.st.Overlays, k)
				}
			}
			changed = true
			log.Printf("overlay %s freed: site %s %s %s answers at another address", a, siteID, e.Printer.Family, e.Printer.IP)
			continue
		}
		e.Online = false
	}
	if changed {
		h.saveLocked()
	}
}

func (h *hub) siteStream(siteID string, ls *liveSite, st *yamux.Stream) {
	defer st.Close()
	r := bufio.NewReader(st)
	kind, err := r.ReadByte()
	if err != nil || kind != wire.KindPrinter {
		return
	}
	var hello wire.Hello
	if err := wire.ReadJSONLine(r, &hello); err != nil {
		return
	}
	h.mu.Lock()
	ls.version = hello.Version
	h.mu.Unlock()
	for {
		var ps []wire.Printer
		if err := wire.ReadJSONLine(r, &ps); err != nil {
			return
		}
		h.report(siteID, ps)
	}
}

// ---- the overlay ----

// take reserves one stream for a connection to this printer, within the limits.
func (h *hub) take(e *printerEntry) (*yamux.Session, string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ls := h.live[e.Site]
	if ls == nil || !e.Online {
		return nil, "site offline", false
	}
	now := time.Now()
	ls.tokens += now.Sub(ls.refill).Seconds() * streamsPerSecond
	if ls.tokens > streamsPerSecond {
		ls.tokens = streamsPerSecond
	}
	ls.refill = now
	switch {
	case ls.tokens < 1:
		return nil, "too many new connections", false
	case ls.streams >= maxStreamsPerSite:
		return nil, "too many connections to this site", false
	case e.streams >= maxStreamsPerPrinter:
		return nil, "too many connections to this printer", false
	}
	ls.tokens--
	ls.streams++
	e.streams++
	return ls.sess, "", true
}

func (h *hub) release(e *printerEntry) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ls := h.live[e.Site]; ls != nil && ls.streams > 0 {
		ls.streams--
	}
	if e.streams > 0 {
		e.streams--
	}
}

func (h *hub) overlayConn(c *net.TCPConn) {
	dst, err := originalDst(c)
	if err != nil {
		c.Close()
		return
	}
	h.mu.Lock()
	e := h.byAddr[dst.Addr()]
	h.mu.Unlock()
	if e == nil {
		c.Close()
		return
	}
	sess, why, ok := h.take(e)
	if !ok {
		log.Printf("%s:%d refused: %s", dst.Addr(), dst.Port(), why)
		c.Close()
		return
	}
	defer h.release(e)
	st, err := sess.OpenStream()
	if err != nil {
		c.Close()
		return
	}
	if _, err := st.Write([]byte{wire.KindData}); err == nil {
		err = wire.WriteJSONLine(st, wire.Open{IP: e.Printer.IP, Port: int(dst.Port())})
	}
	status := []byte{wire.StatusFailed}
	if err == nil {
		_ = st.SetReadDeadline(time.Now().Add(15 * time.Second))
		_, err = st.Read(status)
		_ = st.SetReadDeadline(time.Time{})
	}
	if err != nil || status[0] != wire.StatusOK {
		log.Printf("%s:%d (site %s, %s) refused by the site, status %d", dst.Addr(), dst.Port(), e.Site, e.Printer.IP, status[0])
		st.Close()
		c.Close()
		return
	}
	wire.Pipe(c, st)
}

// ---- the hub's own API, for the hub and the installer ----

type printerView struct {
	Overlay string `json:"overlay"`
	IP      string `json:"ip"`
	Family  string `json:"family"`
	Name    string `json:"name,omitempty"`
	Model   string `json:"model,omitempty"`
	Serial  string `json:"serial,omitempty"`
	Via     string `json:"via"`
	Online  bool   `json:"online"`
}

type siteView struct {
	ID             string        `json:"id"`
	Name           string        `json:"name"`
	PairedAt       time.Time     `json:"paired_at"`
	Fingerprint    string        `json:"fingerprint"`
	Connected      bool          `json:"connected"`
	ConnectedSince *time.Time    `json:"connected_since,omitempty"`
	Remote         string        `json:"remote,omitempty"`
	Version        string        `json:"version,omitempty"`
	ConnectedTwice *time.Time    `json:"connected_twice_at,omitempty"`
	Printers       []printerView `json:"printers"`
}

func (h *hub) status() map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	sites := make([]siteView, 0, len(h.st.Sites))
	for _, s := range h.st.Sites {
		pub, _ := wire.ParsePub(s.PublicKey)
		v := siteView{ID: s.ID, Name: s.Name, PairedAt: s.PairedAt, Fingerprint: wire.Fingerprint(h.static.Public, pub),
			Printers: []printerView{}}
		if ls := h.live[s.ID]; ls != nil && !ls.sess.IsClosed() {
			since := ls.since
			v.Connected, v.ConnectedSince, v.Remote, v.Version = true, &since, ls.remote, ls.version
		}
		if t, ok := h.dupAt[s.ID]; ok && time.Since(t) < 10*time.Minute {
			tt := t
			v.ConnectedTwice = &tt
		}
		for _, e := range h.byAddr {
			if e.Site == s.ID {
				p := e.Printer
				v.Printers = append(v.Printers, printerView{Overlay: e.Overlay, IP: p.IP, Family: p.Family, Name: p.Name,
					Model: p.Model, Serial: p.Serial, Via: p.Via, Online: e.Online})
			}
		}
		sort.Slice(v.Printers, func(i, j int) bool { return v.Printers[i].Overlay < v.Printers[j].Overlay })
		sites = append(sites, v)
	}
	return map[string]any{"hub_public_key": wire.PubString(h.static.Public), "overlay": h.prefix.String(),
		"open_codes": h.codes.Count(), "sites": sites}
}

func (h *hub) revoke(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	kept := h.st.Sites[:0]
	found := false
	for _, s := range h.st.Sites {
		if s.ID == id {
			found = true
			continue
		}
		kept = append(kept, s)
	}
	if !found {
		return false
	}
	h.st.Sites = kept
	for k := range h.st.Overlays {
		if strings.HasPrefix(k, id+"|") {
			delete(h.st.Overlays, k)
		}
	}
	for a, e := range h.byAddr {
		if e.Site == id {
			delete(h.byAddr, a)
		}
	}
	if ls := h.live[id]; ls != nil {
		ls.sess.Close()
		delete(h.live, id)
	}
	h.saveLocked()
	log.Printf("site %s removed; its key no longer connects", id)
	return true
}

func (h *hub) apiHandler(token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	auth := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if token == "" || len(got) != len(token) || !constantEqual(got, token) {
				fail(w, http.StatusUnauthorized, "a valid admin token is needed")
				return
			}
			next(w, r)
		}
	}
	mux.HandleFunc("GET /api/status", auth(func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, h.status()) }))
	mux.HandleFunc("POST /api/pairing-codes", auth(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req)
		code, exp := h.codes.Make(strings.TrimSpace(req.Name))
		writeJSON(w, 200, map[string]any{"code": code, "expires_at": exp.UTC(), "hub_public_key": wire.PubString(h.static.Public)})
	}))
	mux.HandleFunc("DELETE /api/sites/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		if !h.revoke(r.PathValue("id")) {
			fail(w, http.StatusNotFound, "no such site")
			return
		}
		writeJSON(w, 200, map[string]bool{"removed": true})
	}))
	return mux
}

func constantEqual(a, b string) bool {
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

func (h *hub) publicHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/link", h.handleLink)
	mux.HandleFunc("/link/pair", h.handlePair)
	mux.HandleFunc("/link/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "ok", "service": "artisanhub-link-hub", "version": version})
	})
	return mux
}

var version = "dev"
