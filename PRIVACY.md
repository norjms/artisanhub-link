# Privacy

OpenPrintHQ Link collects no personal data and has no telemetry.

## What the site connector sends, and where

- **To the hub you pair it with, and nowhere else:** the list of printers it finds on your network
  (address, family, name, model and serial number as the printer announces them), and the printer
  traffic the hub asks for, such as status, file uploads and camera streams. The hub is your own
  OpenPrintHQ installation, or one you chose to use.
- **On your local network:** it listens for printers announcing themselves (SSDP and mDNS) and asks
  for them. These messages stay on the local network.
- **To GitHub, when automatic updates are on:** a request for the latest release, once a day, and the
  download of a new release. GitHub sees the request as it would any download. Automatic updates can
  be turned off when installing, or later in the hub, for each site. Version 0.1 does not update
  itself yet.

## What stays on the computer

The settings, the site's private key and a log, in a folder only the service and administrators can
read (`%ProgramData%\ArtisanHub Link` on Windows, `/var/lib/artisanhub-link` on Linux,
`/Library/Application Support/ArtisanHub Link` on macOS). The log holds printer addresses and
connection events, never printer passwords or access codes, which the site connector never sees.

## Removing it

Uninstalling removes the program, its service, its settings, its key and its log.
