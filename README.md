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

## Install

**One-liner (recommended)** — detects your OS/arch, verifies SHA256,
strips the macOS Gatekeeper quarantine xattr, and installs to the
current directory:

```bash
curl -sSL https://raw.githubusercontent.com/venkatamutyala/pxe-beacon/main/install.sh | sh
```

Pin a version or override the install dir:

```bash
curl -sSL https://raw.githubusercontent.com/venkatamutyala/pxe-beacon/main/install.sh | sh -s -- --version v0.1.2
curl -sSL https://raw.githubusercontent.com/venkatamutyala/pxe-beacon/main/install.sh | sh -s -- --dir /usr/local/bin
```

Or pick from the [GitHub Releases page](https://github.com/venkatamutyala/pxe-beacon/releases)
manually (`linux-amd64`, `linux-arm64`, `darwin-arm64`; SHA256SUMS
alongside).

**Build from source:**

```bash
git clone https://github.com/venkatamutyala/pxe-beacon
cd pxe-beacon
make                  # local build → ./pxe-beacon
make test             # run the test suite
make cross            # → dist/pxe-beacon-{linux-amd64,linux-arm64,darwin-arm64}
```

No runtime dependencies; the iPXE binaries are embedded via `go:embed`
(see `internal/assets/ipxe/VERSIONS.md` for provenance).

To cut a release: tag and push (`git tag v0.1.0 && git push origin v0.1.0`)
— the `release` workflow runs `go test`, cross-compiles the three
binaries with the tag stamped into `-version`, computes `SHA256SUMS`,
and uploads everything to a GitHub Release with auto-generated notes.

---

## Run — single machine (v0.1 mode)

```bash
sudo ./pxe-beacon                            # auto-detect interface, boot netboot.xyz menu
sudo ./pxe-beacon -interface eth0            # pin an interface
sudo ./pxe-beacon -advertise-ip 192.168.1.10 # override advertised IP
```

Every PXE client on the LAN gets the netboot.xyz menu. Same behavior as v0.1.3.

---

## The minimal v0.5 example

```bash
# One file, one machine, no side-files needed:
cat > ./fleet.yaml <<'EOF'
machines:
  - mac: 58:47:ca:70:c7:c9
    name: venkat-1
    boot: debian-12
EOF

sudo ./pxe-beacon -config ./fleet.yaml
```

That's it. pxe-beacon serves an embedded default preseed (user `pxe`,
password `pxe` — insecure default, change before production) and an
embedded default cloud-init (just phone_home so `/status` shows
`installer-done` when the install finishes). Power-cycle your test
client and walk away.

Edit anything via the admin UI at `http://127.0.0.1:8080/admin`
(loopback only — use `ssh -L 8080:localhost:8080` if remote). Or
hand-edit `fleet.yaml` and SIGHUP-reload (`kill -HUP $(pgrep -x pxe-beacon)`).
Hand-edits preserve comments; UI edits don't.

## Run — fleet mode (v0.2)

Drop a `fleet.yaml` next to the binary describing your machines and
point pxe-beacon at it with `-config`. Each MAC gets its own boot
profile + cloud-init; pxe-beacon serves them automatically and a live
status page shows the rack provisioning at <http://server:8080/status>.

```bash
sudo ./pxe-beacon -config /etc/pxe-beacon/fleet.yaml
```

Minimal `fleet.yaml`:

```yaml
defaults:
  boot: menu        # unknown MACs → netboot.xyz menu (same as v0.1.3)

machines:
  - mac: 58:47:ca:70:c7:c9
    name: kube-1
    boot: ubuntu-22.04
    cloud_init: ./kube-1.yaml      # cloud-init user-data, Go-templated

  - mac: aa:bb:cc:dd:ee:01
    name: db-primary
    boot: debian-12
    cloud_init: ./db-primary.yaml

  - mac: 11:22:33:44:55:66
    name: rescue
    boot: custom
    ipxe_script: ./rescue.ipxe     # raw iPXE for anything not in the built-in list
```

Built-in `boot:` values:

| value          | side-files required                    | one-time setup                              | what happens                                                                 |
|----------------|----------------------------------------|---------------------------------------------|------------------------------------------------------------------------------|
| `menu`         | —                                      | —                                           | netboot.xyz interactive menu (default for unknown MACs)                      |
| `ubuntu-22.04` | `cloud_init:`                          | `pxe-beacon fetch ubuntu-22.04`             | Subiquity autoinstall via cloud-init — fully unattended                      |
| `ubuntu-24.04` | `cloud_init:`                          | `pxe-beacon fetch ubuntu-24.04`             | Subiquity autoinstall via cloud-init — fully unattended                      |
| `debian-12`    | `preseed:` (+optional `cloud_init:`)   | —                                           | unattended d-i via preseed; if `cloud_init:` is also set, cloud-init runs on first boot of the installed system (auto-bridged) |
| `debian-13`    | `preseed:` (+optional `cloud_init:`)   | —                                           | same as `debian-12` but Trixie's kernel/initrd                               |
| `custom`       | `ipxe_script:`                         | —                                           | serve the operator-provided iPXE script verbatim (Go-templated)              |

**`pxe-beacon fetch <target>`** is a one-time-per-distro operator
step. Ubuntu's Subiquity kernel + initrd + filesystem.squashfs only
live inside the live-server ISO; fetch downloads the ISO (~1.5 GB),
extracts those three files into `-data-dir` (default
`~/.local/share/pxe-beacon`), and writes a manifest with SHA-256s.
Idempotent — re-running is a no-op unless `-force` is passed. After
fetching, pxe-beacon serves them over `/assets/<target>/<file>`. No
root needed for the fetch step.

```bash
pxe-beacon fetch ubuntu-22.04
# fetch: target = ubuntu-22.04
# fetch: source = https://releases.ubuntu.com/22.04/ubuntu-22.04.5-live-server-amd64.iso
# fetch: downloading ISO (this is ~1.5GB, several minutes on most links)…
# fetch: 1.4 GB / 1.4 GB (100.0%)
# fetch: extracted:
#   ubuntu-22.04/vmlinuz                 14.0 MB   sha256=8a2b3c4d5e6f…
#   ubuntu-22.04/initrd                  74.2 MB   sha256=1234567890ab…
#   ubuntu-22.04/filesystem.squashfs      1.3 GB   sha256=abcdef012345…
# fetch: done.
```

**Debian preseed + cloud-init bridge.** Debian's d-i installer doesn't
read cloud-init / NoCloud (verified May 2026 against Trixie's
initrd — no cloud-init artifacts anywhere). To install Debian
unattended you need a `preseed.cfg`. If you *also* provide a
`cloud_init:` file, pxe-beacon appends a `late_command` to the served
preseed that:
1. apt-installs `cloud-init` on the target,
2. drops your user-data + meta-data into `/var/lib/cloud/seed/nocloud/`,
3. enables `cloud-init.service` so it runs on first boot.

That gives you "one cloud-init file" semantics on Debian even though
the installer itself is preseed-driven. See
[`examples/debian-preseed.cfg`](./examples/debian-preseed.cfg) for a
starting template — copy, edit user/password/disk to match your fleet
policy, then reference it from `fleet.yaml`.

A full working example lives in [`fleet.example.yaml`](./fleet.example.yaml)
with cloud-init templates under [`examples/`](./examples/). Try it on
loopback (no real PXE clients needed, just verifies the HTTP routes):

```bash
make demo-fleet
# in another terminal:
curl http://127.0.0.1:8080/status.json | jq
curl http://127.0.0.1:8080/autoinstall/58-47-ca-70-c7-c9/user-data
```

**Live config reload:** edit `fleet.yaml`, then `kill -HUP $(pgrep -x pxe-beacon)`.
No restart needed — the next OFFER picks up the new config.

**Cloud-init phone_home:** the example user-data files include a
`phone_home` block that tells cloud-init to POST to pxe-beacon when
the install finishes. That transition flips the machine to
`installer-done` on the status page — so you can see "did kube-3 ever
finish?" from your browser without IPMI.

### Status page

`http://<advertised-ip>:8080/status` shows a live, auto-refreshing
table of every machine: pending → firmware-dhcp → firmware-fetched →
ipxe-dhcp → user-data-fetched → installer-done. Machines stuck > 5
minutes get a ⚠ stalled flag. JSON version at `/status.json`.

### Flags

| flag             | default                                       | what                                                   |
|------------------|-----------------------------------------------|--------------------------------------------------------|
| `-config`        | (unset → single-machine mode)                 | path to `fleet.yaml` — enables per-MAC routing         |
| `-interface`     | (auto)                                        | network interface to advertise                         |
| `-listen`        | `0.0.0.0`                                     | address to bind UDP sockets                            |
| `-advertise-ip`  | (auto, from `-interface`)                     | override the IPv4 sent to clients                      |
| `-http-port`     | `8080`                                        | HTTP port (also the status page port)                  |
| `-tftp-listen`   | `0.0.0.0:69`                                  | TFTP listen address                                    |
| `-chain-url`     | `https://boot.netboot.xyz/menu.ipxe`          | URL the legacy `/boot.ipxe` chain script chainloads    |
| `-ipxe-script`   | (embedded)                                    | path to a custom `/boot.ipxe` template                 |
| `-crosscert`     | off                                           | emit `set crosscert` (older iPXE + HTTPS chain target) |
| `-hint-after`    | `10s`                                         | fire the "client never fetched" hint after this        |
| `-data-dir`      | `~/.local/share/pxe-beacon`                   | dir holding `pxe-beacon fetch` output, served at `/assets/` |
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

## Threat model — lab / trusted-LAN only

PXE booting is fundamentally trust-the-network. The protocol has no
authentication: any device on the same L2 broadcast domain can race
DHCP responses, redirect TFTP filenames, or substitute boot files.
`pxe-beacon` is designed for **trusted LANs** — your home lab, a
dedicated provisioning VLAN at the office, a controlled datacenter
rack — not arbitrary corporate Wi-Fi or shared networks.

Specific risks if you ignore this:

- **Rogue proxyDHCP racing**: an attacker on the LAN can answer
  faster than `pxe-beacon` and redirect clients to attacker-controlled
  iPXE/kernel.
- **TFTP / HTTP without integrity**: the iPXE binary, autoexec.ipxe
  dispatch script, and preseed.cfg all travel over the wire
  unauthenticated. On-path tampering is possible.
- **No UEFI SecureBoot support**: vanilla iPXE shipped by
  `pxe-beacon` is unsigned. On SecureBoot-enabled clients it will
  refuse to load.
- **Unauthenticated cloud-init phone-home**: any LAN client can
  POST `/autoinstall/<mac>/done` and flip a machine's state.

For production fleet management on an untrusted segment, run
`pxe-beacon` on a dedicated VLAN that's only reachable from the
machines you intend to provision.

## License & attribution

`pxe-beacon` itself is MIT-licensed (see [LICENSE](./LICENSE)).

The embedded iPXE binaries are GPLv2+. v0.6.0+ ships **vanilla
upstream iPXE** (no `EMBED`), built from
<https://github.com/ipxe/ipxe>. Source pin + reproducibility notes
are in
[`internal/assets/ipxe/VERSIONS.md`](./internal/assets/ipxe/VERSIONS.md).
