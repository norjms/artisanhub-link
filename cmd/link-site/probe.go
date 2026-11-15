// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/norjms/artisanhub-link/internal/wire"
)

// Prusa printers on the Buddy firmware (MK4, MK4S, Core One, XL, MINI) serve PrusaLink but announce nothing:
// no mDNS service and no SSDP. So the site asks each address of its own network, every probeEvery, on port
// 80 only, whether PrusaLink answers there. PrusaLink asks for a sign-in with the realm "Printer API", or
// names itself in /api/version.
const probeEvery = 10 * time.Minute

// Interfaces that are not the site's own network: container and VM bridges.
var skipInterfaces = []string{"docker", "br-", "veth", "virbr", "cni", "flannel", "tailscale", "zt", "wg", "lo"}

// probeNetworks are the site's own private IPv4 networks of /24 or smaller.
func probeNetworks() []netip.Prefix {
	var out []netip.Prefix
	ifs, err := net.Interfaces()
	if err != nil {
		return nil
	}
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || skipped(ifc.Name) {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if p, ok := ownPrefix(a.String()); ok {
				out = append(out, p)
			}
		}
	}
	return out
}

func skipped(name string) bool {
	for _, prefix := range skipInterfaces {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func ownPrefix(cidr string) (netip.Prefix, bool) {
	p, err := netip.ParsePrefix(cidr)
	if err != nil || !p.Addr().Is4() || !p.Addr().IsPrivate() || p.Bits() < 24 || p.Bits() > 30 {
		return netip.Prefix{}, false
	}
	return p.Masked(), true
}

// isPrusaLink reads an answer to GET /api/version.
func isPrusaLink(status int, header http.Header, body string) bool {
	if status == http.StatusUnauthorized && strings.Contains(header.Get("WWW-Authenticate"), `realm="Printer API"`) {
		return true
	}
	return status == http.StatusOK && strings.Contains(strings.ToLower(body), "prusalink")
}

func (s *site) probePrusaLink(ctx context.Context) {
	client := &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	probe := func(ip string) {
		c, err := net.DialTimeout("tcp4", net.JoinHostPort(ip, "80"), 700*time.Millisecond)
		if err != nil {
			return
		}
		c.Close()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+ip+"/api/version", nil)
		resp, err := client.Do(req)
		if err != nil {
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if isPrusaLink(resp.StatusCode, resp.Header, string(body)) {
			s.add(wire.Printer{IP: ip, Family: "prusalink", Name: "PrusaLink", Via: "probe"})
		}
	}
	for {
		nets := probeNetworks()
		if len(nets) == 0 {
			log.Printf("probe: no local network of /24 or smaller to look for PrusaLink printers on")
		}
		for _, n := range nets {
			var wg sync.WaitGroup
			sem := make(chan struct{}, 32)
			for a := n.Addr().Next(); n.Contains(a) && a.Next().IsValid() && n.Contains(a.Next()); a = a.Next() {
				if ctx.Err() != nil {
					return
				}
				sem <- struct{}{}
				wg.Add(1)
				go func(ip string) {
					defer func() { <-sem; wg.Done() }()
					probe(ip)
				}(a.String())
			}
			wg.Wait()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(probeEvery):
		}
	}
}
