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
	// v0.5.9 ships the diagnostic dispatch (probe-only). Production
	// dispatch returns in v0.5.10. Asserting via the renamed
	// production function so we don't lose coverage in the meantime.
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
		"iseq ${net0/mac:hexhyp} 58-47-ca-70-c7-c9 && goto m_venkat_1",
		// v0.5.13: dhcp at top of script (not per arm). Cleaner +
		// preserves any netmask widening from being overwritten.
		"dhcp || goto top_fail_dhcp",
		":top_fail_dhcp",
		// Per-machine block label.
		":m_venkat_1",
		// HTTP not HTTPS for d-i (PXE expert fix #2).
		"http://deb.debian.org/debian/dists/bookworm/main/installer-amd64/current/images/netboot/debian-installer/amd64/linux",
		// v0.6.16: preseed/url= (explicit form, matches netboot.xyz).
		"preseed/url=http://10.69.69.218:8080/autoinstall/58-47-ca-70-c7-c9/preseed.cfg",
		// v0.6.16: pin mirror suite to match kernel/initrd directory.
		"mirror/suite=bookworm",
		// Narration with sleep before reboot (PXE expert fix #8;
		// v0.5.3 uses goto-labeled error blocks ending in sleep+reboot).
		"echo pxe-beacon:",
		"sleep 30",
		"reboot",
		// v0.5.3: control flow uses explicit gotos to avoid the
		// `cmd || X && Y && reboot` precedence trap. Each fail path
		// has its own labeled block.
		"goto m_venkat_1_fail_kernel",
		":m_venkat_1_fail_kernel",
		"goto m_venkat_1_fail_boot",
		":m_venkat_1_fail_boot",
		// v0.6.3: interactive boot menu with 30s timeout. Letter keys
		// (b/m/s) instead of numeric (some snponly keyboards drop
		// numeric input). 'm' jumps to :menu_netbootxyz (no "NO MATCH"
		// preamble, unlike iseq-miss path).
		"menu pxe-beacon — venkat-1",
		"--default --key b m_venkat_1_boot",
		"--key m menu_netbootxyz",
		"--key s m_venkat_1_shell",
		"choose --timeout 30000 --default m_venkat_1_boot",
		":m_venkat_1_boot",
		":m_venkat_1_shell",
		":menu_netbootxyz",
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

func TestDispatch_RescueArm_OverridesConfiguredTarget(t *testing.T) {
	// v0.11.0: a MAC with a rescue intent armed boots SystemRescue,
	// not its configured fleet target (debian-12 here).
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

	ctx := dispatchCtx()
	ctx.RescueArmed = func(mac string) bool { return mac == "58:47:ca:70:c7:c9" }
	s := string(RenderDispatch(f, ctx))

	for _, want := range []string{
		// SystemRescue kernel/initrd from the assets tree.
		"http://10.69.69.218:8080/assets/systemrescue/sysresccd/boot/x86_64/vmlinuz",
		"http://10.69.69.218:8080/assets/systemrescue/sysresccd/boot/x86_64/sysresccd.img",
		// archiso fetches the squashfs itself from this base (trailing slash).
		"archiso_http_srv=http://10.69.69.218:8080/assets/systemrescue/",
		"archisobasedir=sysresccd",
		// Per-MAC sysrescuecfg delivery.
		"sysrescuecfg=http://10.69.69.218:8080/autoinstall/58-47-ca-70-c7-c9/sysrescue.yaml",
		// Menu surfaces the rescue override.
		"systemrescue (RESCUE)",
		"goto m_venkat_1_fail_kernel",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("rescue dispatch missing %q:\n%s", want, s)
		}
	}
	// The Debian d-i path must NOT be rendered when rescue is armed.
	if strings.Contains(s, "preseed/url=") {
		t.Errorf("rescue arm should not render the debian preseed boot:\n%s", s)
	}
}

func TestDispatch_RescueNotArmed_BootsConfiguredTarget(t *testing.T) {
	// Sanity: with RescueArmed returning false, the configured target
	// (debian-12) still renders — rescue is opt-in per MAC.
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
	ctx := dispatchCtx()
	ctx.RescueArmed = func(mac string) bool { return false }
	s := string(RenderDispatch(f, ctx))
	if !strings.Contains(s, "preseed/url=") {
		t.Errorf("non-rescue arm should render the debian preseed boot:\n%s", s)
	}
	if strings.Contains(s, "archiso_http_srv") {
		t.Errorf("non-rescue arm should not render SystemRescue:\n%s", s)
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
		{"dispatch entry for debian-host", "iseq ${net0/mac:hexhyp} 58-47-ca-70-c7-c9 && goto m_debian_host"},
		{"dispatch entry for ubuntu-host", "iseq ${net0/mac:hexhyp} aa-bb-cc-dd-ee-01 && goto m_ubuntu_host"},
	}
	for _, c := range cases {
		if !strings.Contains(s, c.want) {
			t.Errorf("%s — missing %q:\n%s", c.name, c.want, s)
		}
	}
}

func TestDispatch_Rocky9_Alma9(t *testing.T) {
	// v0.6.7: rocky-9 and alma-9 boot targets emit Anaconda+Kickstart
	// kernel cmdlines with inst.repo + inst.ks pointing at the
	// per-MAC /autoinstall/<mac>/kickstart.cfg route.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fleet.yaml"), []byte(`
machines:
  - mac: aa:bb:cc:dd:ee:01
    name: rocky-host
    boot: rocky-9
  - mac: aa:bb:cc:dd:ee:02
    name: alma-host
    boot: alma-9
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
		{"rocky-9 mirror", "download.rockylinux.org/pub/rocky/9/BaseOS/x86_64/os/images/pxeboot"},
		{"rocky-9 inst.repo", "inst.repo=https://download.rockylinux.org/pub/rocky/9/BaseOS/x86_64/os"},
		{"rocky-9 inst.ks", "inst.ks=http://10.69.69.218:8080/autoinstall/aa-bb-cc-dd-ee-01/kickstart.cfg"},
		{"alma-9 mirror", "repo.almalinux.org/almalinux/9/BaseOS/x86_64/os/images/pxeboot"},
		{"alma-9 inst.repo", "inst.repo=https://repo.almalinux.org/almalinux/9/BaseOS/x86_64/os"},
		{"alma-9 inst.ks", "inst.ks=http://10.69.69.218:8080/autoinstall/aa-bb-cc-dd-ee-02/kickstart.cfg"},
		{"rocky-9 fleet entry", "iseq ${net0/mac:hexhyp} aa-bb-cc-dd-ee-01 && goto m_rocky_host"},
		{"alma-9 fleet entry", "iseq ${net0/mac:hexhyp} aa-bb-cc-dd-ee-02 && goto m_alma_host"},
	}
	for _, c := range cases {
		if !strings.Contains(s, c.want) {
			t.Errorf("%s — missing %q:\n%s", c.name, c.want, s)
		}
	}
	for _, c := range cases {
		if !strings.Contains(s, c.want) {
			t.Errorf("%s — missing %q:\n%s", c.name, c.want, s)
		}
	}
}

func TestDispatch_ARM64_HardRefuse(t *testing.T) {
	// v0.8.1: dispatch script emits a two-line iseq allowlist at the
	// top, before the per-MAC dispatch. ${buildarch} not in {i386,
	// x86_64} hits a reboot block.
	out := RenderDispatch(nil, dispatchCtx())
	s := string(out)

	// The allowlist lines must use the documented one-iseq-per-line
	// pattern (chained `||`/`&&` doesn't work in iPXE — v0.5.14 bug).
	for _, want := range []string{
		"iseq ${buildarch} i386   && goto arch_ok",
		"iseq ${buildarch} x86_64 && goto arch_ok",
		"UNSUPPORTED ARCH",
		":arch_ok",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("dispatch missing %q:\n%s", want, s)
		}
	}

	// The arch check must come BEFORE the stage-0 settings dump (the
	// settings dump references x86-only paths that would confuse an
	// operator on an unsupported board).
	archIdx := strings.Index(s, ":arch_ok")
	stageIdx := strings.Index(s, "[stage 0/5]")
	if archIdx < 0 || stageIdx < 0 {
		t.Fatal("missing required markers")
	}
	if archIdx > stageIdx {
		t.Error("arch check must precede [stage 0/5] settings dump")
	}
}

func TestDispatch_LabelOf_Sanitizes(t *testing.T) {
	// v0.5.15: labels are [a-zA-Z0-9_] only. Hyphens, dots, slashes,
	// spaces, etc. all become '_'. (iPXE's goto silently no-ops on
	// hyphenated labels on some builds — confirmed by venkat@'s
	// shell test.)
	if got := labelOf("58:47:ca:70:c7:c9", "foo bar/baz"); got != "m_foo_bar_baz" {
		t.Errorf("labelOf sanitize = %q, want m_foo_bar_baz", got)
	}
	if got := labelOf("58:47:ca:70:c7:c9", "venkat-1"); got != "m_venkat_1" {
		t.Errorf("labelOf hyphen = %q, want m_venkat_1", got)
	}
	// No name → MAC fallback, underscores instead of hyphens.
	if got := labelOf("58:47:ca:70:c7:c9", ""); got != "m_58_47_ca_70_c7_c9" {
		t.Errorf("labelOf empty name = %q, want m_58_47_ca_70_c7_c9", got)
	}
}
