package cache

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSafeTargetName(t *testing.T) {
	for _, ok := range []string{"ubuntu-22.04", "ubuntu-24.04", "debian-13", "foo_bar"} {
		if !safeTargetName(ok) {
			t.Errorf("safeTargetName(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", ".hidden", "../escape", "a/b", "with space", "/abs"} {
		if safeTargetName(bad) {
			t.Errorf("safeTargetName(%q) = true, want false (path traversal-y)", bad)
		}
	}
}

func TestCache_TargetDir(t *testing.T) {
	c, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p, err := c.TargetDir("ubuntu-22.04")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(p, "/ubuntu-22.04") {
		t.Errorf("TargetDir suffix: %q", p)
	}
	fi, err := os.Stat(p)
	if err != nil || !fi.IsDir() {
		t.Errorf("target dir not created: %v", err)
	}

	if _, err := c.TargetDir("../escape"); err == nil {
		t.Error("expected error for path-traversal target name")
	}
}

func TestCache_AssetPath_RejectsTraversal(t *testing.T) {
	c, _ := New(t.TempDir())
	if _, err := c.AssetPath("ubuntu-22.04", "../escape"); err == nil {
		t.Error("expected error for asset name with ..")
	}
	if _, err := c.AssetPath("../escape", "vmlinuz"); err == nil {
		t.Error("expected error for target name with ..")
	}
	got, err := c.AssetPath("ubuntu-22.04", "vmlinuz")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got, "/ubuntu-22.04/vmlinuz") {
		t.Errorf("AssetPath = %q", got)
	}
}

func TestCache_IsPopulated_FalseWhenEmpty(t *testing.T) {
	c, _ := New(t.TempDir())
	_, _ = c.TargetDir("ubuntu-22.04")
	ok, m := c.IsPopulated("ubuntu-22.04")
	if ok || m != nil {
		t.Errorf("empty target should not be populated, got ok=%v m=%v", ok, m)
	}
}

func TestCache_IsPopulated_TrueAfterWrite(t *testing.T) {
	c, _ := New(t.TempDir())
	tdir, _ := c.TargetDir("ubuntu-22.04")
	for _, f := range AssetFiles {
		if err := os.WriteFile(filepath.Join(tdir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.WriteManifest("ubuntu-22.04", &Manifest{
		Target:    "ubuntu-22.04",
		Source:    "test://iso",
		FetchedAt: time.Now(),
		Files:     map[string]Asset{},
	}); err != nil {
		t.Fatal(err)
	}
	ok, m := c.IsPopulated("ubuntu-22.04")
	if !ok {
		t.Errorf("expected IsPopulated=true after WriteManifest + assets")
	}
	if m == nil || m.Source != "test://iso" {
		t.Errorf("manifest not read back correctly: %+v", m)
	}
}

func TestSHA256File(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	if err := os.WriteFile(p, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	sum, err := SHA256File(p)
	if err != nil {
		t.Fatal(err)
	}
	// sha256("hello")
	want := "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if sum != want {
		t.Errorf("SHA256File = %q, want %q", sum, want)
	}
}

func TestNormalizeISOName(t *testing.T) {
	cases := map[string]string{
		"VMLINUZ;1":  "vmlinuz",
		"vmlinuz":    "vmlinuz",
		"INITRD.GZ;": "initrd.gz",
	}
	for in, want := range cases {
		if got := normalizeISOName(in); got != want {
			t.Errorf("normalizeISOName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTargetsHaveAllAssetPaths(t *testing.T) {
	// Sanity: every Target in the registry maps every AssetFile.
	for name, spec := range Targets {
		for _, f := range AssetFiles {
			if _, ok := spec.ISOPath[f]; !ok {
				t.Errorf("target %q missing ISOPath for %q", name, f)
			}
		}
	}
}
