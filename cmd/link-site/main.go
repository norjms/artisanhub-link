// SPDX-License-Identifier: AGPL-3.0-only

// link-site runs at the printer site. It dials out to the paired hub, reports
// the printers it finds, and opens connections to those printers only, on the
// ports each printer family uses.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/flynn/noise"
	"github.com/hashicorp/mdns"
	"github.com/hashicorp/yamux"
	"github.com/norjms/artisanhub-link/internal/wire"
)

// ports each family may be reached on. Anything else is refused. 8554 is RTSP, where a camera restreamed on
// the printer's own machine (go2rtc, as H.264) is served, so the hub pulls a few Mbps instead of raw MJPEG.
var familyPorts = map[string][]int{
	"bambu":     {8883, 990, 322, 6000},
	"moonraker": {7125, 80, 443, 8080, 8554},
	"octoprint": {80, 443, 5000, 8080, 8554},
	"prusalink": {80, 443, 8554},
	"elegoo":    {3030, 8080},
	"duet":      {80},
}

// After an FTPS control connection, the printer opens a passive data port.
// Allow high ports to that printer for a short window only.
const ftpWindow = 60 * time.Second

// maxOpen caps the printer connections the site carries at once, whatever the hub asks.
const maxOpen = 128

type site struct {
	open     chan struct{}
	mu       sync.Mutex
	printers map[string]wire.Printer // by IP
	ftpOpen  map[string]time.Time    // IP -> window end
	changed  chan struct{}
}

func newSite() *site {
	return &site{printers: map[string]wire.Printer{}, ftpOpen: map[string]time.Time{}, changed: make(chan struct{}, 1),
		open: make(chan struct{}, maxOpen)}
}

func (s *site) add(p wire.Printer) {
	addr, err := netip.ParseAddr(p.IP)
	if err != nil || !addr.Is4() || !(addr.IsPrivate() || addr.IsLinkLocalUnicast()) {
		return // only printers on the local network
	}
	s.mu.Lock()
	old, ok := s.printers[p.IP]
	if ok && old.Via == "static" && p.Via != "static" {
		p.Via = "static"
	}
	if ok && p.Name == "" {
		p.Name = old.Name
	}
	if ok && p.Serial == "" {
		p.Serial = old.Serial // a later announcement without it does not lose it
	}
	s.printers[p.IP] = p
	s.mu.Unlock()
	if !ok || old != p {
		log.Printf("printer %s %s %s %s via %s", p.Family, p.IP, p.Model, p.Name, p.Via)
		select {
		case s.changed <- struct{}{}:
		default:
		}
	}
}

func (s *site) list() []wire.Printer {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]wire.Printer, 0, len(s.printers))
	for _, p := range s.printers {
		out = append(out, p)
	}
	return out
}

// allowed decides whether the hub may reach ip:port.
func (s *site) allowed(ip string, port int) (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.printers[ip]
	if !ok {
		return false, "not a known printer"
	}
	for _, allowedPort := range familyPorts[p.Family] {
		if port == allowedPort {
			if p.Family == "bambu" && port == 990 {
				s.ftpOpen[ip] = time.Now().Add(ftpWindow)
			}
			return true, ""
		}
	}
	if p.Family == "bambu" && port > 1023 && time.Now().Before(s.ftpOpen[ip]) {
		return true, ""
	}
	return false, fmt.Sprintf("port %d is not a %s printer port", port, p.Family)
}

// ---- discovery ----

// Bambu printers announce themselves with SSDP NOTIFY on UDP 2021 and answer
// M-SEARCH on 1990.
func (s *site) bambuSSDP(ctx context.Context) {
	parse := func(b []byte) {
		h := map[string]string{}
		for _, line := range strings.Split(string(b), "\r\n") {
			if k, v, ok := strings.Cut(line, ":"); ok {
				h[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
			}
		}
		if !strings.Contains(h["nt"]+h["st"], "bambulab") {
			return
		}
		s.add(wire.Printer{IP: h["location"], Family: "bambu", Name: h["devname.bambu.com"],
			Model: h["devmodel.bambu.com"], Serial: h["usn"], Via: "ssdp"})
	}
	lc := net.ListenConfig{Control: shareAddress}
	for _, port := range []int{2021, 1990} {
		var conn *net.UDPConn
		for tries := 0; ; tries++ {
			pc, err := lc.ListenPacket(ctx, "udp4", ":"+strconv.Itoa(port))
			if err == nil {
				conn = pc.(*net.UDPConn)
				break
			}
			// Said once, then every ten minutes: Bambu printers are not found while it lasts.
			if tries%60 == 0 {
				log.Printf("Bambu Lab discovery cannot listen on UDP %d (%v); add Bambu printers with LINK_PRINTERS "+
					"until it can. Retrying every 10 s.", port, err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Second):
			}
		}
		go func() {
			<-ctx.Done()
			conn.Close()
		}()
		go func(c *net.UDPConn) {
			buf := make([]byte, 2048)
			for {
				n, _, err := c.ReadFromUDP(buf)
				if err != nil {
					return
				}
				parse(buf[:n])
			}
		}(conn)
		if port == 1990 {
			go func(c *net.UDPConn) {
				msg := "M-SEARCH * HTTP/1.1\r\nHOST: 239.255.255.250:1990\r\nMAN: \"ssdp:discover\"\r\nMX: 1\r\nST: urn:bambulab-com:device:3dprinter:1\r\n\r\n"
				dst := &net.UDPAddr{IP: net.IPv4(239, 255, 255, 250), Port: 1990}
				for {
					_, _ = c.WriteToUDP([]byte(msg), dst)
					select {
					case <-ctx.Done():
						return
					case <-time.After(30 * time.Second):
					}
				}
			}(conn)
		}
	}
}

// Klipper (Moonraker), OctoPrint and PrusaLink advertise with mDNS.
func (s *site) mdnsBrowse(ctx context.Context) {
	services := map[string]string{
		"_moonraker._tcp":  "moonraker",
		"_octoprint._tcp":  "octoprint",
		"_prusa-link._tcp": "prusalink",
	}
	for {
		for svc, family := range services {
			ch := make(chan *mdns.ServiceEntry, 16)
			go func(svc, fam string) {
				for e := range ch {
					// The mDNS client also passes on answers for other services
					// (TVs, speakers). Keep only the service asked for, and only
					// when its port answers.
					if e.AddrV4 == nil || !strings.Contains(e.Name, svc+".") {
						continue
					}
					c, err := net.DialTimeout("tcp4", net.JoinHostPort(e.AddrV4.String(), strconv.Itoa(e.Port)), 3*time.Second)
					if err != nil {
						continue
					}
					c.Close()
					s.add(wire.Printer{IP: e.AddrV4.String(), Family: fam, Name: hostName(e.Host), Via: "mdns"})
				}
			}(svc, family)
			p := mdns.DefaultParams(svc)
			p.Entries = ch
			p.Timeout = 3 * time.Second
			p.DisableIPv6 = true
			_ = mdns.Query(p)
			close(ch)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(60 * time.Second):
		}
	}
}

// hostName is an mDNS host name as people know it: "mk3s01.local." is mk3s01.
func hostName(h string) string {
	return strings.TrimSuffix(strings.TrimSuffix(h, "."), ".local")
}

// ---- hub connection ----

func (s *site) serve(sess *yamux.Session) {
	// Report printers on one stream, refreshed on change and every 30 s.
	go func() {
		st, err := sess.OpenStream()
		if err != nil {
			return
		}
		defer st.Close()
		if _, err := st.Write([]byte{wire.KindPrinter}); err != nil {
			return
		}
		if err := wire.WriteJSONLine(st, wire.Hello{Version: version}); err != nil {
			return
		}
		for {
			if err := wire.WriteJSONLine(st, s.list()); err != nil {
				return
			}
			select {
			case <-s.changed:
			case <-time.After(30 * time.Second):
			case <-sess.CloseChan():
				return
			}
		}
	}()
	for {
		st, err := sess.AcceptStream()
		if err != nil {
			return
		}
		go s.handle(st)
	}
}

func (s *site) handle(st *yamux.Stream) {
	r := bufio.NewReader(st)
	kind, err := r.ReadByte()
	if err != nil || kind != wire.KindData {
		st.Close()
		return
	}
	var o wire.Open
	if err := wire.ReadJSONLine(r, &o); err != nil {
		st.Close()
		return
	}
	select {
	case s.open <- struct{}{}:
		defer func() { <-s.open }()
	default:
		log.Printf("refused %s:%d: %d printer connections open already", o.IP, o.Port, maxOpen)
		_, _ = st.Write([]byte{wire.StatusDenied})
		st.Close()
		return
	}
	if ok, why := s.allowed(o.IP, o.Port); !ok {
		log.Printf("refused %s:%d: %s", o.IP, o.Port, why)
		_, _ = st.Write([]byte{wire.StatusDenied})
		st.Close()
		return
	}
	c, err := net.DialTimeout("tcp4", net.JoinHostPort(o.IP, strconv.Itoa(o.Port)), 8*time.Second)
	if err != nil {
		_, _ = st.Write([]byte{wire.StatusFailed})
		st.Close()
		return
	}
	if _, err := st.Write([]byte{wire.StatusOK}); err != nil {
		c.Close()
		return
	}
	wire.Pipe(c, bufStream{r, st})
}

// bufStream reads through the buffer that already holds the stream header.
type bufStream struct {
	r *bufio.Reader
	*yamux.Stream
}

func (b bufStream) Read(p []byte) (int, error) { return b.r.Read(p) }

func (s *site) run(ctx context.Context, linkURL string, static noise.DHKey, hubPub []byte) error {
	dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(dctx, linkURL, &websocket.DialOptions{HTTPClient: http.DefaultClient})
	if err != nil {
		return err
	}
	ws.SetReadLimit(-1)
	raw := websocket.NetConn(ctx, ws, websocket.MessageBinary)
	defer raw.Close()
	conn, err := wire.SiteHandshake(raw, static, hubPub)
	if err != nil {
		return err
	}
	cfg := yamux.DefaultConfig()
	cfg.KeepAliveInterval = 20 * time.Second
	cfg.LogOutput = os.Stderr
	sess, err := yamux.Client(conn, cfg)
	if err != nil {
		return err
	}
	log.Printf("connected to the hub")
	s.serve(sess)
	return fmt.Errorf("hub connection closed")
}

// version is set at build time.
var version = "dev"

type settings struct {
	hub, code, name, printers, dataDir string
}

func env(name string) string { return strings.TrimSpace(os.Getenv(name)) }

func main() {
	configFile := flag.String("config", "", "settings file (INI, section [link]: hub, code, name, printers)")
	hubURL := flag.String("hub", env("LINK_HUB"), "the hub's address, https://hub.example.com")
	code := flag.String("code", env("LINK_PAIR_CODE"), "the pairing code the hub showed (used once)")
	name := flag.String("name", env("LINK_NAME"), "this site's name in the hub")
	static := flag.String("printers", env("LINK_PRINTERS"), "printers to add by hand, family=ip,family=ip")
	dataDir := flag.String("data", env("LINK_DATA"), "where this site's key and pairing are kept (default: beside the settings file, or /data)")
	showKey := flag.Bool("show-key", false, "print this site's public key and exit")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}
	// Flags and the environment win over the settings file, which is read again until the site is
	// configured, so a service can start before it has a code.
	load := func() settings {
		st := settings{hub: *hubURL, code: *code, name: *name, printers: *static, dataDir: *dataDir}
		if *configFile != "" {
			ini, err := readINI(*configFile, "link")
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				log.Printf("settings: %v", err)
			}
			for _, f := range []struct {
				dst *string
				key string
			}{{&st.hub, "hub"}, {&st.code, "code"}, {&st.name, "name"}, {&st.printers, "printers"}} {
				if *f.dst == "" {
					*f.dst = ini[f.key]
				}
			}
			if st.dataDir == "" {
				st.dataDir = filepath.Dir(*configFile)
			}
		}
		if st.dataDir == "" {
			st.dataDir = "/data"
		}
		if st.name == "" {
			st.name, _ = os.Hostname()
		}
		return st
	}
	if *showKey {
		k, err := wire.LoadOrCreateKey(filepath.Join(load().dataDir, "site.key"))
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(wire.PubString(k.Public))
		return
	}
	run := func(ctx context.Context) { siteMain(ctx, load) }
	if runAsService(run) {
		return
	}
	run(context.Background())
}

func siteMain(ctx context.Context, load func() settings) {
	log.Printf("ArtisanHub Link site %s", version)
	var st settings
	var paired *pairing
	for {
		st = load()
		key, err := wire.LoadOrCreateKey(filepath.Join(st.dataDir, "site.key"))
		if err != nil {
			log.Fatal(err)
		}
		paired, err = ensurePaired(ctx, st, key)
		if err == nil {
			break
		}
		log.Printf("%v; checking again in 30 s", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
		}
	}
	key, _ := wire.LoadOrCreateKey(filepath.Join(st.dataDir, "site.key"))
	log.Printf("paired with %s as site %s; fingerprint %s", paired.Hub, paired.SiteID, paired.Fingerprint)
	hubPub, err := wire.ParsePub(paired.HubKey)
	if err != nil {
		log.Fatal("hub key: ", err)
	}
	linkURL, err := linkAddress(paired.Hub)
	if err != nil {
		log.Fatal(err)
	}
	s := newSite()
	for _, item := range strings.Split(st.printers, ",") {
		if fam, ip, ok := strings.Cut(strings.TrimSpace(item), "="); ok {
			if _, known := familyPorts[fam]; known {
				s.add(wire.Printer{IP: strings.TrimSpace(ip), Family: fam, Via: "static"})
			}
		}
	}
	go s.bambuSSDP(ctx)
	go s.mdnsBrowse(ctx)
	if env("LINK_PROBE") != "off" {
		go s.probePrusaLink(ctx)
	}
	backoff := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err := s.run(ctx, linkURL, key, hubPub)
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) > 5*time.Minute {
			backoff = time.Second
		}
		log.Printf("hub: %v; retrying in %s", err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < time.Minute {
			backoff *= 2
		}
	}
}
