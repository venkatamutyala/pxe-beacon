package httpd

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/venkatamutyala/pxe-beacon/internal/fleet"
	"github.com/venkatamutyala/pxe-beacon/internal/pending"
)

// machineAPIView is the JSON shape returned by every /api/v1/machines/*
// endpoint. Combines fleet config + live tracker state + pending action.
//
// v0.7.1 vocabulary (was {armed, armed_at} in v0.7.0):
//   - pending_action: "deploy" | "rescue" | "" (none)
//   - requested_at:   when the action was queued (omitted when none)
//   - expires_at:     when the queued action will auto-cancel (omitted when none / no expiry)
//   - state:          existing install-progress event from the Tracker
type machineAPIView struct {
	MAC           string    `json:"mac"`
	Name          string    `json:"name,omitempty"`
	Boot          string    `json:"boot,omitempty"`
	PendingAction string    `json:"pending_action,omitempty"`
	RequestedAt   time.Time `json:"requested_at,omitempty"`
	ExpiresAt     time.Time `json:"expires_at,omitempty"`
	State         string    `json:"state,omitempty"`
}

type apiErrorView struct {
	Error string `json:"error"`
}

// apiReady asserts fleet + Pending are wired. Writes a JSON error and
// returns false otherwise.
func (s *Server) apiReady(w http.ResponseWriter) bool {
	if s.opts.Fleet == nil || s.opts.FleetStatus == nil {
		writeAPIError(w, http.StatusNotFound,
			"fleet mode not enabled (start pxe-beacon with -config <fleet.yaml>)")
		return false
	}
	if s.opts.Pending == nil {
		writeAPIError(w, http.StatusNotFound,
			"pending-action store not configured")
		return false
	}
	return true
}

// apiExtractFleetMAC canonicalizes the {mac} path value AND verifies
// the MAC is a known fleet member. Returns canonical MAC + Profile on
// success; writes JSON error + ""+zero on failure.
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

// handleAPIDeploy queues a deploy action.
func (s *Server) handleAPIDeploy(w http.ResponseWriter, r *http.Request) {
	if !s.apiReady(w) {
		return
	}
	mac, p := s.apiExtractFleetMAC(w, r)
	if mac == "" {
		return
	}
	if _, err := s.opts.Pending.Deploy(mac); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "deploy: "+err.Error())
		return
	}
	s.log.Infof("POST %s -> 200, deploy queued for %s (%s)", r.URL.Path, p.Name, mac)
	writeAPIView(w, s.buildView(mac, p))
}

// handleAPIRescue is a v0.7.1 stub. Returns 501 until the rescue
// boot target is wired into the dispatch script.
func (s *Server) handleAPIRescue(w http.ResponseWriter, r *http.Request) {
	if !s.apiReady(w) {
		return
	}
	if _, p := s.apiExtractFleetMAC(w, r); p.Name == "" {
		return
	}
	writeAPIError(w, http.StatusNotImplemented,
		"rescue boot target not yet implemented; tracked in TODO")
}

// handleAPICancel clears any pending action.
func (s *Server) handleAPICancel(w http.ResponseWriter, r *http.Request) {
	if !s.apiReady(w) {
		return
	}
	mac, p := s.apiExtractFleetMAC(w, r)
	if mac == "" {
		return
	}
	s.opts.Pending.Cancel(mac)
	s.log.Infof("POST %s -> 200, cancelled pending action for %s (%s)", r.URL.Path, p.Name, mac)
	writeAPIView(w, s.buildView(mac, p))
}

func (s *Server) handleAPIMachine(w http.ResponseWriter, r *http.Request) {
	if !s.apiReady(w) {
		return
	}
	mac, p := s.apiExtractFleetMAC(w, r)
	if mac == "" {
		return
	}
	writeAPIView(w, s.buildView(mac, p))
}

func (s *Server) handleAPIList(w http.ResponseWriter, r *http.Request) {
	if !s.apiReady(w) {
		return
	}
	machines := s.opts.Fleet.ListMachines()
	out := make([]machineAPIView, 0, len(machines))
	for _, m := range machines {
		out = append(out, s.buildView(m.MAC, m.Profile))
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(map[string]any{
		"pending_ttl_s": pendingTTLSeconds(s.opts.Pending),
		"machines":      out,
	}); err != nil {
		s.log.Warnf("GET %s: encode error: %v", r.URL.Path, err)
	}
}

func (s *Server) buildView(canon string, p fleet.Profile) machineAPIView {
	view := machineAPIView{
		MAC:  canon,
		Name: p.Name,
		Boot: p.Boot,
	}
	if s.opts.Pending != nil {
		action, at, exp, ok := s.opts.Pending.Status(canon)
		if ok {
			view.PendingAction = string(action)
			view.RequestedAt = at
			view.ExpiresAt = exp
		}
	}
	if s.opts.FleetStatus != nil {
		for _, st := range s.opts.FleetStatus.Snapshot() {
			if st.MAC == canon {
				view.State = string(st.State)
				break
			}
		}
	}
	return view
}

func writeAPIView(w http.ResponseWriter, v machineAPIView) {
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

// pendingTTLSeconds returns the store's TTL in seconds; 0 if nil or
// no expiry configured.
func pendingTTLSeconds(s *pending.Store) int {
	if s == nil {
		return 0
	}
	return int(s.TTL() / time.Second)
}
