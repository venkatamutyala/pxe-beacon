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

// desiredView is the operator's queued intent for next PXE boot.
type desiredView struct {
	Action      string    `json:"action,omitempty"` // "install" | "rescue" | ""
	RequestedAt time.Time `json:"requested_at,omitempty"`
	ExpiresAt   time.Time `json:"expires_at,omitempty"`
}

// observedView is what the install-progress tracker most recently saw.
type observedView struct {
	Phase    string    `json:"phase,omitempty"`
	LastSeen time.Time `json:"last_seen,omitempty"`
}

// machineAPIView is the JSON shape returned by GET /api/v1/machines/{mac}
// and (per-entry) by GET /api/v1/machines.
type machineAPIView struct {
	MAC      string       `json:"mac"`
	Name     string       `json:"name,omitempty"`
	Boot     string       `json:"boot,omitempty"`
	Desired  desiredView  `json:"desired"`
	Observed observedView `json:"observed"`
}

// intentView is the standalone shape returned by GET /api/v1/machines/{mac}/intent.
// Same desired + observed pair but at the resource root (no surrounding
// machine fields), matching K8s subresource conventions.
type intentView struct {
	MAC      string       `json:"mac"`
	Desired  desiredView  `json:"desired"`
	Observed observedView `json:"observed"`
}

// intentPUTBody is what a client PUTs to set desired intent. We
// parse Action as a json.RawMessage so we can distinguish three
// states the spec demands:
//
//	missing key             → 400 (operator must be explicit)
//	"action": null          → clear pending
//	"action": "install"|... → queue that action
type intentPUTBody struct {
	Action json.RawMessage `json:"action"`
}

// Error code constants. Stable identifiers — clients should branch on
// these, not on the prose `message`. New codes are additive; existing
// codes don't change meaning across releases. v0.9.0+.
const (
	ErrCodeFleetNotLoaded       = "fleet_not_loaded"
	ErrCodePendingNotConfigured = "pending_not_configured"
	ErrCodeMACMissing           = "mac_missing"
	ErrCodeMACInvalid           = "mac_invalid"
	ErrCodeMACNotInFleet        = "mac_not_in_fleet"
	ErrCodeBodyInvalid          = "body_invalid"
	ErrCodeActionMissing        = "action_missing"
	ErrCodeActionInvalid        = "action_invalid"
	ErrCodeRescueUnimplemented  = "rescue_unimplemented"
	ErrCodePendingFailed        = "pending_failed"
	ErrCodePagingInvalid        = "paging_invalid"
	// v0.9.0 fleet-CRUD codes.
	ErrCodeContentType          = "content_type_unsupported"
	ErrCodeMACExists            = "mac_exists"
	ErrCodeBootInvalid          = "boot_invalid"
	ErrCodeValidationFailed     = "validation_failed"
	ErrCodePreconditionRequired = "precondition_required"
	ErrCodePreconditionFailed   = "precondition_failed"
	ErrCodeSaveFailed           = "save_failed"
)

// apiErrorView is the v0.9.0+ structured error response. `code` is the
// machine-readable identifier clients should branch on; `message` is
// human-prose; `details` is an optional bag for context (e.g. accepted
// values, the offending field).
type apiErrorView struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// apiReady asserts fleet + Pending are wired.
//
// v0.9.0: returns 503 (not 404) when fleet mode isn't enabled. 404
// implied "you have the URL wrong"; the right semantic is "service is
// up but this part of it isn't configured."
func (s *Server) apiReady(w http.ResponseWriter) bool {
	if s.opts.Fleet == nil || s.opts.FleetStatus == nil {
		writeAPIError(w, http.StatusServiceUnavailable, ErrCodeFleetNotLoaded,
			"fleet mode not enabled (start pxe-beacon with -config <fleet.yaml>)", nil)
		return false
	}
	if s.opts.Pending == nil {
		writeAPIError(w, http.StatusServiceUnavailable, ErrCodePendingNotConfigured,
			"pending-action store not configured", nil)
		return false
	}
	return true
}

func (s *Server) apiExtractFleetMAC(w http.ResponseWriter, r *http.Request) (string, fleet.Profile) {
	raw := r.PathValue("mac")
	if raw == "" {
		writeAPIError(w, http.StatusBadRequest, ErrCodeMACMissing, "missing mac in URL", nil)
		return "", fleet.Profile{}
	}
	canon, err := fleet.CanonicalMAC(raw)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, ErrCodeMACInvalid,
			"invalid mac: "+err.Error(),
			map[string]any{"input": raw})
		return "", fleet.Profile{}
	}
	p := s.opts.Fleet.Lookup(canon)
	if p.Name == "" {
		writeAPIError(w, http.StatusNotFound, ErrCodeMACNotInFleet,
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
		writeAPIError(w, http.StatusBadRequest, ErrCodeBodyInvalid,
			"read body: "+err.Error(), nil)
		return
	}
	var in intentPUTBody
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeAPIError(w, http.StatusBadRequest, ErrCodeBodyInvalid,
			"decode body: "+err.Error(), nil)
		return
	}
	// Require explicit "action" key. A naked {} is ambiguous.
	if len(in.Action) == 0 {
		writeAPIError(w, http.StatusBadRequest, ErrCodeActionMissing,
			`body must include "action" key with value "install", "rescue", or null`,
			map[string]any{"accepted": []string{"install", "rescue", "null"}})
		return
	}
	// Decode the action value. JSON null → empty string (= cancel).
	// A string value → that string. Anything else → 400.
	var action string
	if string(in.Action) != "null" {
		if err := json.Unmarshal(in.Action, &action); err != nil {
			writeAPIError(w, http.StatusBadRequest, ErrCodeActionInvalid,
				`"action" must be a string or null; got `+string(in.Action),
				map[string]any{"got": string(in.Action), "accepted": []string{"install", "rescue", "null"}})
			return
		}
	}
	switch action {
	case "install":
		if _, err := s.opts.Pending.Install(mac); err != nil {
			writeAPIError(w, http.StatusInternalServerError, ErrCodePendingFailed,
				"install: "+err.Error(), nil)
			return
		}
		s.logIntent(r, mac, p.Name, "install", 200)
	case "rescue":
		// v0.8.1: rescue boot target not yet wired into the dispatch
		// script (tracked for v0.8.2). Return 501 and do NOT touch
		// the pending store — accepting rescue today would queue an
		// intent that silently boots the configured install OS
		// instead, which is worse than refusing the call.
		s.logIntent(r, mac, p.Name, "rescue", 501)
		writeAPIError(w, http.StatusNotImplemented, ErrCodeRescueUnimplemented,
			"rescue boot target not yet wired (tracked for v0.8.2); intent NOT queued",
			map[string]any{"tracked": "v0.8.2"})
		return
	case "":
		s.opts.Pending.Cancel(mac)
		s.logIntent(r, mac, p.Name, "cancel", 200)
	default:
		writeAPIError(w, http.StatusBadRequest, ErrCodeActionInvalid,
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

// machineConfigBody is the JSON body for POST/PUT /api/v1/machines.
// Mirrors the fleet.yaml per-machine fields. For POST, `mac` is taken
// from the body; for PUT it comes from the URL path and any body `mac`
// is ignored.
type machineConfigBody struct {
	MAC        string `json:"mac,omitempty"`
	Name       string `json:"name"`
	Boot       string `json:"boot"`
	Preseed    string `json:"preseed,omitempty"`
	Kickstart  string `json:"kickstart,omitempty"`
	CloudInit  string `json:"cloud_init,omitempty"`
	IPXEScript string `json:"ipxe_script,omitempty"`
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
		writeAPIError(w, http.StatusUnsupportedMediaType, ErrCodeContentType,
			"Content-Type must be application/json",
			map[string]any{"got": r.Header.Get("Content-Type")})
		return false
	}
	return true
}

// decodeMachineBody reads + strictly decodes the JSON config body.
func decodeMachineBody(w http.ResponseWriter, r *http.Request) (machineConfigBody, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 16384))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, ErrCodeBodyInvalid, "read body: "+err.Error(), nil)
		return machineConfigBody{}, false
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var b machineConfigBody
	if err := dec.Decode(&b); err != nil {
		writeAPIError(w, http.StatusBadRequest, ErrCodeBodyInvalid, "decode body: "+err.Error(), nil)
		return machineConfigBody{}, false
	}
	return b, true
}

// profileFromBody validates boot + resolves relative side-file paths
// against the fleet.yaml directory (same rule as the admin form).
func (s *Server) profileFromBody(w http.ResponseWriter, r *http.Request, b machineConfigBody) (fleet.Profile, bool) {
	if !fleet.ValidBootTargets[b.Boot] {
		writeAPIError(w, http.StatusBadRequest, ErrCodeBootInvalid,
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
		IPXEScript: resolve(b.IPXEScript),
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
		writeAPIError(w, http.StatusBadRequest, ErrCodeMACInvalid,
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
		writeAPIError(w, http.StatusBadRequest, ErrCodeMACInvalid,
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
		writeAPIError(w, http.StatusBadRequest, ErrCodeMACInvalid,
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
		writeAPIError(w, http.StatusConflict, ErrCodeMACExists,
			"machine "+mac+" already exists (use PUT to update)", map[string]any{"mac": mac})
	case errors.Is(err, fleet.ErrMACAbsent):
		writeAPIError(w, http.StatusNotFound, ErrCodeMACNotInFleet,
			"machine "+mac+" is not in fleet.yaml", map[string]any{"mac": mac})
	case errors.Is(err, fleet.ErrPreconditionRequired):
		writeAPIError(w, http.StatusPreconditionRequired, ErrCodePreconditionRequired,
			"If-Match header required (GET the resource first for its ETag)", map[string]any{"mac": mac})
	case errors.Is(err, fleet.ErrPreconditionFailed):
		writeAPIError(w, http.StatusPreconditionFailed, ErrCodePreconditionFailed,
			"If-Match does not match current ETag (resource changed; re-GET)", map[string]any{"mac": mac})
	default:
		// Validation, bad MAC, or Save failure.
		s.log.Errorf("%s %s failed for %s: %v", op, r.URL.Path, mac, err)
		writeAPIError(w, http.StatusInternalServerError, ErrCodeSaveFailed,
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

	out := make([]machineAPIView, 0, len(page))
	for _, m := range page {
		out = append(out, s.buildMachineView(m.MAC, m.Profile))
	}
	writeJSON(w, map[string]any{
		"pending_ttl_s": pendingTTLSeconds(s.opts.Pending),
		"total":         total,
		"limit":         limit,
		"offset":        offset,
		"machines":      out,
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
			writeAPIError(w, http.StatusBadRequest, ErrCodePagingInvalid,
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
			writeAPIError(w, http.StatusBadRequest, ErrCodePagingInvalid,
				"offset must be a non-negative integer", map[string]any{"got": v})
			return 0, 0, false
		}
		offset = n
	}
	return limit, offset, true
}

func (s *Server) buildDesired(canon string) desiredView {
	v := desiredView{}
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

func (s *Server) buildObserved(canon string) observedView {
	v := observedView{}
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

func (s *Server) buildIntentView(canon string) intentView {
	return intentView{
		MAC:      canon,
		Desired:  s.buildDesired(canon),
		Observed: s.buildObserved(canon),
	}
}

func (s *Server) buildMachineView(canon string, p fleet.Profile) machineAPIView {
	return machineAPIView{
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
// Clients should branch on `code` (one of the ErrCode* constants),
// not on `msg` — message text is allowed to change between releases
// for clarity, codes are not.
func writeAPIError(w http.ResponseWriter, status int, code, msg string, details map[string]any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(apiErrorView{
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
func (s *Server) writeError(w http.ResponseWriter, r *http.Request, status int, code, msg string, details map[string]any) {
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
	writeJSON(w, map[string]any{
		"status":     "ok",
		"uptime_s":   int(time.Since(s.startedAt).Seconds()),
		"started_at": s.startedAt.UTC().Format(time.RFC3339),
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

	body := map[string]any{
		"components": components,
	}
	if ready {
		body["status"] = "ok"
		// Counts are only meaningful when components are loaded.
		body["pending_count"] = pendingCount(s.opts.Pending)
		body["tracker_count"] = trackerCount(s.opts.FleetStatus)
	} else {
		body["status"] = "not_ready"
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
