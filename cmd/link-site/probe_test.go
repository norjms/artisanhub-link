// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"net"
	"testing"

	"github.com/miekg/dns"
)

// A Buddy printer answers a reverse mDNS lookup with its name, and the probe names it by that.
func TestAPrusaLinkPrinterIsNamedByItsOwnMDNSAnswer(t *testing.T) {
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		if r.Question[0].Name == "1.0.0.127.in-addr.arpa." && r.Question[0].Qtype == dns.TypePTR {
			m.Answer = append(m.Answer, &dns.PTR{
				Hdr: dns.RR_Header{Name: r.Question[0].Name, Rrtype: dns.TypePTR, Class: dns.ClassINET, Ttl: 10},
				Ptr: "prusa-mk4.local.",
			})
		}
		_ = w.WriteMsg(m)
	})}
	go func() { _ = srv.ActivateAndServe() }()
	defer func() { _ = srv.Shutdown() }()
	if got := mdnsName(context.Background(), pc.LocalAddr().String()); got != "prusa-mk4" {
		t.Errorf("mdnsName = %q, want prusa-mk4", got)
	}
	// Nothing answering: no name, so the probe keeps calling it PrusaLink.
	closed, _ := net.ListenPacket("udp4", "127.0.0.1:0")
	addr := closed.LocalAddr().String()
	closed.Close()
	if got := mdnsName(context.Background(), addr); got != "" {
		t.Errorf("mdnsName with nothing answering = %q, want empty", got)
	}
}
