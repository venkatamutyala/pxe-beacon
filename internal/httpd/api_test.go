package httpd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/venkatamutyala/pxe-beacon/internal/armstate"
	"github.com/venkatamutyala/pxe-beacon/internal/fleet"
	"github.com/venkatamutyala/pxe-beacon/internal/narrlog"
)

// newAPIServer builds a Server with one fleet entry and a fresh
// armstate.Store, returned alongside both so tests can inspect.
func newAPIServer(t *testing.T) (*Server, *armstate.Store, *fleet.Fleet) {
	t.Helper()

	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "fleet.yaml")
	if err := os.WriteFile(yamlPath, []byte(`
machines:
  - mac: 58:47:ca:70:c7:c9
    name: venkat-1
    boot: debian-12
`), 0o644); err != nil {
		t.Fatal(err)
	}
	log := narrlog.New("test", narrlog.LevelDebug, nil)
	fl, err := fleet.Load(yamlPath, log)
	if err != nil {
		t.Fatal(err)
	}
	tracker := fleet.NewTracker(fl, 5*time.Minute)
	armSt := armstate.New(15 * time.Minute)

	srv, err := New(Options{
		Listen:       "127.0.0.1:0",
		AdvertisedIP: "127.0.0.1",
		HTTPPort:     8080,
		Logger:       log,
		Fleet:        fl,
		FleetStatus:  tracker,
		ArmState:     armSt,
	})
	if err != nil {
		t.Fatal(err)
	}
	return srv, armSt, fl
}

// doLoopbackReq sets the RemoteAddr to loopback so loopbackOnly
// middleware admits the request.
func doLoopbackReq(srv *Server, method, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	req.RemoteAddr = "127.0.0.1:54321"
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	return w
}

func TestAPI_ArmDisarmRoundTrip(t *testing.T) {
	srv, armSt, _ := newAPIServer(t)
	mac := "58:47:ca:70:c7:c9"

	// Initially disarmed.
	if armSt.IsArmed(mac) {
		t.Fatal("fresh state: should be disarmed")
	}

	// Arm via API.
	w := doLoopbackReq(srv, "POST", "/api/v1/machines/"+mac+"/arm")
	if w.Code != 200 {
		t.Fatalf("arm: status %d, body=%s", w.Code, w.Body.String())
	}
	var view machineAPIView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode arm body: %v body=%s", err, w.Body.String())
	}
	if !view.Armed {
		t.Fatalf("arm response should have armed=true, got %+v", view)
	}
	if view.Name != "venkat-1" || view.Boot != "debian-12" {
		t.Errorf("arm response missing fleet fields: %+v", view)
	}
	if !armSt.IsArmed(mac) {
		t.Fatal("Store should be armed after API call")
	}

	// Disarm via API.
	w = doLoopbackReq(srv, "POST", "/api/v1/machines/"+mac+"/disarm")
	if w.Code != 200 {
		t.Fatalf("disarm: status %d, body=%s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode disarm body: %v", err)
	}
	if view.Armed {
		t.Fatalf("disarm response should have armed=false, got %+v", view)
	}
	if armSt.IsArmed(mac) {
		t.Fatal("Store should be disarmed after API call")
	}
}

func TestAPI_ArmUnknownMAC_404(t *testing.T) {
	srv, _, _ := newAPIServer(t)
	w := doLoopbackReq(srv, "POST", "/api/v1/machines/aa:bb:cc:dd:ee:ff/arm")
	if w.Code != 404 {
		t.Fatalf("unknown MAC arm: want 404, got %d body=%s", w.Code, w.Body.String())
	}
	var errView apiErrorView
	if err := json.Unmarshal(w.Body.Bytes(), &errView); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if !strings.Contains(errView.Error, "not in fleet") {
		t.Errorf("error msg missing 'not in fleet': %q", errView.Error)
	}
}

func TestAPI_BadMACFormat_400(t *testing.T) {
	srv, _, _ := newAPIServer(t)
	w := doLoopbackReq(srv, "POST", "/api/v1/machines/not-a-mac/arm")
	if w.Code != 400 {
		t.Fatalf("bad MAC: want 400, got %d", w.Code)
	}
}

func TestAPI_GetMachine(t *testing.T) {
	srv, armSt, _ := newAPIServer(t)
	mac := "58:47:ca:70:c7:c9"

	w := doLoopbackReq(srv, "GET", "/api/v1/machines/"+mac)
	if w.Code != 200 {
		t.Fatalf("get: status %d", w.Code)
	}
	var view machineAPIView
	_ = json.Unmarshal(w.Body.Bytes(), &view)
	if view.Armed {
		t.Errorf("disarmed GET should report armed=false, got %+v", view)
	}

	if _, err := armSt.Arm(mac); err != nil {
		t.Fatal(err)
	}
	w = doLoopbackReq(srv, "GET", "/api/v1/machines/"+mac)
	_ = json.Unmarshal(w.Body.Bytes(), &view)
	if !view.Armed {
		t.Errorf("armed GET should report armed=true, got %+v", view)
	}
}

func TestAPI_List(t *testing.T) {
	srv, armSt, _ := newAPIServer(t)
	_, _ = armSt.Arm("58:47:ca:70:c7:c9")

	w := doLoopbackReq(srv, "GET", "/api/v1/machines")
	if w.Code != 200 {
		t.Fatalf("list: status %d", w.Code)
	}
	var resp struct {
		ArmTTLs  int              `json:"arm_ttl_s"`
		Machines []machineAPIView `json:"machines"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode list: %v body=%s", err, w.Body.String())
	}
	if len(resp.Machines) != 1 {
		t.Fatalf("want 1 machine, got %d", len(resp.Machines))
	}
	if !resp.Machines[0].Armed {
		t.Errorf("expected armed=true in list, got %+v", resp.Machines[0])
	}
	if resp.ArmTTLs != int((15 * time.Minute).Seconds()) {
		t.Errorf("arm_ttl_s wrong: got %d", resp.ArmTTLs)
	}
}

func TestAPI_NonLoopback_403(t *testing.T) {
	srv, _, _ := newAPIServer(t)
	// Don't go through doLoopbackReq — set a non-loopback RemoteAddr.
	req := httptest.NewRequest("POST", "/api/v1/machines/58:47:ca:70:c7:c9/arm", nil)
	req.RemoteAddr = "10.69.7.55:54321"
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-loopback: want 403, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestAPI_InstallerDone_AutoDisarms(t *testing.T) {
	srv, armSt, _ := newAPIServer(t)
	mac := "58:47:ca:70:c7:c9"
	if _, err := armSt.Arm(mac); err != nil {
		t.Fatal(err)
	}
	if !armSt.IsArmed(mac) {
		t.Fatal("precondition: should be armed")
	}
	// cloud-init phone_home POSTs here.
	req := httptest.NewRequest("POST", "/autoinstall/58-47-ca-70-c7-c9/done", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("done: status %d body=%s", w.Code, w.Body.String())
	}
	if armSt.IsArmed(mac) {
		t.Fatal("phone_home should have disarmed the machine")
	}
}
