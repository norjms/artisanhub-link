// SPDX-License-Identifier: AGPL-3.0-only

// Package wire holds what both sides share: keys, the Noise handshake, pairing
// messages, and the stream header.
package wire

import (
	"bufio"
	"encoding/json"
	"io"
)

// Printer is one printer the site found.
type Printer struct {
	IP     string `json:"ip"`
	Family string `json:"family"` // bambu, moonraker, octoprint, prusalink, elegoo
	Name   string `json:"name,omitempty"`
	Model  string `json:"model,omitempty"`
	Serial string `json:"serial,omitempty"`
	Via    string `json:"via"` // ssdp, mdns, static
}

// Open is the first line of every data stream the hub opens.
type Open struct {
	IP   string `json:"ip"`
	Port int    `json:"port"`
}

// Stream status bytes sent by the site after an Open.
const (
	StatusOK     byte = 0
	StatusDenied byte = 1
	StatusFailed byte = 2
)

// Stream kinds, the first byte of a stream.
const (
	KindData    byte = 'D'
	KindPrinter byte = 'P'
)

// WriteJSONLine writes v as one JSON line.
func WriteJSONLine(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// ReadJSONLine reads one JSON line (max 64 KB) into v.
func ReadJSONLine(r *bufio.Reader, v any) error {
	line, err := r.ReadSlice('\n')
	if err != nil {
		return err
	}
	return json.Unmarshal(line, v)
}

// Pipe copies both ways until either side closes.
func Pipe(a, b io.ReadWriteCloser) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(a, b); done <- struct{}{} }()
	go func() { _, _ = io.Copy(b, a); done <- struct{}{} }()
	<-done
	_ = a.Close()
	_ = b.Close()
	<-done
}

// PairRequest is what a site sends to /link/pair with the one-time code.
type PairRequest struct {
	Code      string `json:"code"`
	Name      string `json:"name"`
	PublicKey string `json:"public_key"`
	Version   string `json:"version,omitempty"`
}

// PairResponse is the hub's answer: its key to pin, and the fingerprint both show.
type PairResponse struct {
	SiteID      string `json:"site_id"`
	HubKey      string `json:"hub_public_key"`
	Fingerprint string `json:"fingerprint"`
}

// Hello is the first thing a site sends on its printer stream: who it is.
type Hello struct {
	Version string `json:"version"`
}
