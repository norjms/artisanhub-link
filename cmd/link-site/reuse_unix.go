// SPDX-License-Identifier: AGPL-3.0-only

//go:build !windows

package main

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// shareAddress lets the discovery sockets share their ports with other programs on the same machine that
// listen for the same announcements (Bambu Studio, other printer tools): each gets its own copy.
func shareAddress(_, _ string, c syscall.RawConn) error {
	var serr error
	err := c.Control(func(fd uintptr) {
		if serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); serr != nil {
			return
		}
		serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
	})
	if err != nil {
		return err
	}
	return serr
}
