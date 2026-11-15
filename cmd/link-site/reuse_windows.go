// SPDX-License-Identifier: AGPL-3.0-only

//go:build windows

package main

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// shareAddress lets the discovery sockets share their ports with other programs on the same machine that
// listen for the same announcements (Bambu Studio, other printer tools).
func shareAddress(_, _ string, c syscall.RawConn) error {
	var serr error
	err := c.Control(func(fd uintptr) {
		serr = windows.SetsockoptInt(windows.Handle(fd), windows.SOL_SOCKET, windows.SO_REUSEADDR, 1)
	})
	if err != nil {
		return err
	}
	return serr
}
