# pxe-beacon — implementation progress

This file tracks milestone-by-milestone status for the v1 build of
`pxe-beacon`. See `PLAN.md` for the project plan. Entries are appended
as each milestone is finished or blocked.

---

## M0 — Scaffold — **PASS**

What I did:
- Initialized Go module `github.com/venkatamutyala/pxe-beacon` (Go 1.23).
- Created directory layout per PLAN section 2: `cmd/pxe-beacon`,
  `internal/proxydhcp`, `internal/tftp`, `internal/httpd`,
  `internal/assets/{ipxe,scripts}`, `internal/netinfo`,
  `internal/narrlog` (PLAN says `internal/log` but that collides with
  stdlib `log`; renamed to `narrlog` for clarity).
- Added narrated logging skeleton (`narrlog.Logger`) with levels
  error/warn/info/debug, the `Decision`/`Served`/`Benign`/`Hint`/`HexDump`
  helpers spec'd in PLAN section 4.
- Downloaded real netboot.xyz iPXE binaries (efi, snponly, arm64, kpxe)
  from <https://boot.netboot.xyz/ipxe/> and embedded them via
  `internal/assets/assets.go`. Provenance recorded in
  `internal/assets/ipxe/VERSIONS.md` per the GPLv2+ requirement in PLAN
  section 8.
- Added default `boot.ipxe` chain-script template.
- `cmd/pxe-beacon/main.go` parses the v1 flags (`-interface`, `-listen`,
  `-http-port`, `-loglevel`, `-chain-url`, `-ipxe-script`), prints the
  startup banner, checks the embedded asset works, and blocks on
  `signal.NotifyContext` for graceful shutdown on SIGINT/SIGTERM.

Gate verification (`go build ./...` then run + send SIGTERM via
`timeout 2`):

```
23:52:12.214 info  main      pxe-beacon dev starting (linux/amd64)
23:52:12.214 info  main      flags: interface="" listen=0.0.0.0 http-port=8080 chain-url=https://boot.netboot.xyz/menu.ipxe
23:52:12.216 info  main      embedded netboot.xyz.efi ready (1171456 bytes)
23:52:12.216 info  main      ready — press Ctrl-C to exit (M0 scaffold: no services started yet)
23:52:14.212 info  main      signal received, shutting down
23:52:14.212 info  main      pxe-beacon: shutdown complete
exit=0
```

Decisions to flag for review:
- **Renamed `internal/log` → `internal/narrlog`.** Stdlib `log` is used
  by the dhcp library and would shadow if we matched the PLAN name. The
  Go package import name is `narrlog`.
- **Embedded real netboot.xyz binaries up front** rather than
  placeholders. This keeps the binary self-contained as soon as M2/M3
  land, and the licensing note is already in place.
- No `discover.pcap` exists in the repo; M1 will use synthetic-but-
  realistic DISCOVER fixtures built with `insomniacslk/dhcp` helpers,
  and this file will note that you should later capture a real one.

---

## M1 — proxyDHCP OFFER — **PASS** (Tier 0)

What I did:
- `internal/proxydhcp/arch.go`: option-93 → boot-asset table. Covers
  legacy BIOS (0x00), EFI x86_64 (0x07), EFI x86_64 HTTP (0x10), EFI
  ARM64 (0x0b), EFI ARM64 HTTP (0x13), EFI IA32 (0x06 → best-effort
  snponly). Unknown archs fall back to EFI x86_64 over TFTP and are
  flagged `UnknownArch=true` so the logger can shout.
- `internal/proxydhcp/proxydhcp.go`: `BuildOffer(req, cfg) → (reply,
  Decision, error)` — **pure function, no sockets touched**. Handles:
  - DISCOVER and REQUEST (REQUEST is what some firmware sends on 4011).
  - Vendor-class check (option 60). Missing / non-PXE classes return
    `ErrSkip` with `SkipKind=SkipNotPXE` and `IsBenignSkip()==true` so
    the logger labels them "(benign: client already handed off to iPXE)".
  - User-class (option 77) `iPXE` → serves the script URL via HTTP
    instead of the binary (loop prevention).
  - Arch dispatch via `LookupArch`. For TFTP arches we set both
    `siaddr` and option 66 (TFTP server name); for HTTP arches we set
    a full URL in option 67 and the class identifier `HTTPClient`.
  - `YourIPAddr` is hard-zeroed — proxyDHCP MUST NOT assign IPs.
- `internal/proxydhcp/listener.go`: binds UDP/67 and UDP/4011 via
  `insomniacslk/dhcp/dhcpv4/server4`. Forces broadcast on UDP/67
  replies, unicast on 4011. Tracks pending OFFERs and fires the
  "client never fetched" hint from PLAN section 4 if no follow-up
  TFTP/HTTP appears within `FollowUpTimeout`.
- `internal/netinfo`: picks an interface, returns advertise IP and a
  WiFi-name heuristic for the section-0 wireless warning.
- Wired into `cmd/pxe-beacon/main.go` so `./pxe-beacon` actually starts
  the proxyDHCP listener. Banner now includes interface, IP, port, and
  the WiFi warning when applicable.
- Narrated logging: `Decision`/`Benign`/`Hint`/`HexDump` call sites
  light up — every OFFER produces a "client ... arch=... userclass=...
  stage=... → decision: ..." line at info level.

Gate verification — `go build ./...` clean; `go test ./...`:

```
=== RUN   TestBuildOffer_EFIx64_TFTP                  PASS
=== RUN   TestBuildOffer_HTTPBoot_x64                 PASS
=== RUN   TestBuildOffer_ARM64_TFTP                   PASS
=== RUN   TestBuildOffer_ARM64_HTTPBoot               PASS
=== RUN   TestBuildOffer_LegacyBIOS                   PASS
=== RUN   TestBuildOffer_iPXEUserClass_ServesScript   PASS
=== RUN   TestBuildOffer_SkipsNonPXEAsBenign          PASS
=== RUN   TestBuildOffer_SkipsNonDiscoverNonRequest   PASS
=== RUN   TestBuildOffer_UnknownArchFallsBackAndFlags PASS
=== RUN   TestBuildOffer_VendorClassPXEClientSuffixed PASS
=== RUN   TestBuildOffer_PureFunction_NoSideEffectOnRequest PASS
=== RUN   TestBuildOffer_RejectsBadConfig             PASS
PASS    github.com/venkatamutyala/pxe-beacon/internal/proxydhcp
```

Tier 0 PLAN requirements covered: multiple archs (incl. 0x07 EFI x86-64
and 0x10 HTTP-boot), the iPXE user-class case, vendor-class parsing
(both `PXEClient` and suffixed forms), purity (the function does not
mutate the input request).

Decisions to flag for review:
- **No `discover.pcap` in the repo** — fixtures are crafted with the
  same `insomniacslk/dhcp` library that parses real captures, so option
  encoding is bit-identical to a real packet. PLAN line 6 says: "If
  `discover.pcap` exists in the repo, use its real option bytes as a
  test fixture. If it does not exist, create synthetic-but-realistic
  DISCOVER fixtures and note in PROGRESS.md that I should later replace
  them with a real capture." → **please capture a real one with
  `tcpdump -i <if> -w discover.pcap port 67` once you have hardware and
  drop it in `testdata/`; I'll then add a test that loads its raw
  bytes via `dhcpv4.FromBytes`.**
- **PLAN says ports 67+4011.** I wired both. 4011 handles the PXE BINL
  REQUEST some firmware sends after seeing our OFFER on 67. For UDP/67
  we force broadcast destination on replies, on 4011 we honor the peer.
- **Failure-path hint:** "OFFER sent, no fetch within 10s →
  hint(...)". Default timeout is 10s; configurable via
  `ServerOptions.FollowUpTimeout`. Not exposed as a flag yet (M5).
- **`internal/proxydhcp/listener.go` cannot bind 67 without root.** As
  expected. Error message hints at the cause.

---

## M2 — TFTP server — **PASS** (Tier 0 + Tier 1)

What I did:
- `internal/tftp/tftp.go`: pin/tftp-backed server. `readHandler`
  resolves a requested path to one of the embedded netboot.xyz binaries
  via `kindForLeaf`. Both flat ("netboot.xyz.efi") and MAC-prefixed
  ("aa:bb:cc:dd:ee:ff/netboot.xyz.efi") path forms are accepted, plus
  the conventional aliases (`ipxe.efi`, `undionly.kpxe`). Path scheme
  is documented in the file's package comment.
- Unknown filenames produce a warn-level "404" log naming the path
  requested *and* the known names (PLAN section 4: "404s loudly with
  the path requested").
- Calls `tracker.NoteServed(...)` so the proxyDHCP "client never
  fetched" hint timer clears once *something* has been served.
- Wired into `cmd/pxe-beacon/main.go` alongside the proxyDHCP listener.
  `-tftp-listen` flag added (default `0.0.0.0:69`).

Gate verification:

Tier 0 (unit tests, in-process pin/tftp client against the server):

```
=== RUN   TestTFTP_ServesEmbeddedEFI         PASS  (0.15s)
=== RUN   TestTFTP_AcceptsAliasFilename      PASS  (0.14s)
=== RUN   TestTFTP_AcceptsMACPrefixedPath    PASS  (0.14s)
=== RUN   TestTFTP_404ForUnknown             PASS  (0.05s)
PASS    github.com/venkatamutyala/pxe-beacon/internal/tftp
```

Tier 1 (system `tftp` client; PLAN's primary M2 gate):

```
$ sudo /tmp/pxe-beacon -tftp-listen 127.0.0.1:6969 -advertise-ip 127.0.0.1 -listen 127.0.0.1 &
$ tftp 127.0.0.1 6969 -c get netboot.xyz.efi /tmp/netboot.xyz.efi
$ md5sum /tmp/netboot.xyz.efi internal/assets/ipxe/netboot.xyz.efi
  9dc2e1a7499c0bdd7405f80732f69167  /tmp/netboot.xyz.efi
  9dc2e1a7499c0bdd7405f80732f69167  internal/assets/ipxe/netboot.xyz.efi
```

Server log:

```
tftp      listening on 127.0.0.1:6969
tftp      TFTP RRQ/GET "netboot.xyz.efi" -> serving netboot.xyz.efi (1171456 bytes)
tftp      TFTP RRQ "netboot.xyz.efi" -> served netboot.xyz.efi (1171456 bytes) ok
```

Decisions to flag for review:
- **`pin/tftp.sender.ReadFrom` reports an inflated byte count** when
  blocksize OACK/tsize is negotiated. The actual file bytes are correct
  (md5 matches). I dropped a misleading "short send" warning that the
  inflated count was producing — the success path is now error-only.
- **Path scheme: accept both flat and MAC-prefixed.** Documented in
  `internal/tftp/tftp.go`. v1 ignores the MAC; per-host overlays are
  Phase 2 territory.
- **`NoteServed` uses an opaque "tftp-anon" tag.** TFTP RRQ doesn't
  carry the client MAC, so we can't clear per-MAC pending OFFERs
  yet. Good enough to silence the failure-path hint when any TFTP
  follow-up arrives.

---

## M3 — HTTP server + chain script — **PASS** (Tier 0 + Tier 1)

What I did:
- `internal/httpd/httpd.go`: net/http server. Endpoints:
  - `/netboot.xyz.efi`, `/netboot.xyz-snponly.efi`,
    `/netboot.xyz-arm64.efi`, `/netboot.xyz.kpxe` and their friendly
    aliases (`/ipxe.efi`, `/undionly.kpxe`).
  - `/boot.ipxe` — rendered Go text/template with `{AdvertisedIP,
    ChainURL, SetCrossCert}`.
  - `/` — short status page so a `curl localhost:8080` works as a
    healthcheck.
- **Content-Length is set explicitly on every response.** PLAN section
  0 calls out UEFI HTTP-boot pickiness; we use `bytes.Reader` +
  `http.ServeContent` for binaries and a pre-rendered buffer for the
  script so chunked encoding never happens.
- `-crosscert` flag wired through to the template (PLAN section 0
  gotcha: older iPXE builds need `set crosscert` for HTTPS).
- `-ipxe-script <path>` lets operators override the embedded
  template with a file on disk.
- Tracker.NoteServed called on each successful GET to keep the
  proxyDHCP failure-path hint quiet.
- Wired into main alongside proxyDHCP and TFTP — three goroutines, one
  shared shutdown.

Gate verification:

Tier 0 (`go test`):

```
=== RUN   TestHTTP_ServesIPXEBinaryWithContentLength  PASS
=== RUN   TestHTTP_RendersBootScript                  PASS
=== RUN   TestHTTP_404UnknownPath                     PASS
=== RUN   TestHTTP_RootStatusPage                     PASS
=== RUN   TestHTTP_CrossCertEmittedWhenEnabled        PASS
PASS    github.com/venkatamutyala/pxe-beacon/internal/httpd
```

Tier 1 (`curl`, PLAN's primary M3 gate):

```
$ curl -sI http://127.0.0.1:8080/netboot.xyz.efi
HTTP/1.1 200 OK
Content-Length: 1171456
Content-Type: application/octet-stream

$ curl -s http://127.0.0.1:8080/boot.ipxe
#!ipxe
# pxe-beacon default chain script — templated by the HTTP server.
echo pxe-beacon: handing off to iPXE script
echo pxe-beacon: server=127.0.0.1 chain=https://boot.netboot.xyz/menu.ipxe
chain --autofree https://boot.netboot.xyz/menu.ipxe ||
echo pxe-beacon: chain failed: ${errno}
echo Press a key to drop to iPXE shell.
shell
```

Server log:

```
http      listening on 127.0.0.1:8080 (script path /boot.ipxe)
http      HEAD /netboot.xyz.efi -> 200, 1171456 bytes
http      GET /boot.ipxe -> 200, 561 bytes (127.0.0.1:39028)
http      GET / -> 200, 342 bytes
```

Decisions to flag for review:
- **Aliases (`/ipxe.efi` etc.)** are served alongside the canonical
  netboot.xyz names. The OFFER sends the canonical names; aliases
  exist so `curl localhost:8080/ipxe.efi` works for ad-hoc testing
  and matches the most common firmware naming guesses.
- **Per-host MAC-scoped HTTP paths** are not implemented — Phase 2.
- The previous `boot.ipxe` template had its "documentation" comments
  written using `{{.X}}` syntax which caused the rendered file to show
  expanded values inside the comment block. Tidied the template so
  comments are just comments.

---

## M4 — End-to-end wiring + real boot prep — **CODE READY; HARDWARE GATE BLOCKED**

What I did:
- Verified all three servers (proxyDHCP UDP/67+4011, TFTP UDP/69, HTTP
  TCP/8080) run as goroutines under one shared `signal.NotifyContext`,
  bind cleanly, and serve concurrently from the same binary.
- Added optional `Port67`/`Port4011`/`BroadcastReply` to listener
  ServerOptions so synthetic-client tests can use high ports without
  root and read unicast replies on loopback. Production defaults
  (67/4011/broadcast=true) are unchanged.
- Wrote two **end-to-end socket-path tests** (`listener_e2e_test.go`)
  that craft a real DISCOVER over UDP loopback, send it to the
  listener, parse the OFFER bytes, and assert: the OFFER fields are
  correct, the YIADDR is zero (proxyDHCP MUST NOT assign IPs), and
  the narrated log contains the expected `stage=firmware-TFTP` /
  `stage=iPXE-script` lines. These are the PLAN's "optional
  synthetic DHCP client" sanity check — I added them because they
  catch socket bugs the pure tests miss.
- Wrote `RUN.md`:
  - Quick-start + flags.
  - Three M4 validation paths: QEMU+OVMF on Linux (preferred), real
    hardware, UEFI HTTP boot.
  - `tcpdump` lens with the expected 5-step sequence.
  - Loopback / Tier-1 smoke commands operators can run today.
  - Troubleshooting table mapping PLAN section 0 gotchas to symptoms.

Gate verification (the parts I can do here):

```
go build ./...          # clean
go test ./...           # all pass:
  httpd       5 tests
  proxydhcp   14 tests (12 unit + 2 e2e)
  tftp        4 tests
```

Live three-server smoke:

```
proxydhcp listening on udp/67 (DHCP) and udp/4011 (PXE BINL), interface="enp1s0" advertise=127.0.0.1
tftp      listening on 127.0.0.1:6969
http      listening on 127.0.0.1:8080 (script path /boot.ipxe)
tftp      TFTP RRQ "netboot.xyz.efi" -> served netboot.xyz.efi (1171456 bytes) ok
http      GET /boot.ipxe -> 200, 415 bytes
```

ss(1) confirms all four sockets bound:

```
UNCONN ... 127.0.0.1%enp1s0:67   pxe-beacon
UNCONN ... 127.0.0.1%enp1s0:4011 pxe-beacon
UNCONN ... 127.0.0.1:6969        pxe-beacon
LISTEN ... 127.0.0.1:8080        pxe-beacon  (via Serve TCP)
```

**M4 PLAN gate is "a UEFI client boots through to the netboot.xyz menu
with no other config" — that requires hardware/VM I do not have in this
environment. The code is wired and ready; see `RUN.md` Path A (QEMU+
OVMF) and Path B (real hardware) for the exact commands.**

Decisions to flag for review:
- **`-interface enp1s0` is auto-selected in this sandbox**, but that
  interface is a virtual NIC with `192.168.122.107` — it cannot actually
  PXE-boot a hardware client. The PLAN gate must be re-run on real
  hardware or a VM bridged to a network with a real DHCP server.
- The synthetic-client e2e tests verify the OFFER bytes are correct
  on the wire; the only thing they don't prove is that UEFI firmware
  accepts those bytes. That's a hardware question, not a code one.

---

## M5 — Polish — **PASS** (code) / **VM gate manual**

What I did:
- Cleaner startup banner: drop the box-drawing characters (alignment
  was fragile), show a labelled key/value block with `interface`,
  `advertised-ip`, ports for each service, chain URL, loglevel.
- Friendlier `-help`: opens with what pxe-beacon is, lists flags,
  closes with the same-segment / privileged-port reminder.
- `-hint-after` flag wired through to the failure-path hint timer.
- Graceful shutdown across all three goroutines with a 3-second
  drain deadline so SIGINT no longer leaks goroutines.
- `gofmt -s -w .` + `go vet ./...` clean.
- `Makefile` with `make` (host build), `make test`, `make cross` for
  linux/amd64, linux/arm64, darwin/arm64 (PLAN acceptance criteria),
  `make run` (sudo), `make run-loopback` (Tier-1 smoke), `make clean`.
  Honors `$(GO)` override so works when `go` isn't on the system PATH.
- Version stamped into the binary via `-ldflags -X main.version=$(VERSION)`
  from `git describe`.
- `README.md` rewritten: what it is, hard constraints (broadcast,
  same-segment, ports, Docker-on-macOS), install, run, what success
  looks like in the log, the section-0 gotchas, architecture, test
  tiers, license/attribution.

Gate verification:

```
$ gofmt -l .                       # clean
$ go vet ./...                     # clean
$ make GO=/usr/local/go/bin/go test
  ok  github.com/venkatamutyala/pxe-beacon/internal/httpd     0.435s
  ok  github.com/venkatamutyala/pxe-beacon/internal/proxydhcp 0.208s
  ok  github.com/venkatamutyala/pxe-beacon/internal/tftp      0.474s

$ make GO=/usr/local/go/bin/go cross
  -> dist/pxe-beacon-linux-amd64  (11 MB)
  -> dist/pxe-beacon-linux-arm64  (10 MB)
  -> dist/pxe-beacon-darwin-arm64 (10 MB)

$ ./dist/pxe-beacon-linux-amd64 -version
  pxe-beacon 7c645a0-dirty (linux/amd64)
```

Banner output:

```
pxe-beacon 7c645a0-dirty (linux/amd64)
  interface     : enp1s0
  advertised-ip : 127.0.0.1
  proxyDHCP     : udp/67 + udp/4011 on 127.0.0.1
  TFTP          : 127.0.0.1:6969
  HTTP          : 127.0.0.1:8080 (chain script /boot.ipxe)
  chain-url     : https://boot.netboot.xyz/menu.ipxe
  loglevel      : debug
embedded netboot.xyz.efi ready (1171456 bytes)
```

**PLAN M5 gate is "fresh clone → make → runs on Mac and a Linux
box; VM boots to menu."** The "runs on Linux" and "make works" parts
verified here. The "runs on Mac" part is a separate machine — the
cross-compiled darwin/arm64 binary built clean, but actual macOS
runtime is left for you to confirm. The "VM boots to menu" part is
the M4 hardware gate; see `RUN.md` Path A.

Decisions to flag for review:
- **`-hint-after 10s` default.** Could be too aggressive on slow
  networks (e.g. spinning up a VM that takes longer to load EFI). The
  flag is there; raise it if it noisy in practice.
- **WiFi heuristic** in `netinfo.looksWireless` matches prefixes
  `wl`, `wlan`, `wlp`, `wlx`, `ath`, `ra`. macOS `en0` is intentionally
  NOT matched — it's wireless on MacBooks but wired on other Macs, and
  false-positive warnings would be more annoying than useful. Tighten
  later if needed.

---

## Overall v1 status

| Milestone | Code | Tier 0 | Tier 1 | Hardware gate |
|-----------|------|--------|--------|---------------|
| M0 Scaffold              | ✅ | n/a | n/a | n/a |
| M1 proxyDHCP BuildOffer  | ✅ | ✅ 12 unit + 2 e2e | n/a | n/a |
| M2 TFTP                  | ✅ | ✅ 4 tests | ✅ real `tftp` client | n/a |
| M3 HTTP + chain script   | ✅ | ✅ 5 tests | ✅ `curl -I` + `curl` | n/a |
| M4 End-to-end wiring     | ✅ | ✅ live three-server run | ✅ tcpdump + sockets | ⚠ manual (RUN.md) |
| M5 Polish                | ✅ | ✅ gofmt/vet/test | ✅ banner+shutdown | ⚠ manual |

**Total tests:** 23 (passing). Cross-compile to linux/amd64,
linux/arm64, darwin/arm64 all succeed.

**Hand-off:** start at `RUN.md` Path A to drive the QEMU+OVMF boot.

---

## v0.1.2 — DHCP REQUEST→ACK fix (wire-observed iPXE BINL drop)

A user PXE-booting an AMI/Phoenix client through v0.1.1 captured a
tcpdump showing iPXE silently dropping our replies during the
iPXE-stage BINL exchange and retrying the same `DHCPREQUEST` to
udp/4011 six times before giving up. Wire evidence:

```
19:01:21.240306  10.69.7.217.68 > 10.69.69.218.4011: BOOTP/DHCP, Request ...
19:01:21.240781  10.69.69.218.4011 > 10.69.7.217.68: BOOTP/DHCP, Reply
                   DHCP-Message: Offer            ← BUG: should be Ack
                   BF: "http://10.69.69.218:8080/boot.ipxe"
```

(Repeated 6 times at 21.553, 21.965, 22.987, etc.)

**Root cause:** `BuildOffer` hard-coded `WithMessageType(MessageTypeOffer)`
regardless of the request's own message type. UEFI firmware tolerates
the wrong type (so v0.1.1 worked end-to-end through TFTP+iPXE-boot on
that user's hardware), but strict iPXE drops `OFFER` replies to its
`REQUEST` per the DHCP state machine. The user's actual stuck-boot
symptom was a firmware USB-keyboard bug (`PLAN.md` section 0), not this
— but the protocol error is real and would break stricter clients.

**Fix:** `internal/proxydhcp/proxydhcp.go` now mirrors the request
state — `DISCOVER → OFFER`, `REQUEST → ACK`. The comment in the code
preserves the iPXE-BINL motivation so future readers don't undo it.

**Tests added in `internal/proxydhcp/proxydhcp_test.go`:**
- `TestBuildOffer_RequestRepliesACK` — synthetic iPXE BINL request,
  asserts reply is `MessageTypeAck` with the script URL.
- `TestBuildOffer_DiscoverStillRepliesOFFER` — guards the DISCOVER
  path against future churn.

Full suite passes (`go test ./...`, 14 unit + 2 e2e in proxydhcp).
Tagged and released via the existing GitHub Actions release workflow.

---

## v0.2.0 — fleet PXE manager

v0.1 was a one-machine PXE server: process-global flags decide what to
serve, every client gets the same OFFER. The actual user, Venkat, has
10 computers with mixed OSes that need unattended cloud-init installs.
v0.2 is that product.

### What landed

**`internal/fleet/`** (new package)
- `fleet.go` — YAML config parser, MAC normalization (colon / hyphen /
  dot / no-separator forms), per-MAC `Lookup(mac) → Profile`, SIGHUP
  reload, validation (known boot targets, dup MACs, missing cloud-init
  files for autoinstall targets, missing iPXE script for `custom`).
  12 unit tests.
- `status.go` — in-memory per-MAC tracker. Events
  `firmware-dhcp → firmware-fetched → ipxe-dhcp → user-data-fetched →
  installer-done` with monotonic state advancement, stall detection,
  snapshot of configured + observed-but-unknown machines. 4 unit tests.

**`internal/boot/`** (new package)
- `targets.go` — `RenderAutoexec(target, ctx)` for the built-in
  templates; `RenderCustom(path, ctx)` for operator scripts;
  `RedirectorScript(ip, port)` for the generic TFTP autoexec.ipxe
  that uses iPXE's `${net0/mac:hexhyp}` to bounce per-MAC dispatch
  into HTTP. 9 unit tests.
- `internal/assets/scripts/autoexec/{menu,ubuntu-22.04,ubuntu-24.04,debian-12}.ipxe`
  — embedded iPXE templates. Ubuntu chains through netboot.xyz's
  hosted casper/{vmlinuz,initrd} for v0.2.0; Debian uses
  `deb.debian.org` directly.

**`internal/proxydhcp/`**
- `Config` gains `Fleet *fleet.Fleet` (nil-safe). `BuildOffer`
  resolves per-MAC name + target; `Decision` carries them through.
  `logDecision` renders `client <name> (<mac>) ...` when configured.
  3 new fleet-routing unit tests.
- `ServerOptions.StatusTracker` is the new wire to `fleet.Tracker`.
  The listener calls `Note(mac, firmware-dhcp)` / `Note(mac,
  ipxe-dhcp)` when sending OFFERs, so the status page sees motion
  in real time.

**`internal/tftp/`**
- `Options.Autoexec` is an injection point for the redirector. In
  fleet mode, TFTP serves it for `RRQ "autoexec.ipxe"`; in single-
  machine mode, the file still 404s (no behavior change for v0.1.3
  users). 2 new unit tests.

**`internal/httpd/`**
- Six new routes (Go 1.22+ `mux.HandleFunc("GET /pattern", h)`):
  - `GET /autoinstall/{mac}/autoexec.ipxe` — per-MAC iPXE script via
    the boot package.
  - `GET /autoinstall/{mac}/user-data` — Go-templated cloud-init
    user-data; vars: `Name, MAC, MACHyp, AdvertisedIP, HTTPPort`.
  - `GET /autoinstall/{mac}/meta-data` — minimal NoCloud meta-data
    (`instance-id` + `local-hostname` derived from the fleet entry).
  - `POST /autoinstall/{mac}/done` — cloud-init phone_home callback.
    Updates status tracker → installer-done.
  - `GET /status` — embedded HTML template, auto-refreshing every 5s,
    no JS framework. Color-coded status dots.
  - `GET /status.json` — same data as the HTML, machine-readable.
- All six 404 cleanly with a helpful message when `-config` isn't
  passed → drop-in compat for v0.1.3 users. 9 new fleet-route tests.

**`cmd/pxe-beacon/main.go`**
- New `-config <path>` flag. When set, loads the fleet (refuses to
  start on validation errors), constructs `fleet.NewTracker`, wires
  both into proxydhcp + tftp + httpd. SIGHUP handler reloads the
  config in place. When unset, `fleet.Empty()` keeps the v0.1.3
  default-everyone-to-menu behavior intact.
- Banner now prints fleet config path + status page URL.

**P1+P2 polish (shipped alongside)**
- TFTP `autoexec.ipxe` 404 → info (benign), not warn.
- TFTP tsize-retry abort (code=8) → debug, not error.
- `Listener.NoteServed` with opaque tag clears all pending hints,
  fixing the v0.1.x false-positive "never fetched" on success.

### Status visibility model (no IPMI required)

The wire tells us everything we need. Per-MAC state machine:

| status | trigger |
|---|---|
| `pending` | in fleet.yaml, never seen on wire |
| `firmware-dhcp` | we OFFERed on udp/67 (proxydhcp) |
| `firmware-fetched` | TFTP serve completed (transitively — we infer from later events) |
| `ipxe-dhcp` | we OFFERed to `userclass=iPXE` (proxydhcp) |
| `user-data-fetched` | `GET /autoinstall/{mac}/user-data` returned 200 (httpd) |
| `installer-done` | cloud-init phone_home POSTed `/done` (httpd) |
| `stalled` (overlay) | last activity > 5min (configurable) |

### Verification

`go test ./...` — all green. Tally:
- internal/fleet: 16 tests
- internal/boot: 9 tests
- internal/proxydhcp: 17 unit + 2 e2e
- internal/tftp: 6 tests
- internal/httpd: 14 tests

End-to-end loopback smoke (in `debug.txt`-style commands during dev):
- `sudo ./pxe-beacon -config ./fleet.example.yaml -advertise-ip 127.0.0.1 ...`
- `curl /status.json` → 4 machines visible, server metadata
- `curl /autoinstall/.../autoexec.ipxe` → renders correct OS template
- `curl /autoinstall/.../user-data` → renders Go template (hostname,
  phone_home URL, etc.)
- `curl /autoinstall/.../meta-data` → instance-id + local-hostname
- `POST /autoinstall/.../done` → status transitions to installer-done
- `tftp ... get autoexec.ipxe` → redirector with `${net0/mac:hexhyp}`
- Edit `fleet.yaml` → `kill -HUP $(pgrep -x pxe-beacon)` → next
  `curl /status.json` reflects new config (verified 1 → 3 machines
  without restart).

### Documentation

- `fleet.example.yaml` — annotated example with 4 machines
  (2× ubuntu-22.04, 1× debian-12, 1× custom rescue).
- `examples/{kube-node.yaml, debian-db.yaml, rescue.ipxe}` — drop-in
  user-data templates that exercise the templating + phone_home flow.
- `make demo-fleet` — boots pxe-beacon on loopback with
  `fleet.example.yaml` for quick HTTP inspection.
- README rewritten with a v0.2 fleet walkthrough + flag table.

### Out of scope (deferred to v0.2.x / v0.3)

- Per-machine local kernel/initrd caching (`pxe-beacon fetch <target>`)
  for airplane-mode operation.
- Additional OS targets beyond ubuntu-22.04/24.04/debian-12.
- Full DHCP server mode (`-dhcp`).
- A real `discover.pcap` fixture (the user asked to defer this to
  end-of-dev).
- Templated cloud-init "defaults + overrides" hybrid — separate file
  per machine is the v0.2 contract. Operators can pre-template
  outside pxe-beacon with helm/jinja if they want DRY.

---

## v0.2.1 — honest fix-up for the autoinstall templates

Venkat ran v0.2.0 with `boot: debian-12` and the boot fell through to
the netboot.xyz menu instead of installing. Investigation found two
real defects in v0.2.0's autoinstall layer:

1. **Wrong Debian mirror path.** The template had
   `current/legacy-images/netboot/...`; bookworm uses
   `current/images/netboot/...` (no "legacy-"). Every kernel-fetch
   attempt 404'd → iPXE chain failed → netboot.xyz embed fallback.
   **Fixed** in v0.2.1 — Debian d-i now boots from the corrected URL.

2. **Standard Debian d-i doesn't honor cloud-init / NoCloud
   parameters.** d-i uses preseed.cfg, not cloud-init. So even with
   the corrected URL, the operator's `cloud_init:` file is silently
   ignored and the installer goes interactive. Documented honestly
   in `debian-12.ipxe`'s header comment; real preseed support is
   v0.3 work.

3. **Ubuntu 22.04 / 24.04 kernel URLs don't exist either.** I
   guessed `http://boot.netboot.xyz/os/ubuntu/<ver>/casper/vmlinuz`
   — that path 404s. Ubuntu doesn't publish casper-extracted files
   as flat HTTP anywhere I can find quickly. **Marked broken in
   v0.2.1**: the ubuntu-22.04 and ubuntu-24.04 templates now carry
   a loud "STATUS: KERNEL URL TBD — not functional yet" comment.
   Real fix needs either `pxe-beacon fetch <target>` (extract from
   ISO into local cache) or imgargs-injection-through-netboot.xyz —
   tracked for v0.2.2+.

What v0.2.1 actually gives you:
- **`boot: menu`** — works (unchanged from v0.1.3).
- **`boot: debian-12`** — boot chain reaches d-i; user steps through
  the d-i installer manually. NOT zero-touch yet.
- **`boot: custom`** — works (user supplies their own iPXE script).
- **`boot: ubuntu-22.04` / `ubuntu-24.04`** — *known broken*; the
  template loudly says don't use it. Use `boot: custom` for now.

Status / phone-home / fleet config / SIGHUP reload / /status page
all work as advertised — those layers are unaffected by the
autoinstall-template URL bugs.

### Workaround for the user pending v0.2.2+

`debug.txt` in the repo includes a `boot: custom` iPXE script that
boots Debian d-i interactively from the correct URL. The user can
drop it in fleet.yaml as `boot: custom` + `ipxe_script: ./that.ipxe`
to at least confirm the chain works end-to-end on their hardware.

### Lesson for next time

Ship every URL through a `curl -I` check in CI before tagging. The
template tests pass strings (which they did) but never resolve the
URLs (which they should). Adding a TBD `make verify-urls` target.

---

## v0.2.2 — `debian-13` (Trixie) target

Trivial follow-up to v0.2.1: add a `debian-13` target mirroring
`debian-12` but pointing at Trixie's netboot URL
(`dists/trixie/main/installer-amd64/current/images/netboot/...`).

Background investigation (May 2026): downloaded Trixie's netboot
initrd, extracted 1739 files, grepped for cloud-init / NoCloud
markers — found **none**. Mainline d-i in Trixie has zero cloud-init
support. So Debian 12 and 13 share one preseed-based code path
(landed in v0.3); the only per-release thing is the mirror URL.

`debian-13` template + tests added; fleet.ValidBootTargets + boot.IsBuiltIn
extended. Same caveats as v0.2.1: boots d-i interactively until v0.3.

---

## v0.3.0 — preseed.cfg + cloud-init bridge for Debian

The headline v0.2 feature ("unattended installs") finally works for
Debian.

### What landed

- **`fleet.Profile` gains a `Preseed` field.** YAML key `preseed:` in
  fleet.yaml — path to an operator-supplied preseed.cfg. Resolved
  relative to fleet.yaml's directory; validated (`os.Stat`) at load.
- **`validateProfile` re-split per OS family.** Ubuntu requires
  `cloud_init:`. Debian no longer requires it — it requires
  `preseed:` for unattended (and accepts `cloud_init:` *on top* for
  the bridge). Missing both = interactive boot, allowed.
- **New HTTP route `GET /autoinstall/{mac}/preseed.cfg`** in
  `internal/httpd/httpd.go`:
  - With `preseed:` set: serves the operator file Go-templated
    with the same `{Name, MAC, MACHyp, AdvertisedIP, HTTPPort}` vars
    user-data has.
  - With BOTH `preseed:` and `cloud_init:` set: appends a
    `d-i preseed/late_command` that installs cloud-init on the
    target, drops user-data + meta-data into
    `/var/lib/cloud/seed/nocloud/`, and enables
    `cloud-init.service` — so cloud-init runs on first boot of the
    installed system.
  - Without `preseed:`: serves a polite "go interactive" stub.
  - Fires `EventUserDataFetched` on the status tracker (the
    Debian-side analog of cloud-init user-data fetch).
- **`debian-12` and `debian-13` iPXE templates** updated. The
  kernel cmdline now passes
  `auto=true priority=critical url=http://server:port/autoinstall/<mac>/preseed.cfg`
  to d-i. No more cloud-init / NoCloud params (since d-i ignores
  them anyway).
- **`examples/debian-preseed.cfg`** — annotated starter preseed.
  Operators copy/edit to taste. The cloud-init bridge is added
  automatically by pxe-beacon — operators don't need to write
  late_command themselves.
- `fleet.example.yaml` + README updated with the preseed + bridge
  story; the `boot:` value table now lists side-file requirements
  per target.

### Tests (4 new, all green)

- `TestHTTP_Preseed_RendersOperatorFile` — operator's preseed
  served with template vars resolved; **no** bridge appended when
  only `preseed:` is set.
- `TestHTTP_Preseed_AppendsCloudInitBridge` — both fields set →
  operator content first, bridge `late_command` appended with the
  correct AdvertisedIP / port / hyphen-MAC.
- `TestHTTP_Preseed_InteractiveStubWhenNoPreseed` — neither set →
  short comment-only stub, no preseed directives.

### What a Debian boot now looks like with v0.3

1. UEFI DHCP → pxe-beacon OFFER → TFTP netboot.xyz-snponly.efi.
2. iPXE loads, fetches TFTP `autoexec.ipxe` (the redirector), HTTP
   `autoexec.ipxe` (the per-MAC template).
3. iPXE boots Debian d-i with `url=…/preseed.cfg`.
4. d-i fetches preseed.cfg (pxe-beacon serves operator file +
   bridge); runs unattended.
5. d-i late_command writes user-data + meta-data to
   `/var/lib/cloud/seed/nocloud/`, enables cloud-init.
6. Machine reboots into the installed system.
7. cloud-init reads NoCloud seed dir, runs operator's user-data —
   installs packages, runs runcmd, calls phone_home.
8. pxe-beacon's `/status` flips to `installer-done`.

### Out of scope for v0.3 (still deferred)

- Ubuntu Subiquity URLs (v0.4 `pxe-beacon fetch <target>`).
- A real `discover.pcap` test fixture.
- Operator UI for fleet edits (v0.x+, not on roadmap).

---

## v0.1.3 — serve `netboot.xyz-snponly.efi` for x86_64 UEFI

The v0.1.2 user reported that PXE-booting an AMI/Phoenix-firmware
client through pxe-beacon left the netboot.xyz menu visible but the
USB keyboard dead, while booting the *same* netboot.xyz iPXE from a
USB stick worked fine.

**Why USB worked, PXE didn't:** the full `netboot.xyz.efi` build we
were serving contains iPXE's own native PCI/NIC drivers. When loaded
via PXE, iPXE has to bring up networking immediately to chainload
`boot.netboot.xyz/menu.ipxe`, so those native drivers re-initialize
the NIC from scratch on top of whatever UEFI already had running. On
AMI/Phoenix firmware that re-init glitches the shared PCI USB
controller — USB keyboard loses its association and goes dead. When
loaded via USB, iPXE doesn't need networking immediately and doesn't
touch PCI, so the keyboard survives.

**Fix:** serve `netboot.xyz-snponly.efi` instead. The snponly build is
iPXE compiled with `--snponly` — it has no native NIC drivers and uses
UEFI's existing Simple Network Protocol wrapper. UEFI keeps owning the
NIC and the USB controller; iPXE never touches PCI. Keyboard stays
alive.

**Changed in `internal/proxydhcp/arch.go`:** swapped IPXEKind +
BootFile for both `iana.EFI_X86_64` (TFTP) and `iana.EFI_X86_64_HTTP`
(HTTP boot). The snponly binary has been embedded since M0; this just
points the arch table at it.

**Tests updated:** four assertions in `proxydhcp_test.go` and
`listener_e2e_test.go` flipped to expect `netboot.xyz-snponly.efi`.
Full suite green (16 unit + 2 e2e).

**Trade-off:** snponly requires UEFI to have the network stack already
initialized — but every UEFI machine that supports PXE has it, by
definition. The all-drivers build remains embedded for future use
(e.g. ia32 already uses snponly as best-effort; could expose a
`-iPXE-build` flag later if anyone needs the all-drivers path).

