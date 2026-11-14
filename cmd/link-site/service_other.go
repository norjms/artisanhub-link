// SPDX-License-Identifier: AGPL-3.0-only

//go:build !windows

package main

import "context"

// runAsService reports whether the program ran as a Windows service. On other
// systems the service manager (systemd, launchd) runs it as a normal program.
func runAsService(func(context.Context)) bool { return false }
