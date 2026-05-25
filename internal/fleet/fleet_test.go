package fleet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/venkatamutyala/pxe-beacon/internal/narrlog"
)

func writeFile(t *testing.T, dir, name, contents string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

func newLog() *narrlog.Logger {
	return narrlog.New("test", narrlog.LevelDebug, nil)
}

// ---- CanonicalMAC ----

func TestCanonicalMAC_Formats(t *testing.T) {
	want := "58:47:ca:70:c7:c9"
	for _, in := range []string{
		"58:47:ca:70:c7:c9",
		"58:47:CA:70:C7:C9",
		"58-47-ca-70-c7-c9",
		"58-47-CA-70-C7-C9",
		"5847.ca70.c7c9",
		"5847ca70c7c9",
		"5847CA70C7C9",
	} {
		got, err := CanonicalMAC(in)
		if err != nil {
			t.Errorf("CanonicalMAC(%q) error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("CanonicalMAC(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCanonicalMAC_Rejects(t *testing.T) {
	for _, in := range []string{
		"",
		"58:47:ca:70:c7",       // too short
		"58:47:ca:70:c7:c9:00", // EUI-64
		"not a mac",
		"zz:zz:zz:zz:zz:zz",
	} {
		if got, err := CanonicalMAC(in); err == nil {
			t.Errorf("CanonicalMAC(%q) = %q, want error", in, got)
		}
	}
}

// ---- Empty fleet ----

func TestEmptyFleet_DefaultsToMenu(t *testing.T) {
	f := Empty(newLog())
	p := f.Lookup("58:47:ca:70:c7:c9")
	if p.Boot != "menu" {
		t.Errorf("Empty fleet lookup boot = %q, want menu", p.Boot)
	}
	if !p.IsDefault {
		t.Errorf("Empty fleet should return IsDefault=true")
	}
}

// ---- Load / Lookup ----

func TestLoad_HappyPath(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "ubuntu.yaml", "#cloud-config\nidentity: {username: ops}")
	writeFile(t, dir, "rescue.ipxe", "#!ipxe\nshell")
	cfg := writeFile(t, dir, "fleet.yaml", `
defaults:
  boot: menu

machines:
  - mac: 58:47:ca:70:c7:c9
    name: kube-1
    boot: ubuntu-22.04
    cloud_init: ./ubuntu.yaml
  - mac: AA-BB-CC-DD-EE-01
    name: rescue
    boot: custom
    ipxe_script: rescue.ipxe
`)

	f, err := Load(cfg, newLog())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Known MAC, exact match
	p := f.Lookup("58:47:ca:70:c7:c9")
	if p.Boot != "ubuntu-22.04" {
		t.Errorf("kube-1 boot = %q, want ubuntu-22.04", p.Boot)
	}
	if p.Name != "kube-1" {
		t.Errorf("kube-1 name = %q, want kube-1", p.Name)
	}
	if !strings.HasSuffix(p.CloudInit, "ubuntu.yaml") {
		t.Errorf("cloud_init = %q, want absolute path ending in ubuntu.yaml", p.CloudInit)
	}

	// Known MAC, alternate format
	p2 := f.Lookup("aa:bb:cc:dd:ee:01")
	if p2.Boot != "custom" {
		t.Errorf("rescue boot = %q, want custom", p2.Boot)
	}
	if p2.IPXEScript == "" || !strings.HasSuffix(p2.IPXEScript, "rescue.ipxe") {
		t.Errorf("ipxe_script unexpectedly empty or wrong: %q", p2.IPXEScript)
	}

	// Unknown MAC → defaults
	p3 := f.Lookup("00:11:22:33:44:55")
	if p3.Boot != "menu" {
		t.Errorf("unknown MAC boot = %q, want menu", p3.Boot)
	}
	if !p3.IsDefault {
		t.Errorf("unknown MAC should return IsDefault=true")
	}
}

func TestLoad_UbuntuWithoutCloudInit_OK_v050(t *testing.T) {
	// v0.5.0: cloud_init is OPTIONAL for ubuntu-* — embedded default
	// user-data is served when omitted. Previously this was an error.
	dir := t.TempDir()
	cfg := writeFile(t, dir, "fleet.yaml", `
machines:
  - mac: 58:47:ca:70:c7:c9
    name: kube-1
    boot: ubuntu-22.04
`)
	f, err := Load(cfg, newLog())
	if err != nil {
		t.Fatalf("Load should succeed without cloud_init in v0.5.0: %v", err)
	}
	p := f.Lookup("58:47:ca:70:c7:c9")
	if p.Boot != "ubuntu-22.04" {
		t.Errorf("Boot = %q, want ubuntu-22.04", p.Boot)
	}
	if p.CloudInit != "" {
		t.Errorf("CloudInit should be empty (no field set), got %q", p.CloudInit)
	}
}

func TestLoad_RejectsCustomWithoutScript(t *testing.T) {
	dir := t.TempDir()
	cfg := writeFile(t, dir, "fleet.yaml", `
machines:
  - mac: 58:47:ca:70:c7:c9
    boot: custom
`)
	_, err := Load(cfg, newLog())
	if err == nil {
		t.Fatal("expected error: custom without ipxe_script should fail")
	}
}

func TestLoad_RejectsOperatorPhoneHome(t *testing.T) {
	// v0.12.0: pxe-beacon owns phone_home; an operator cloud_init that
	// defines its own (even with unrendered template placeholders) must
	// be rejected at load with a clear message.
	dir := t.TempDir()
	writeFile(t, dir, "ci.yaml", "#cloud-config\nhostname: {{.Name}}\nphone_home:\n  url: http://{{.AdvertisedIP}}:{{.HTTPPort}}/autoinstall/{{.MACHyp}}/done\n")
	cfg := writeFile(t, dir, "fleet.yaml", `
machines:
  - mac: 58:47:ca:70:c7:c9
    boot: debian-12
    cloud_init: ./ci.yaml
`)
	_, err := Load(cfg, newLog())
	if err == nil {
		t.Fatal("expected error: operator-defined phone_home should be rejected")
	}
	if !strings.Contains(err.Error(), "phone_home") {
		t.Errorf("error should mention phone_home, got: %v", err)
	}
}

func TestLoad_AllowsCommentedPhoneHome(t *testing.T) {
	// A commented-out phone_home (column-0 `# phone_home:`) is fine.
	dir := t.TempDir()
	writeFile(t, dir, "ci.yaml", "#cloud-config\nhostname: {{.Name}}\n# phone_home: disabled\n")
	cfg := writeFile(t, dir, "fleet.yaml", `
machines:
  - mac: 58:47:ca:70:c7:c9
    boot: debian-12
    cloud_init: ./ci.yaml
`)
	if _, err := Load(cfg, newLog()); err != nil {
		t.Fatalf("commented phone_home should not be rejected: %v", err)
	}
}

func TestLoad_RejectsBadBootTarget(t *testing.T) {
	dir := t.TempDir()
	cfg := writeFile(t, dir, "fleet.yaml", `
machines:
  - mac: 58:47:ca:70:c7:c9
    boot: bogus-os
`)
	_, err := Load(cfg, newLog())
	if err == nil {
		t.Fatal("expected error: unknown boot target should fail")
	}
}

func TestLoad_RejectsDuplicateMAC(t *testing.T) {
	dir := t.TempDir()
	cfg := writeFile(t, dir, "fleet.yaml", `
machines:
  - mac: 58:47:ca:70:c7:c9
    name: a
    boot: menu
  - mac: 58-47-ca-70-c7-c9
    name: b
    boot: menu
`)
	_, err := Load(cfg, newLog())
	if err == nil {
		t.Fatal("expected error: duplicate MAC should fail")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("error should mention duplicate: %v", err)
	}
}

func TestLoad_RejectsInvalidMAC(t *testing.T) {
	dir := t.TempDir()
	cfg := writeFile(t, dir, "fleet.yaml", `
machines:
  - mac: not-a-mac
    boot: menu
`)
	_, err := Load(cfg, newLog())
	if err == nil {
		t.Fatal("expected error: invalid MAC should fail")
	}
}

func TestLoad_MissingCloudInitFile(t *testing.T) {
	dir := t.TempDir()
	cfg := writeFile(t, dir, "fleet.yaml", `
machines:
  - mac: 58:47:ca:70:c7:c9
    boot: ubuntu-22.04
    cloud_init: ./does-not-exist.yaml
`)
	_, err := Load(cfg, newLog())
	if err == nil {
		t.Fatal("expected error: missing cloud_init file should fail")
	}
}

func TestReload_SwapsContent(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "user-data.yaml", "#cloud-config")
	cfgPath := writeFile(t, dir, "fleet.yaml", `
machines:
  - mac: 58:47:ca:70:c7:c9
    name: kube-1
    boot: menu
`)
	f, err := Load(cfgPath, newLog())
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Lookup("58:47:ca:70:c7:c9").Boot; got != "menu" {
		t.Fatalf("initial Boot = %q, want menu", got)
	}

	// Rewrite the file
	writeFile(t, dir, "fleet.yaml", `
machines:
  - mac: 58:47:ca:70:c7:c9
    name: kube-1
    boot: ubuntu-22.04
    cloud_init: ./user-data.yaml
`)
	if err := f.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := f.Lookup("58:47:ca:70:c7:c9").Boot; got != "ubuntu-22.04" {
		t.Errorf("after reload Boot = %q, want ubuntu-22.04", got)
	}
}

func TestLoad_StrictRejectsNameUnderDefaults(t *testing.T) {
	// The exact mistake from the v0.4.1 user report: machine fields
	// pasted under `defaults:` instead of a `machines:` list. Pre-v0.4.2
	// this silently parsed with `name:` being dropped; v0.4.2 strict
	// mode rejects it loudly.
	dir := t.TempDir()
	cfg := writeFile(t, dir, "fleet.yaml", `
defaults:
  name: db-primary
  boot: debian-12
`)
	_, err := Load(cfg, newLog())
	if err == nil {
		t.Fatal("expected error: name: under defaults: should be rejected")
	}
	if !strings.Contains(err.Error(), "name") {
		t.Errorf("error should mention the offending field: %v", err)
	}
	if !strings.Contains(err.Error(), "hint") {
		t.Errorf("error should include the operator-friendly hint: %v", err)
	}
}

func TestLoad_StrictRejectsUnknownTopLevel(t *testing.T) {
	dir := t.TempDir()
	cfg := writeFile(t, dir, "fleet.yaml", `
defaults:
  boot: menu
machine:                                  # typo: missing 's'
  - mac: 58:47:ca:70:c7:c9
`)
	_, err := Load(cfg, newLog())
	if err == nil {
		t.Fatal("expected error: 'machine' (typo) should be rejected at top level")
	}
}

func TestEmpty_ReloadIsError(t *testing.T) {
	f := Empty(newLog())
	if err := f.Reload(); err == nil {
		t.Error("Empty.Reload() should return an error")
	}
}
