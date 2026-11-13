// link-hub runs next to the hub. Paired sites dial in over a WebSocket. Each
// printer a site reports gets an overlay address (198.18.x.y by default); TCP sent to that
// address is redirected here by iptables and carried to the site.
package main

import (
	"bufio"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"
	"github.com/norjms/artisanhub-link/internal/wire"
)

type overlayEntry struct {
	Overlay string       `json:"overlay"`
	Site    string       `json:"site"`
	Printer wire.Printer `json:"printer"`
	Online  bool         `json:"online"`
}

type hub struct {
	priv    ed25519.PrivateKey
	paired  map[string]string // site pubkey -> site name
	prefix  netip.Prefix
	mu      sync.Mutex
	next    uint32
	byAddr  map[netip.Addr]*overlayEntry
	byKey   map[string]netip.Addr // site|ip -> overlay
	session map[string]*yamux.Session
}

func (h *hub) assign(site string, p wire.Printer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	key := site + "|" + p.IP
	if a, ok := h.byKey[key]; ok {
		e := h.byAddr[a]
		e.Printer, e.Online = p, true
		return
	}
	h.next++
	b := h.prefix.Addr().As4()
	v := binary.BigEndian.Uint32(b[:]) + h.next
	binary.BigEndian.PutUint32(b[:], v)
	a := netip.AddrFrom4(b)
	h.byKey[key] = a
	h.byAddr[a] = &overlayEntry{Overlay: a.String(), Site: site, Printer: p, Online: true}
	log.Printf("overlay %s -> %s %s %s", a, site, p.Family, p.IP)
}

func (h *hub) siteGone(site string, sess *yamux.Session) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.session[site] == sess {
		delete(h.session, site)
		for _, e := range h.byAddr {
			if e.Site == site {
				e.Online = false
			}
		}
	}
}

// ServeHTTP accepts a site connection.
func (h *hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	ws.SetReadLimit(-1)
	conn := websocket.NetConn(r.Context(), ws, websocket.MessageBinary)
	defer conn.Close()
	var site string
	pub, err := wire.HubHandshake(conn, h.priv, func(k ed25519.PublicKey) bool {
		name, ok := h.paired[wire.PubString(k)]
		site = name
		return ok
	})
	if err != nil {
		log.Printf("refused site from %s: %v", r.RemoteAddr, err)
		return
	}
	_ = pub
	cfg := yamux.DefaultConfig()
	cfg.KeepAliveInterval = 20 * time.Second
	cfg.LogOutput = os.Stderr
	sess, err := yamux.Server(conn, cfg)
	if err != nil {
		return
	}
	h.mu.Lock()
	if old := h.session[site]; old != nil {
		old.Close()
	}
	h.session[site] = sess
	h.mu.Unlock()
	log.Printf("site %s connected from %s", site, r.Header.Get("X-Forwarded-For")+" "+r.RemoteAddr)
	defer h.siteGone(site, sess)
	for {
		st, err := sess.AcceptStream()
		if err != nil {
			log.Printf("site %s disconnected", site)
			return
		}
		go h.siteStream(site, st)
	}
}

func (h *hub) siteStream(site string, st *yamux.Stream) {
	defer st.Close()
	r := bufio.NewReader(st)
	kind, err := r.ReadByte()
	if err != nil || kind != wire.KindPrinter {
		return
	}
	for {
		var ps []wire.Printer
		if err := wire.ReadJSONLine(r, &ps); err != nil {
			return
		}
		seen := map[string]bool{}
		for _, p := range ps {
			h.assign(site, p)
			seen[p.IP] = true
		}
		// Each report is the site's full list: anything missing is offline.
		h.mu.Lock()
		for _, e := range h.byAddr {
			if e.Site == site && !seen[e.Printer.IP] {
				e.Online = false
			}
		}
		h.mu.Unlock()
	}
}

// ---- overlay side ----

const soOriginalDst = 80

func originalDst(c *net.TCPConn) (netip.AddrPort, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return netip.AddrPort{}, err
	}
	var sa syscall.RawSockaddrInet4
	var serr error
	err = raw.Control(func(fd uintptr) {
		size := uint32(unsafe.Sizeof(sa))
		_, _, e := syscall.Syscall6(syscall.SYS_GETSOCKOPT, fd, syscall.IPPROTO_IP, soOriginalDst,
			uintptr(unsafe.Pointer(&sa)), uintptr(unsafe.Pointer(&size)), 0)
		if e != 0 {
			serr = e
		}
	})
	if err != nil {
		return netip.AddrPort{}, err
	}
	if serr != nil {
		return netip.AddrPort{}, serr
	}
	port := int(sa.Port>>8 | sa.Port<<8&0xff00)
	return netip.AddrPortFrom(netip.AddrFrom4(sa.Addr), uint16(port)), nil
}

func (h *hub) overlayConn(c *net.TCPConn) {
	dst, err := originalDst(c)
	if err != nil {
		c.Close()
		return
	}
	h.mu.Lock()
	e := h.byAddr[dst.Addr()]
	var sess *yamux.Session
	var target wire.Printer
	var site string
	if e != nil {
		sess, target, site = h.session[e.Site], e.Printer, e.Site
	}
	h.mu.Unlock()
	if e == nil || sess == nil {
		c.Close()
		return
	}
	st, err := sess.OpenStream()
	if err != nil {
		c.Close()
		return
	}
	if _, err := st.Write([]byte{wire.KindData}); err == nil {
		err = wire.WriteJSONLine(st, wire.Open{IP: target.IP, Port: int(dst.Port())})
	}
	status := []byte{wire.StatusFailed}
	if err == nil {
		_ = st.SetReadDeadline(time.Now().Add(15 * time.Second))
		_, err = st.Read(status)
		_ = st.SetReadDeadline(time.Time{})
	}
	if err != nil || status[0] != wire.StatusOK {
		log.Printf("%s:%d (%s %s) refused by site, status %d", dst.Addr(), dst.Port(), site, target.IP, status[0])
		st.Close()
		c.Close()
		return
	}
	wire.Pipe(c, st)
}

func iptables(prefix netip.Prefix, port int) error {
	for _, chain := range []string{"PREROUTING", "OUTPUT"} {
		rule := []string{"-t", "nat", "-C", chain, "-d", prefix.String(), "-p", "tcp", "-j", "REDIRECT", "--to-ports", strconv.Itoa(port)}
		if exec.Command("iptables", rule...).Run() == nil {
			continue
		}
		rule[2] = "-A"
		if out, err := exec.Command("iptables", rule...).CombinedOutput(); err != nil {
			return fmt.Errorf("iptables %s: %v %s", chain, err, out)
		}
	}
	return nil
}

func main() {
	listen := flag.String("listen", ":8080", "site WebSocket listener (behind the reverse proxy)")
	overlayPort := flag.Int("overlay-port", 7000, "redirect target port for overlay TCP")
	api := flag.String("api", "127.0.0.1:7001", "local API: GET /printers")
	prefixS := flag.String("prefix", "198.18.0.0/15", "overlay prefix")
	keyFile := flag.String("key", "hub.key", "hub private key file")
	pairedS := flag.String("paired", "", "paired sites, name=pubkey,name=pubkey")
	showKey := flag.Bool("show-key", false, "print the hub public key and exit")
	flag.Parse()

	priv, err := wire.LoadOrCreateKey(*keyFile)
	if err != nil {
		log.Fatal(err)
	}
	if *showKey {
		fmt.Println(wire.PubString(priv.Public().(ed25519.PublicKey)))
		return
	}
	prefix, err := netip.ParsePrefix(*prefixS)
	if err != nil {
		log.Fatal(err)
	}
	h := &hub{priv: priv, paired: map[string]string{}, prefix: prefix.Masked(),
		byAddr: map[netip.Addr]*overlayEntry{}, byKey: map[string]netip.Addr{}, session: map[string]*yamux.Session{}}
	for _, item := range strings.Split(*pairedS, ",") {
		if name, k, ok := strings.Cut(strings.TrimSpace(item), "="); ok {
			if _, err := wire.ParsePub(k); err != nil {
				log.Fatalf("paired %s: %v", name, err)
			}
			h.paired[k] = name
		}
	}
	if err := iptables(h.prefix, *overlayPort); err != nil {
		log.Fatal(err)
	}
	ln, err := net.ListenTCP("tcp4", &net.TCPAddr{Port: *overlayPort})
	if err != nil {
		log.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.AcceptTCP()
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					return
				}
				continue
			}
			go h.overlayConn(c)
		}
	}()
	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/printers", func(w http.ResponseWriter, _ *http.Request) {
			h.mu.Lock()
			out := make([]overlayEntry, 0, len(h.byAddr))
			for _, e := range h.byAddr {
				out = append(out, *e)
			}
			h.mu.Unlock()
			sort.Slice(out, func(i, j int) bool { return out[i].Overlay < out[j].Overlay })
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(out)
		})
		log.Fatal(http.ListenAndServe(*api, mux))
	}()
	mux := http.NewServeMux()
	mux.Handle("/link", h)
	log.Printf("hub key %s; overlay %s", wire.PubString(priv.Public().(ed25519.PublicKey)), h.prefix)
	log.Fatal(http.ListenAndServe(*listen, mux))
}
