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

// doLoopback issues a request on the loopback interface so loopbackOnly
// admits it. body may be nil.
func doLoopback(srv *Server, method, target, body string) *httptest.ResponseRecorder {
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, target, nil)
	} else {
		req = httptest.NewRequest(method, target, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	req.RemoteAddr = "127.0.0.1:54321"
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	return w
}

// decode parses w.Body into v, failing the test on error.
func decode(t *testing.T, w *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(w.Body.Bytes(), v); err != nil {
		t.Fatalf("decode body: %v body=%s", err, w.Body.String())
	}
}

func TestAPI_SetIntent_Install(t *testing.T) {
	srv, pSt, _ := newAPIServer(t)
	mac := "58:47:ca:70:c7:c9"

	w := doLoopback(srv, "PUT", "/api/v1/machines/"+mac+"/intent", `{"action":"install"}`)
	if w.Code != 200 {
		t.Fatalf("PUT install: status %d body=%s", w.Code, w.Body.String())
	}
	var view intentView
	decode(t, w, &view)
	if view.Desired.Action != "install" {
		t.Fatalf("want desired.action=install, got %+v", view)
	}
	if view.MAC != mac {
		t.Errorf("want mac=%s, got %s", mac, view.MAC)
	}
	if !pSt.IsPending(mac) {
		t.Fatal("Store should be pending")
	}
}

func TestAPI_SetIntent_Rescue_Returns501(t *testing.T) {
	// v0.8.1: rescue boot target not yet wired (v0.8.2). API must
	// 501 and NOT touch the pending store.
	srv, pSt, _ := newAPIServer(t)
	mac := "58:47:ca:70:c7:c9"

	w := doLoopback(srv, "PUT", "/api/v1/machines/"+mac+"/intent", `{"action":"rescue"}`)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("PUT rescue: want 501, got %d body=%s", w.Code, w.Body.String())
	}
	if pSt.IsPending(mac) {
		t.Fatal("rescue 501 must NOT queue an intent")
	}
}

func TestAPI_SetIntent_Null_Clears(t *testing.T) {
	srv, pSt, _ := newAPIServer(t)
	mac := "58:47:ca:70:c7:c9"
	if _, err := pSt.Install(mac); err != nil {
		t.Fatal(err)
	}
	w := doLoopback(srv, "PUT", "/api/v1/machines/"+mac+"/intent", `{"action":null}`)
	if w.Code != 200 {
		t.Fatalf("PUT null: status %d body=%s", w.Code, w.Body.String())
	}
	var view intentView
	decode(t, w, &view)
	if view.Desired.Action != "" {
		t.Fatalf("want desired.action='', got %+v", view)
	}
	if pSt.IsPending(mac) {
		t.Fatal("Store should be cleared")
	}
}

func TestAPI_SetIntent_Idempotent(t *testing.T) {
	srv, pSt, _ := newAPIServer(t)
	mac := "58:47:ca:70:c7:c9"
	body := `{"action":"install"}`

	// Same body twice — should converge to same state.
	w1 := doLoopback(srv, "PUT", "/api/v1/machines/"+mac+"/intent", body)
	if w1.Code != 200 {
		t.Fatalf("first PUT: status %d", w1.Code)
	}
	w2 := doLoopback(srv, "PUT", "/api/v1/machines/"+mac+"/intent", body)
	if w2.Code != 200 {
		t.Fatalf("second PUT: status %d", w2.Code)
	}
	if !pSt.IsPending(mac) {
		t.Fatal("Store should be pending after idempotent calls")
	}
}

func TestAPI_SetIntent_UnknownMAC_404(t *testing.T) {
	srv, _, _ := newAPIServer(t)
	w := doLoopback(srv, "PUT", "/api/v1/machines/aa:bb:cc:dd:ee:ff/intent", `{"action":"install"}`)
	if w.Code != 404 {
		t.Fatalf("unknown MAC: want 404, got %d body=%s", w.Code, w.Body.String())
	}
	var errView apiErrorView
	decode(t, w, &errView)
	// v0.9.0: assert structured envelope — `code` is the machine-readable
	// field clients should branch on, `message` is prose.
	if errView.Code != ErrCodeMACNotInFleet {
		t.Errorf("code = %q, want %q", errView.Code, ErrCodeMACNotInFleet)
	}
	if !strings.Contains(errView.Message, "not in fleet") {
		t.Errorf("message missing 'not in fleet': %q", errView.Message)
	}
	if errView.Details["mac"] == nil {
		t.Errorf("details.mac missing: %+v", errView.Details)
	}
}

// TestAPI_ErrorCodes locks the v0.9.0 contract: every error response
// carries a stable machine-readable `code` field. New codes are
// additive; existing codes don't change meaning across releases.
func TestAPI_ErrorCodes(t *testing.T) {
	srv, _, _ := newAPIServer(t)
	mac := "58:47:ca:70:c7:c9"

	cases := []struct {
		name       string
		method     string
		path       string
		body       string
		wantStatus int
		wantCode   string
	}{
		{"mac_invalid", "PUT", "/api/v1/machines/not-a-mac/intent", `{"action":"install"}`, 400, ErrCodeMACInvalid},
		{"mac_not_in_fleet", "PUT", "/api/v1/machines/11:22:33:44:55:66/intent", `{"action":"install"}`, 404, ErrCodeMACNotInFleet},
		{"action_missing", "PUT", "/api/v1/machines/" + mac + "/intent", `{}`, 400, ErrCodeActionMissing},
		{"action_invalid", "PUT", "/api/v1/machines/" + mac + "/intent", `{"action":"frobnicate"}`, 400, ErrCodeActionInvalid},
		{"action_wrong_type", "PUT", "/api/v1/machines/" + mac + "/intent", `{"action":42}`, 400, ErrCodeActionInvalid},
		{"body_invalid", "PUT", "/api/v1/machines/" + mac + "/intent", `not json`, 400, ErrCodeBodyInvalid},
		{"rescue_unimplemented", "PUT", "/api/v1/machines/" + mac + "/intent", `{"action":"rescue"}`, 501, ErrCodeRescueUnimplemented},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := doLoopback(srv, c.method, c.path, c.body)
			if w.Code != c.wantStatus {
				t.Fatalf("status = %d, want %d. body=%s", w.Code, c.wantStatus, w.Body.String())
			}
			var ev apiErrorView
			decode(t, w, &ev)
			if ev.Code != c.wantCode {
				t.Errorf("code = %q, want %q. body=%s", ev.Code, c.wantCode, w.Body.String())
			}
			if ev.Message == "" {
				t.Errorf("message must be non-empty. body=%s", w.Body.String())
			}
		})
	}
}

// TestAPI_FleetNotLoaded_Returns503 — v0.9.0 fixes the wrong-status
// bug: "fleet mode not enabled" used to return 404 (which suggested
// the URL was wrong); correct semantic is 503 Service Unavailable
// (URL is right, the feature isn't configured).
func TestAPI_FleetNotLoaded_Returns503(t *testing.T) {
	// Build a server with NO fleet wired.
	srv, err := New(Options{
		Listen:       "127.0.0.1:0",
		AdvertisedIP: "127.0.0.1",
		HTTPPort:     8080,
		Logger:       narrlog.New("test", narrlog.LevelDebug, nil),
		// Fleet, FleetStatus, Pending all nil.
	})
	if err != nil {
		t.Fatal(err)
	}
	w := doLoopback(srv, "PUT", "/api/v1/machines/58:47:ca:70:c7:c9/intent", `{"action":"install"}`)
	if w.Code != 503 {
		t.Fatalf("fleet-not-loaded: want 503, got %d body=%s", w.Code, w.Body.String())
	}
	var ev apiErrorView
	decode(t, w, &ev)
	if ev.Code != ErrCodeFleetNotLoaded {
		t.Errorf("code = %q, want %q", ev.Code, ErrCodeFleetNotLoaded)
	}
}

func TestAPI_SetIntent_InvalidAction_400(t *testing.T) {
	srv, _, _ := newAPIServer(t)
	mac := "58:47:ca:70:c7:c9"
	w := doLoopback(srv, "PUT", "/api/v1/machines/"+mac+"/intent", `{"action":"reformat-the-universe"}`)
	if w.Code != 400 {
		t.Fatalf("bad action: want 400, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestAPI_SetIntent_MissingAction_400(t *testing.T) {
	// Body without an "action" key. We require explicit intent even if null.
	srv, _, _ := newAPIServer(t)
	mac := "58:47:ca:70:c7:c9"
	w := doLoopback(srv, "PUT", "/api/v1/machines/"+mac+"/intent", `{}`)
	if w.Code != 400 {
		t.Fatalf("missing action: want 400, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestAPI_GetIntent(t *testing.T) {
	srv, pSt, _ := newAPIServer(t)
	mac := "58:47:ca:70:c7:c9"

	// idle
	w := doLoopback(srv, "GET", "/api/v1/machines/"+mac+"/intent", "")
	if w.Code != 200 {
		t.Fatalf("get idle: status %d", w.Code)
	}
	var view intentView
	decode(t, w, &view)
	if view.Desired.Action != "" {
		t.Errorf("idle GET should report empty desired.action, got %+v", view)
	}

	// after Install
	if _, err := pSt.Install(mac); err != nil {
		t.Fatal(err)
	}
	w = doLoopback(srv, "GET", "/api/v1/machines/"+mac+"/intent", "")
	view = intentView{}
	decode(t, w, &view)
	if view.Desired.Action != "install" {
		t.Errorf("install GET should report action=install, got %+v", view)
	}
}

func TestAPI_GetMachine_HasDesiredAndObserved(t *testing.T) {
	srv, pSt, _ := newAPIServer(t)
	mac := "58:47:ca:70:c7:c9"
	if _, err := pSt.Install(mac); err != nil {
		t.Fatal(err)
	}
	w := doLoopback(srv, "GET", "/api/v1/machines/"+mac, "")
	if w.Code != 200 {
		t.Fatalf("get machine: status %d", w.Code)
	}
	var view machineAPIView
	decode(t, w, &view)
	if view.Name != "venkat-1" || view.Boot != "debian-12" {
		t.Errorf("missing fleet fields: %+v", view)
	}
	if view.Desired.Action != "install" {
		t.Errorf("missing desired.action: %+v", view)
	}
}

func TestAPI_List(t *testing.T) {
	srv, pSt, _ := newAPIServer(t)
	_, _ = pSt.Install("58:47:ca:70:c7:c9")

	w := doLoopback(srv, "GET", "/api/v1/machines", "")
	if w.Code != 200 {
		t.Fatalf("list: status %d", w.Code)
	}
	var resp struct {
		PendingTTLs int              `json:"pending_ttl_s"`
		Machines    []machineAPIView `json:"machines"`
	}
	decode(t, w, &resp)
	if len(resp.Machines) != 1 {
		t.Fatalf("want 1 machine, got %d", len(resp.Machines))
	}
	if resp.Machines[0].Desired.Action != "install" {
		t.Errorf("expected desired.action=install in list, got %+v", resp.Machines[0])
	}
	if resp.PendingTTLs != int((15 * time.Minute).Seconds()) {
		t.Errorf("pending_ttl_s wrong: got %d", resp.PendingTTLs)
	}
}

func TestAPI_NonLoopback_403(t *testing.T) {
	srv, _, _ := newAPIServer(t)
	req := httptest.NewRequest("PUT", "/api/v1/machines/58:47:ca:70:c7:c9/intent",
		strings.NewReader(`{"action":"install"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "10.69.7.55:54321"
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-loopback: want 403, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestAPI_Healthz(t *testing.T) {
	srv, _, _ := newAPIServer(t)
	w := doLoopback(srv, "GET", "/healthz", "")
	if w.Code != 200 {
		t.Fatalf("healthz: status %d", w.Code)
	}
	var body map[string]any
	decode(t, w, &body)
	if body["status"] != "ok" {
		t.Errorf("healthz status = %v, want ok", body["status"])
	}
}

func TestAPI_Readyz_Ready(t *testing.T) {
	srv, _, _ := newAPIServer(t) // fully wired
	w := doLoopback(srv, "GET", "/readyz", "")
	if w.Code != 200 {
		t.Fatalf("readyz (wired): status %d body=%s", w.Code, w.Body.String())
	}
	var body map[string]any
	decode(t, w, &body)
	if body["status"] != "ok" {
		t.Errorf("readyz status = %v, want ok", body["status"])
	}
	comps, _ := body["components"].(map[string]any)
	if comps["fleet"] != "ok" || comps["tracker"] != "ok" || comps["pending"] != "ok" {
		t.Errorf("components not all ok: %+v", comps)
	}
}

func TestAPI_Readyz_NotReady_503(t *testing.T) {
	srv, err := New(Options{
		Listen:       "127.0.0.1:0",
		AdvertisedIP: "127.0.0.1",
		HTTPPort:     8080,
		Logger:       narrlog.New("test", narrlog.LevelDebug, nil),
		// no Fleet/FleetStatus/Pending
	})
	if err != nil {
		t.Fatal(err)
	}
	w := doLoopback(srv, "GET", "/readyz", "")
	if w.Code != 503 {
		t.Fatalf("readyz (unwired): want 503, got %d", w.Code)
	}
	var body map[string]any
	decode(t, w, &body)
	if body["status"] != "not_ready" {
		t.Errorf("readyz status = %v, want not_ready", body["status"])
	}
}

func TestStatusJSON_UnifiedShape_AndDeprecated(t *testing.T) {
	// v0.9.0: /status.json now returns the SAME nested {desired,
	// observed} shape as /api/v1/machines, and carries a Deprecation
	// header pointing at the successor.
	srv, pSt, _ := newAPIServer(t)
	_, _ = pSt.Install("58:47:ca:70:c7:c9")

	w := doLoopback(srv, "GET", "/status.json", "")
	if w.Code != 200 {
		t.Fatalf("status.json: %d", w.Code)
	}
	if w.Header().Get("Deprecation") != "true" {
		t.Error("status.json must carry Deprecation: true")
	}
	if !strings.Contains(w.Header().Get("Link"), "/api/v1/machines") {
		t.Errorf("status.json Link should point at successor, got %q", w.Header().Get("Link"))
	}
	var body struct {
		Machines []machineAPIView `json:"machines"`
	}
	decode(t, w, &body)
	if len(body.Machines) != 1 {
		t.Fatalf("want 1 machine, got %d", len(body.Machines))
	}
	// The nested desired.action must be present — proves shape unity.
	if body.Machines[0].Desired.Action != "install" {
		t.Errorf("status.json should use nested desired.action, got %+v", body.Machines[0])
	}
}

func TestAPI_InstallerDone_CancelsPendingInstall(t *testing.T) {
	srv, pSt, _ := newAPIServer(t)
	mac := "58:47:ca:70:c7:c9"
	if _, err := pSt.Install(mac); err != nil {
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
		t.Fatal("phone_home should have cancelled the pending install")
	}
}

func TestAPI_InstallerDone_PreservesPendingRescue(t *testing.T) {
	// v0.8.1: a stale cloud-init phone_home from a previous install
	// must NOT cancel a freshly-queued rescue session.
	srv, pSt, _ := newAPIServer(t)
	mac := "58:47:ca:70:c7:c9"
	// Bypass the API's rescue 501 by going directly to the Store —
	// this models the v0.8.2 state where rescue is real, and
	// validates the v0.8.1 handler-side selective-cancel logic.
	if _, err := pSt.Rescue(mac); err != nil {
		t.Fatal(err)
	}
	if !pSt.IsPending(mac) {
		t.Fatal("precondition: rescue should be pending")
	}
	req := httptest.NewRequest("POST", "/autoinstall/58-47-ca-70-c7-c9/done", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("done: status %d body=%s", w.Code, w.Body.String())
	}
	if !pSt.IsPending(mac) {
		t.Fatal("phone_home from a stale install must NOT cancel a fresh rescue")
	}
}
