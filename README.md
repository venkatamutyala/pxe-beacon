# pxe-beacon

A single self-contained Go binary that network-boots machines on a LAN.
It bundles a proxyDHCP responder, a TFTP server, and an HTTP server,
with the iPXE bootloader embedded inside. Default behavior: chainload
[netboot.xyz](https://netboot.xyz) with zero configuration.

**North-star:** be the PXE server that is *actually debuggable*. Every
log line names the boot stage, echoes the parsed DHCP options, says
what was served (and how big), and labels benign post-handoff packets
as benign so you don't chase them.

> Repo: <https://github.com/venkatamutyala/pxe-beacon> · Go module:
> `github.com/venkatamutyala/pxe-beacon` · binary: `pxe-beacon` ·
> built on Go 1.23, Linux (amd64/arm64) + macOS (arm64). Windows is
> intentionally not supported.

---

## What it actually does

When a PXE-capable machine powers on with nothing installed, its UEFI
firmware broadcasts a DHCP DISCOVER on the LAN, tagged as a PXE
client. A normal DHCP server assigns the machine an IP. **`pxe-beacon`
is *proxyDHCP*: it does NOT assign IPs.** It listens for the same
DISCOVER, parses the architecture (option 93) and vendor/user class
(options 60/77), and sends back a *boot-only* OFFER that tells the
firmware which file to fetch, from where, over which transport.

The firmware then fetches an iPXE binary (over TFTP or HTTP depending
on its option-93), boots it, and re-DHCPs as `user-class=iPXE`. We
recognize the second pass and serve it the chain *script* — not the
binary — which breaks what would otherwise be a chainload loop. The
script chainloads netboot.xyz (or whatever URL you point it at).

---

## Hard constraints (physics, not preferences)

- **proxyDHCP is broadcast-based.** Must run on the **same L2
  broadcast segment** as the client. Cannot cross routers/subnets.
  Cannot run on a cloud VPS. **Cannot run inside a NAT'd Docker on
  macOS** — the container sits behind a VM and never sees LAN
  broadcasts. Native binary only. (On Linux, `--network host`
  containers work; we ship a binary regardless.)
- **Privileged UDP ports** 67, 69, and 4011 require root, sudo, or
  `CAP_NET_BIND_SERVICE`. The error message says so.
- **TFTP** moves to a random high UDP port after the initial request.
  If you have a firewall, allow ephemeral UDP back to the server.
  Docker-on-macOS breaks this too.

---

## Install / build

```bash
git clone https://github.com/venkatamutyala/pxe-beacon
cd pxe-beacon
make                  # local build → ./pxe-beacon
make test             # run the test suite
make cross            # → dist/pxe-beacon-{linux-amd64,linux-arm64,darwin-arm64}
```

No runtime dependencies; the iPXE binaries are embedded via `go:embed`
(see `internal/assets/ipxe/VERSIONS.md` for provenance).

---

## Run

```bash
sudo ./pxe-beacon                            # auto-detect interface
sudo ./pxe-beacon -interface eth0            # pin an interface
sudo ./pxe-beacon -advertise-ip 192.168.1.10 # override advertised IP
```

Useful flags:

| flag             | default                                       | what                                                   |
|------------------|-----------------------------------------------|--------------------------------------------------------|
| `-interface`     | (auto)                                        | network interface to advertise                         |
| `-listen`        | `0.0.0.0`                                     | address to bind UDP sockets                            |
| `-advertise-ip`  | (auto, from `-interface`)                     | override the IPv4 sent to clients                      |
| `-http-port`     | `8080`                                        | HTTP port for iPXE binary + chain script               |
| `-tftp-listen`   | `0.0.0.0:69`                                  | TFTP listen address                                    |
| `-chain-url`     | `https://boot.netboot.xyz/menu.ipxe`          | URL the chain script chainloads                        |
| `-ipxe-script`   | (embedded)                                    | path to a custom boot.ipxe template                    |
| `-crosscert`     | off                                           | emit `set crosscert` (older iPXE + HTTPS chain target) |
| `-hint-after`    | `10s`                                         | fire the "client never fetched" hint after this        |
| `-loglevel`      | `info`                                        | `error`, `warn`, `info`, `debug`                       |

`./pxe-beacon -help` for the full list. See [`RUN.md`](./RUN.md) for
full operational notes, including the QEMU+OVMF VM test setup, the
`tcpdump` lens, and the troubleshooting table.

---

## What success looks like in the log

```
client 58:47:ca:70:c7:c9 arch=0x07(EFI x86-64) userclass=<none> stage=firmware-TFTP -> decision: serve netboot.xyz.efi via TFTP from 192.168.1.10
TFTP RRQ "netboot.xyz.efi" -> served netboot.xyz.efi (1171456 bytes) ok
client 58:47:ca:70:c7:c9 arch=0x07(EFI x86-64) userclass=iPXE stage=iPXE-script -> decision: serve http://192.168.1.10:8080/boot.ipxe via HTTP from 192.168.1.10
GET /boot.ipxe -> 200, 415 bytes (192.168.1.20:34022)
```

If the client never fetches, after `-hint-after` you get:

```
hint: client 58:47:ca:70:c7:c9 got the OFFER but never fetched within 10s — check same-segment, firewall, and that advertised IP 192.168.1.10 is reachable from the client
```

The "missing option 60" packets that flood once iPXE takes over are
labelled `(benign: client already handed off to iPXE)` so you don't
chase them.

`-loglevel debug` adds hex dumps of every packet sent and received.

---

## Gotchas baked in (from many hours of pain)

- **WiFi causes TFTP timeouts.** The startup banner warns if the
  selected interface looks wireless. Prefer wired.
- **UEFI HTTP boot is picky about `Content-Length`.** We always set
  it; never chunked.
- **netboot.xyz over HTTPS needs a current iPXE cert trust.** If you
  see `Could not verify: Permission denied`, try `-crosscert`.
- **AMI/Phoenix firmware sometimes loses USB keyboard input once iPXE
  takes the bus.** Not our bug. Documented for sanity.
- **"missing option 60 / not a PXE client" is NORMAL** after iPXE has
  taken over — we label these as benign.

See `RUN.md` for the full troubleshooting table and `tcpdump` lens.

---

## Architecture

```
cmd/pxe-beacon/main.go         flags, wiring, banner, signal handling
internal/proxydhcp/
  proxydhcp.go                 BuildOffer(parsedReq) -> reply  (PURE, unit-tested)
  listener.go                  binds 67 + 4011, broadcast socket
  arch.go                      option-93 table + transport decision
internal/tftp/                 pin/tftp serving embedded iPXE
internal/httpd/                net/http serving binary + rendered chain script
internal/assets/               go:embed: netboot.xyz .efi/.kpxe + boot.ipxe template
internal/netinfo/              interface pick + WiFi heuristic
internal/narrlog/              narrated logging (decision/served/benign/hint/hexdump)
RUN.md                         operational notes / M4 manual validation
PROGRESS.md                    milestone-by-milestone implementation log
```

The non-negotiable design rule: **`BuildOffer` is a pure function**.
Parsed request in, reply out. No sockets, no goroutines. Sockets live
only in `listener.go`. This is what makes the primary test loop
(`go test ./...`) faithful to production.

---

## Test tiers

- **Tier 0 (unit, runs everywhere):** `BuildOffer` against synthetic
  DISCOVER inputs across archs, vendor classes, and user classes;
  TFTP/HTTP servers tested in-process. Run via `make test`.
- **Tier 1 (loopback, runs everywhere):** `tftp localhost`,
  `curl localhost:8080`. See `make run-loopback` and the commands in
  `RUN.md`.
- **Milestone gate (M4):** VM or real hardware — proves the
  conversation actually completes through UEFI firmware. **Done
  manually** on machines that can PXE-boot; see `RUN.md` Path A
  (QEMU+OVMF) and Path B (real hardware).

---

## License & attribution

`pxe-beacon` itself is MIT-licensed (see [LICENSE](./LICENSE)).

The embedded netboot.xyz / iPXE binaries are GPLv2+. Their source URLs
and versions are recorded in
[`internal/assets/ipxe/VERSIONS.md`](./internal/assets/ipxe/VERSIONS.md);
upstream is <https://github.com/ipxe/ipxe> and
<https://netboot.xyz>.
