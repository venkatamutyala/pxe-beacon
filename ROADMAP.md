# pxe-beacon roadmap

Living document. Updated as releases ship.

## Where we are

**v0.8.1 shipped** (current).
Closed the safety footguns from v0.8.0: already-installed guard, listener
races, ARM64 hard-refuse, rescue 501, audit logging. K8s-style declarative
`PUT /api/v1/machines/{mac}/intent` is the stable boot-intent contract.

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

## v0.8.2 — "Power and parameters" (features)

**Theme:** Un-stub everything v0.8.1 promised. Ship the BMC + rescue + templating story the panel kept asking about.
**Estimate:** 2–3 weeks.
**Headline:** "Per-machine config, real rescue, remote power."

### Items

| # | Item | Why | Effort |
|---|---|---|---|
| 1 | **`params:` map in `machineYAML`** + Go template substitution into preseed / cloud-init / kickstart | Foreman expert's #1 ask. Today N machines with N hostnames = N preseed copies. One map, one merge. | small |
| 2 | **Real SystemRescue rescue boot target** in `dispatch.go` + un-501 the API | One global arm (rescue isn't per-machine). Use SystemRescue HTTP mirror, `archisobasedir=sysresccd` cmdline. | small |
| 3 | **`bmc:` schema in `machineYAML`** (`address`, `protocol`, `username`, `password_env`, `insecure_tls`) | DC engineer's foundation. Pointer struct so absent = nil = no BMC. Validated at `validateProfile`. | small |
| 4 | **`POST /api/v1/machines/{mac}/power`** — real, Redfish vocabulary | Body: `{"reset_type":"ForceRestart"\|"GracefulRestart"\|"PowerCycle"\|"On"\|"ForceOff"\|"GracefulShutdown"}`. Returns **202 + status URL**. **Per-BMC mutex** to respect iLO's ~5-conn cap. Pass-through to BMC; surface BMC's error verbatim. | medium |
| 5 | **Bootstrap tokens for `/autoinstall/{mac}/done`** | HMAC-derived from server secret (`HMAC-SHA256(serverSecret, mac \|\| requestedAt-truncated-to-15min)`). Survives restart, no on-disk state, naturally TTL-bound. Templated into cloud-init via `phone_home.url: .../done?t=<token>`. Validated via constant-time compare. Mismatch → 403, no state change. | medium |
| 6 | **`POST /autoinstall/{mac}/log` capture endpoint** | DC's missing-diagnostic gap. Cloud-init `runcmd` posts kernel ring buffer + cloud-init logs on success AND on `errors:` hook. In-memory ring per MAC, viewable via `/api/v1/machines/{mac}/logs` (last ~64KB). | small |
| 7 | **Container image** | Multi-stage Dockerfile, `setcap cap_net_bind_service+ep` before `USER nonroot`, `VOLUME /var/lib/pxe-beacon`, `EXPOSE 67/udp 69/udp 4011/udp 8080/tcp`, README docs `--network host` + `--cap-add=NET_BIND_SERVICE`. GHCR push in release matrix. | small |
| 8 | **Quick-win bugfixes** from the API review | • "Fleet mode not enabled" returns 404 → fix to 503<br>• `handleAdminFleetDelete` returns 404 on second call → 200 (idempotency)<br>• `handleStatusJSON` silently swallows encode errors → buffer-then-flush<br>• Default shutdown drain 3s → 30s (configurable) | trivial |

### Out of scope for v0.8.2

- API surface absorption (admin/fleet → /api/v1/*) — that's v0.9
- Structured error envelope — v0.9
- Persistence — v0.10

---

## v0.9.0 — "API as a contract"

**Theme:** One control plane, one error shape, one state shape, one spec — so Terraform / Ansible / React Query / LLM agents have a stable surface.
**Estimate:** 3–4 weeks.
**Headline:** "Terraform-grade API. OpenAPI spec. Sunset policy."

### Items (ordering matters)

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
| SSE for live observed state | Wait for second UI consumer; polling at homelab scale is trivial |
| Stable `system_id` (MACs as list) | DC engineer's bigger refactor; needs migration story |
| Batch endpoints (`POST /api/v1/machines:batch_install`) | Design after OpenAPI settles |
| Multi-arch dispatch (un-refuse ARM64) | Distinct feature; needs per-arch mirror config |
| Enlistment-lite (auto-add unknown MACs to /admin pending list) | Distinct UX feature |
| Webhook notifications (`installer-done`, `installer-failed`) | Fold into the events resource |
| Per-MAC mutex on `Pending` | Only matters if pending grows per-entry metadata |
| Remote API access (token-bearer auth, lift loopback-only) | Needs CA/secret-management story |
