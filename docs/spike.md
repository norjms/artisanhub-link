# Prototype results

Date: 13 November 2026.

## Setup

- **Hub:** the ArtisanHub beta in Oracle Cloud (arm64, Always Free), reached at
  its public name over HTTPS. Link Hub ran as a container on the hub's network
  with an iptables redirect for the overlay range, and one host route.
- **Site:** Link Site on a Linux machine at home (amd64), behind an ordinary
  home router with no port forwards for Link.
- **Tests** ran inside the farm engine container's own network, so they
  connected exactly as the farm does.
- Bambu access codes were not used, so each protocol was taken as far as its
  login. A real login answer proves the whole path to the printer's service.

## Discovery

| Printer | Found by | Result |
| --- | --- | --- |
| Bambu Lab H2C | SSDP | Found with name, model and serial |
| Bambu Lab H2D | SSDP | Found with name, model and serial |
| Prusa MK3S with OctoPrint | mDNS | Found |
| Klipper (simulated Moonraker) | Added by hand | Reached |
| Two NVIDIA devices and two NAS boxes | mDNS | Wrongly reported by the first build. Fixed by checking the service name and the port before reporting. |

The Voron and the MK4S were not on the network during the test.

## Traffic from the cloud to the printers

| Test | Result |
| --- | --- |
| H2C MQTT (8883) | TLS in 142 ms, broker answered CONNACK "not authorised" |
| H2C FTPS (990) | `220 (vsFTPd 3.0.5)`, then `530 Login incorrect` |
| H2C camera RTSPS (322) | TLS in 126 ms, `RTSP/1.0 200 OK` |
| H2C camera (6000) | TLS handshake in 123 ms |
| H2D MQTT, RTSPS, 6000 | Same answers; TLS about 900 ms |
| H2D FTPS (990) | No answer. The same happens on the home network with no Link in the path, so it is the printer, not Link. |
| Moonraker (7125) | `200 OK` with the printer state, 20 of 20 requests, 48 ms each |
| OctoPrint (80) | `403` from the OctoPrint API (needs a key), as expected |
| Bulk download from OctoPrint | 15.6 to 19 Mbit/s for 0.7 and 1.7 MB files |

The H2D's TLS takes about 1 second both through Link and directly on the home
network (the H2C takes about 110 ms directly), so Link adds little delay.

## Refused, as designed

| Request from the hub | Result |
| --- | --- |
| SSH (22) on the H2D | Refused by the site |
| SSH (22) on the Moonraker host | Refused by the site |
| An overlay address with no printer | Closed by the hub |

## Problems found and fixed in the prototype

1. mDNS returned devices that are not printers. Fix: match the service name
   and test the port before reporting.
2. Two copies of the same site knocked each other off every few seconds.
   Design: the hub keeps the first connection and flags the duplicate.
3. A restarted site could not listen for SSDP while an old copy held the
   ports. Fix: keep retrying.
4. The hub kept printers the site no longer reported. Fix: each report is the
   full list; missing printers are offline.
5. Overlay addresses changed when the hub restarted. Design: keep them per
   site and printer serial.
6. The first range, `100.96.0.0/16`, sits inside Tailscale's range. Design:
   use `198.18.0.0/15`.
