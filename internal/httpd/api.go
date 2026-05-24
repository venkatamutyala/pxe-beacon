package httpd

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
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

type apiErrorView struct {
	Error string `json:"error"`
}

// apiReady asserts fleet + Pending are wired.
func (s *Server) apiReady(w http.ResponseWriter) bool {
	if s.opts.Fleet == nil || s.opts.FleetStatus == nil {
		writeAPIError(w, http.StatusNotFound,
			"fleet mode not enabled (start pxe-beacon with -config <fleet.yaml>)")
		return false
	}
	if s.opts.Pending == nil {
		writeAPIError(w, http.StatusNotFound, "pending-action store not configured")
		return false
	}
	return true
}

func (s *Server) apiExtractFleetMAC(w http.ResponseWriter, r *http.Request) (string, fleet.Profile) {
	raw := r.PathValue("mac")
	if raw == "" {
		writeAPIError(w, http.StatusBadRequest, "missing mac in URL")
		return "", fleet.Profile{}
	}
	canon, err := fleet.CanonicalMAC(raw)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid mac: "+err.Error())
		return "", fleet.Profile{}
	}
	p := s.opts.Fleet.Lookup(canon)
	if p.Name == "" {
		writeAPIError(w, http.StatusNotFound, "mac "+canon+" is not in fleet.yaml")
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
		writeAPIError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	var in intentPUTBody
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeAPIError(w, http.StatusBadRequest, "decode body: "+err.Error())
		return
	}
	// Require explicit "action" key. A naked {} is ambiguous.
	if len(in.Action) == 0 {
		writeAPIError(w, http.StatusBadRequest,
			`body must include "action" key with value "install", "rescue", or null`)
		return
	}
	// Decode the action value. JSON null → empty string (= cancel).
	// A string value → that string. Anything else → 400.
	var action string
	if string(in.Action) != "null" {
		if err := json.Unmarshal(in.Action, &action); err != nil {
			writeAPIError(w, http.StatusBadRequest,
				`"action" must be a string or null; got `+string(in.Action))
			return
		}
	}
	switch action {
	case "install":
		if _, err := s.opts.Pending.Install(mac); err != nil {
			writeAPIError(w, http.StatusInternalServerError, "install: "+err.Error())
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
		writeAPIError(w, http.StatusNotImplemented,
			"rescue boot target not yet wired (tracked for v0.8.2); intent NOT queued")
		return
	case "":
		s.opts.Pending.Cancel(mac)
		s.logIntent(r, mac, p.Name, "cancel", 200)
	default:
		writeAPIError(w, http.StatusBadRequest,
			`action must be "install", "rescue", or null; got `+action)
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
func (s *Server) handleAPIMachine(w http.ResponseWriter, r *http.Request) {
	if !s.apiReady(w) {
		return
	}
	mac, p := s.apiExtractFleetMAC(w, r)
	if mac == "" {
		return
	}
	writeJSON(w, s.buildMachineView(mac, p))
}

// handleAPIList — GET /api/v1/machines
func (s *Server) handleAPIList(w http.ResponseWriter, r *http.Request) {
	if !s.apiReady(w) {
		return
	}
	machines := s.opts.Fleet.ListMachines()
	out := make([]machineAPIView, 0, len(machines))
	for _, m := range machines {
		out = append(out, s.buildMachineView(m.MAC, m.Profile))
	}
	writeJSON(w, map[string]any{
		"pending_ttl_s": pendingTTLSeconds(s.opts.Pending),
		"machines":      out,
	})
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

func writeAPIError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(apiErrorView{Error: msg})
}

func pendingTTLSeconds(s *pending.Store) int {
	if s == nil {
		return 0
	}
	return int(s.TTL() / time.Second)
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

