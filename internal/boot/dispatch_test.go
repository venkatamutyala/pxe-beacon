package boot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/venkatamutyala/pxe-beacon/internal/fleet"
	"github.com/venkatamutyala/pxe-beacon/internal/narrlog"
)

func dispatchCtx() DispatchContext {
	return DispatchContext{AdvertisedIP: "10.69.69.218", HTTPPort: 8080}
}

func TestDispatch_EmptyFleet_DefaultArmOnly(t *testing.T) {
	out := RenderDispatch(nil, dispatchCtx())
	s := string(out)
	if !strings.HasPrefix(s, "#!ipxe") {
		t.Errorf("missing shebang: %s", s)
	}
	if !strings.Contains(s, ":target_default") {
		t.Errorf("missing default arm label: %s", s)
	}
	if !strings.Contains(s, "boot.netboot.xyz/menu.ipxe") {
		t.Errorf("default arm should chain netboot.xyz: %s", s)
	}
	// No machine blocks at all — only target_default.
	if strings.Contains(s, ":m_") {
		t.Errorf("unexpected per-machine block in empty fleet:\n%s", s)
	}
}

func TestDispatch_UserMAC_Debian12(t *testing.T) {
	// The v0.5.0 acceptance scenario: minimal fleet with the user's
	// MAC + boot: debian-12 + no other fields. Boot must work via
	// embedded defaults.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fleet.yaml"), []byte(`
machines:
  - mac: 58:47:ca:70:c7:c9
    name: venkat-1
    boot: debian-12
`), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := fleet.Load(filepath.Join(dir, "fleet.yaml"), narrlog.New("test", narrlog.LevelDebug, nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	out := RenderDispatch(f, dispatchCtx())
	s := string(out)

	for _, want := range []string{
		// Dispatch line for the user's MAC.
		"iseq ${net0/mac:hexhyp} 58-47-ca-70-c7-c9 && goto m_venkat-1",
		// v0.5.2: also matches against the boot-NIC alias for multi-NIC boxes.
		"iseq ${mac:hexhyp} 58-47-ca-70-c7-c9 && goto m_venkat-1",
		// Per-machine block label.
		":m_venkat-1",
		// dhcp inside the arm (PXE expert fix #1).
		"dhcp ||",
		// HTTP not HTTPS for d-i (PXE expert fix #2).
		"http://deb.debian.org/debian/dists/bookworm/main/installer-amd64/current/images/netboot/debian-installer/amd64/linux",
		// preseed URL points back at pxe-beacon.
		"url=http://10.69.69.218:8080/autoinstall/58-47-ca-70-c7-c9/preseed.cfg",
		// Console for headless boards (PXE expert fix #7).
		"console=tty0 console=ttyS0,115200n8",
		// Narration with sleep before reboot (PXE expert fix #8;
		// v0.5.2 swapped shell→reboot so messages persist).
		"echo pxe-beacon:",
		"sleep 15",
		"reboot",
		// Default fallback arm still present.
		":target_default",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("dispatch missing %q:\n%s", want, s)
		}
	}

	// No HTTPS to debian mirror.
	if strings.Contains(s, "https://deb.debian.org") {
		t.Errorf("dispatch should use HTTP not HTTPS for d-i (PXE expert fix #2):\n%s", s)
	}
}

func TestDispatch_MixedFleet(t *testing.T) {
	dir := t.TempDir()
	// Build operator files referenced by the custom entry — fleet
	// validator stats them at load time.
	if err := os.WriteFile(filepath.Join(dir, "rescue.ipxe"), []byte("#!ipxe\nshell"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fleet.yaml"), []byte(`
machines:
  - mac: 58:47:ca:70:c7:c9
    name: debian-host
    boot: debian-13
  - mac: aa:bb:cc:dd:ee:01
    name: ubuntu-host
    boot: ubuntu-22.04
  - mac: 11:22:33:44:55:66
    name: rescue
    boot: custom
    ipxe_script: ./rescue.ipxe
  - mac: 99:88:77:66:55:44
    name: just-menu
    boot: menu
`), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := fleet.Load(filepath.Join(dir, "fleet.yaml"), narrlog.New("test", narrlog.LevelDebug, nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	out := RenderDispatch(f, dispatchCtx())
	s := string(out)

	cases := []struct{ name, want string }{
		{"debian-13 mirror", "trixie/main/installer-amd64"},
		{"ubuntu-22.04 assets", "/assets/ubuntu-22.04"},
		{"ubuntu autoinstall trailing ---", "autoinstall ds=nocloud-net"},
		{"custom chain to operator script", "/autoinstall/11-22-33-44-55-66/autoexec.ipxe"},
		{"menu arm chains netboot.xyz", "boot.netboot.xyz/menu.ipxe"},
		{"dispatch entry for debian-host", "iseq ${net0/mac:hexhyp} 58-47-ca-70-c7-c9 && goto m_debian-host"},
		{"dispatch entry for ubuntu-host", "iseq ${net0/mac:hexhyp} aa-bb-cc-dd-ee-01 && goto m_ubuntu-host"},
	}
	for _, c := range cases {
		if !strings.Contains(s, c.want) {
			t.Errorf("%s — missing %q:\n%s", c.name, c.want, s)
		}
	}
}

func TestDispatch_LabelOf_Sanitizes(t *testing.T) {
	// Names with spaces / weird chars must produce valid iPXE labels.
	if got := labelOf("58:47:ca:70:c7:c9", "foo bar/baz"); got != "m_foo_bar_baz" {
		t.Errorf("labelOf sanitize = %q, want m_foo_bar_baz", got)
	}
	// No name → MAC fallback.
	if got := labelOf("58:47:ca:70:c7:c9", ""); got != "m_58-47-ca-70-c7-c9" {
		t.Errorf("labelOf empty name = %q, want m_58-47-ca-70-c7-c9", got)
	}
}
