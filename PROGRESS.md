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

