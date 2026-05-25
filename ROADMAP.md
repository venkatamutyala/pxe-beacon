# pxe-beacon roadmap

Living document. Updated as releases ship.

## Where we are

**v0.9.0 shipped** (current) — "API as a contract".
The JSON API is now the single mutation control plane: fleet CRUD
(POST/PUT/DELETE `/api/v1/machines`), `/events` install-lifecycle
reporting, ETag/If-Match optimistic concurrency, structured error
envelope, pagination, `/healthz`+`/readyz`, and a hand-written OpenAPI
3 spec at `/openapi.yaml`. `/admin` is a fetch() client of the same
endpoints; mutations require `Content-Type: application/json` (CSRF
defense) + audit logging.

Prior: **v0.8.1** closed the safety footguns (already-installed guard,
listener races, ARM64 hard-refuse, rescue 501, audit logging).
**v0.8.0** shipped the K8s-style declarative `PUT /intent` contract.

**Decision (2026-05-25):** BMC integration + `POST /power` deferred to
an unscheduled future iteration. Everything below excludes it.

## Cross-release principles

These apply to every PR until further notice:

1. **One control plane** — every mutation lands in `/api/v1/*`. `/admin/*` becomes the HTML client of the JSON API.
2. **One error shape** — `{code, message, details}` everywhere by v0.9.
3. **One state shape** — `{desired, observed}` everywhere by v0.9.
4. **One journal** — audit + tracker + pending all in the same NDJSON by v0.10.
5. **Single binary, no DB, in-memory hot path.** Never breaks across these releases.

## What is NOT changing

- v0.8.0's K8s-style `PUT /intent` resource shape stays stable across all three planned releases.
- `fleet.yaml` schema is additive only (new fields, no renames).
- The single-binary distribution story.
- The loopback-only security boundary (until v0.9+1 considers token-bearer remote auth).

---

## v0.10.0 — "Config + packaging" ✅ SHIPPED

**Shipped:** per-machine `params:` map (nested `{{.Params.key}}`,
defaults-merge with machine-wins, round-trips cleanly because Lookup
merges at read time while the stored map keeps only own-params; ETag
includes sorted params; settable via fleet.yaml / API / admin form)
and the multi-arch container image (GHCR, non-root + setcap,
`VOLUME /var/lib/pxe-beacon`, `--network host` + `--cap-add` documented).

## v0.11.0 — "Rescue" ✅ SHIPPED

**Shipped:** SystemRescue as a real rescue boot target. `pxe-beacon
fetch systemrescue` downloads + extracts the ISO preserving its native
archiso tree (`sysresccd/...`); the per-MAC dispatch arm boots it when a
rescue intent is queued (`PUT /intent {"action":"rescue"}` — un-501'd),
regardless of the machine's configured `boot:` target. Served via a
wildcard `/assets/{target}/{file...}` route because archiso constructs
the squashfs URL itself (`archiso_http_srv` + `archisobasedir` →
`/assets/systemrescue/sysresccd/x86_64/airootfs.sfs`).

Access reuses the cloud-init *delivery pattern* (not cloud-init itself —
SystemRescue is Arch/archiso): a per-MAC `sysrescuecfg` YAML at
`/autoinstall/{mac}/sysrescue.yaml`, templated from `params`. SSH key
(`params.ssh_authorized_key`) rides an `autorun` setup script (SystemRescue
YAML runs external scripts, not inline); root password
(`params.rescue_root_password`, defaults to `pxe`) is the one native key.
Custom config via the optional `rescue:` profile field (parallel to
`cloud_init:`).

**Decision:** rescue is **per-MAC** via the existing intent API (not a
global arm) — consistent with everything else and reuses the whole
pending/dispatch path.

## v0.12.0 — "Secure callbacks" ✅ SHIPPED

**Shipped:** the public phone-home callback is now authenticated. Each
served cloud-init carries a short-lived HMAC bearer token
(`<expUnix>.<hmac(secret, mac+exp)>`, secret from
`$PXE_BEACON_TOKEN_SECRET` with a logged random fallback, `-callback-ttl`
default 24h); `POST /done` + `POST /log` 403 a missing/invalid one.
Enforced by default, `-insecure-callbacks` for migration.

**Key design decision (from the conversation):** pxe-beacon **owns**
`phone_home` rather than asking operators to add a token. It appends its
own tokenized `phone_home` to every served cloud-init (YAML payloads;
warns + skips on script/jinja/multipart), and **fleet load errors** if an
operator file defines its own — chosen over silent override because a doc
can't carry two `phone_home` keys. `/events` stays loopback-only (no
token). Debian/RHEL installed-system callbacks: the preseed bridge
re-fetches `/user-data` at first boot (token minted fresh then); the
kickstart `%post` cloud-init carries the token inline.

Install diagnostics: installer failure hooks (Subiquity `error-commands`,
kickstart `%onerror`) POST logs to the token-guarded `POST /log`; read at
`GET /api/v1/machines/{mac}/logs` (in-memory ~64 KiB/MAC, loopback-only).

## Next: "Persistence + history" (features)

**Theme:** State survives restart; audit log is the same journal.
See the detailed v0.10.0-era plan below (NDJSON journal + replay +
compaction) — still the intended shape.

### Out of scope

- BMC integration + `POST /power` — deferred (see bottom).

---

## v0.9.0 — "API as a contract" ✅ SHIPPED (2026-05-25)

**Theme:** One control plane, one error shape, one state shape, one spec — so Terraform / Ansible / React Query / LLM agents have a stable surface.
**Headline:** "Terraform-grade API. OpenAPI spec. Sunset policy."

All 10 items below shipped. Notable implementation decisions from the
review panel: a dedicated `saveMu` makes fleet CRUD transactional with
rollback-on-Save-failure; `Content-Type: application/json` enforcement
is the CSRF defense (admin became a fetch() client, no token); per-entry
ETags (not file mtime); `installer-failed` keeps pending intent and
stays off the monotonic event ladder.

### Items (all shipped)

| # | Item | Why | Effort |
|---|---|---|---|
| 1 | **Structured error envelope** with machine-readable `code` field | Today: `{"error":"..."}` free-text. Future: `{"code":"rescue_unimplemented","message":"...","details":{...}}`. Foundation for OpenAPI codegen + Terraform provider error wrapping. | small |
| 2 | **Unified content-negotiating error helper** replacing `http.Error` + `writeAPIError` + `redirectFlash` | Today four error patterns. Negotiate JSON vs HTML by `Accept`. | medium |
| 3 | **`/admin/fleet` CRUD absorbed into `/api/v1/machines`** (POST/PUT/DELETE) | Today: form-encoded HTML, 303 redirects, hex-flash. Unusable from Terraform. Same `Fleet` machinery; admin HTML becomes a *client* of the JSON API. | medium |
| 4 | **`POST /autoinstall/{mac}/done` → `POST /api/v1/machines/{mac}/events`** | Structurally an "agent reports observed state" call. JSON body lets cloud-init report failures with reasons. Old path kept as deprecated alias for one release. | small |
| 5 | **Unify `/status.json` ↔ `/api/v1/machines` shape** (nested `desired`/`observed`) | UI dev trap today: flat in /status.json, nested in /api/v1. Deprecate /status.json. | small |
| 6 | **Pagination on `GET /api/v1/machines`** (`?limit=&offset=&total=`) | Reserve query-param namespace before there's a Terraform provider that depends on the current shape. | small |
| 7 | **ETag / If-Match on fleet entries** | Profile hash → 412 on mismatch. Unlocks safe concurrent Terraform-style edits without lost-update. | medium |
| 8 | **`/healthz` + `/readyz`** with structured status | Surfaces `last_fleet_reload_ok`, `pending_count`, `tracker_count`. Three-line refactor of existing `apiReady` / `fleetReady`. | trivial |
| 9 | **OpenAPI 3 spec** scoped to `/api/v1/*` only | Single `openapi.yaml`, ~200 lines. Enables TypeScript SDK + Go server stub codegen. Authoritative contract document. | medium |
| 10 | **RFC 8594 versioning policy** documented in README | `Deprecation: true` + `Sunset: <RFC 1123 date>` headers. Minimum 2-minor-release overlap (v2 in N.0, v1 sunset no earlier than N+2.0). | docs |

### Strict ordering

`#1 (error envelope)` blocks `#2`, `#9`. `#3 (admin absorption)` blocks `#7`, `#9`. `#4 (events endpoint)` is independent. `#5`, `#6`, `#8`, `#10` are independent and can ship anywhere in the window.

### Out of scope for v0.9

- Persistence — v0.10
- SSE live updates — v0.11+ (wait for 2nd UI consumer)
- Stable `system_id` — v0.11+
- Batch endpoints — wait for OpenAPI to settle then add

---

## v0.10.0 — "Persistence + history"

**Theme:** State survives restart. Audit log is the same journal. History is queryable. Closes the v0.8.1 README caveat about the guard wiping on restart.
**Estimate:** 2–3 weeks.
**Headline:** "Boring infrastructure."

### Items

| # | Item | Detail |
|---|---|---|
| 1 | **NDJSON history journal** at `<data-dir>/history.ndjson` | One JSON object per line. Three event kinds: `intent.set`, `intent.cancel` (from PUT /intent), `event` (from Tracker.Note). |
| 2 | **Startup replay** | Stream the file, apply events in order to in-memory `Tracker` + `Pending`. Bad-line tolerant (log and skip). |
| 3 | **Periodic compaction** | Size-triggered at 1MB: write `{"kind":"snapshot","tracker":{...},"pending":{...}}` event, then atomic-rename `history.ndjson.new` over `history.ndjson`. Pre-snapshot events dropped. |
| 4 | **`GET /api/v1/machines/{mac}/events`** | Real endpoint. Returns post-snapshot tail filtered by MAC. Paginated. |
| 5 | **Audit log = same journal** | Single source of truth. `logIntent` writes to the journal AND stderr (for ops greppability). |
| 6 | **README update** | Remove the "guard wipes on restart" caveat. Document the journal location, compaction policy, and how to rotate manually. |

### Why NDJSON, not SQLite

- Scale is ~5K events/decade at high end — DB indexes are pointless
- No new dependency (~3MB binary saved)
- `tail -f history.ndjson | jq` works at the shell
- New event kinds = "write a new JSON object", no schema migrations
- Single-writer process — ACID overkill

### Settled design decisions

- **Compaction trigger:** size-based (1MB)
- **Pre-snapshot retention:** drop (operators wanting deep history snapshot externally via `cp history.ndjson archive/`)
- **Corruption tolerance:** log bad lines, continue
- **File location:** `<data-dir>/history.ndjson` (operators already bind-mount this dir)

---

## Deferred beyond v0.10 (queued, not promised)

| Item | Notes |
|---|---|
| **BMC integration + `POST /power`** | **Deferred 2026-05-25.** Redfish vocabulary, 202-async, per-BMC mutex, `bmc:` schema in `machineYAML`. The full spec lives in git history (v0.8.2 plan + the architectural-batch review). Pick back up when remote power-cycling is actually needed. |
| SSE for live observed state | Wait for second UI consumer; polling at homelab scale is trivial |
| Stable `system_id` (MACs as list) | DC engineer's bigger refactor; needs migration story |
| Batch endpoints (`POST /api/v1/machines:batch_install`) | Design after OpenAPI settles |
| Multi-arch dispatch (un-refuse ARM64) | Distinct feature; needs per-arch mirror config |
| Enlistment-lite (auto-add unknown MACs to /admin pending list) | Distinct UX feature |
| Webhook notifications (`installer-done`, `installer-failed`) | Fold into the events resource |
| Per-MAC mutex on `Pending` | Only matters if pending grows per-entry metadata |
| Remote API access (token-bearer auth, lift loopback-only) | Needs CA/secret-management story |
