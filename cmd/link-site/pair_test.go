package main

import "testing"

func TestHubAddresses(t *testing.T) {
	cases := map[string]string{
		"hub.example.com":               "wss://hub.example.com/link",
		"https://hub.example.com/":      "wss://hub.example.com/link",
		" https://hub.example.com/x/y ": "wss://hub.example.com/link",
		"http://10.0.0.5:8080":          "ws://10.0.0.5:8080/link",
	}
	for in, want := range cases {
		got, err := linkAddress(in)
		if err != nil || got != want {
			t.Errorf("linkAddress(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "ftp://hub.example.com", "https://"} {
		if _, err := linkAddress(bad); err == nil {
			t.Errorf("linkAddress(%q) should fail", bad)
		}
	}
}
