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

func TestLoad_RejectsUbuntuWithoutCloudInit(t *testing.T) {
	dir := t.TempDir()
	cfg := writeFile(t, dir, "fleet.yaml", `
machines:
  - mac: 58:47:ca:70:c7:c9
    name: kube-1
    boot: ubuntu-22.04
`)
	_, err := Load(cfg, newLog())
	if err == nil {
		t.Fatal("expected error: ubuntu-22.04 without cloud_init should fail validation")
	}
	if !strings.Contains(err.Error(), "cloud_init") {
		t.Errorf("error should mention cloud_init: %v", err)
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

func TestEmpty_ReloadIsError(t *testing.T) {
	f := Empty(newLog())
	if err := f.Reload(); err == nil {
		t.Error("Empty.Reload() should return an error")
	}
}
