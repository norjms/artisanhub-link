// SPDX-License-Identifier: AGPL-3.0-only

// link-hub runs in the hub's stack. Paired sites dial in over a WebSocket behind the hub's reverse proxy
// (/link), with Noise IK inside it. Each printer a site reports gets an overlay address that stays the same
// (198.18.0.0/15 by default); TCP sent to that address is redirected here by iptables in this container's
// own network namespace and carried to the site. Sites pair with a one-time code from the hub's API.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
	"unsafe"

	"github.com/norjms/artisanhub-link/internal/wire"
)

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

func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func main() {
	listen := flag.String("listen", env("LINK_LISTEN", ":8080"), "site listener, behind the reverse proxy (/link, /link/pair)")
	api := flag.String("api", env("LINK_API", ":7001"), "the hub's own API (status, pairing codes), on the stack network only")
	overlayPort := flag.Int("overlay-port", 7000, "where iptables sends overlay TCP")
	prefixS := flag.String("prefix", env("LINK_PREFIX", "198.18.0.0/15"), "overlay range")
	dataDir := flag.String("data", env("LINK_DATA", "/data"), "where the hub key and paired sites are kept")
	showKey := flag.Bool("show-key", false, "print the hub public key and exit")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}

	static, err := wire.LoadOrCreateKey(filepath.Join(*dataDir, "hub.key"))
	if err != nil {
		log.Fatal(err)
	}
	if *showKey {
		fmt.Println(wire.PubString(static.Public))
		return
	}
	token := os.Getenv("LINK_ADMIN_TOKEN")
	if token == "" {
		log.Fatal("LINK_ADMIN_TOKEN is not set: the hub's API needs it")
	}
	prefix, err := netip.ParsePrefix(*prefixS)
	if err != nil || !prefix.Addr().Is4() {
		log.Fatalf("overlay range %q is not an IPv4 prefix", *prefixS)
	}
	h, err := newHub(static, *dataDir, prefix)
	if err != nil {
		log.Fatal(err)
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
		srv := &http.Server{Addr: *api, Handler: h.apiHandler(token, os.Getenv("LINK_STATUS_TOKEN")), ReadHeaderTimeout: 10 * time.Second}
		log.Fatal(srv.ListenAndServe())
	}()
	log.Printf("OpenPrintHQ Link hub %s; key %s; overlay %s; %d paired sites", version, wire.PubString(static.Public), h.prefix, len(h.st.Sites))
	srv := &http.Server{Addr: *listen, Handler: h.publicHandler(), ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
