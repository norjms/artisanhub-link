// SPDX-License-Identifier: AGPL-3.0-only

// link-site runs at the printer site. It dials out to the paired hub, reports
// the printers it finds, and opens connections to those printers only, on the
// ports each printer family uses.
package main

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/mdns"
	"github.com/hashicorp/yamux"
	"github.com/norjms/artisanhub-link/internal/wire"
)

// ports each family may be reached on. Anything else is refused.
var familyPorts = map[string][]int{
	"bambu":     {8883, 990, 322, 6000},
	"moonraker": {7125, 80, 443, 8080},
	"octoprint": {80, 443, 5000, 8080},
	"prusalink": {80, 443},
	"elegoo":    {3030, 8080},
	"duet":      {80},
}

// After an FTPS control connection, the printer opens a passive data port.
// Allow high ports to that printer for a short window only.
const ftpWindow = 60 * time.Second

type site struct {
	mu       sync.Mutex
	printers map[string]wire.Printer // by IP
	ftpOpen  map[string]time.Time    // IP -> window end
	changed  chan struct{}
}

func newSite() *site {
	return &site{printers: map[string]wire.Printer{}, ftpOpen: map[string]time.Time{}, changed: make(chan struct{}, 1)}
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
	for _, port := range []int{2021, 1990} {
		var conn *net.UDPConn
		for {
			var err error
			conn, err = net.ListenUDP("udp4", &net.UDPAddr{Port: port})
			if err == nil {
				break
			}
			log.Printf("ssdp listen %d: %v; retrying", port, err)
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
					s.add(wire.Printer{IP: e.AddrV4.String(), Family: fam, Name: e.Host, Via: "mdns"})
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

func (s *site) run(ctx context.Context, hubURL string, priv ed25519.PrivateKey, hubPub ed25519.PublicKey) error {
	dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(dctx, hubURL, &websocket.DialOptions{HTTPClient: http.DefaultClient})
	if err != nil {
		return err
	}
	ws.SetReadLimit(-1)
	conn := websocket.NetConn(ctx, ws, websocket.MessageBinary)
	defer conn.Close()
	if err := wire.SiteHandshake(conn, priv, hubPub); err != nil {
		return err
	}
	cfg := yamux.DefaultConfig()
	cfg.KeepAliveInterval = 20 * time.Second
	cfg.LogOutput = os.Stderr
	sess, err := yamux.Client(conn, cfg)
	if err != nil {
		return err
	}
	log.Printf("connected to hub")
	s.serve(sess)
	return fmt.Errorf("hub connection closed")
}

func main() {
	hubURL := flag.String("hub", "", "hub link URL, wss://host/link")
	hubKey := flag.String("hub-key", "", "hub public key pinned at pairing")
	keyFile := flag.String("key", "site.key", "this site's private key file")
	static := flag.String("printers", "", "extra printers, family=ip,family=ip")
	showKey := flag.Bool("show-key", false, "print this site's public key and exit")
	flag.Parse()

	priv, err := wire.LoadOrCreateKey(*keyFile)
	if err != nil {
		log.Fatal(err)
	}
	if *showKey {
		fmt.Println(wire.PubString(priv.Public().(ed25519.PublicKey)))
		return
	}
	hubPub, err := wire.ParsePub(*hubKey)
	if err != nil {
		log.Fatal("hub-key: ", err)
	}
	s := newSite()
	for _, item := range strings.Split(*static, ",") {
		if fam, ip, ok := strings.Cut(strings.TrimSpace(item), "="); ok {
			if _, known := familyPorts[fam]; known {
				s.add(wire.Printer{IP: ip, Family: fam, Via: "static"})
			}
		}
	}
	ctx := context.Background()
	go s.bambuSSDP(ctx)
	go s.mdnsBrowse(ctx)
	backoff := time.Second
	for {
		err := s.run(ctx, *hubURL, priv, hubPub)
		log.Printf("hub: %v; retrying in %s", err, backoff)
		time.Sleep(backoff)
		if backoff < time.Minute {
			backoff *= 2
		}
	}
}
