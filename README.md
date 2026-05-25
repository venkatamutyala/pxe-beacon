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

Plus a **rescue** target that isn't a `boot:` value — it's a runtime
intent (`PUT /intent {"action":"rescue"}`) that boots SystemRescue on
any machine after `pxe-beacon fetch systemrescue`. See [Rescue mode](#rescue-mode-v0110).

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

### Per-machine params (v0.10.0+)

When several machines share a preseed / cloud-init / kickstart but
differ by a few values (hostname, static IP, SSH key), use a `params:`
map instead of N near-duplicate files. Params are exposed to the
templates as `{{.Params.key}}`:

```yaml
defaults:
  params:
    domain: lab.local        # fleet-wide; merged into every machine

machines:
  - mac: "58:47:ca:70:c7:c9"
    name: kube-1
    boot: debian-12
    preseed: ./worker.cfg     # one template, many machines
    params:
      hostname: kube-1
      address: "10.69.7.11"
```

```
# in worker.cfg:
d-i netcfg/get_hostname string {{.Params.hostname}}
d-i netcfg/get_ipaddress string {{.Params.address}}
# {{.Params.domain}} comes from defaults
```

`defaults.params` merge with machine-level params; the machine wins on
a key collision. Values are substituted literally — you own escaping.
Set them via `fleet.yaml`, the `/admin` form, or `params` in the API's
`MachineConfig` body.

### Run via Docker (v0.10.0+)

Multi-arch images are published to GHCR on each release:

```bash
docker run --network host \
  --cap-add NET_BIND_SERVICE \
  -v /etc/pxe-beacon:/etc/pxe-beacon \
  -v pxe-beacon-data:/var/lib/pxe-beacon \
  ghcr.io/venkatamutyala/pxe-beacon:latest \
  -config /etc/pxe-beacon/fleet.yaml
```

Two flags are **non-negotiable**, not conveniences:
- `--network host` — proxyDHCP DISCOVER is a broadcast; Docker's
  userland-proxy NAT silently drops it, so the container must share the
  host network namespace.
- `--cap-add NET_BIND_SERVICE` — UDP 67/69/4011 are privileged ports.

The image runs as a non-root user (`setcap` grants the port capability)
and `VOLUME`s `/var/lib/pxe-beacon`, where `pxe-beacon fetch` writes
distro assets and `/admin` writes template overrides — mount it to a
named volume so a multi-GB fetch survives container restarts.

### Boot intent (v0.8.0+)

Machines in `fleet.yaml` are **idle by default** — pxe-beacon ignores
their PXE DHCP requests and the box falls through to its local-disk
boot. To install (or re-install) the OS, set the desired action via
the K8s-style `intent` resource:

```bash
curl -X PUT http://127.0.0.1:8080/api/v1/machines/58:47:ca:70:c7:c9/intent \
  -H 'content-type: application/json' \
  -d '{"action":"install"}'
# {
#   "mac": "58:47:ca:70:c7:c9",
#   "desired": {"action":"install", "requested_at":"...", "expires_at":"..."},
#   "observed": {"phase":"", "last_seen":"0001-01-01T00:00:00Z"}
# }
```

The action is idempotent — same body produces the same state.
Cancel by PUT'ing `null`:

```bash
curl -X PUT http://127.0.0.1:8080/api/v1/machines/58:47:ca:70:c7:c9/intent \
  -H 'content-type: application/json' -d '{"action":null}'
```

The pending action auto-expires after `-pending-ttl` (default `15m`)
and is auto-cancelled when cloud-init phones home on first boot.
Restart of pxe-beacon also clears every queued action. The `/admin`
UI has per-row install / rescue / cancel buttons that PUT the same
endpoint with the matching body.

Full REST surface (loopback-only — see security model below):

| method | path | what |
|---|---|---|
| `GET`    | `/api/v1/machines` | list fleet machines (paginated: `?limit=&offset=`) |
| `POST`   | `/api/v1/machines` | create a machine (JSON body; 201 + ETag) |
| `GET`    | `/api/v1/machines/{mac}` | full per-machine view; sets `ETag` |
| `PUT`    | `/api/v1/machines/{mac}` | update config (requires `If-Match`) |
| `DELETE` | `/api/v1/machines/{mac}` | delete (idempotent; honors `If-Match`) |
| `PUT`    | `/api/v1/machines/{mac}/intent` | set desired action (`install`, `rescue`, or `null`) |
| `GET`    | `/api/v1/machines/{mac}/intent` | read desired + observed |
| `POST`   | `/api/v1/machines/{mac}/events` | report install lifecycle (`installer-done`/`installer-failed`); JSON or form |
| `GET`    | `/api/v1/machines/{mac}/logs` | read captured install diagnostics (last ~64 KiB; see Secure callbacks) |
| `GET`    | `/api/v1/discovered` | list unknown MACs seen PXE-booting (see Discovery) |
| `DELETE` | `/api/v1/discovered/{mac}` | dismiss a discovered sighting |
| `GET`    | `/openapi.yaml` | the OpenAPI 3 spec for the above |
| `GET`    | `/healthz`, `/readyz` | liveness / readiness probes |

The `/admin` page is a browser client of these same endpoints (v0.9.0
folded fleet CRUD out of form-encoded `/admin/fleet` into the JSON API).

**Security model (v0.9.0):** all `/api/v1/*` is loopback-only. Mutation
endpoints additionally require `Content-Type: application/json` — a
cross-origin browser can't send that without a CORS preflight that
fails (no CORS headers are emitted), so this is the CSRF defense in
lieu of a token. Every config mutation is audit-logged
(`event=fleet-mutation`). `PUT`/`DELETE` use ETag `If-Match` for
optimistic concurrency (412 on mismatch, 428 when required-but-absent).
For remote access, SSH-tunnel; token-bearer auth is a future release.

**Fleet config CRUD (v0.9.0+):** create with `POST`, update with `PUT`
(GET first for the ETag), delete with `DELETE`:

```bash
# Create
curl -X POST http://127.0.0.1:8080/api/v1/machines \
  -H 'content-type: application/json' \
  -d '{"mac":"58:47:ca:70:c7:c9","name":"venkat-1","boot":"debian-12"}'

# Update (If-Match from the GET's ETag header)
etag=$(curl -sI http://127.0.0.1:8080/api/v1/machines/58:47:ca:70:c7:c9 | grep -i etag | cut -d' ' -f2- | tr -d '\r')
curl -X PUT http://127.0.0.1:8080/api/v1/machines/58:47:ca:70:c7:c9 \
  -H 'content-type: application/json' -H "if-match: $etag" \
  -d '{"name":"venkat-1","boot":"debian-13"}'

# Delete
curl -X DELETE http://127.0.0.1:8080/api/v1/machines/58:47:ca:70:c7:c9
```

K8s-style declarative shape was picked over POST-verbs and Hetzner's
per-feature subtrees for tool-friendliness — PUT is idempotent so
Terraform / Ansible / React Query map cleanly to it. Unknown MACs
(not in `fleet.yaml`) cannot have intent set and keep their
netboot.xyz fallback, so booting a random box doesn't require any
prior queueing.

#### Secure callbacks (v0.12.0+)

The booting machine reports "install done" by POSTing to the **public**
`POST /autoinstall/{mac}/done`. Before v0.12.0 that was unauthenticated —
any host on the LAN could flip another machine's state. Now each served
cloud-init carries a short-lived **bearer token**, and `/done` (plus the
new `/log`) reject a missing/invalid one with 403.

**pxe-beacon owns `phone_home`.** It appends its own tokenized
`phone_home` block to every cloud-init it serves, so you never write one.
Defining your own makes fleet load fail (a cloud-config can't have two
`phone_home` keys, and pxe-beacon's must carry the token):

```
machine "kube-1": cloud_init ./kube-node.yaml defines phone_home —
remove it; pxe-beacon appends its own tokenized phone_home.
```

The token:

- is `<expUnix>.<hmac-sha256(secret, mac+exp)>`, bound to one MAC, minted
  at serve time, valid for `-callback-ttl` (default **24h** — must outlast
  install→first-boot or a slow install's callback 403s and the box
  reinstall-loops).
- secret comes from **`$PXE_BEACON_TOKEN_SECRET`** (set this in production
  so tokens survive a restart; an unset secret falls back to a random
  per-start value and logs a warning).
- travels in the callback URL over plaintext LAN HTTP — it's a bearer
  token, **not** crypto-grade auth without TLS. It raises the bar from
  "any host can spoof any MAC" to "you must have seen that MAC's served
  config", consistent with the trusted-LAN model.

Enforcement is on by default; `-insecure-callbacks` accepts a *missing*
token (a present-but-invalid one is always rejected) while you migrate
custom templates to carry `?t={{.CallbackToken}}`.

**Install diagnostics.** Installer failure hooks (Subiquity `error-commands`,
kickstart `%onerror`) POST the kernel ring buffer + logs to the
token-guarded `POST /autoinstall/{mac}/log`; read the last ~64 KiB at
`GET /api/v1/machines/{mac}/logs` (loopback-only, in-memory, cleared on
restart). Closes the "it failed and I have no idea why" gap.

#### Editing cloud-init from the UI (v0.14.0+)

The `cloud_init:` field in `fleet.yaml` (and the admin form) is a **file
path**. To author content without SSHing to the box, each machine row in
`/admin` has a **cloud-init** editor: paste the body into a textarea and
it's saved as a per-MAC override under
`<data-dir>/machines/<mac>/cloud-init.yaml`. At serve time the precedence
is **override file > `cloud_init:` path > embedded default**, so the
editor wins without touching `fleet.yaml`. "Delete override" reverts to
the path/default. Requires `-data-dir` (defaults to `./.pxe-beacon`).
Saving content that defines its own `phone_home:` is rejected (pxe-beacon
appends its own — see Secure callbacks); `{{.Name}}`, `{{.MACHyp}}`,
`{{.Params.x}}`, etc. are available in the body.

#### Discovery (v0.13.0+)

proxyDHCP sees every PXE `DISCOVER` on the segment — including from
machines not in `fleet.yaml`. Those unknown MACs are now recorded in a
discovery feed so you can enroll them without hand-typing MACs:

```bash
curl http://127.0.0.1:8080/api/v1/discovered
# {"total":1,"discovered":[{"mac":"dc:a6:32:...","arch":"arm64 UEFI",
#   "vendor":"Raspberry Pi","vendor_class":"PXEClient:Arch:00011",
#   "first_seen":"...","last_seen":"...","count":3}]}
```

The `/admin` "Discovered" panel lists them with **Add to fleet** (prefills
the create form — MAC + arch-derived boot default) and **Dismiss**
(`DELETE /api/v1/discovered/{mac}`). `vendor` comes from a small built-in
OUI table (common server/SBC makers; blank if unrecognized); `arch` is the
DHCP option-93 arch. Sightings dedup by MAC (with `count` + `last_seen`),
are bounded + in-memory (cleared on restart), and a sighting disappears
from the feed once its MAC is enrolled.

It's **observational only** — discovery never changes how an unknown box
boots (still the netboot.xyz fallback) and never installs anything;
enrollment + intent stay explicit operator actions. There's no
auto-enroll: `fleet.yaml` only changes when you click.

#### Rescue mode (v0.11.0+)

`PUT /intent {"action":"rescue"}` boots **SystemRescue** instead of the
machine's configured `boot:` target — a per-MAC, one-shot rescue
environment (think Hetzner Robot's rescue toggle). It uses the same
pending/TTL/auto-expiry mechanics as `install`.

One-time setup downloads SystemRescue into `-data-dir` (it's served over
HTTP at boot, so don't hot-fetch from a flaky CDN mid-boot):

```bash
pxe-beacon fetch systemrescue
```

Then arm a machine:

```bash
curl -X PUT http://127.0.0.1:8080/api/v1/machines/58:47:ca:70:c7:c9/intent \
  -H 'content-type: application/json' -d '{"action":"rescue"}'
```

**Access** to the booted rescue environment comes from `params:` (the
same per-machine map used for installs):

- `ssh_authorized_key` — injected into the live root account; sshd is
  started. Headless SSH-in.
- `rescue_root_password` — console / SSH root password. Defaults to
  `pxe` (trusted-LAN only — change it).

SystemRescue is Arch/archiso, **not** a cloud-init system, so this
reuses the cloud-init *delivery pattern*, not cloud-init: pxe-beacon
serves a per-MAC `sysrescuecfg` YAML at `/autoinstall/{mac}/sysrescue.yaml`
(SSH-key injection rides an `autorun` setup script, since the YAML runs
external scripts rather than inline). For a fully custom config, point a
`rescue:` field at your own sysrescuecfg YAML (parallel to `cloud_init:`).

**Already-installed guard (v0.8.1+):** once a machine reaches
`installer-done` via cloud-init phone-home, pxe-beacon will no longer
OFFER it a PXE boot. To re-install, `PUT /intent {"action":"install"}`
to re-arm — this is the explicit force flag.

**Restart caveat:** the already-installed guard is in-memory only. A
`pxe-beacon` restart wipes the tracker and re-arms ALL non-pending
machines. Power down test machines or set `boot=menu` in `fleet.yaml`
before restarting in production. Persistent tracker is a v0.9 task.

**No sticky install mode:** each install cycle requires a fresh `PUT
/intent`. `-pending-ttl=0` works for indefinite-pending semantics.

#### Error responses (v0.9.0+)

Every `/api/v1/*` error response is a structured JSON envelope:

```json
{
  "code": "mac_not_in_fleet",
  "message": "mac 11:22:33:44:55:66 is not in fleet.yaml",
  "details": {"mac": "11:22:33:44:55:66"}
}
```

Clients should branch on `code` (stable identifier — added codes are
additive, existing codes don't change meaning across releases). The
`message` is human prose and may be reworded for clarity.

Codes shipped in v0.9.0:

| code | http | meaning |
|---|---|---|
| `fleet_not_loaded` | 503 | server started without `-config <fleet.yaml>` |
| `pending_not_configured` | 503 | no pending-action store wired (internal misconfiguration) |
| `mac_missing` | 400 | URL path was missing the `{mac}` segment |
| `mac_invalid` | 400 | `{mac}` couldn't be parsed as a MAC address |
| `mac_not_in_fleet` | 404 | MAC is well-formed but not in `fleet.yaml` |
| `body_invalid` | 400 | request body couldn't be parsed as JSON |
| `action_missing` | 400 | body is JSON but the `action` key isn't present |
| `action_invalid` | 400 | `action` value isn't `install`, `rescue`, or `null` |
| `pending_failed` | 500 | internal error in the pending store |
| `paging_invalid` | 400 | `?limit=` / `?offset=` not a non-negative integer |

#### Pagination (v0.9.0+)

`GET /api/v1/machines` is paginated. `?limit=` defaults to (and caps at)
500; `?offset=` defaults to 0. The response echoes `total` (full fleet
size), `limit`, and `offset` so clients can iterate:

```bash
curl 'http://127.0.0.1:8080/api/v1/machines?limit=50&offset=100'
# {"total":237,"limit":50,"offset":100,"pending_ttl_s":900,"machines":[...]}
```

The underlying list is stably sorted (by name, then MAC), so offset
paging is deterministic across calls.

#### API versioning policy (v0.9.0+)

The API is versioned in the path (`/api/v1/`). Compatibility contract:

- **Additive changes** (new endpoints, new optional fields, new error
  `code` values) ship within `v1` without a version bump. Clients must
  ignore unknown fields.
- **Breaking changes** ship under a new path prefix (`/api/v2/`).
- When `v2` ships, `v1` routes carry RFC 8594 headers — `Deprecation:
  true` and `Sunset: <RFC 1123 date>` — and remain functional for a
  **minimum of two minor releases** (if `v2` lands in `N.0`, `v1` is
  removed no earlier than `N+2.0`).
- `code` identifiers in the error envelope never change meaning; only
  the human-readable `message` may be reworded.

Deprecated-but-still-served endpoints (like `/status.json`, superseded
by `GET /api/v1/machines`) carry the same `Deprecation` + `Link:
rel="successor-version"` headers.

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
| `-data-dir`      | `./.pxe-beacon`                               | dir holding `pxe-beacon fetch` output + per-machine overrides, served at `/assets/` (`$PXE_BEACON_DATA` overrides) |
| `-pending-ttl`   | `15m`                                         | how long a queued deploy / rescue stays valid before auto-cancel (`0` = no expiry) |
| `-callback-ttl`  | `24h`                                         | lifetime of the callback bearer token (see Secure callbacks) |
| `-insecure-callbacks` | off                                      | accept callbacks without a token (present-but-invalid still rejected) |
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
