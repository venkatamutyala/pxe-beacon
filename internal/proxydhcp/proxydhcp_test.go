package proxydhcp

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/iana"

	"github.com/venkatamutyala/pxe-beacon/internal/fleet"
)

// newDiscover crafts a synthetic DISCOVER closely matching what a UEFI
// firmware sends in the wild. The "real fixture" PLAN section 5
// envisages (discover.pcap) does not exist in this repo; these
// synthetic builds use the same library that parses real captures, so
// every option is encoded exactly as a real client would encode it.
// PROGRESS.md notes this fallback.
func newDiscover(t *testing.T, mac string, arch iana.Arch, vendorClass string, userClass string) *dhcpv4.DHCPv4 {
	t.Helper()
	hw, err := net.ParseMAC(mac)
	if err != nil {
		t.Fatalf("parse mac: %v", err)
	}
	mods := []dhcpv4.Modifier{
		dhcpv4.WithMessageType(dhcpv4.MessageTypeDiscover),
		dhcpv4.WithHwAddr(hw),
	}
	if vendorClass != "" {
		mods = append(mods, dhcpv4.WithOption(dhcpv4.OptClassIdentifier(vendorClass)))
	}
	if userClass != "" {
		// rfc=false matches what iPXE actually emits (single string,
		// not the RFC 3004 length-prefixed multi-class form).
		mods = append(mods, dhcpv4.WithUserClass(userClass, false))
	}
	// Encode option 93 as two big-endian bytes (one arch entry).
	if arch != 0 || vendorClass != "" {
		mods = append(mods, dhcpv4.WithOption(dhcpv4.OptGeneric(
			dhcpv4.OptionClientSystemArchitectureType,
			iana.Archs{arch}.ToBytes(),
		)))
	}
	d, err := dhcpv4.New(mods...)
	if err != nil {
		t.Fatalf("new discover: %v", err)
	}
	return d
}

func defaultCfg() Config {
	return Config{
		AdvertisedIP:   net.ParseIP("10.0.0.5"),
		HTTPPort:       8080,
		IPXEScriptPath: "/boot.ipxe",
	}
}

func TestBuildOffer_EFIx64_TFTP(t *testing.T) {
	req := newDiscover(t, "58:47:ca:70:c7:c9", iana.EFI_X86_64, "PXEClient:Arch:00007:UNDI:003016", "")
	reply, dec, err := BuildOffer(req, defaultCfg())
	if err != nil {
		t.Fatalf("BuildOffer: %v", err)
	}
	if reply == nil {
		t.Fatal("reply is nil")
	}
	if dec.Stage != StageFirmwareTFTP {
		t.Errorf("stage = %q, want %q", dec.Stage, StageFirmwareTFTP)
	}
	if dec.Transport != TransportTFTP {
		t.Errorf("transport = %s, want TFTP", dec.Transport)
	}
	if dec.BootFile != "snponly.efi" {
		t.Errorf("bootfile = %q, want snponly.efi", dec.BootFile)
	}
	if got := reply.MessageType(); got != dhcpv4.MessageTypeOffer {
		t.Errorf("reply msg type = %s, want OFFER", got)
	}
	if got := reply.ServerIPAddr.String(); got != "10.0.0.5" {
		t.Errorf("siaddr = %s, want 10.0.0.5", got)
	}
	if got := reply.YourIPAddr.String(); got != "0.0.0.0" {
		t.Errorf("yiaddr = %s, want 0.0.0.0 (proxyDHCP MUST NOT assign IPs)", got)
	}
	if got := reply.BootFileName; got != "snponly.efi" {
		t.Errorf("bootfile name = %q, want snponly.efi", got)
	}
	if got := reply.TFTPServerName(); got != "10.0.0.5" {
		t.Errorf("opt 66 tftp server = %q, want 10.0.0.5", got)
	}
	if got := reply.ClassIdentifier(); !strings.HasPrefix(got, "PXEClient") {
		t.Errorf("reply vendor class = %q, want PXEClient prefix", got)
	}
	if dec.UnknownArch {
		t.Errorf("unknownArch = true, want false for EFI_X86_64")
	}
}

func TestBuildOffer_HTTPBoot_x64(t *testing.T) {
	req := newDiscover(t, "aa:bb:cc:dd:ee:ff", iana.EFI_X86_64_HTTP, "HTTPClient:Arch:00016:UNDI:003016", "")
	reply, dec, err := BuildOffer(req, defaultCfg())
	if err != nil {
		t.Fatalf("BuildOffer: %v", err)
	}
	if dec.Stage != StageFirmwareHTTP {
		t.Errorf("stage = %q, want %q", dec.Stage, StageFirmwareHTTP)
	}
	if dec.Transport != TransportHTTP {
		t.Errorf("transport = %s, want HTTP", dec.Transport)
	}
	wantURL := "http://10.0.0.5:8080/snponly.efi"
	if reply.BootFileName != wantURL {
		t.Errorf("bootfile URL = %q, want %q", reply.BootFileName, wantURL)
	}
	// UEFI HTTP boot expects class identifier HTTPClient in OFFER.
	if got := reply.ClassIdentifier(); got != "HTTPClient" {
		t.Errorf("class identifier = %q, want HTTPClient", got)
	}
	if got := reply.YourIPAddr.String(); got != "0.0.0.0" {
		t.Errorf("yiaddr = %s, want 0.0.0.0", got)
	}
}

func TestBuildOffer_ARM64_TFTP(t *testing.T) {
	req := newDiscover(t, "00:11:22:33:44:55", iana.EFI_ARM64, "PXEClient", "")
	_, dec, err := BuildOffer(req, defaultCfg())
	if err != nil {
		t.Fatalf("BuildOffer: %v", err)
	}
	if dec.Stage != StageFirmwareTFTP {
		t.Errorf("stage = %q, want firmware-TFTP", dec.Stage)
	}
	if dec.BootFile != "ipxe-arm64.efi" {
		t.Errorf("bootfile = %q, want ipxe-arm64.efi", dec.BootFile)
	}
}

func TestBuildOffer_ARM64_HTTPBoot(t *testing.T) {
	req := newDiscover(t, "00:11:22:33:44:66", iana.EFI_ARM64_HTTP, "HTTPClient", "")
	reply, dec, err := BuildOffer(req, defaultCfg())
	if err != nil {
		t.Fatalf("BuildOffer: %v", err)
	}
	if dec.Stage != StageFirmwareHTTP {
		t.Errorf("stage = %q, want firmware-HTTP", dec.Stage)
	}
	if !strings.HasSuffix(reply.BootFileName, "/ipxe-arm64.efi") {
		t.Errorf("bootfile = %q, want /ipxe-arm64.efi suffix", reply.BootFileName)
	}
}

func TestBuildOffer_LegacyBIOS(t *testing.T) {
	// PXE legacy BIOS clients often omit option 93 entirely. We should
	// fall back to INTEL_X86PC and serve undionly.kpxe over TFTP.
	hw, _ := net.ParseMAC("00:0c:29:aa:bb:cc")
	req, err := dhcpv4.New(
		dhcpv4.WithMessageType(dhcpv4.MessageTypeDiscover),
		dhcpv4.WithHwAddr(hw),
		dhcpv4.WithOption(dhcpv4.OptClassIdentifier("PXEClient")),
	)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	_, dec, err := BuildOffer(req, defaultCfg())
	if err != nil {
		t.Fatalf("BuildOffer: %v", err)
	}
	if dec.SelectedArch != iana.INTEL_X86PC {
		t.Errorf("selectedArch = %v, want INTEL_X86PC", dec.SelectedArch)
	}
	if dec.Transport != TransportTFTP {
		t.Errorf("transport = %s, want TFTP", dec.Transport)
	}
	if dec.BootFile != "undionly.kpxe" {
		t.Errorf("bootfile = %q, want undionly.kpxe", dec.BootFile)
	}
}

func TestBuildOffer_iPXEUserClass_ServesScript(t *testing.T) {
	// After iPXE chainloads, it re-DHCPs with userclass=iPXE. PLAN says
	// we MUST serve the script (not the binary) here — that's what
	// breaks the chainload loop.
	req := newDiscover(t, "58:47:ca:70:c7:c9", iana.EFI_X86_64, "PXEClient:Arch:00007:UNDI:003016", "iPXE")
	reply, dec, err := BuildOffer(req, defaultCfg())
	if err != nil {
		t.Fatalf("BuildOffer: %v", err)
	}
	if !dec.IsIPXEStage {
		t.Errorf("IsIPXEStage = false, want true")
	}
	if dec.Stage != StageIPXEScript {
		t.Errorf("stage = %q, want %q", dec.Stage, StageIPXEScript)
	}
	if dec.Transport != TransportHTTP {
		t.Errorf("transport = %s, want HTTP", dec.Transport)
	}
	wantURL := "http://10.0.0.5:8080/boot.ipxe"
	if reply.BootFileName != wantURL {
		t.Errorf("bootfile = %q, want %q", reply.BootFileName, wantURL)
	}
	if got := reply.YourIPAddr.String(); got != "0.0.0.0" {
		t.Errorf("yiaddr = %s, want 0.0.0.0 (proxyDHCP never assigns IPs)", got)
	}
}

func TestBuildOffer_SkipsNonPXEAsBenign(t *testing.T) {
	// A normal DHCP DISCOVER with no option 60 (e.g. a freshly-booted
	// Linux box doing its own DHCP) MUST be silently skipped and
	// labelled benign per PLAN section 0.
	hw, _ := net.ParseMAC("aa:bb:cc:dd:ee:ff")
	req, err := dhcpv4.New(
		dhcpv4.WithMessageType(dhcpv4.MessageTypeDiscover),
		dhcpv4.WithHwAddr(hw),
	)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	reply, dec, err := BuildOffer(req, defaultCfg())
	if !errors.Is(err, ErrSkip) {
		t.Errorf("err = %v, want ErrSkip", err)
	}
	if reply != nil {
		t.Errorf("reply = %v, want nil", reply)
	}
	if dec.Skip != SkipNotPXE {
		t.Errorf("skip = %v, want SkipNotPXE", dec.Skip)
	}
	if !dec.IsBenignSkip() {
		t.Errorf("IsBenignSkip = false, want true")
	}
}

func TestBuildOffer_SkipsNonDiscoverNonRequest(t *testing.T) {
	// An ACK (or anything that isn't DISCOVER/REQUEST) is never our
	// job; skip with a non-benign reason.
	hw, _ := net.ParseMAC("aa:bb:cc:dd:ee:ff")
	req, err := dhcpv4.New(
		dhcpv4.WithMessageType(dhcpv4.MessageTypeAck),
		dhcpv4.WithHwAddr(hw),
		dhcpv4.WithOption(dhcpv4.OptClassIdentifier("PXEClient")),
	)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	_, dec, err := BuildOffer(req, defaultCfg())
	if !errors.Is(err, ErrSkip) {
		t.Errorf("err = %v, want ErrSkip", err)
	}
	if dec.Skip != SkipUnsupportedMessageType {
		t.Errorf("skip = %v, want SkipUnsupportedMessageType", dec.Skip)
	}
}

func TestBuildOffer_UnknownArchFallsBackAndFlags(t *testing.T) {
	// Some firmware reports an arch we don't have a table entry for.
	// We should pick the canonical fallback (EFI x86_64 over TFTP)
	// and flag UnknownArch=true so the logger can shout about it.
	req := newDiscover(t, "00:00:00:00:00:01", iana.Arch(0xfeed), "PXEClient", "")
	_, dec, err := BuildOffer(req, defaultCfg())
	if err != nil {
		t.Fatalf("BuildOffer: %v", err)
	}
	if !dec.UnknownArch {
		t.Errorf("UnknownArch = false, want true")
	}
	if dec.Transport != TransportTFTP {
		t.Errorf("fallback transport = %s, want TFTP", dec.Transport)
	}
}

func TestBuildOffer_VendorClassPXEClientSuffixed(t *testing.T) {
	// Real-world PXEClient strings look like
	// "PXEClient:Arch:00007:UNDI:003016" — must still be recognized.
	req := newDiscover(t, "00:00:00:00:00:02", iana.EFI_X86_64, "PXEClient:Arch:00007:UNDI:003016", "")
	_, dec, err := BuildOffer(req, defaultCfg())
	if err != nil {
		t.Fatalf("BuildOffer: %v", err)
	}
	if dec.Stage != StageFirmwareTFTP {
		t.Errorf("stage = %q, want firmware-TFTP", dec.Stage)
	}
}

func TestBuildOffer_PureFunction_NoSideEffectOnRequest(t *testing.T) {
	// BuildOffer must not mutate its input. PLAN's purity rule
	// motivates testing this directly.
	req := newDiscover(t, "58:47:ca:70:c7:c9", iana.EFI_X86_64, "PXEClient", "")
	before := req.Summary()
	_, _, err := BuildOffer(req, defaultCfg())
	if err != nil {
		t.Fatal(err)
	}
	after := req.Summary()
	if before != after {
		t.Errorf("BuildOffer mutated request:\nbefore=%s\nafter=%s", before, after)
	}
}

// TestBuildOffer_RequestRepliesACK is the regression for the iPXE BINL
// stuck-loop seen in the wild: iPXE sends a unicast DHCPREQUEST to
// udp/4011 with our Server-ID, expecting a DHCPACK. We were replying
// with DHCPOFFER, which iPXE silently dropped. UEFI firmware tolerated
// the wrong type during the firmware stage, but iPXE didn't.
func TestBuildOffer_RequestRepliesACK(t *testing.T) {
	hw, _ := net.ParseMAC("58:47:ca:70:c7:c9")
	req, err := dhcpv4.New(
		dhcpv4.WithMessageType(dhcpv4.MessageTypeRequest),
		dhcpv4.WithHwAddr(hw),
		dhcpv4.WithOption(dhcpv4.OptClassIdentifier("PXEClient:Arch:00007:UNDI:003010")),
		dhcpv4.WithOption(dhcpv4.OptGeneric(
			dhcpv4.OptionClientSystemArchitectureType,
			iana.Archs{iana.EFI_X86_64}.ToBytes(),
		)),
		dhcpv4.WithUserClass("iPXE", false),
		// Mirror the wire-captured iPXE BINL REQUEST: Server-ID set
		// to our advertised IP.
		dhcpv4.WithOption(dhcpv4.OptServerIdentifier(net.ParseIP("10.0.0.5").To4())),
	)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	reply, dec, err := BuildOffer(req, defaultCfg())
	if err != nil {
		t.Fatalf("BuildOffer: %v", err)
	}
	if got := reply.MessageType(); got != dhcpv4.MessageTypeAck {
		t.Errorf("reply msg type = %s, want ACK (iPXE BINL drops OFFER replies to REQUEST)", got)
	}
	if dec.Stage != StageIPXEScript {
		t.Errorf("stage = %q, want %q", dec.Stage, StageIPXEScript)
	}
	wantURL := "http://10.0.0.5:8080/boot.ipxe"
	if reply.BootFileName != wantURL {
		t.Errorf("bootfile = %q, want %q", reply.BootFileName, wantURL)
	}
}

// TestBuildOffer_DiscoverStillRepliesOFFER ensures the fix above didn't
// regress the DISCOVER path.
func TestBuildOffer_DiscoverStillRepliesOFFER(t *testing.T) {
	req := newDiscover(t, "58:47:ca:70:c7:c9", iana.EFI_X86_64, "PXEClient", "")
	reply, _, err := BuildOffer(req, defaultCfg())
	if err != nil {
		t.Fatalf("BuildOffer: %v", err)
	}
	if got := reply.MessageType(); got != dhcpv4.MessageTypeOffer {
		t.Errorf("reply msg type = %s, want OFFER", got)
	}
}

// fleetCfg writes a tiny fleet.yaml + side-files and returns a loaded
// *fleet.Fleet pointing at them. Used by the per-MAC routing tests.
func fleetCfg(t *testing.T) *fleet.Fleet {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ubuntu.yaml"), []byte("#cloud-config"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fleet.yaml"), []byte(`
machines:
  - mac: 58:47:ca:70:c7:c9
    name: kube-1
    boot: ubuntu-22.04
    cloud_init: ./ubuntu.yaml
  - mac: aa:bb:cc:dd:ee:01
    name: rescue-jumpbox
    boot: menu
`), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := fleet.Load(filepath.Join(dir, "fleet.yaml"), nil)
	if err != nil {
		t.Fatalf("fleet.Load: %v", err)
	}
	return f
}

func TestBuildOffer_FleetPopulatesMachineName(t *testing.T) {
	req := newDiscover(t, "58:47:ca:70:c7:c9", iana.EFI_X86_64, "PXEClient", "")
	cfg := defaultCfg()
	cfg.Fleet = fleetCfg(t)

	_, dec, err := BuildOffer(req, cfg)
	if err != nil {
		t.Fatalf("BuildOffer: %v", err)
	}
	if dec.MachineName != "kube-1" {
		t.Errorf("MachineName = %q, want kube-1", dec.MachineName)
	}
	if dec.BootTarget != "ubuntu-22.04" {
		t.Errorf("BootTarget = %q, want ubuntu-22.04", dec.BootTarget)
	}
}

func TestBuildOffer_FleetUnknownMAC_DefaultsToMenu(t *testing.T) {
	req := newDiscover(t, "11:22:33:44:55:66", iana.EFI_X86_64, "PXEClient", "")
	cfg := defaultCfg()
	cfg.Fleet = fleetCfg(t)

	_, dec, err := BuildOffer(req, cfg)
	if err != nil {
		t.Fatalf("BuildOffer: %v", err)
	}
	if dec.MachineName != "" {
		t.Errorf("MachineName = %q, want empty (unknown MAC)", dec.MachineName)
	}
	if dec.BootTarget != "menu" {
		t.Errorf("BootTarget = %q, want menu (default for unknown MAC)", dec.BootTarget)
	}
}

func TestBuildOffer_NilFleet_NoCrashAndDefaultsToMenu(t *testing.T) {
	req := newDiscover(t, "58:47:ca:70:c7:c9", iana.EFI_X86_64, "PXEClient", "")
	cfg := defaultCfg()
	// cfg.Fleet is nil — should be safe, behave like v0.1.3.

	_, dec, err := BuildOffer(req, cfg)
	if err != nil {
		t.Fatalf("BuildOffer with nil Fleet: %v", err)
	}
	if dec.MachineName != "" {
		t.Errorf("MachineName should be empty with no fleet, got %q", dec.MachineName)
	}
	if dec.BootTarget != "menu" {
		t.Errorf("BootTarget = %q, want menu (nil fleet path)", dec.BootTarget)
	}
}

// v0.7.1 pending-action tests.

func TestBuildOffer_NoPendingAction_SkipsOffer(t *testing.T) {
	req := newDiscover(t, "58:47:ca:70:c7:c9", iana.EFI_X86_64, "PXEClient", "")
	cfg := defaultCfg()
	cfg.Fleet = fleetCfg(t)
	cfg.Pending = func(mac string) bool { return false }

	reply, dec, err := BuildOffer(req, cfg)
	if !errors.Is(err, ErrSkip) {
		t.Fatalf("expected ErrSkip, got err=%v", err)
	}
	if reply != nil {
		t.Errorf("MAC with no pending action should not produce a reply, got %+v", reply)
	}
	if dec.Skip != SkipNoPendingAction {
		t.Errorf("Decision.Skip = %v, want SkipNoPendingAction", dec.Skip)
	}
	if dec.Stage != StageSkip {
		t.Errorf("Decision.Stage = %q, want %q", dec.Stage, StageSkip)
	}
	if !strings.Contains(dec.SkipReason, "no pending action") {
		t.Errorf("SkipReason should mention 'no pending action', got %q", dec.SkipReason)
	}
}

func TestBuildOffer_PendingAction_ProducesOffer(t *testing.T) {
	req := newDiscover(t, "58:47:ca:70:c7:c9", iana.EFI_X86_64, "PXEClient", "")
	cfg := defaultCfg()
	cfg.Fleet = fleetCfg(t)
	cfg.Pending = func(mac string) bool { return true }

	reply, dec, err := BuildOffer(req, cfg)
	if err != nil {
		t.Fatalf("BuildOffer: %v", err)
	}
	if reply == nil {
		t.Fatal("MAC with pending action should produce a reply")
	}
	if dec.Skip != NotSkipped {
		t.Errorf("Decision.Skip = %v, want NotSkipped", dec.Skip)
	}
}

func TestBuildOffer_UnknownMAC_BypassesPendingCheck(t *testing.T) {
	// Unknown MACs aren't subject to the pending check — they should
	// still reach the OFFER path even when Pending returns false.
	req := newDiscover(t, "11:22:33:44:55:66", iana.EFI_X86_64, "PXEClient", "")
	cfg := defaultCfg()
	cfg.Fleet = fleetCfg(t)
	cfg.Pending = func(mac string) bool { return false }

	reply, dec, err := BuildOffer(req, cfg)
	if err != nil {
		t.Fatalf("BuildOffer: %v", err)
	}
	if reply == nil {
		t.Fatal("unknown MAC should still get a reply (fallback path)")
	}
	if dec.Skip != NotSkipped {
		t.Errorf("Decision.Skip = %v, want NotSkipped for unknown MAC", dec.Skip)
	}
}

func TestBuildOffer_NilPendingCallback_AllowsAll(t *testing.T) {
	// Backwards compatibility: when the pending Store isn't wired,
	// behavior matches <= v0.6.x — all fleet members get OFFERs.
	req := newDiscover(t, "58:47:ca:70:c7:c9", iana.EFI_X86_64, "PXEClient", "")
	cfg := defaultCfg()
	cfg.Fleet = fleetCfg(t)
	cfg.Pending = nil

	reply, _, err := BuildOffer(req, cfg)
	if err != nil {
		t.Fatalf("BuildOffer: %v", err)
	}
	if reply == nil {
		t.Fatal("nil Pending callback: should get a reply (compat path)")
	}
}

// v0.8.1 tests: iPXE-stage bypass, already-installed guard.

func TestBuildOffer_iPXEStage_BypassesPendingCheck(t *testing.T) {
	// PXE-expert blocker: an iPXE-stage REQUEST (userclass=iPXE) must
	// always get the script URL OFFER, even when Pending returns false.
	// Otherwise we strand iPXE mid-chainload if intent is cancelled
	// right after install kickoff.
	req := newDiscover(t, "58:47:ca:70:c7:c9", iana.EFI_X86_64, "PXEClient", "iPXE")
	cfg := defaultCfg()
	cfg.Fleet = fleetCfg(t)
	cfg.Pending = func(mac string) bool { return false } // explicit "no pending"

	reply, dec, err := BuildOffer(req, cfg)
	if err != nil {
		t.Fatalf("iPXE-stage with no pending: want OFFER, got err=%v", err)
	}
	if reply == nil {
		t.Fatal("iPXE-stage must get script URL OFFER regardless of pending state")
	}
	if dec.Stage != StageIPXEScript {
		t.Errorf("Decision.Stage = %q, want %q", dec.Stage, StageIPXEScript)
	}
}

func TestBuildOffer_iPXEStage_BypassesAlreadyInstalledGuard(t *testing.T) {
	// Same logic for the already-installed guard.
	req := newDiscover(t, "58:47:ca:70:c7:c9", iana.EFI_X86_64, "PXEClient", "iPXE")
	cfg := defaultCfg()
	cfg.Fleet = fleetCfg(t)
	cfg.Pending = func(mac string) bool { return false }
	cfg.LastEvent = func(mac string) fleet.Event { return fleet.EventInstallerDone }

	reply, dec, err := BuildOffer(req, cfg)
	if err != nil {
		t.Fatalf("iPXE-stage on installer-done box: want OFFER, got err=%v", err)
	}
	if reply == nil {
		t.Fatal("iPXE-stage must get script URL OFFER even when installer-done")
	}
	if dec.Stage != StageIPXEScript {
		t.Errorf("Decision.Stage = %q, want %q", dec.Stage, StageIPXEScript)
	}
}

func TestBuildOffer_AlreadyInstalled_NoPending_SkipsOffer(t *testing.T) {
	// The load-bearing v0.8.1 invariant (TPM regression-test ask).
	// installer-done + no pending intent → no OFFER.
	req := newDiscover(t, "58:47:ca:70:c7:c9", iana.EFI_X86_64, "PXEClient", "")
	cfg := defaultCfg()
	cfg.Fleet = fleetCfg(t)
	cfg.Pending = func(mac string) bool { return false }
	cfg.LastEvent = func(mac string) fleet.Event { return fleet.EventInstallerDone }

	reply, dec, err := BuildOffer(req, cfg)
	if !errors.Is(err, ErrSkip) {
		t.Fatalf("want ErrSkip, got err=%v", err)
	}
	if reply != nil {
		t.Error("installer-done + no pending should produce no reply")
	}
	if dec.Skip != SkipAlreadyDeployed {
		t.Errorf("Decision.Skip = %v, want SkipAlreadyDeployed", dec.Skip)
	}
}

func TestBuildOffer_AlreadyInstalled_WithPendingInstall_StillOffers(t *testing.T) {
	// Operator's explicit PUT /intent re-arms. Pending is the force flag.
	req := newDiscover(t, "58:47:ca:70:c7:c9", iana.EFI_X86_64, "PXEClient", "")
	cfg := defaultCfg()
	cfg.Fleet = fleetCfg(t)
	cfg.Pending = func(mac string) bool { return true }
	cfg.LastEvent = func(mac string) fleet.Event { return fleet.EventInstallerDone }

	reply, dec, err := BuildOffer(req, cfg)
	if err != nil {
		t.Fatalf("installer-done + pending install: want OFFER, got err=%v", err)
	}
	if reply == nil {
		t.Fatal("pending intent must override the already-installed guard")
	}
	if dec.Skip != NotSkipped {
		t.Errorf("Decision.Skip = %v, want NotSkipped", dec.Skip)
	}
}

func TestBuildOffer_UnknownMAC_BypassesAlreadyInstalledGuard(t *testing.T) {
	// Unknown MACs aren't fleet-known, so the guard never fires for them.
	req := newDiscover(t, "11:22:33:44:55:66", iana.EFI_X86_64, "PXEClient", "")
	cfg := defaultCfg()
	cfg.Fleet = fleetCfg(t)
	cfg.Pending = func(mac string) bool { return false }
	cfg.LastEvent = func(mac string) fleet.Event { return fleet.EventInstallerDone }

	reply, _, err := BuildOffer(req, cfg)
	if err != nil {
		t.Fatalf("unknown MAC: want OFFER fallback, got err=%v", err)
	}
	if reply == nil {
		t.Fatal("unknown MAC should still get a reply (netboot.xyz fallback)")
	}
}

func TestBuildOffer_RejectsBadConfig(t *testing.T) {
	req := newDiscover(t, "00:00:00:00:00:03", iana.EFI_X86_64, "PXEClient", "")
	// missing AdvertisedIP
	if _, _, err := BuildOffer(req, Config{HTTPPort: 8080}); err == nil {
		t.Error("expected error for missing AdvertisedIP")
	}
	// bad port
	if _, _, err := BuildOffer(req, Config{
		AdvertisedIP: net.ParseIP("10.0.0.5"), HTTPPort: 0,
	}); err == nil {
		t.Error("expected error for HTTPPort=0")
	}
}

func TestBuildOffer_NoteSighting_UnknownMACOnly(t *testing.T) {
	// v0.13.0: a firmware-stage DISCOVER from an unknown MAC fires the
	// discovery callback; a fleet-known MAC does not.
	var seen []string
	cfg := defaultCfg()
	cfg.NoteSighting = func(mac, arch, vc string) {
		seen = append(seen, mac+"|"+arch+"|"+vc)
	}

	// Unknown MAC, firmware stage → one sighting with arch label.
	req := newDiscover(t, "52:54:00:ab:cd:ef", iana.EFI_X86_64, "PXEClient:Arch:00007", "")
	if _, _, err := BuildOffer(req, cfg); err != nil {
		t.Fatalf("BuildOffer: %v", err)
	}
	if len(seen) != 1 {
		t.Fatalf("want 1 sighting, got %d (%v)", len(seen), seen)
	}
	if !strings.Contains(seen[0], "x86_64 UEFI") {
		t.Errorf("sighting missing arch label: %q", seen[0])
	}

	// iPXE-stage request from the same MAC must NOT add a sighting.
	seen = nil
	reqIPXE := newDiscover(t, "52:54:00:ab:cd:ef", iana.EFI_X86_64, "PXEClient", "iPXE")
	if _, _, err := BuildOffer(reqIPXE, cfg); err != nil {
		t.Fatalf("BuildOffer iPXE: %v", err)
	}
	if len(seen) != 0 {
		t.Errorf("iPXE-stage should not be sighted, got %v", seen)
	}
}
