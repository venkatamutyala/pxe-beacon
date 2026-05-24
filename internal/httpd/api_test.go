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

	"github.com/venkatamutyala/pxe-beacon/internal/fleet"
	"github.com/venkatamutyala/pxe-beacon/internal/narrlog"
	"github.com/venkatamutyala/pxe-beacon/internal/pending"
)

func newAPIServer(t *testing.T) (*Server, *pending.Store, *fleet.Fleet) {
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
	pSt := pending.New(15 * time.Minute)

	srv, err := New(Options{
		Listen:       "127.0.0.1:0",
		AdvertisedIP: "127.0.0.1",
		HTTPPort:     8080,
		Logger:       log,
		Fleet:        fl,
		FleetStatus:  tracker,
		Pending:      pSt,
	})
	if err != nil {
		t.Fatal(err)
	}
	return srv, pSt, fl
}

func doLoopbackReq(srv *Server, method, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	req.RemoteAddr = "127.0.0.1:54321"
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	return w
}

func TestAPI_DeployCancelRoundTrip(t *testing.T) {
	srv, pSt, _ := newAPIServer(t)
	mac := "58:47:ca:70:c7:c9"

	if pSt.IsPending(mac) {
		t.Fatal("fresh state: should be idle")
	}

	// Deploy via API.
	w := doLoopbackReq(srv, "POST", "/api/v1/machines/"+mac+"/deploy")
	if w.Code != 200 {
		t.Fatalf("deploy: status %d, body=%s", w.Code, w.Body.String())
	}
	var view machineAPIView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode deploy body: %v body=%s", err, w.Body.String())
	}
	if view.PendingAction != "deploy" {
		t.Fatalf("deploy response should have pending_action='deploy', got %+v", view)
	}
	if view.Name != "venkat-1" || view.Boot != "debian-12" {
		t.Errorf("deploy response missing fleet fields: %+v", view)
	}
	if !pSt.IsPending(mac) {
		t.Fatal("Store should be pending after API call")
	}

	// Cancel via API. Use a fresh view because the omitempty tag on
	// pending_action means the cancel response omits the field, and
	// json.Unmarshal won't clear what's already there.
	w = doLoopbackReq(srv, "POST", "/api/v1/machines/"+mac+"/cancel")
	if w.Code != 200 {
		t.Fatalf("cancel: status %d, body=%s", w.Code, w.Body.String())
	}
	view = machineAPIView{}
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode cancel body: %v", err)
	}
	if view.PendingAction != "" {
		t.Fatalf("cancel response should have empty pending_action, got %+v", view)
	}
	if pSt.IsPending(mac) {
		t.Fatal("Store should be idle after Cancel API call")
	}
}

func TestAPI_Rescue_Returns501(t *testing.T) {
	srv, pSt, _ := newAPIServer(t)
	mac := "58:47:ca:70:c7:c9"

	w := doLoopbackReq(srv, "POST", "/api/v1/machines/"+mac+"/rescue")
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("rescue: want 501, got %d body=%s", w.Code, w.Body.String())
	}
	// Rescue stub must NOT queue an action.
	if pSt.IsPending(mac) {
		t.Fatal("rescue 501 stub should NOT have queued an action")
	}
}

func TestAPI_DeployUnknownMAC_404(t *testing.T) {
	srv, _, _ := newAPIServer(t)
	w := doLoopbackReq(srv, "POST", "/api/v1/machines/aa:bb:cc:dd:ee:ff/deploy")
	if w.Code != 404 {
		t.Fatalf("unknown MAC deploy: want 404, got %d body=%s", w.Code, w.Body.String())
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
	w := doLoopbackReq(srv, "POST", "/api/v1/machines/not-a-mac/deploy")
	if w.Code != 400 {
		t.Fatalf("bad MAC: want 400, got %d", w.Code)
	}
}

func TestAPI_GetMachine(t *testing.T) {
	srv, pSt, _ := newAPIServer(t)
	mac := "58:47:ca:70:c7:c9"

	w := doLoopbackReq(srv, "GET", "/api/v1/machines/"+mac)
	if w.Code != 200 {
		t.Fatalf("get: status %d", w.Code)
	}
	var view machineAPIView
	_ = json.Unmarshal(w.Body.Bytes(), &view)
	if view.PendingAction != "" {
		t.Errorf("idle GET should report empty pending_action, got %+v", view)
	}

	if _, err := pSt.Deploy(mac); err != nil {
		t.Fatal(err)
	}
	w = doLoopbackReq(srv, "GET", "/api/v1/machines/"+mac)
	view = machineAPIView{}
	_ = json.Unmarshal(w.Body.Bytes(), &view)
	if view.PendingAction != "deploy" {
		t.Errorf("deploy-pending GET should report pending_action='deploy', got %+v", view)
	}
}

func TestAPI_List(t *testing.T) {
	srv, pSt, _ := newAPIServer(t)
	_, _ = pSt.Deploy("58:47:ca:70:c7:c9")

	w := doLoopbackReq(srv, "GET", "/api/v1/machines")
	if w.Code != 200 {
		t.Fatalf("list: status %d", w.Code)
	}
	var resp struct {
		PendingTTLs int              `json:"pending_ttl_s"`
		Machines    []machineAPIView `json:"machines"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode list: %v body=%s", err, w.Body.String())
	}
	if len(resp.Machines) != 1 {
		t.Fatalf("want 1 machine, got %d", len(resp.Machines))
	}
	if resp.Machines[0].PendingAction != "deploy" {
		t.Errorf("expected pending_action='deploy' in list, got %+v", resp.Machines[0])
	}
	if resp.PendingTTLs != int((15 * time.Minute).Seconds()) {
		t.Errorf("pending_ttl_s wrong: got %d", resp.PendingTTLs)
	}
}

func TestAPI_NonLoopback_403(t *testing.T) {
	srv, _, _ := newAPIServer(t)
	req := httptest.NewRequest("POST", "/api/v1/machines/58:47:ca:70:c7:c9/deploy", nil)
	req.RemoteAddr = "10.69.7.55:54321"
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-loopback: want 403, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestAPI_InstallerDone_AutoCancels(t *testing.T) {
	srv, pSt, _ := newAPIServer(t)
	mac := "58:47:ca:70:c7:c9"
	if _, err := pSt.Deploy(mac); err != nil {
		t.Fatal(err)
	}
	if !pSt.IsPending(mac) {
		t.Fatal("precondition: should be pending")
	}
	req := httptest.NewRequest("POST", "/autoinstall/58-47-ca-70-c7-c9/done", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("done: status %d body=%s", w.Code, w.Body.String())
	}
	if pSt.IsPending(mac) {
		t.Fatal("phone_home should have cancelled the pending action")
	}
}
