# ArtisanHub Link

ArtisanHub Link connects an ArtisanHub hub to 3D printers on another network.
Use it when the hub runs in the cloud and the printers are at home, or when one
hub runs a farm spread over several sites.

Link has two parts:

- **Link Hub** runs next to the hub. The ArtisanHub installer sets it up when
  you choose to.
- **Link Site** runs on one always-on computer at each place with printers.
  It finds the printers on that network and carries printer traffic to the hub.

The site connector only makes outgoing connections (HTTPS on port 443). Nothing
on the printer network is opened to the internet, and no router or firewall
changes are needed.

Printers keep working the way they do on a local network: control, file upload,
and camera all pass through, so the farm and OrcaSlicer talk to each printer
with its own protocol.

## Status

Design and prototype. The prototype in this repository has carried real
traffic from printers at a home network to a hub in Oracle Cloud. See
[docs/spike.md](docs/spike.md) for what was tested and what it found.

Installers are not published yet.

## Documents

- [How Link works](docs/design.md): pairing, the connection, addresses,
  discovery, security, updates, packaging, and the installer flow.
- [Prototype results](docs/spike.md)

## Platforms (planned)

| Platform | Package |
| --- | --- |
| Windows 10 and 11, x64 and Arm | Signed MSI |
| macOS 12 and later, Intel and Apple silicon | Signed and notarized pkg |
| Linux x86-64 and arm64 (Debian, Ubuntu, Raspberry Pi OS, Fedora) | .deb, .rpm, apt and dnf repository |
| Docker, amd64 and arm64 | ghcr.io/norjms/artisanhub-link |

Installing the site connector needs administrator rights, because it runs as a
system service that starts with the computer.

Link replaces the OpenPrintHQ client app and broker for reaching printers on
another network. See [section 12 of the design](docs/design.md#12-replacing-the-openprinthq-client-app-and-broker).

## License

GNU Affero General Public License v3.0 only. See [LICENSE](LICENSE).

## Trying the preview on Windows

Releases are on the [Releases page](https://github.com/norjms/artisanhub-link/releases). Pairing by
code is not built yet, so the preview is set up with the hub's address and key:

```powershell
msiexec /i ArtisanHubLink-<version>-x64.msi HUBURL=wss://<your hub>/link HUBKEY=<hub key>
& "C:\Program Files\ArtisanHub Link\link-site.exe" -config "C:\ProgramData\ArtisanHub Link\config.ini" -show-key
```

The second command prints this site's key, which the hub needs. The service is called
**ArtisanHub Link** and keeps its log in `C:\ProgramData\ArtisanHub Link`.

## Code signing

Free code signing provided by [SignPath.io](https://about.signpath.io/), certificate by
[SignPath Foundation](https://signpath.org/). See the [code signing policy](CODE_SIGNING.md) and
[privacy](PRIVACY.md).

## Building the prototype

Go 1.25 or later:

```sh
go build ./cmd/link-hub
go build ./cmd/link-site
```
