package httpd

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/venkatamutyala/pxe-beacon/internal/fleet"
	"github.com/venkatamutyala/pxe-beacon/internal/pending"
	"github.com/venkatamutyala/pxe-beacon/pkg/pxebeacon"
)

// v0.8.0 K8s-style boot-intent API.
//
// One resource per machine, idempotent PUT to set desired state.
// Compared to v0.7.1's POST /deploy / /rescue / /cancel verbs:
//
//	PUT  /api/v1/machines/{mac}/intent   {"action": "install"|"rescue"|null}
//	GET  /api/v1/machines/{mac}/intent   → {desired, observed}
//	GET  /api/v1/machines                → list with desired + observed
//	GET  /api/v1/machines/{mac}          → single with desired + observed
//
// Picked over per-feature subtrees (Hetzner-style /boot/install +
// /boot/rescue) for tool-friendliness: Terraform / Ansible / React
// Query map cleanly to PUT-on-resource. See the v0.8.0 PM review
// in plan history.

// The wire types (Machine, Desired, Observed, Intent, MachineConfig,
// APIError, the response bodies) and the ErrCode constants moved to
// the importable pkg/pxebeacon package in v0.9.1 so SDK/Terraform/UI
// authors can depend on them. This file builds and returns them.

// intentPUTBody is what a client PUTs to set desired intent. We parse
// Action as a json.RawMessage so we can distinguish three states the
// spec demands:
//
//	missing key             → 400 (operator must be explicit)
//	"action": null          → clear pending
//	"action": "install"|... → queue that action
//
// Kept unexported here (not in pkg/pxebeacon): the RawMessage is a
// server-side parsing detail, not part of the documented wire shape —
// the OpenAPI spec documents the body as {action: string|null}.
type intentPUTBody struct {
	Action json.RawMessage `json:"action"`
}

// apiReady asserts fleet + Pending are wired.
//
// v0.9.0: returns 503 (not 404) when fleet mode isn't enabled. 404
// implied "you have the URL wrong"; the right semantic is "service is
// up but this part of it isn't configured."
func (s *Server) apiReady(w http.ResponseWriter) bool {
	if s.opts.Fleet == nil || s.opts.FleetStatus == nil {
		writeAPIError(w, http.StatusServiceUnavailable, pxebeacon.ErrCodeFleetNotLoaded,
			"fleet mode not enabled (start pxe-beacon with -config <fleet.yaml>)", nil)
		return false
	}
	if s.opts.Pending == nil {
		writeAPIError(w, http.StatusServiceUnavailable, pxebeacon.ErrCodePendingNotConfigured,
			"pending-action store not configured", nil)
		return false
	}
	return true
}

func (s *Server) apiExtractFleetMAC(w http.ResponseWriter, r *http.Request) (string, fleet.Profile) {
	raw := r.PathValue("mac")
	if raw == "" {
		writeAPIError(w, http.StatusBadRequest, pxebeacon.ErrCodeMACMissing, "missing mac in URL", nil)
		return "", fleet.Profile{}
	}
	canon, err := fleet.CanonicalMAC(raw)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, pxebeacon.ErrCodeMACInvalid,
			"invalid mac: "+err.Error(),
			map[string]any{"input": raw})
		return "", fleet.Profile{}
	}
	p := s.opts.Fleet.Lookup(canon)
	if p.Name == "" {
		writeAPIError(w, http.StatusNotFound, pxebeacon.ErrCodeMACNotInFleet,
			"mac "+canon+" is not in fleet.yaml",
			map[string]any{"mac": canon})
		return "", fleet.Profile{}
	}
	return canon, p
}

// handleAPISetIntent — PUT /api/v1/machines/{mac}/intent
// Idempotent. Body: {"action": "install" | "rescue" | null}.
// Null clears any pending intent. Missing "action" field is a 400.
func (s *Server) handleAPISetIntent(w http.ResponseWriter, r *http.Request) {
	if !s.apiReady(w) {
		return
	}
	mac, p := s.apiExtractFleetMAC(w, r)
	if mac == "" {
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 8192))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, pxebeacon.ErrCodeBodyInvalid,
			"read body: "+err.Error(), nil)
		return
	}
	var in intentPUTBody
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeAPIError(w, http.StatusBadRequest, pxebeacon.ErrCodeBodyInvalid,
			"decode body: "+err.Error(), nil)
		return
	}
	// Require explicit "action" key. A naked {} is ambiguous.
	if len(in.Action) == 0 {
		writeAPIError(w, http.StatusBadRequest, pxebeacon.ErrCodeActionMissing,
			`body must include "action" key with value "install", "rescue", or null`,
			map[string]any{"accepted": []string{"install", "rescue", "null"}})
		return
	}
	// Decode the action value. JSON null → empty string (= cancel).
	// A string value → that string. Anything else → 400.
	var action string
	if string(in.Action) != "null" {
		if err := json.Unmarshal(in.Action, &action); err != nil {
			writeAPIError(w, http.StatusBadRequest, pxebeacon.ErrCodeActionInvalid,
				`"action" must be a string or null; got `+string(in.Action),
				map[string]any{"got": string(in.Action), "accepted": []string{"install", "rescue", "null"}})
			return
		}
	}
	switch action {
	case "install":
		if _, err := s.opts.Pending.Install(mac); err != nil {
			writeAPIError(w, http.StatusInternalServerError, pxebeacon.ErrCodePendingFailed,
				"install: "+err.Error(), nil)
			return
		}
		s.logIntent(r, mac, p.Name, "install", 200)
	case "rescue":
		// v0.11.0: rescue queues a SystemRescue boot. The dispatch
		// script renders the rescue arm for any MAC with this intent
		// (requires `pxe-beacon fetch systemrescue` to have populated
		// the data-dir; access via params.rescue_root_password /
		// params.ssh_authorized_key, served at /autoinstall/{mac}/sysrescue.yaml).
		if _, err := s.opts.Pending.Rescue(mac); err != nil {
			writeAPIError(w, http.StatusInternalServerError, pxebeacon.ErrCodePendingFailed,
				"rescue: "+err.Error(), nil)
			return
		}
		s.logIntent(r, mac, p.Name, "rescue", 200)
	case "":
		s.opts.Pending.Cancel(mac)
		s.logIntent(r, mac, p.Name, "cancel", 200)
	default:
		writeAPIError(w, http.StatusBadRequest, pxebeacon.ErrCodeActionInvalid,
			`action must be "install", "rescue", or null; got `+action,
			map[string]any{"got": action, "accepted": []string{"install", "rescue", "null"}})
		return
	}
	writeJSON(w, s.buildIntentView(mac))
}

// handleAPIGetIntent — GET /api/v1/machines/{mac}/intent
func (s *Server) handleAPIGetIntent(w http.ResponseWriter, r *http.Request) {
	if !s.apiReady(w) {
		return
	}
	mac, _ := s.apiExtractFleetMAC(w, r)
	if mac == "" {
		return
	}
	writeJSON(w, s.buildIntentView(mac))
}

// handleAPIMachine — GET /api/v1/machines/{mac}
// v0.9.0: sets the ETag header so clients can If-Match on PUT/DELETE.
func (s *Server) handleAPIMachine(w http.ResponseWriter, r *http.Request) {
	if !s.fleetReady(w, r) {
		return
	}
	mac, p := s.apiExtractFleetMAC(w, r)
	if mac == "" {
		return
	}
	if etag, ok := s.opts.Fleet.ETag(mac); ok {
		w.Header().Set("ETag", etag)
	}
	writeJSON(w, s.buildMachineView(mac, p))
}

// requireJSON enforces Content-Type: application/json on fleet-mutation
// endpoints. This is the v0.9.0 CSRF defense: a cross-origin browser
// fetch with application/json triggers a CORS preflight that fails
// (pxe-beacon sends no CORS headers), so a malicious page can't drive
// these mutations through the loopback gate. A form POST (which CAN go
// cross-origin without preflight) is rejected here with 415.
func (s *Server) requireJSON(w http.ResponseWriter, r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	if strings.TrimSpace(ct) != "application/json" {
		writeAPIError(w, http.StatusUnsupportedMediaType, pxebeacon.ErrCodeContentType,
			"Content-Type must be application/json",
			map[string]any{"got": r.Header.Get("Content-Type")})
		return false
	}
	return true
}

// decodeMachineBody reads + strictly decodes the JSON config body.
func decodeMachineBody(w http.ResponseWriter, r *http.Request) (pxebeacon.MachineConfig, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 16384))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, pxebeacon.ErrCodeBodyInvalid, "read body: "+err.Error(), nil)
		return pxebeacon.MachineConfig{}, false
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var b pxebeacon.MachineConfig
	if err := dec.Decode(&b); err != nil {
		writeAPIError(w, http.StatusBadRequest, pxebeacon.ErrCodeBodyInvalid, "decode body: "+err.Error(), nil)
		return pxebeacon.MachineConfig{}, false
	}
	return b, true
}

// profileFromBody validates boot + resolves relative side-file paths
// against the fleet.yaml directory (same rule as the admin form).
func (s *Server) profileFromBody(w http.ResponseWriter, r *http.Request, b pxebeacon.MachineConfig) (fleet.Profile, bool) {
	if !fleet.ValidBootTargets[b.Boot] {
		writeAPIError(w, http.StatusBadRequest, pxebeacon.ErrCodeBootInvalid,
			"unknown boot target", map[string]any{"boot": b.Boot})
		return fleet.Profile{}, false
	}
	resolve := func(p string) string {
		if p == "" || filepath.IsAbs(p) {
			return p
		}
		return filepath.Clean(filepath.Join(s.opts.Fleet.BaseDir(), p))
	}
	return fleet.Profile{
		Name:       b.Name,
		Boot:       b.Boot,
		Preseed:    resolve(b.Preseed),
		Kickstart:  resolve(b.Kickstart),
		CloudInit:  resolve(b.CloudInit),
		Rescue:     resolve(b.Rescue),
		IPXEScript: resolve(b.IPXEScript),
		Params:     b.Params,
	}, true
}

// handleAPICreateMachine — POST /api/v1/machines
func (s *Server) handleAPICreateMachine(w http.ResponseWriter, r *http.Request) {
	if !s.fleetReady(w, r) || !s.requireJSON(w, r) {
		return
	}
	b, ok := decodeMachineBody(w, r)
	if !ok {
		return
	}
	canon, err := fleet.CanonicalMAC(b.MAC)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, pxebeacon.ErrCodeMACInvalid,
			"invalid mac: "+err.Error(), map[string]any{"input": b.MAC})
		return
	}
	p, ok := s.profileFromBody(w, r, b)
	if !ok {
		return
	}
	etag, err := s.opts.Fleet.CreateAndSave(fleet.Machine{MAC: canon, Profile: p})
	if err != nil {
		s.writeFleetMutationError(w, r, canon, "create", err)
		return
	}
	s.logFleetMutation(r, canon, p.Name, "create", 201)
	w.Header().Set("ETag", etag)
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, s.buildMachineView(canon, p))
}

// handleAPIUpdateMachine — PUT /api/v1/machines/{mac}. If-Match required.
func (s *Server) handleAPIUpdateMachine(w http.ResponseWriter, r *http.Request) {
	if !s.fleetReady(w, r) || !s.requireJSON(w, r) {
		return
	}
	raw := r.PathValue("mac")
	canon, err := fleet.CanonicalMAC(raw)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, pxebeacon.ErrCodeMACInvalid,
			"invalid mac: "+err.Error(), map[string]any{"input": raw})
		return
	}
	b, ok := decodeMachineBody(w, r)
	if !ok {
		return
	}
	p, ok := s.profileFromBody(w, r, b)
	if !ok {
		return
	}
	etag, err := s.opts.Fleet.UpdateAndSave(fleet.Machine{MAC: canon, Profile: p}, r.Header.Get("If-Match"))
	if err != nil {
		s.writeFleetMutationError(w, r, canon, "update", err)
		return
	}
	s.logFleetMutation(r, canon, p.Name, "update", 200)
	w.Header().Set("ETag", etag)
	writeJSON(w, s.buildMachineView(canon, p))
}

// handleAPIDeleteMachine — DELETE /api/v1/machines/{mac}. Idempotent;
// If-Match honored when present.
func (s *Server) handleAPIDeleteMachine(w http.ResponseWriter, r *http.Request) {
	if !s.fleetReady(w, r) {
		return
	}
	raw := r.PathValue("mac")
	canon, err := fleet.CanonicalMAC(raw)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, pxebeacon.ErrCodeMACInvalid,
			"invalid mac: "+err.Error(), map[string]any{"input": raw})
		return
	}
	existed, err := s.opts.Fleet.DeleteAndSave(canon, r.Header.Get("If-Match"))
	if err != nil {
		s.writeFleetMutationError(w, r, canon, "delete", err)
		return
	}
	// Dropping a machine from the fleet should also clear any pending
	// intent for it (same as the SIGHUP reload prune).
	if existed && s.opts.Pending != nil {
		s.opts.Pending.Cancel(canon)
	}
	s.logFleetMutation(r, canon, "", "delete", 204)
	w.WriteHeader(http.StatusNoContent)
}

// writeFleetMutationError maps fleet sentinel errors to HTTP responses.
func (s *Server) writeFleetMutationError(w http.ResponseWriter, r *http.Request, mac, op string, err error) {
	switch {
	case errors.Is(err, fleet.ErrMACExists):
		writeAPIError(w, http.StatusConflict, pxebeacon.ErrCodeMACExists,
			"machine "+mac+" already exists (use PUT to update)", map[string]any{"mac": mac})
	case errors.Is(err, fleet.ErrMACAbsent):
		writeAPIError(w, http.StatusNotFound, pxebeacon.ErrCodeMACNotInFleet,
			"machine "+mac+" is not in fleet.yaml", map[string]any{"mac": mac})
	case errors.Is(err, fleet.ErrPreconditionRequired):
		writeAPIError(w, http.StatusPreconditionRequired, pxebeacon.ErrCodePreconditionRequired,
			"If-Match header required (GET the resource first for its ETag)", map[string]any{"mac": mac})
	case errors.Is(err, fleet.ErrPreconditionFailed):
		writeAPIError(w, http.StatusPreconditionFailed, pxebeacon.ErrCodePreconditionFailed,
			"If-Match does not match current ETag (resource changed; re-GET)", map[string]any{"mac": mac})
	default:
		// Validation, bad MAC, or Save failure.
		s.log.Errorf("%s %s failed for %s: %v", op, r.URL.Path, mac, err)
		writeAPIError(w, http.StatusInternalServerError, pxebeacon.ErrCodeSaveFailed,
			op+": "+err.Error(), nil)
	}
}

// logFleetMutation is the structured audit line for fleet config
// mutations (the higher bar config mutation gets in lieu of CSRF).
func (s *Server) logFleetMutation(r *http.Request, mac, name, op string, status int) {
	s.log.Infof("audit event=fleet-mutation op=%s target_mac=%s target_name=%q result=%d from=%s",
		op, mac, name, status, r.RemoteAddr)
}

// defaultPageLimit caps an unbounded GET /api/v1/machines so a huge
// fleet never returns an unbounded body. Clients page past it with
// ?offset=. v0.9.0+.
const defaultPageLimit = 500

// handleAPIList — GET /api/v1/machines?limit=&offset=
//
// v0.9.0: paginated. `limit` defaults to 500 (also the max); `offset`
// defaults to 0. Response includes `total` (full fleet size before
// paging) plus `limit`/`offset` echoes so clients can iterate. The
// underlying ListMachines() is stably sorted, so offset paging is
// deterministic.
func (s *Server) handleAPIList(w http.ResponseWriter, r *http.Request) {
	if !s.fleetReady(w, r) {
		return
	}

	limit, offset, ok := parsePaging(w, r)
	if !ok {
		return
	}

	machines := s.opts.Fleet.ListMachines()
	total := len(machines)

	// Clamp the window to the slice bounds.
	lo := offset
	if lo > total {
		lo = total
	}
	hi := lo + limit
	if hi > total {
		hi = total
	}
	page := machines[lo:hi]

	out := make([]pxebeacon.Machine, 0, len(page))
	for _, m := range page {
		out = append(out, s.buildMachineView(m.MAC, m.Profile))
	}
	writeJSON(w, pxebeacon.ListResponse{
		PendingTTLs: pendingTTLSeconds(s.opts.Pending),
		Total:       total,
		Limit:       limit,
		Offset:      offset,
		Machines:    out,
	})
}

// parsePaging reads + validates ?limit= and ?offset=. On bad input it
// writes a structured 400 and returns ok=false. Empty params take the
// defaults (limit=defaultPageLimit, offset=0).
func parsePaging(w http.ResponseWriter, r *http.Request) (limit, offset int, ok bool) {
	limit = defaultPageLimit
	offset = 0
	q := r.URL.Query()
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeAPIError(w, http.StatusBadRequest, pxebeacon.ErrCodePagingInvalid,
				"limit must be a non-negative integer", map[string]any{"got": v})
			return 0, 0, false
		}
		if n == 0 || n > defaultPageLimit {
			n = defaultPageLimit
		}
		limit = n
	}
	if v := q.Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeAPIError(w, http.StatusBadRequest, pxebeacon.ErrCodePagingInvalid,
				"offset must be a non-negative integer", map[string]any{"got": v})
			return 0, 0, false
		}
		offset = n
	}
	return limit, offset, true
}

// handleAPIDiscovered — GET /api/v1/discovered (v0.13.0). Lists unknown
// MACs seen PXE-booting, for one-click enrollment. MACs that have since
// become fleet members are filtered out (a discovered box vanishes from
// the feed once enrolled).
func (s *Server) handleAPIDiscovered(w http.ResponseWriter, r *http.Request) {
	if s.opts.Sightings == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, pxebeacon.ErrCodeFleetNotLoaded,
			"discovery not enabled", nil)
		return
	}
	limit, offset, ok := parsePaging(w, r)
	if !ok {
		return
	}
	all := s.opts.Sightings.List()
	// Drop MACs that are now in the fleet.
	filtered := all[:0]
	for _, sg := range all {
		if s.opts.Fleet == nil || s.opts.Fleet.Lookup(sg.MAC).Name == "" {
			filtered = append(filtered, sg)
		}
	}
	total := len(filtered)
	lo := offset
	if lo > total {
		lo = total
	}
	hi := lo + limit
	if hi > total {
		hi = total
	}
	page := filtered[lo:hi]
	out := make([]pxebeacon.Sighting, 0, len(page))
	for _, sg := range page {
		out = append(out, pxebeacon.Sighting{
			MAC: sg.MAC, Arch: sg.Arch, Vendor: sg.Vendor, VendorClass: sg.VendorClass,
			FirstSeen: sg.FirstSeen, LastSeen: sg.LastSeen, Count: sg.Count,
		})
	}
	writeJSON(w, pxebeacon.DiscoveredResponse{
		Total: total, Limit: limit, Offset: offset, Discovered: out,
	})
}

// handleAPIDismissDiscovered — DELETE /api/v1/discovered/{mac} (v0.13.0).
// Idempotent: dismissing an absent sighting still returns 204.
func (s *Server) handleAPIDismissDiscovered(w http.ResponseWriter, r *http.Request) {
	if s.opts.Sightings == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, pxebeacon.ErrCodeFleetNotLoaded,
			"discovery not enabled", nil)
		return
	}
	mac, err := fleet.CanonicalMAC(r.PathValue("mac"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, pxebeacon.ErrCodeMACInvalid,
			"invalid MAC", map[string]any{"mac": r.PathValue("mac")})
		return
	}
	s.opts.Sightings.Forget(mac)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) buildDesired(canon string) pxebeacon.Desired {
	v := pxebeacon.Desired{}
	if s.opts.Pending != nil {
		action, at, exp, ok := s.opts.Pending.Status(canon)
		if ok {
			v.Action = string(action)
			v.RequestedAt = at
			v.ExpiresAt = exp
		}
	}
	return v
}

func (s *Server) buildObserved(canon string) pxebeacon.Observed {
	v := pxebeacon.Observed{}
	if s.opts.FleetStatus != nil {
		for _, st := range s.opts.FleetStatus.Snapshot() {
			if st.MAC == canon {
				v.Phase = string(st.State)
				v.LastSeen = st.LastSeen
				break
			}
		}
	}
	return v
}

func (s *Server) buildIntentView(canon string) pxebeacon.Intent {
	return pxebeacon.Intent{
		MAC:      canon,
		Desired:  s.buildDesired(canon),
		Observed: s.buildObserved(canon),
	}
}

func (s *Server) buildMachineView(canon string, p fleet.Profile) pxebeacon.Machine {
	return pxebeacon.Machine{
		MAC:      canon,
		Name:     p.Name,
		Boot:     p.Boot,
		Desired:  s.buildDesired(canon),
		Observed: s.buildObserved(canon),
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// writeAPIError emits a structured v0.9.0+ JSON error envelope. Used
// directly by the /api/v1/* handlers, which are always JSON. `status`
// is the HTTP status code; `code` is the stable machine-readable
// identifier; `msg` is human prose; `details` is optional context.
//
// Clients should branch on `code` (one of the pxebeacon.ErrCode* constants),
// not on `msg` — message text is allowed to change between releases
// for clarity, codes are not.
func writeAPIError(w http.ResponseWriter, status int, code pxebeacon.ErrCode, msg string, details map[string]any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(pxebeacon.APIError{
		Code:    code,
		Message: msg,
		Details: details,
	})
}

// wantsJSON reports whether the caller wants a JSON error body. True
// for any /api/ path, or when the Accept header asks for JSON. The
// /autoinstall/* and /assets/* wire endpoints (cloud-init, d-i,
// Anaconda) don't send Accept: application/json, so they keep getting
// plain text — which is what those consumers parse. v0.9.0+.
func wantsJSON(r *http.Request) bool {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		return true
	}
	return strings.Contains(r.Header.Get("Accept"), "application/json")
}

// writeError is the v0.9.0 unified, content-negotiating error path. It
// emits the structured JSON envelope to JSON clients (wantsJSON) and
// plain text otherwise. Replaces scattered http.Error calls so the
// machine-facing surface has one error mechanism.
//
// (The /admin HTML flow keeps redirectFlash until v0.9 item #3 folds
// admin mutations into /api/v1/*.)
func (s *Server) writeError(w http.ResponseWriter, r *http.Request, status int, code pxebeacon.ErrCode, msg string, details map[string]any) {
	if wantsJSON(r) {
		writeAPIError(w, status, code, msg, details)
		return
	}
	http.Error(w, msg, status)
}

func pendingTTLSeconds(s *pending.Store) int {
	if s == nil {
		return 0
	}
	return int(s.TTL() / time.Second)
}

// handleHealthz — GET /healthz. Liveness probe. Returns 200 unconditionally
// as long as the HTTP server is answering. v0.9.0+.
//
// Body: {"status":"ok","uptime_s":N,"started_at":"..."}
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, pxebeacon.HealthzResponse{
		Status:    "ok",
		UptimeS:   int(time.Since(s.startedAt).Seconds()),
		StartedAt: s.startedAt.UTC().Format(time.RFC3339),
	})
}

// handleReadyz — GET /readyz. Readiness probe. Returns 200 when Fleet
// + FleetStatus + Pending are all wired (fleet mode is active); 503
// otherwise. Body always carries the structured component state so
// monitoring can pinpoint exactly which subsystem isn't ready.
// v0.9.0+.
//
// Body:
//
//	{
//	  "status": "ok" | "not_ready",
//	  "components": {
//	    "fleet":        "ok" | "not_loaded",
//	    "tracker":      "ok" | "not_loaded",
//	    "pending":      "ok" | "not_configured"
//	  },
//	  "pending_count":  N,    // when pending is loaded
//	  "tracker_count":  N     // when tracker is loaded
//	}
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	components := map[string]string{}
	ready := true

	if s.opts.Fleet != nil {
		components["fleet"] = "ok"
	} else {
		components["fleet"] = "not_loaded"
		ready = false
	}
	if s.opts.FleetStatus != nil {
		components["tracker"] = "ok"
	} else {
		components["tracker"] = "not_loaded"
		ready = false
	}
	if s.opts.Pending != nil {
		components["pending"] = "ok"
	} else {
		components["pending"] = "not_configured"
		ready = false
	}

	body := pxebeacon.ReadyzResponse{Components: components}
	if ready {
		body.Status = "ok"
		// Counts are only meaningful when components are loaded.
		pc := pendingCount(s.opts.Pending)
		tc := trackerCount(s.opts.FleetStatus)
		body.PendingCount = &pc
		body.TrackerCount = &tc
	} else {
		body.Status = "not_ready"
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if !ready {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(body)
}

func pendingCount(s *pending.Store) int {
	if s == nil {
		return 0
	}
	return s.Len()
}

func trackerCount(t *fleet.Tracker) int {
	if t == nil {
		return 0
	}
	return len(t.Snapshot())
}

// logIntent writes a structured audit-log line for every PUT /intent
// mutation. v0.8.1: format is key=value pairs (not free text) so v0.9
// can layer real user identity from bootstrap tokens without changing
// the schema, and ops tooling can grep by `target_mac=` or `action=`
// reliably.
//
// from= is always 127.0.0.1 today (loopbackOnly) but kept for v0.9
// when token-bearer auth lifts the loopback constraint.
func (s *Server) logIntent(r *http.Request, mac, name, action string, status int) {
	s.log.Infof("audit event=set-intent action=%s target_mac=%s target_name=%q result=%d from=%s",
		action, mac, name, status, r.RemoteAddr)
}
