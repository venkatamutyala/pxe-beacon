package httpd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// startServerFromFleetYAML boots an httpd.Server backed by the given
// fleet.yaml content (written into a temp dir). Returns the listen addr.
func startServerFromFleetYAML(t *testing.T, yaml string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fleet.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	logBuf := &bytes.Buffer{}
	log := narrlog.New("test", narrlog.LevelDebug, logBuf)
	f, err := fleet.Load(filepath.Join(dir, "fleet.yaml"), log)
	if err != nil {
		t.Fatal(err)
	}
	tr := fleet.NewTracker(f, 5*time.Second)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	_, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)
	_ = ln.Close()
	s, err := New(Options{
		Listen: addr, AdvertisedIP: "10.0.0.5", HTTPPort: port,
		Logger: log, Fleet: f, FleetStatus: tr,
	})
	if err != nil {
		t.Fatal(err)
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

func TestHTTP_SysrescueConfig_RendersParams(t *testing.T) {
	// v0.11.0: the embedded default sysrescuecfg templates the root
	// password + (when a key is present) an autorun block pointing at
	// the setup script. Both come from params.
	addr := startServerFromFleetYAML(t, `
machines:
  - mac: 58:47:ca:70:c7:c9
    name: venkat-1
    boot: debian-12
    params:
      rescue_root_password: hunter2
      ssh_authorized_key: "ssh-ed25519 AAAAEXAMPLE op@host"
`)
	resp, err := http.Get("http://" + addr + "/autoinstall/58-47-ca-70-c7-c9/sysrescue.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	for _, want := range []string{
		`rootpass: "hunter2"`,
		"autorun:",
		"http://10.0.0.5:" + portOf(addr) + "/autoinstall/58-47-ca-70-c7-c9/sysrescue-setup.sh",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("sysrescue.yaml missing %q:\n%s", want, s)
		}
	}

	// The setup script must carry the SSH key.
	resp2, err := http.Get("http://" + addr + "/autoinstall/58-47-ca-70-c7-c9/sysrescue-setup.sh")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	setup, _ := io.ReadAll(resp2.Body)
	if !strings.Contains(string(setup), "ssh-ed25519 AAAAEXAMPLE op@host") {
		t.Errorf("setup script missing SSH key:\n%s", setup)
	}
	if !strings.Contains(string(setup), "authorized_keys") {
		t.Errorf("setup script missing authorized_keys write:\n%s", setup)
	}
}

func TestHTTP_SysrescueConfig_DefaultsWhenNoParams(t *testing.T) {
	// No params → default weak password, no autorun block.
	addr := startServerFromFleetYAML(t, `
machines:
  - mac: 58:47:ca:70:c7:c9
    name: venkat-1
    boot: debian-12
`)
	resp, err := http.Get("http://" + addr + "/autoinstall/58-47-ca-70-c7-c9/sysrescue.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	if !strings.Contains(s, `rootpass: "pxe"`) {
		t.Errorf("expected default rootpass=pxe:\n%s", s)
	}
	if strings.Contains(s, "autorun:") {
		t.Errorf("no SSH key → no autorun block expected:\n%s", s)
	}
}

func portOf(addr string) string {
	_, p, _ := net.SplitHostPort(addr)
	return p
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

func TestHTTP_FleetRoutes_503WithoutConfig(t *testing.T) {
	// startTestServer doesn't pass Fleet → fleet routes should 503.
	// v0.9.0: changed from 404 to 503 — "service up, feature not
	// configured" is the correct semantic; 404 wrongly implied the
	// URL was bad.
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
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("%s status = %d, want 503 (no -config)", p, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

func startFleetServerWithPreseed(t *testing.T) (string, *fleet.Tracker) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "user-data.yaml"),
		[]byte("#cloud-config\nhostname: {{.Name}}\nphone_home: {url: http://{{.AdvertisedIP}}:{{.HTTPPort}}/autoinstall/{{.MACHyp}}/done}\n"),
		0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "preseed.cfg"),
		[]byte(`# example preseed
d-i debian-installer/locale string en_US.UTF-8
d-i netcfg/get_hostname string {{.Name}}
d-i passwd/username string ops
`),
		0o644); err != nil {
		t.Fatal(err)
	}
	// Two machines: one preseed-only, one preseed+cloud_init bridged.
	if err := os.WriteFile(filepath.Join(dir, "fleet.yaml"), []byte(`
machines:
  - mac: 58:47:ca:70:c7:c9
    name: deb-bridge
    boot: debian-12
    preseed: ./preseed.cfg
    cloud_init: ./user-data.yaml
  - mac: aa:bb:cc:dd:ee:01
    name: deb-preseed-only
    boot: debian-12
    preseed: ./preseed.cfg
  - mac: 11:22:33:44:55:66
    name: deb-interactive
    boot: debian-12
`), 0o644); err != nil {
		t.Fatal(err)
	}
	logBuf := &bytes.Buffer{}
	log := narrlog.New("test", narrlog.LevelDebug, logBuf)
	f, err := fleet.Load(filepath.Join(dir, "fleet.yaml"), log)
	if err != nil {
		t.Fatal(err)
	}
	tr := fleet.NewTracker(f, 5*time.Second)

	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
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
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
		t.Logf("log dump:\n%s", logBuf.String())
	})
	return addr, tr
}

func TestHTTP_Preseed_RendersOperatorFile(t *testing.T) {
	addr, _ := startFleetServerWithPreseed(t)
	resp, err := http.Get("http://" + addr + "/autoinstall/aa-bb-cc-dd-ee-01/preseed.cfg")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	for _, want := range []string{
		"d-i debian-installer/locale",
		"d-i netcfg/get_hostname string deb-preseed-only", // templated name
		"d-i passwd/username string ops",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("preseed missing %q:\n%s", want, s)
		}
	}
	// No bridge for this machine — no cloud_init configured.
	if strings.Contains(s, "cloud-init bridge") {
		t.Errorf("preseed-only machine should NOT have the cloud-init bridge:\n%s", s)
	}
}

func TestHTTP_Preseed_AppendsCloudInitBridge(t *testing.T) {
	addr, _ := startFleetServerWithPreseed(t)
	resp, err := http.Get("http://" + addr + "/autoinstall/58-47-ca-70-c7-c9/preseed.cfg")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	s := string(body)

	// Operator's preseed content present.
	if !strings.Contains(s, "d-i debian-installer/locale") {
		t.Errorf("operator preseed not present:\n%s", s)
	}
	if !strings.Contains(s, "d-i netcfg/get_hostname string deb-bridge") {
		t.Errorf("operator preseed not templated with this machine's name:\n%s", s)
	}

	// Bridge appended.
	for _, want := range []string{
		"cloud-init bridge",
		"d-i preseed/late_command string",
		"apt-get install -y --no-install-recommends cloud-init",
		"/var/lib/cloud/seed/nocloud/user-data",
		"/var/lib/cloud/seed/nocloud/meta-data",
		"http://10.0.0.5:", // AdvertisedIP templated into the wget URL
		"58-47-ca-70-c7-c9/user-data",
		"systemctl enable cloud-init.service",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("bridge missing %q:\n%s", want, s)
		}
	}
}

func TestHTTP_Preseed_UsesEmbeddedDefaultWhenOmitted_v050(t *testing.T) {
	// v0.5.0: omitting `preseed:` on a debian-12/13 entry no longer
	// returns an interactive stub — pxe-beacon serves the embedded
	// default preseed (the user can override via /admin or by
	// dropping a file at <data-dir>/templates/defaults/debian-preseed.cfg).
	addr, _ := startFleetServerWithPreseed(t)
	resp, err := http.Get("http://" + addr + "/autoinstall/11-22-33-44-55-66/preseed.cfg")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	if !strings.Contains(s, "d-i debian-installer/locale") {
		t.Errorf("expected embedded default preseed directives:\n%s", s)
	}
	if !strings.Contains(s, "d-i passwd/username string pxe") {
		t.Errorf("expected the default pxe user:\n%s", s)
	}
	// Bridge appended automatically because no operator preseed was
	// supplied (signals "use embedded default + bridge").
	if !strings.Contains(s, "cloud-init bridge") {
		t.Errorf("bridge should be auto-appended when using embedded default preseed:\n%s", s)
	}
	if !strings.Contains(s, "datasource_list: [NoCloud, None]") {
		t.Errorf("bridge should pin cloud-init datasource (PXE expert fix #4):\n%s", s)
	}
}

// ----- v0.4 /assets/ route tests -----

func startFleetServerWithDataDir(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	dataDir := t.TempDir()
	// Seed an asset under data-dir/<target>/<file>.
	tdir := filepath.Join(dataDir, "ubuntu-22.04")
	if err := os.MkdirAll(tdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tdir, "vmlinuz"), []byte("FAKEVMLINUZ"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fleet.yaml"), []byte(`
defaults:
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
	tr := fleet.NewTracker(f, 5*time.Second)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
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
		DataDir:      dataDir,
	})
	if err != nil {
		t.Fatal(err)
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
	return addr, dataDir
}

func TestHTTP_Assets_ServesFromDataDir(t *testing.T) {
	addr, _ := startFleetServerWithDataDir(t)
	resp, err := http.Get("http://" + addr + "/assets/ubuntu-22.04/vmlinuz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "FAKEVMLINUZ" {
		t.Errorf("body = %q, want FAKEVMLINUZ", body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want application/octet-stream", ct)
	}
}

func TestHTTP_Assets_ServesNestedPath(t *testing.T) {
	// v0.11.0: the wildcard {file...} route must serve the nested
	// archiso layout SystemRescue's firmware constructs itself.
	addr, dataDir := startFleetServerWithDataDir(t)
	nested := filepath.Join(dataDir, "systemrescue", "sysresccd", "x86_64")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "airootfs.sfs"), []byte("FAKESQUASHFS"), 0o644); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get("http://" + addr + "/assets/systemrescue/sysresccd/x86_64/airootfs.sfs")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 for nested asset", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "FAKESQUASHFS" {
		t.Errorf("body = %q, want FAKESQUASHFS", body)
	}
}

func TestHTTP_Assets_404WhenFileMissing(t *testing.T) {
	addr, _ := startFleetServerWithDataDir(t)
	resp, err := http.Get("http://" + addr + "/assets/ubuntu-22.04/missing")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (file not yet fetched)", resp.StatusCode)
	}
}

func TestHTTP_Assets_RejectsTraversal(t *testing.T) {
	addr, _ := startFleetServerWithDataDir(t)
	// Built-in Go mux normalizes ../ at the URL level, so a direct
	// path-traversal URL becomes something else. Test the named-
	// segment path-traversal-via-name vector that the cache package
	// guards against.
	resp, err := http.Get("http://" + addr + "/assets/.dotfile/vmlinuz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for path-traversal target name", resp.StatusCode)
	}
}

func TestHTTP_Assets_404WithoutDataDir(t *testing.T) {
	// startFleetServer (the existing helper) doesn't set DataDir.
	addr, _, _, _ := startFleetServer(t)
	resp, err := http.Get("http://" + addr + "/assets/ubuntu-22.04/vmlinuz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when DataDir unset", resp.StatusCode)
	}
}

// ----- v0.5.0 admin UI tests -----

func startFleetServerWithAdminData(t *testing.T) (addr string, fleetPath string, dataDir string) {
	t.Helper()
	dir := t.TempDir()
	dataDir = t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fleet.yaml"), []byte(`
defaults:
  boot: menu
`), 0o644); err != nil {
		t.Fatal(err)
	}
	fleetPath = filepath.Join(dir, "fleet.yaml")
	logBuf := &bytes.Buffer{}
	log := narrlog.New("test", narrlog.LevelDebug, logBuf)
	f, err := fleet.Load(fleetPath, log)
	if err != nil {
		t.Fatal(err)
	}
	tr := fleet.NewTracker(f, 5*time.Second)

	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr = ln.Addr().String()
	_, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)
	_ = ln.Close()

	// Wire data-dir for the assets override path.
	assets.SetOverrideDir(dataDir)

	s, err := New(Options{
		Listen:       addr,
		AdvertisedIP: "10.0.0.5",
		HTTPPort:     port,
		Logger:       log,
		Fleet:        f,
		FleetStatus:  tr,
		DataDir:      dataDir,
	})
	if err != nil {
		t.Fatal(err)
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
		assets.SetOverrideDir("")
		t.Logf("log dump:\n%s", logBuf.String())
	})
	return addr, fleetPath, dataDir
}

func TestHTTP_Admin_IndexRendersHTML(t *testing.T) {
	addr, _, _ := startFleetServerWithAdminData(t)
	resp, err := http.Get("http://" + addr + "/admin")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	for _, want := range []string{
		"<html",
		"pxe-beacon — admin",
		"fleet machines",
		"add or update a machine",
		"templates",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("/admin missing %q", want)
		}
	}
}

// v0.9.0: fleet CRUD moved to POST /api/v1/machines (JSON). This test
// replaces the old form-encoded /admin/fleet WritesYAML test.
func TestHTTP_API_CreateMachine_WritesYAML(t *testing.T) {
	addr, fleetPath, _ := startFleetServerWithAdminData(t)

	body := `{"mac":"58:47:ca:70:c7:c9","name":"venkat-1","boot":"debian-12"}`
	resp, err := http.Post("http://"+addr+"/api/v1/machines", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 201. body=%s", resp.StatusCode, b)
	}
	if resp.Header.Get("ETag") == "" {
		t.Error("create response should carry an ETag")
	}

	raw, err := os.ReadFile(fleetPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"58:47:ca:70:c7:c9", "venkat-1", "debian-12"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("written fleet.yaml missing %q:\n%s", want, raw)
		}
	}
}

// v0.9.0: the fleet-mutation CSRF defense is Content-Type enforcement,
// not a token. A form-encoded POST (which a cross-origin browser CAN
// send without a preflight) must be rejected with 415. Replaces the
// old TestHTTP_Admin_RejectsCSRFMismatch.
func TestHTTP_API_CreateMachine_RejectsNonJSON(t *testing.T) {
	addr, _, _ := startFleetServerWithAdminData(t)
	form := url.Values{
		"mac":  {"58:47:ca:70:c7:c9"},
		"name": {"x"},
		"boot": {"debian-12"},
	}
	resp, err := http.PostForm("http://"+addr+"/api/v1/machines", form)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want 415 (non-JSON content-type)", resp.StatusCode)
	}
}

func TestHTTP_Admin_TemplateView_EmbeddedByDefault(t *testing.T) {
	addr, _, _ := startFleetServerWithAdminData(t)
	resp, err := http.Get("http://" + addr + "/admin/templates/defaults/debian-preseed.cfg")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	if !strings.Contains(s, "embedded default") {
		t.Errorf("template view should label as embedded:\n%s", s[:min2(500, len(s))])
	}
}

func TestHTTP_Admin_TemplateSave_WritesOverride(t *testing.T) {
	addr, _, dataDir := startFleetServerWithAdminData(t)
	csrf := fetchCSRF(t, "http://"+addr+"/admin")

	content := "# override\nd-i passwd/username string customuser\n"
	form := url.Values{
		"csrf":    {csrf},
		"content": {content},
	}
	// Disable redirect-following so we can assert the 303 response
	// from the POST itself (PostForm follows by default and we'd
	// otherwise see the eventual 200 from the edit page).
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, _ := http.NewRequest(http.MethodPost,
		"http://"+addr+"/admin/templates/defaults/debian-preseed.cfg",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}

	// Override should be on disk.
	override := filepath.Join(dataDir, "templates", "defaults", "debian-preseed.cfg")
	raw, err := os.ReadFile(override)
	if err != nil {
		t.Fatalf("override file: %v", err)
	}
	if string(raw) != content {
		t.Errorf("override content mismatch:\n got: %q\nwant: %q", raw, content)
	}

	// Next ReadDefault should return the override.
	resolved, _ := assets.ReadDefault("debian-preseed.cfg")
	if !strings.Contains(string(resolved), "customuser") {
		t.Errorf("ReadDefault did not pick up disk override:\n%s", resolved)
	}
}

func TestHTTP_Admin_LoopbackOnly_RejectsRemoteAddr(t *testing.T) {
	// We can't easily fake RemoteAddr from inside http.PostForm, so
	// directly test the middleware with a manually-constructed
	// request.
	mw := loopbackOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.RemoteAddr = "10.0.0.42:54321" // non-loopback
	mw.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("non-loopback got status %d, want 403", rec.Code)
	}
	// Confirm loopback IS allowed.
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/admin", nil)
	req2.RemoteAddr = "127.0.0.1:54321"
	mw.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Errorf("loopback got status %d, want 200", rec2.Code)
	}
}

// fetchCSRF scrapes the CSRF hidden input from the admin page so a
// subsequent POST passes the guard.
func fetchCSRF(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	// Look for: name="csrf" value="<token>"
	const marker = `name="csrf" value="`
	i := strings.Index(string(body), marker)
	if i < 0 {
		t.Fatalf("no csrf token in admin page: %s", body[:min2(500, len(body))])
	}
	rest := string(body)[i+len(marker):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		t.Fatalf("malformed csrf attribute")
	}
	return rest[:j]
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
