package main

import (
	"context"
	"github.com/norjms/artisanhub-link/internal/wire"
	"net"
	"net/http"
	"runtime"
	"testing"
)

func TestHostNames(t *testing.T) {
	for in, want := range map[string]string{"mk3s01.local.": "mk3s01", "voron.local": "voron", "printer": "printer"} {
		if got := hostName(in); got != want {
			t.Errorf("hostName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPrusaLinkIsRecognised(t *testing.T) {
	h := http.Header{}
	h.Set("WWW-Authenticate", `Digest realm="Printer API", nonce="1", stale=false`)
	if !isPrusaLink(401, h, "") {
		t.Fatal("PrusaLink's sign-in was not recognised")
	}
	if !isPrusaLink(200, http.Header{}, `{"server":"2.1.2","text":"PrusaLink"}`) {
		t.Fatal("PrusaLink's version answer was not recognised")
	}
	other := http.Header{}
	other.Set("WWW-Authenticate", `Basic realm="router"`)
	if isPrusaLink(401, other, "") || isPrusaLink(200, http.Header{}, `{"text":"OctoPrint"}`) {
		t.Fatal("something else was taken for PrusaLink")
	}
}

func TestOnlyTheSitesOwnSmallNetworksAreProbed(t *testing.T) {
	for cidr, want := range map[string]bool{
		"192.168.1.181/24": true, "10.0.5.2/28": true,
		"172.17.0.1/16": false, "192.168.80.1/20": false, "8.8.8.8/24": false, "fe80::1/64": false,
	} {
		if _, ok := ownPrefix(cidr); ok != want {
			t.Errorf("ownPrefix(%q) = %v, want %v", cidr, ok, want)
		}
	}
	for name, want := range map[string]bool{"eth0": false, "br-6b66ad456984": true, "docker0": true, "veth12ab": true, "wlan0": false} {
		if skipped(name) != want {
			t.Errorf("skipped(%q) = %v, want %v", name, !want, want)
		}
	}
}

func TestDiscoverySharesItsPort(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SO_REUSEPORT is not on Windows")
	}
	lc := net.ListenConfig{Control: shareAddress}
	a, err := lc.ListenPacket(context.Background(), "udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := lc.ListenPacket(context.Background(), "udp4", a.LocalAddr().String())
	if err != nil {
		t.Fatalf("a second listener on the same port was refused: %v", err)
	}
	b.Close()
}

func TestASerialAddedLaterIsKept(t *testing.T) {
	s := newSite()
	s.add(wireBambu("192.168.1.60", "", "static"))
	s.add(wireBambu("192.168.1.60", "01P00A1", "ssdp"))
	s.add(wireBambu("192.168.1.60", "", "ssdp"))
	p := s.list()[0]
	if p.Serial != "01P00A1" || p.Via != "static" {
		t.Fatalf("got %+v", p)
	}
}

func wireBambu(ip, serial, via string) wire.Printer {
	return wire.Printer{IP: ip, Family: "bambu", Serial: serial, Via: via}
}
