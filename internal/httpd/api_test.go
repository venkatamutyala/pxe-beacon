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
	"github.com/venkatamutyala/pxe-beacon/pkg/pxebeacon"
	"gopkg.in/yaml.v3"
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
	var view pxebeacon.Intent
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

func TestAPI_SetIntent_Rescue_Queues(t *testing.T) {
	// v0.11.0: rescue is wired (SystemRescue). API must 200 and queue
	// a rescue intent in the pending store.
	srv, pSt, _ := newAPIServer(t)
	mac := "58:47:ca:70:c7:c9"

	w := doLoopback(srv, "PUT", "/api/v1/machines/"+mac+"/intent", `{"action":"rescue"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT rescue: want 200, got %d body=%s", w.Code, w.Body.String())
	}
	var view pxebeacon.Intent
	decode(t, w, &view)
	if view.Desired.Action != "rescue" {
		t.Fatalf("want desired.action=rescue, got %+v", view)
	}
	action, _, _, ok := pSt.Status(mac)
	if !ok || action != pending.ActionRescue {
		t.Fatalf("Store should hold a rescue intent; got action=%q ok=%v", action, ok)
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
	var view pxebeacon.Intent
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
	var errView pxebeacon.APIError
	decode(t, w, &errView)
	// v0.9.0: assert structured envelope — `code` is the machine-readable
	// field clients should branch on, `message` is prose.
	if errView.Code != pxebeacon.ErrCodeMACNotInFleet {
		t.Errorf("code = %q, want %q", errView.Code, pxebeacon.ErrCodeMACNotInFleet)
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
		wantCode   pxebeacon.ErrCode
	}{
		{"mac_invalid", "PUT", "/api/v1/machines/not-a-mac/intent", `{"action":"install"}`, 400, pxebeacon.ErrCodeMACInvalid},
		{"mac_not_in_fleet", "PUT", "/api/v1/machines/11:22:33:44:55:66/intent", `{"action":"install"}`, 404, pxebeacon.ErrCodeMACNotInFleet},
		{"action_missing", "PUT", "/api/v1/machines/" + mac + "/intent", `{}`, 400, pxebeacon.ErrCodeActionMissing},
		{"action_invalid", "PUT", "/api/v1/machines/" + mac + "/intent", `{"action":"frobnicate"}`, 400, pxebeacon.ErrCodeActionInvalid},
		{"action_wrong_type", "PUT", "/api/v1/machines/" + mac + "/intent", `{"action":42}`, 400, pxebeacon.ErrCodeActionInvalid},
		{"body_invalid", "PUT", "/api/v1/machines/" + mac + "/intent", `not json`, 400, pxebeacon.ErrCodeBodyInvalid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := doLoopback(srv, c.method, c.path, c.body)
			if w.Code != c.wantStatus {
				t.Fatalf("status = %d, want %d. body=%s", w.Code, c.wantStatus, w.Body.String())
			}
			var ev pxebeacon.APIError
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
	var ev pxebeacon.APIError
	decode(t, w, &ev)
	if ev.Code != pxebeacon.ErrCodeFleetNotLoaded {
		t.Errorf("code = %q, want %q", ev.Code, pxebeacon.ErrCodeFleetNotLoaded)
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
	var view pxebeacon.Intent
	decode(t, w, &view)
	if view.Desired.Action != "" {
		t.Errorf("idle GET should report empty desired.action, got %+v", view)
	}

	// after Install
	if _, err := pSt.Install(mac); err != nil {
		t.Fatal(err)
	}
	w = doLoopback(srv, "GET", "/api/v1/machines/"+mac+"/intent", "")
	view = pxebeacon.Intent{}
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
	var view pxebeacon.Machine
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
		PendingTTLs int                 `json:"pending_ttl_s"`
		Total       int                 `json:"total"`
		Limit       int                 `json:"limit"`
		Offset      int                 `json:"offset"`
		Machines    []pxebeacon.Machine `json:"machines"`
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
	if resp.Total != 1 {
		t.Errorf("total = %d, want 1", resp.Total)
	}
	if resp.Limit != defaultPageLimit {
		t.Errorf("limit = %d, want default %d", resp.Limit, defaultPageLimit)
	}
}

func TestAPI_List_Pagination(t *testing.T) {
	// Build a server with several machines so paging is observable.
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "fleet.yaml")
	if err := os.WriteFile(yamlPath, []byte(`
machines:
  - {mac: "aa:bb:cc:dd:ee:01", name: m1, boot: debian-12}
  - {mac: "aa:bb:cc:dd:ee:02", name: m2, boot: debian-12}
  - {mac: "aa:bb:cc:dd:ee:03", name: m3, boot: debian-12}
  - {mac: "aa:bb:cc:dd:ee:04", name: m4, boot: debian-12}
  - {mac: "aa:bb:cc:dd:ee:05", name: m5, boot: debian-12}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	log := narrlog.New("test", narrlog.LevelDebug, nil)
	fl, err := fleet.Load(yamlPath, log)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(Options{
		Listen: "127.0.0.1:0", AdvertisedIP: "127.0.0.1", HTTPPort: 8080,
		Logger: log, Fleet: fl, FleetStatus: fleet.NewTracker(fl, time.Minute),
		Pending: pending.New(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}

	// limit=2 offset=2 → middle page of 2.
	w := doLoopback(srv, "GET", "/api/v1/machines?limit=2&offset=2", "")
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	var resp struct {
		Total, Limit, Offset int
		Machines             []pxebeacon.Machine
	}
	decode(t, w, &resp)
	if resp.Total != 5 {
		t.Errorf("total = %d, want 5", resp.Total)
	}
	if resp.Limit != 2 || resp.Offset != 2 {
		t.Errorf("limit/offset = %d/%d, want 2/2", resp.Limit, resp.Offset)
	}
	if len(resp.Machines) != 2 {
		t.Fatalf("page size = %d, want 2", len(resp.Machines))
	}

	// offset past the end → empty page, no error.
	w = doLoopback(srv, "GET", "/api/v1/machines?offset=99", "")
	decode(t, w, &struct{ Machines []pxebeacon.Machine }{})
	if w.Code != 200 {
		t.Errorf("offset-past-end: status %d, want 200", w.Code)
	}

	// bad limit → 400 paging_invalid.
	w = doLoopback(srv, "GET", "/api/v1/machines?limit=-3", "")
	if w.Code != 400 {
		t.Fatalf("bad limit: want 400, got %d", w.Code)
	}
	var ev pxebeacon.APIError
	decode(t, w, &ev)
	if ev.Code != pxebeacon.ErrCodePagingInvalid {
		t.Errorf("code = %q, want %q", ev.Code, pxebeacon.ErrCodePagingInvalid)
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

func TestOpenAPISpec_ParsesAndCoversSurface(t *testing.T) {
	// The embedded spec must be valid YAML with the right top-level
	// shape, and document exactly the /api/v1 paths we serve. The
	// second half is the "added an endpoint, forgot to document it"
	// (and vice versa) guard.
	var doc struct {
		OpenAPI string                   `yaml:"openapi"`
		Info    struct{ Version string } `yaml:"info"`
		Paths   map[string]any           `yaml:"paths"`
	}
	if err := yaml.Unmarshal(openAPISpec, &doc); err != nil {
		t.Fatalf("openapi.yaml is not valid YAML: %v", err)
	}
	if doc.OpenAPI == "" {
		t.Error("openapi version missing")
	}
	if doc.Info.Version == "" {
		t.Error("info.version missing")
	}

	documented := map[string]bool{}
	for p := range doc.Paths {
		if strings.HasPrefix(p, "/api/v1/") {
			documented[p] = true
		}
	}
	want := map[string]bool{
		"/api/v1/machines":              true,
		"/api/v1/machines/{mac}":        true,
		"/api/v1/machines/{mac}/intent": true,
		"/api/v1/machines/{mac}/events": true,
		"/api/v1/machines/{mac}/logs":   true,
		"/api/v1/discovered":            true,
		"/api/v1/discovered/{mac}":      true,
	}
	for p := range want {
		if !documented[p] {
			t.Errorf("spec missing documented path %q", p)
		}
	}
	for p := range documented {
		if !want[p] {
			t.Errorf("spec documents unexpected path %q (update test or spec)", p)
		}
	}
}

func TestOpenAPISpec_Served(t *testing.T) {
	srv, _, _ := newAPIServer(t)
	w := doLoopback(srv, "GET", "/openapi.yaml", "")
	if w.Code != 200 {
		t.Fatalf("/openapi.yaml: status %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "yaml") {
		t.Errorf("content-type = %q, want yaml", ct)
	}
	if !strings.Contains(w.Body.String(), "openapi:") {
		t.Error("served spec doesn't look like OpenAPI")
	}
}

func TestAPI_CRUD_FullCycle(t *testing.T) {
	srv, _, _ := newAPIServer(t)
	newMAC := "aa:bb:cc:dd:ee:09"

	// CREATE
	w := doLoopback(srv, "POST", "/api/v1/machines",
		`{"mac":"`+newMAC+`","name":"node9","boot":"debian-12"}`)
	if w.Code != 201 {
		t.Fatalf("create: status %d body=%s", w.Code, w.Body.String())
	}
	etag := w.Header().Get("ETag")
	if etag == "" {
		t.Fatal("create should set ETag header")
	}

	// CREATE again → 409 mac_exists
	w = doLoopback(srv, "POST", "/api/v1/machines",
		`{"mac":"`+newMAC+`","name":"dup","boot":"debian-12"}`)
	if w.Code != 409 {
		t.Fatalf("re-create: want 409, got %d", w.Code)
	}
	var ev pxebeacon.APIError
	decode(t, w, &ev)
	if ev.Code != pxebeacon.ErrCodeMACExists {
		t.Errorf("code = %q, want %q", ev.Code, pxebeacon.ErrCodeMACExists)
	}

	// UPDATE without If-Match → 428
	w = doLoopback(srv, "PUT", "/api/v1/machines/"+newMAC,
		`{"name":"node9b","boot":"debian-13"}`)
	if w.Code != 428 {
		t.Fatalf("update w/o If-Match: want 428, got %d", w.Code)
	}

	// UPDATE with stale If-Match → 412
	reqStale := httptest.NewRequest("PUT", "/api/v1/machines/"+newMAC,
		strings.NewReader(`{"name":"node9b","boot":"debian-13"}`))
	reqStale.Header.Set("Content-Type", "application/json")
	reqStale.Header.Set("If-Match", `W/"stale"`)
	reqStale.RemoteAddr = "127.0.0.1:5"
	wr := httptest.NewRecorder()
	srv.mux.ServeHTTP(wr, reqStale)
	if wr.Code != 412 {
		t.Fatalf("update stale If-Match: want 412, got %d", wr.Code)
	}

	// UPDATE with correct If-Match → 200, new ETag
	reqOK := httptest.NewRequest("PUT", "/api/v1/machines/"+newMAC,
		strings.NewReader(`{"name":"node9b","boot":"debian-13"}`))
	reqOK.Header.Set("Content-Type", "application/json")
	reqOK.Header.Set("If-Match", etag)
	reqOK.RemoteAddr = "127.0.0.1:5"
	wr = httptest.NewRecorder()
	srv.mux.ServeHTTP(wr, reqOK)
	if wr.Code != 200 {
		t.Fatalf("update correct If-Match: want 200, got %d body=%s", wr.Code, wr.Body.String())
	}

	// DELETE (idempotent) → 204, then 204 again
	w = doLoopback(srv, "DELETE", "/api/v1/machines/"+newMAC, "")
	if w.Code != 204 {
		t.Fatalf("delete: want 204, got %d", w.Code)
	}
	w = doLoopback(srv, "DELETE", "/api/v1/machines/"+newMAC, "")
	if w.Code != 204 {
		t.Fatalf("delete again (idempotent): want 204, got %d", w.Code)
	}
}

func TestAPI_CreateMachine_WithParams(t *testing.T) {
	srv, _, fl := newAPIServer(t)
	mac := "aa:bb:cc:dd:ee:0a"

	w := doLoopback(srv, "POST", "/api/v1/machines",
		`{"mac":"`+mac+`","name":"p1","boot":"debian-12","params":{"hostname":"p1","disk":"/dev/nvme0n1"}}`)
	if w.Code != 201 {
		t.Fatalf("create with params: status %d body=%s", w.Code, w.Body.String())
	}
	// The stored profile carries the params, and Lookup exposes them.
	p := fl.Lookup(mac)
	if p.Params["hostname"] != "p1" || p.Params["disk"] != "/dev/nvme0n1" {
		t.Errorf("params not stored: %+v", p.Params)
	}
}

func TestAPI_CreateMachine_RejectsNonJSON(t *testing.T) {
	srv, _, _ := newAPIServer(t)
	// Form-encoded body → 415 (the CSRF defense).
	req := httptest.NewRequest("POST", "/api/v1/machines",
		strings.NewReader("mac=aa:bb:cc:dd:ee:09&name=x&boot=debian-12"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "127.0.0.1:5"
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != 415 {
		t.Fatalf("non-JSON: want 415, got %d", w.Code)
	}
	var ev pxebeacon.APIError
	decode(t, w, &ev)
	if ev.Code != pxebeacon.ErrCodeContentType {
		t.Errorf("code = %q, want %q", ev.Code, pxebeacon.ErrCodeContentType)
	}
}

func TestAPI_Events_FailedKeepsPending(t *testing.T) {
	srv, pSt, _ := newAPIServer(t)
	mac := "58:47:ca:70:c7:c9"
	if _, err := pSt.Install(mac); err != nil {
		t.Fatal(err)
	}
	// installer-failed must NOT cancel the pending install (retry).
	w := doLoopback(srv, "POST", "/api/v1/machines/"+mac+"/events",
		`{"phase":"installer-failed","reason":"disk error"}`)
	if w.Code != 200 {
		t.Fatalf("events failed: status %d body=%s", w.Code, w.Body.String())
	}
	if !pSt.IsPending(mac) {
		t.Fatal("installer-failed must keep pending intent for retry")
	}
}

func TestAPI_Events_DoneCancelsInstall(t *testing.T) {
	srv, pSt, _ := newAPIServer(t)
	mac := "58:47:ca:70:c7:c9"
	if _, err := pSt.Install(mac); err != nil {
		t.Fatal(err)
	}
	w := doLoopback(srv, "POST", "/api/v1/machines/"+mac+"/events",
		`{"phase":"installer-done"}`)
	if w.Code != 200 {
		t.Fatalf("events done: status %d", w.Code)
	}
	if pSt.IsPending(mac) {
		t.Fatal("installer-done should cancel pending install")
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
		Machines []pxebeacon.Machine `json:"machines"`
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
