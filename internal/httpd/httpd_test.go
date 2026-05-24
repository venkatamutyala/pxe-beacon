package httpd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/venkatamutyala/pxe-beacon/internal/assets"
	"github.com/venkatamutyala/pxe-beacon/internal/fleet"
	"github.com/venkatamutyala/pxe-beacon/internal/narrlog"
)

func startTestServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	logBuf := &bytes.Buffer{}
	log := narrlog.New("test", narrlog.LevelDebug, logBuf)

	s, err := New(Options{
		Listen:       addr,
		AdvertisedIP: "10.0.0.5",
		ChainURL:     "https://boot.netboot.xyz/menu.ipxe",
		Logger:       log,
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = s.Serve(ctx); close(done) }()
	time.Sleep(80 * time.Millisecond)
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
		t.Logf("log dump:\n%s", logBuf.String())
	})
	return addr
}

func TestHTTP_ServesIPXEBinaryWithContentLength(t *testing.T) {
	addr := startTestServer(t)

	// HEAD first — PLAN gate uses curl -I.
	headReq, _ := http.NewRequest(http.MethodHead, "http://"+addr+"/netboot.xyz.efi", nil)
	resp, err := http.DefaultClient.Do(headReq)
	if err != nil {
		t.Fatalf("HEAD: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("HEAD status = %d, want 200", resp.StatusCode)
	}
	want, _ := assets.ReadIPXE(assets.IPXEEFIx64)
	gotCL := resp.Header.Get("Content-Length")
	if gotCL != strconv.Itoa(len(want)) {
		t.Errorf("HEAD Content-Length = %q, want %d", gotCL, len(want))
	}
	if got := resp.Header.Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("HEAD Content-Type = %q, want application/octet-stream", got)
	}
	_ = resp.Body.Close()

	// GET full body.
	resp2, err := http.Get("http://" + addr + "/netboot.xyz.efi")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d", resp2.StatusCode)
	}
	body, err := io.ReadAll(resp2.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, want) {
		t.Errorf("GET body diff: %d vs %d bytes", len(body), len(want))
	}
}

func TestHTTP_RendersBootScript(t *testing.T) {
	addr := startTestServer(t)
	resp, err := http.Get("http://" + addr + "/boot.ipxe")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	if !strings.Contains(s, "#!ipxe") {
		t.Errorf("missing #!ipxe shebang:\n%s", s)
	}
	if !strings.Contains(s, "10.0.0.5") {
		t.Errorf("AdvertisedIP not templated in:\n%s", s)
	}
	if !strings.Contains(s, "https://boot.netboot.xyz/menu.ipxe") {
		t.Errorf("ChainURL not templated in:\n%s", s)
	}
	if got := resp.Header.Get("Content-Length"); got != strconv.Itoa(len(body)) {
		t.Errorf("script Content-Length = %q, want %d", got, len(body))
	}
}

func TestHTTP_404UnknownPath(t *testing.T) {
	addr := startTestServer(t)
	resp, err := http.Get("http://" + addr + "/no-such-thing")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestHTTP_RootStatusPage(t *testing.T) {
	addr := startTestServer(t)
	resp, err := http.Get("http://" + addr + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "pxe-beacon") {
		t.Errorf("root page missing identifier:\n%s", body)
	}
}

func TestHTTP_CrossCertEmittedWhenEnabled(t *testing.T) {
	logBuf := &bytes.Buffer{}
	log := narrlog.New("test", narrlog.LevelInfo, logBuf)

	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	_ = ln.Close()

	s, err := New(Options{
		Listen:       addr,
		AdvertisedIP: "10.0.0.5",
		ChainURL:     "https://boot.netboot.xyz/menu.ipxe",
		SetCrossCert: true,
		Logger:       log,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Serve(ctx)
	time.Sleep(80 * time.Millisecond)

	resp, err := http.Get("http://" + addr + "/boot.ipxe")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "set crosscert") {
		t.Errorf("SetCrossCert=true did not emit crosscert directive:\n%s", body)
	}
}

// ----- v0.2 fleet-mode tests -----

func startFleetServer(t *testing.T) (addr string, f *fleet.Fleet, tr *fleet.Tracker, cleanup func()) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ubuntu.yaml"),
		[]byte("#cloud-config\nidentity:\n  username: ops\n  hostname: {{.Name}}\nphone_home:\n  url: http://{{.AdvertisedIP}}:{{.HTTPPort}}/autoinstall/{{.MACHyp}}/done\n  post: all\n"),
		0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fleet.yaml"), []byte(`
machines:
  - mac: 58:47:ca:70:c7:c9
    name: kube-1
    boot: ubuntu-22.04
    cloud_init: ./ubuntu.yaml
  - mac: aa:bb:cc:dd:ee:01
    name: rescue
    boot: menu
`), 0o644); err != nil {
		t.Fatal(err)
	}
	logBuf := &bytes.Buffer{}
	log := narrlog.New("test", narrlog.LevelDebug, logBuf)
	f, err := fleet.Load(filepath.Join(dir, "fleet.yaml"), log)
	if err != nil {
		t.Fatal(err)
	}
	tr = fleet.NewTracker(f, 5*time.Second)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr = ln.Addr().String()
	_, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)
	_ = ln.Close()

	s, err := New(Options{
		Listen:       addr,
		AdvertisedIP: "10.0.0.5",
		HTTPPort:     port,
		Logger:       log,
		Fleet:        f,
		FleetStatus:  tr,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = s.Serve(ctx); close(done) }()
	time.Sleep(80 * time.Millisecond)
	cleanup = func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
		t.Logf("log dump:\n%s", logBuf.String())
	}
	t.Cleanup(cleanup)
	return addr, f, tr, cleanup
}

func TestHTTP_Autoexec_PerTarget(t *testing.T) {
	addr, _, _, _ := startFleetServer(t)
	resp, err := http.Get("http://" + addr + "/autoinstall/58-47-ca-70-c7-c9/autoexec.ipxe")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	for _, want := range []string{"#!ipxe", "autoinstall", "kube-1", "10.0.0.5"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in ubuntu-22.04 autoexec:\n%s", want, s)
		}
	}
}

func TestHTTP_Autoexec_MenuTarget(t *testing.T) {
	addr, _, _, _ := startFleetServer(t)
	resp, err := http.Get("http://" + addr + "/autoinstall/aa-bb-cc-dd-ee-01/autoexec.ipxe")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "boot.netboot.xyz/menu.ipxe") {
		t.Errorf("menu autoexec missing chain URL:\n%s", body)
	}
}

func TestHTTP_UserData_RendersTemplate(t *testing.T) {
	addr, _, tr, _ := startFleetServer(t)
	resp, err := http.Get("http://" + addr + "/autoinstall/58-47-ca-70-c7-c9/user-data")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	if !strings.Contains(s, "hostname: kube-1") {
		t.Errorf("hostname template did not render: %s", s)
	}
	if !strings.Contains(s, "http://10.0.0.5:") {
		t.Errorf("AdvertisedIP not in templated phone_home: %s", s)
	}
	// Status tracker should record the user-data fetch.
	snap := tr.Snapshot()
	var found bool
	for _, m := range snap {
		if m.Name == "kube-1" && m.State == fleet.EventUserDataFetched {
			found = true
		}
	}
	if !found {
		t.Errorf("user-data fetch did not update status tracker: %+v", snap)
	}
}

func TestHTTP_MetaData(t *testing.T) {
	addr, _, _, _ := startFleetServer(t)
	resp, err := http.Get("http://" + addr + "/autoinstall/58-47-ca-70-c7-c9/meta-data")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	if !strings.Contains(s, "instance-id: kube-1") {
		t.Errorf("missing instance-id: %s", s)
	}
	if !strings.Contains(s, "local-hostname: kube-1") {
		t.Errorf("missing local-hostname: %s", s)
	}
}

func TestHTTP_InstallerDonePhoneHome(t *testing.T) {
	addr, _, tr, _ := startFleetServer(t)
	resp, err := http.Post("http://"+addr+"/autoinstall/58-47-ca-70-c7-c9/done", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	snap := tr.Snapshot()
	for _, m := range snap {
		if m.Name == "kube-1" {
			if m.State != fleet.EventInstallerDone {
				t.Errorf("kube-1 state = %q, want installer-done", m.State)
			}
		}
	}
}

func TestHTTP_StatusJSON(t *testing.T) {
	addr, _, tr, _ := startFleetServer(t)
	tr.Note("58:47:ca:70:c7:c9", fleet.EventFirmwareDHCP)

	resp, err := http.Get("http://" + addr + "/status.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	machines, ok := got["machines"].([]any)
	if !ok {
		t.Fatalf("machines is not a list: %#v", got["machines"])
	}
	if len(machines) != 2 {
		t.Errorf("machines count = %d, want 2", len(machines))
	}
}

func TestHTTP_StatusHTML(t *testing.T) {
	addr, _, _, _ := startFleetServer(t)
	resp, err := http.Get("http://" + addr + "/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Content-Type"); !strings.Contains(got, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", got)
	}
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	for _, want := range []string{"<html", "kube-1", "rescue", "ubuntu-22.04", "menu"} {
		if !strings.Contains(s, want) {
			t.Errorf("status HTML missing %q:\n(snippet)\n%s", want, s[:min(500, len(s))])
		}
	}
}

func TestHTTP_FleetRoutes_404WithoutConfig(t *testing.T) {
	// startTestServer doesn't pass Fleet → fleet routes should 404.
	addr := startTestServer(t)
	for _, p := range []string{
		"/autoinstall/58-47-ca-70-c7-c9/autoexec.ipxe",
		"/autoinstall/58-47-ca-70-c7-c9/user-data",
		"/autoinstall/58-47-ca-70-c7-c9/meta-data",
		"/status",
		"/status.json",
	} {
		resp, err := http.Get("http://" + addr + p)
		if err != nil {
			t.Errorf("%s: %v", p, err)
			continue
		}
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s status = %d, want 404 (no -config)", p, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

func TestHTTP_Autoexec_RejectsBadMAC(t *testing.T) {
	addr, _, _, _ := startFleetServer(t)
	resp, err := http.Get("http://" + addr + "/autoinstall/not-a-mac/autoexec.ipxe")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for invalid MAC", resp.StatusCode)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
