package boot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sampleCtx() RenderContext {
	return RenderContext{
		Name:         "kube-1",
		MAC:          "58:47:ca:70:c7:c9",
		MACHyp:       "58-47-ca-70-c7-c9",
		AdvertisedIP: "10.0.0.5",
		HTTPPort:     8080,
	}
}

func TestIsBuiltIn(t *testing.T) {
	for _, want := range []string{"menu", "ubuntu-22.04", "ubuntu-24.04", "debian-12", "debian-13"} {
		if !IsBuiltIn(want) {
			t.Errorf("IsBuiltIn(%q) = false, want true", want)
		}
	}
	for _, neg := range []string{"", "custom", "bogus", "ubuntu-99.99"} {
		if IsBuiltIn(neg) {
			t.Errorf("IsBuiltIn(%q) = true, want false", neg)
		}
	}
}

func TestRenderAutoexec_Menu(t *testing.T) {
	out, err := RenderAutoexec("menu", sampleCtx())
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.HasPrefix(s, "#!ipxe") {
		t.Errorf("menu autoexec missing #!ipxe shebang:\n%s", s)
	}
	if !strings.Contains(s, "boot.netboot.xyz/menu.ipxe") {
		t.Errorf("menu autoexec should chain to netboot.xyz menu:\n%s", s)
	}
	if !strings.Contains(s, "kube-1") {
		t.Errorf("Name not templated in:\n%s", s)
	}
}

func TestRenderAutoexec_Ubuntu2204(t *testing.T) {
	out, err := RenderAutoexec("ubuntu-22.04", sampleCtx())
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		"#!ipxe",
		"autoinstall",
		"ds=nocloud-net",
		"10.0.0.5",                      // AdvertisedIP
		"8080",                          // HTTPPort
		"58-47-ca-70-c7-c9",             // MACHyp
		"/assets/ubuntu-22.04",          // assets base URL (template uses iPXE ${assets} after)
		"${assets}/vmlinuz",             // kernel path via var
		"${assets}/filesystem.squashfs", // squashfs path
		"boot=casper",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("ubuntu-22.04 autoexec missing %q:\n%s", want, s)
		}
	}
}

func TestRenderAutoexec_Ubuntu2404(t *testing.T) {
	out, err := RenderAutoexec("ubuntu-24.04", sampleCtx())
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{"24.04", "/assets/ubuntu-24.04", "${assets}/vmlinuz", "boot=casper"} {
		if !strings.Contains(s, want) {
			t.Errorf("ubuntu-24.04 autoexec missing %q:\n%s", want, s)
		}
	}
}

func TestRenderAutoexec_Debian13(t *testing.T) {
	out, err := RenderAutoexec("debian-13", sampleCtx())
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		"#!ipxe",
		"trixie",
		"58-47-ca-70-c7-c9",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("debian-13 autoexec missing %q:\n%s", want, s)
		}
	}
}

func TestRenderAutoexec_Debian12(t *testing.T) {
	out, err := RenderAutoexec("debian-12", sampleCtx())
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		"#!ipxe",
		"bookworm",
		"url=", // preseed URL — v0.3 onwards
		"preseed.cfg",
		"58-47-ca-70-c7-c9",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("debian-12 autoexec missing %q:\n%s", want, s)
		}
	}
}

func TestRenderAutoexec_UnknownTargetRejected(t *testing.T) {
	if _, err := RenderAutoexec("freebsd-14", sampleCtx()); err == nil {
		t.Error("RenderAutoexec on unknown target should error")
	}
}

func TestRenderCustom_FromFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "rescue.ipxe")
	if err := os.WriteFile(p, []byte("#!ipxe\necho rescue {{.Name}} at {{.AdvertisedIP}}\nshell\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := RenderCustom(p, sampleCtx())
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "rescue kube-1 at 10.0.0.5") {
		t.Errorf("custom script not rendered:\n%s", s)
	}
}

func TestRedirectorScript(t *testing.T) {
	out := RedirectorScript("10.0.0.5", 8080)
	s := string(out)
	if !strings.Contains(s, "#!ipxe") {
		t.Errorf("redirector missing shebang:\n%s", s)
	}
	if !strings.Contains(s, "${net0/mac:hexhyp}") {
		t.Errorf("redirector should use iPXE MAC substitution:\n%s", s)
	}
	if !strings.Contains(s, "http://10.0.0.5:8080/autoinstall/") {
		t.Errorf("redirector URL malformed:\n%s", s)
	}
}

func TestRenderAutoexec_MACFallback(t *testing.T) {
	// Unnamed MAC: Name should fall back to MAC string in output.
	ctx := sampleCtx()
	ctx.Name = ""
	out, err := RenderAutoexec("menu", ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "58:47:ca:70:c7:c9") {
		t.Errorf("Name fallback to MAC not visible:\n%s", out)
	}
}
