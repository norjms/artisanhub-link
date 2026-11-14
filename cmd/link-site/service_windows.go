// SPDX-License-Identifier: AGPL-3.0-only

//go:build windows

package main

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows/svc"
)

const serviceName = "ArtisanHubLink"

// The log is kept beside the settings and started again past this size.
const maxLog = 5 << 20

type handler struct{ run func(context.Context) }

func (h handler) Execute(_ []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		h.run(ctx)
		close(done)
	}()
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				cancel()
				select {
				case <-done:
				case <-time.After(10 * time.Second):
				}
				return false, 0
			}
		case <-done:
			// The site stopped by itself: exit with an error so Windows
			// restarts the service.
			return false, 1
		}
	}
}

// runAsService runs the site under the Windows service manager when Windows
// started it as a service, and reports whether it did.
func runAsService(run func(context.Context)) bool {
	is, err := svc.IsWindowsService()
	if err != nil || !is {
		return false
	}
	if dir := os.Getenv("ProgramData"); dir != "" {
		path := filepath.Join(dir, "ArtisanHub Link", "link-site.log")
		if fi, err := os.Stat(path); err == nil && fi.Size() > maxLog {
			_ = os.Truncate(path, 0)
		}
		if f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
			log.SetOutput(f)
		}
	}
	if err := svc.Run(serviceName, handler{run}); err != nil {
		log.Printf("service: %v", err)
	}
	return true
}
