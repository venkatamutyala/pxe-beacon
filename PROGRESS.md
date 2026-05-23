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

