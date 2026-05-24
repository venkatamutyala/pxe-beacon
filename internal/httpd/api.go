package httpd

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/venkatamutyala/pxe-beacon/internal/armstate"
	"github.com/venkatamutyala/pxe-beacon/internal/fleet"
)

// machineAPIView is the JSON shape returned by every /api/v1/machines/*
// endpoint. Combines fleet config + live tracker state + arming.
type machineAPIView struct {
	MAC       string    `json:"mac"`
	Name      string    `json:"name,omitempty"`
	Boot      string    `json:"boot,omitempty"`
	Armed     bool      `json:"armed"`
	ArmedAt   time.Time `json:"armed_at,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	State     string    `json:"state,omitempty"`
}

type apiErrorView struct {
	Error string `json:"error"`
}

// apiReady asserts fleet + ArmState are wired. If not, writes a JSON
// error and returns false.
func (s *Server) apiReady(w http.ResponseWriter) bool {
	if s.opts.Fleet == nil || s.opts.FleetStatus == nil {
		writeAPIError(w, http.StatusNotFound,
			"fleet mode not enabled (start pxe-beacon with -config <fleet.yaml>)")
		return false
	}
	if s.opts.ArmState == nil {
		writeAPIError(w, http.StatusNotFound,
			"arming disabled (ArmState not configured)")
		return false
	}
	return true
}

// apiExtractFleetMAC pulls and canonicalizes the {mac} path value, AND
// verifies the MAC is a known fleet member. Returns canonical MAC +
// Profile on success; writes a 400/404 JSON error and returns ""+zero
// on failure.
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

func (s *Server) handleAPIArm(w http.ResponseWriter, r *http.Request) {
	if !s.apiReady(w) {
		return
	}
	mac, p := s.apiExtractFleetMAC(w, r)
	if mac == "" {
		return
	}
	if _, err := s.opts.ArmState.Arm(mac); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "arm: "+err.Error())
		return
	}
	s.log.Infof("POST %s -> 200, armed %s (%s)", r.URL.Path, p.Name, mac)
	writeAPIView(w, s.buildView(mac, p))
}

func (s *Server) handleAPIDisarm(w http.ResponseWriter, r *http.Request) {
	if !s.apiReady(w) {
		return
	}
	mac, p := s.apiExtractFleetMAC(w, r)
	if mac == "" {
		return
	}
	s.opts.ArmState.Disarm(mac)
	s.log.Infof("POST %s -> 200, disarmed %s (%s)", r.URL.Path, p.Name, mac)
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
		"arm_ttl_s": armTTLSeconds(s.opts.ArmState),
		"machines":  out,
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
	if s.opts.ArmState != nil {
		at, exp, armed := s.opts.ArmState.Status(canon)
		view.Armed = armed
		view.ArmedAt = at
		view.ExpiresAt = exp
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

// armTTLSeconds is a tiny helper for /status.json + GET /api/v1/machines.
// Returns 0 when the store is nil or has no expiry configured.
func armTTLSeconds(s *armstate.Store) int {
	if s == nil {
		return 0
	}
	return int(s.TTL() / time.Second)
}

