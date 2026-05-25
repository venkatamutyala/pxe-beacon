package proxydhcp

import (
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/iana"

	"github.com/venkatamutyala/pxe-beacon/internal/fleet"
)

// Config carries the deployment-specific knobs BuildOffer needs. It is
// immutable for the lifetime of a request — listener.go fills it in
// from CLI flags and netinfo detection at startup.
type Config struct {
	// AdvertisedIP is the IPv4 address the OFFER points clients at.
	// This is the IP firmware will TFTP/HTTP from. Must be reachable
	// from the client (same L2 segment for proxyDHCP).
	AdvertisedIP net.IP
	// HTTPPort is the port the pxe-beacon HTTP server listens on.
	HTTPPort int
	// IPXEScriptPath is the URL path the iPXE-stage client should
	// fetch (the chain script). Conventionally "/boot.ipxe".
	IPXEScriptPath string
	// IPXEUserClass is the user-class string we treat as "this is
	// iPXE asking for its script, not firmware asking for a binary".
	// Defaults to "iPXE" if zero.
	IPXEUserClass string
	// Fleet provides per-MAC profile lookup so BuildOffer can enrich
	// the Decision (and the narrated log line) with the operator's
	// machine name. May be nil — when nil, every MAC is treated as
	// unknown and gets the v0.1.3 "menu" defaults.
	//
	// Note: the OFFER itself does NOT change shape per-MAC in v0.2 —
	// netboot.xyz's iPXE ignores the OFFER's bootfile and uses its
	// own embedded chain. Per-MAC dispatch happens via the
	// autoexec.ipxe HTTP route. Fleet here is for logging + future
	// non-netboot.xyz iPXE builds that DO honor the OFFER bootfile.
	Fleet *fleet.Fleet
	// Pending, when non-nil, is consulted for every fleet-known MAC.
	// If it returns false (no pending action), BuildOffer skips OFFER
	// (SkipNoPendingAction) and the client's PXE firmware falls
	// through to its next boot device — usually local disk. Unknown
	// MACs (no fleet entry) bypass this check entirely; they keep the
	// netboot.xyz fallback behavior so "I just want to PXE-boot a
	// random box" still works. v0.7.1+ (was Armed in v0.7.0).
	//
	// v0.8.1: consulted ONLY in the firmware stage. iPXE-stage
	// REQUESTs (userclass=iPXE) bypass — otherwise we strand iPXE
	// mid-chainload if intent is cancelled mid-install.
	Pending func(mac string) bool
	// LastEvent, when non-nil, returns the most recent fleet-tracker
	// event for the MAC (or "" if never seen). BuildOffer uses it to
	// implement the already-installed guard: a fleet-known MAC whose
	// LastEvent is EventInstallerDone AND has no pending intent is
	// silently skipped, so a stray BIOS-order rotation can't trigger
	// an accidental reinstall. Operator's explicit PUT /intent re-arms
	// (pending becomes non-nil; guard does not fire). v0.8.1+.
	LastEvent func(mac string) fleet.Event
	// NoteSighting, when non-nil, is called once per firmware-stage
	// DISCOVER from a MAC NOT in the fleet, so the discovery feed can
	// surface unknown machines for one-click enrollment. archLabel is a
	// human arch string; vendorClass is the raw DHCP option-60. Known
	// MACs and iPXE-stage requests are never reported. v0.13.0+.
	NoteSighting func(mac, archLabel, vendorClass string)
}

// defaultUserClass returns the configured iPXE user class, defaulting
// to "iPXE" as defined by iPXE itself.
func (c Config) defaultUserClass() string {
	if c.IPXEUserClass == "" {
		return "iPXE"
	}
	return c.IPXEUserClass
}

// Stage names — kept as string consts so logs are searchable and
// stable across versions.
const (
	StageFirmwareTFTP = "firmware-TFTP"
	StageFirmwareHTTP = "firmware-HTTP"
	StageIPXEScript   = "iPXE-script"
	StageSkip         = "skip"
)

// SkipKind disambiguates the "we did not OFFER" outcomes. Every skip
// kind has an explanation so the logger can render the right message.
type SkipKind int

const (
	NotSkipped SkipKind = iota
	SkipNotPXE
	SkipUnsupportedMessageType
	SkipMissingArch
	// SkipNoPendingAction indicates a known fleet member with no
	// pending deploy/rescue action. The client falls through to
	// local-disk boot. v0.7.1+ (was SkipDisarmed in v0.7.0).
	SkipNoPendingAction
	// SkipAlreadyDeployed indicates a known fleet member whose last
	// recorded event is EventInstallerDone, with no pending intent.
	// Protects a running production box from accidental reinstall
	// if its BIOS boot order ever rotates to PXE. v0.8.1+.
	SkipAlreadyDeployed
)

// Decision describes everything BuildOffer concluded about a request.
// It exists so the listener can write a single decision-level log
// line that names the stage, the parsed options, and the action.
type Decision struct {
	ClientMAC    string
	VendorClass  string
	UserClass    string
	Archs        []iana.Arch
	SelectedArch iana.Arch
	Stage        string
	Transport    Transport
	BootFile     string // option 67 value (filename or URL)
	NextServer   string // option 66 / siaddr
	Skip         SkipKind
	SkipReason   string // human-readable; benign when SkipKind is SkipNotPXE
	UnknownArch  bool   // we fell back because option 93 was unrecognized
	IsIPXEStage  bool   // user-class said "iPXE"
	// MachineName is the operator-friendly name from fleet.yaml.
	// Empty when the MAC isn't configured or no fleet is loaded.
	MachineName string
	// BootTarget is the fleet-configured boot target ("menu",
	// "ubuntu-22.04", etc.). Defaults to "menu" for unknown MACs.
	BootTarget string
}

// IsBenignSkip reports whether the skip is the expected
// "post-handoff, not actually a PXE conversation" case. The PLAN
// says these MUST be labelled benign so users don't chase them.
func (d Decision) IsBenignSkip() bool {
	return d.Skip == SkipNotPXE
}

// ErrSkip is returned by BuildOffer when no OFFER should be sent. The
// Decision still carries the diagnostic so the caller can log it.
var ErrSkip = errors.New("proxydhcp: skip request")

// BuildOffer is the *pure* core of pxe-beacon. Given a parsed
// DHCPv4 request and immutable config, it produces a populated reply
// (or ErrSkip) along with a Decision describing what it did and why.
//
// The function MUST NOT touch sockets. listener.go owns IO.
func BuildOffer(req *dhcpv4.DHCPv4, cfg Config) (*dhcpv4.DHCPv4, Decision, error) {
	d := Decision{}

	if req == nil {
		return nil, d, fmt.Errorf("nil request")
	}
	if cfg.AdvertisedIP == nil || cfg.AdvertisedIP.To4() == nil {
		return nil, d, fmt.Errorf("config: AdvertisedIP must be set to an IPv4 address")
	}
	if cfg.HTTPPort <= 0 || cfg.HTTPPort > 65535 {
		return nil, d, fmt.Errorf("config: HTTPPort %d invalid", cfg.HTTPPort)
	}

	d.ClientMAC = req.ClientHWAddr.String()
	d.VendorClass = req.ClassIdentifier()
	if uc := req.UserClass(); len(uc) > 0 {
		d.UserClass = strings.Join(uc, ",")
	}
	d.Archs = req.ClientArch()

	// Resolve the per-MAC fleet profile. Cheap; safe for nil Fleet.
	d.BootTarget = "menu"
	machineKnown := false
	if cfg.Fleet != nil {
		p := cfg.Fleet.Lookup(d.ClientMAC)
		d.MachineName = p.Name
		d.BootTarget = p.Boot
		machineKnown = p.Name != ""
	}

	// We respond to DISCOVER (initial broadcast on 67) and REQUEST
	// (unicast on 4011 used by some firmware after picking an OFFER).
	mt := req.MessageType()
	switch mt {
	case dhcpv4.MessageTypeDiscover, dhcpv4.MessageTypeRequest:
		// proceed
	default:
		d.Stage = StageSkip
		d.Skip = SkipUnsupportedMessageType
		d.SkipReason = fmt.Sprintf("not DISCOVER/REQUEST (got %s)", mt)
		return nil, d, ErrSkip
	}

	// Option 60 vendor-class check. Per PLAN section 0, post-handoff
	// packets without option 60 must be labelled benign, not error.
	// Standard PXE clients put "PXEClient" (sometimes with a suffix);
	// some firmware also uses "HTTPClient" for UEFI HTTP boot.
	if !isPXEVendorClass(d.VendorClass) {
		d.Stage = StageSkip
		d.Skip = SkipNotPXE
		d.SkipReason = "missing or non-PXE vendor class (option 60) — client likely already handed off to iPXE"
		return nil, d, ErrSkip
	}

	// User-class detection — option 77. Once iPXE has chainloaded and
	// is doing its own DHCP, it sets userclass="iPXE". We then serve
	// the script, not the binary. This is what prevents the
	// chainload loop the PLAN warns about.
	d.IsIPXEStage = userClassContains(req.UserClass(), cfg.defaultUserClass())

	// Build the base reply: copy XID, hardware addr, etc.; mirror the
	// request's DHCP state — DISCOVER → OFFER, REQUEST → ACK. UEFI
	// firmware tolerates either, but strict iPXE clients drop our
	// reply silently if we OFFER in response to their REQUEST (seen
	// on the wire: iPXE retries 6× then gives up). We also send the
	// server-identifier and (for proxyDHCP) the PXEClient class so
	// the client recognizes us as the boot instructor.
	replyMT := dhcpv4.MessageTypeOffer
	if mt == dhcpv4.MessageTypeRequest {
		replyMT = dhcpv4.MessageTypeAck
	}
	reply, err := dhcpv4.NewReplyFromRequest(req,
		dhcpv4.WithMessageType(replyMT),
		dhcpv4.WithOption(dhcpv4.OptServerIdentifier(cfg.AdvertisedIP.To4())),
		dhcpv4.WithOption(dhcpv4.OptClassIdentifier("PXEClient")),
	)
	if err != nil {
		return nil, d, fmt.Errorf("build reply: %w", err)
	}
	// proxyDHCP does NOT assign an IP — zero yiaddr explicitly in
	// case the library default ever changes.
	reply.YourIPAddr = net.IPv4zero

	if d.IsIPXEStage {
		// iPXE-stage: hand it the chain script over HTTP. Arch is
		// largely irrelevant here because iPXE itself does the HTTP
		// fetch — it just needs the URL.
		//
		// v0.8.1: iPXE-stage REQUESTs are NOT subject to the pending
		// or already-installed guards. If we don't OFFER here we
		// strand iPXE mid-chainload — a previously-OK install that
		// the operator just cancelled would brick. The firmware-stage
		// guards below already prevented the box from entering iPXE
		// in the first place; if iPXE is alive, let it finish.
		scriptURL := fmt.Sprintf("http://%s:%d%s",
			cfg.AdvertisedIP.String(), cfg.HTTPPort, cfg.IPXEScriptPath)
		reply.BootFileName = scriptURL
		reply.UpdateOption(dhcpv4.OptBootFileName(scriptURL))
		// siaddr/TFTP-server are unset; iPXE uses the URL form in BootFileName.
		d.Stage = StageIPXEScript
		d.Transport = TransportHTTP
		d.BootFile = scriptURL
		d.NextServer = cfg.AdvertisedIP.String()
		d.SelectedArch = selectArch(d.Archs)
		return reply, d, nil
	}

	// Firmware stage: per-fleet-member gating.
	//
	// v0.8.1: Two gates, in this order, before any arch-specific work:
	//   (a) pending-action check — operator must have queued an
	//       install or rescue intent via PUT /api/v1/machines/{mac}/intent.
	//   (b) already-installed guard — if the last observed event is
	//       installer-done AND there's no pending intent (or pending
	//       is install — operator's explicit re-arm wins), skip OFFER.
	//
	// Unknown MACs (machineKnown == false) bypass both — they get
	// the netboot.xyz fallback as in <= v0.6.x. Both gates apply only
	// to firmware-stage; iPXE-stage was already handled above.
	if machineKnown {
		hasPending := cfg.Pending != nil && cfg.Pending(d.ClientMAC)
		if !hasPending {
			// No pending intent. Two reasons we'd skip the OFFER;
			// prefer the MORE SPECIFIC one for clearer operator log
			// messages: already-installed > generic no-pending.
			//
			// Both reasons require the operator's explicit PUT
			// /intent to re-arm — pending of ANY kind (install or
			// rescue) overrides both guards.
			if cfg.LastEvent != nil && cfg.LastEvent(d.ClientMAC) == fleet.EventInstallerDone {
				d.Stage = StageSkip
				d.Skip = SkipAlreadyDeployed
				d.SkipReason = fmt.Sprintf("%s (%s) already reached installer-done — PUT /api/v1/machines/%s/intent {\"action\":\"install\"} to re-arm",
					d.MachineName, d.ClientMAC, d.ClientMAC)
				return nil, d, ErrSkip
			}
			if cfg.Pending != nil {
				d.Stage = StageSkip
				d.Skip = SkipNoPendingAction
				d.SkipReason = fmt.Sprintf("%s (%s) has no pending action — PUT /api/v1/machines/%s/intent {\"action\":\"install\"} to queue one",
					d.MachineName, d.ClientMAC, d.ClientMAC)
				return nil, d, ErrSkip
			}
		}
	}

	// Firmware stage: arch dictates transport + which binary.
	if len(d.Archs) == 0 {
		// Some legacy BIOS clients omit option 93 entirely. PXE
		// convention is to assume INTEL_X86PC (0x00) in that case.
		d.Archs = []iana.Arch{iana.INTEL_X86PC}
		d.SkipReason = ""
	}
	d.SelectedArch = selectArch(d.Archs)
	profile, ok := LookupArch(d.SelectedArch)
	d.UnknownArch = !ok
	d.Transport = profile.Transport
	d.BootFile = profile.BootFile

	// v0.13.0: record unknown MACs for the discovery feed. Firmware
	// stage only (iPXE-stage returned earlier), so one fire per
	// firmware DISCOVER; the sightings store dedups retries by MAC.
	if !machineKnown && cfg.NoteSighting != nil {
		cfg.NoteSighting(d.ClientMAC, ArchLabel(d.SelectedArch), d.VendorClass)
	}

	switch profile.Transport {
	case TransportTFTP:
		// Set option 66 (TFTP server name) AND siaddr (next-server).
		// Some firmware reads one, some the other; setting both is
		// what every reference implementation does.
		reply.ServerIPAddr = cfg.AdvertisedIP.To4()
		reply.BootFileName = profile.BootFile
		reply.UpdateOption(dhcpv4.OptTFTPServerName(cfg.AdvertisedIP.String()))
		reply.UpdateOption(dhcpv4.OptBootFileName(profile.BootFile))
		d.NextServer = cfg.AdvertisedIP.String()
		d.Stage = StageFirmwareTFTP
	case TransportHTTP:
		// UEFI HTTP boot wants a full URL in option 67. Some
		// implementations also key off the vendor-class being
		// "HTTPClient" rather than "PXEClient"; we set both
		// liberally so the firmware actually loads it.
		url := fmt.Sprintf("http://%s:%d/%s",
			cfg.AdvertisedIP.String(), cfg.HTTPPort, profile.BootFile)
		reply.BootFileName = url
		reply.UpdateOption(dhcpv4.OptBootFileName(url))
		reply.UpdateOption(dhcpv4.OptClassIdentifier("HTTPClient"))
		d.NextServer = cfg.AdvertisedIP.String()
		d.BootFile = url
		d.Stage = StageFirmwareHTTP
	default:
		return nil, d, fmt.Errorf("internal: arch profile has unknown transport")
	}

	return reply, d, nil
}

// isPXEVendorClass returns whether the option-60 string identifies
// this as a PXE conversation. Real-world strings observed: "PXEClient",
// "PXEClient:Arch:00007:UNDI:003016", "HTTPClient:Arch:00016:UNDI:003016".
func isPXEVendorClass(s string) bool {
	if s == "" {
		return false
	}
	return strings.HasPrefix(s, "PXEClient") || strings.HasPrefix(s, "HTTPClient")
}

// userClassContains looks for a case-sensitive match against the
// configured iPXE user-class. iPXE sets it to literally "iPXE".
func userClassContains(uc []string, want string) bool {
	for _, v := range uc {
		if v == want {
			return true
		}
	}
	return false
}

// selectArch picks one arch from the option-93 list. RFC 4578 says
// the client lists them in preference order, so we honor the first.
func selectArch(archs []iana.Arch) iana.Arch {
	if len(archs) == 0 {
		return iana.INTEL_X86PC
	}
	return archs[0]
}
