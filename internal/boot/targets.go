// Package boot exposes per-OS boot-target metadata and renders the
// autoexec.ipxe iPXE script for a given machine.
//
// The autoexec.ipxe content depends on the target (menu, ubuntu-XX,
// debian-12, custom). For built-in targets the iPXE template is
// embedded under internal/assets/scripts/autoexec/; for `custom` the
// raw script comes from the fleet config (an operator-supplied file).
package boot

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"text/template"

	"github.com/venkatamutyala/pxe-beacon/internal/assets"
)

// RenderContext is the set of template variables exposed to embedded
// autoexec.ipxe scripts. Stable shape — if you add a field, every
// template can use it but old templates won't break.
type RenderContext struct {
	// Name is the operator-friendly machine name from fleet.yaml.
	// Falls back to MAC for unconfigured machines.
	Name string
	// MAC is the canonical colon-separated lowercase MAC.
	MAC string
	// MACHyp is the hyphen-separated form, used in URL path segments
	// because iPXE's ${net0/mac:hexhyp} formatter produces this form
	// and colons in URL paths are clunky.
	MACHyp string
	// AdvertisedIP is the IP pxe-beacon advertises to clients.
	AdvertisedIP string
	// HTTPPort is the pxe-beacon HTTP port.
	HTTPPort int
}

// IsBuiltIn reports whether `target` is one of pxe-beacon's bundled
// autoexec templates. (Custom and unknown targets aren't.)
func IsBuiltIn(target string) bool {
	switch target {
	case "menu", "ubuntu-22.04", "ubuntu-24.04", "debian-12", "debian-13":
		return true
	}
	return false
}

// RenderAutoexec returns the autoexec.ipxe content for a target +
// per-machine render context. For built-in targets the embedded
// template is rendered. For "custom", the caller must read the
// operator's file separately (see RenderCustom). Returns the bytes
// the HTTP handler will send back to iPXE.
func RenderAutoexec(target string, ctx RenderContext) ([]byte, error) {
	if !IsBuiltIn(target) {
		return nil, fmt.Errorf("not a built-in target: %q (use RenderCustom for boot=custom)", target)
	}
	raw, err := assets.ReadAutoexec(target)
	if err != nil {
		return nil, err
	}
	tmpl, err := template.New("autoexec-" + target).Parse(string(raw))
	if err != nil {
		return nil, fmt.Errorf("parse autoexec/%s.ipxe: %w", target, err)
	}
	if ctx.Name == "" {
		ctx.Name = ctx.MAC
	}
	if ctx.MACHyp == "" {
		ctx.MACHyp = strings.ReplaceAll(ctx.MAC, ":", "-")
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, ctx); err != nil {
		return nil, fmt.Errorf("render autoexec/%s.ipxe: %w", target, err)
	}
	return buf.Bytes(), nil
}

// RenderCustom reads an operator-supplied iPXE script verbatim
// (after running it through text/template so the same vars are
// available — e.g. {{.AdvertisedIP}} or {{.HTTPPort}}).
func RenderCustom(path string, ctx RenderContext) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read custom ipxe_script %s: %w", path, err)
	}
	tmpl, err := template.New("custom-ipxe").Parse(string(raw))
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if ctx.Name == "" {
		ctx.Name = ctx.MAC
	}
	if ctx.MACHyp == "" {
		ctx.MACHyp = strings.ReplaceAll(ctx.MAC, ":", "-")
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, ctx); err != nil {
		return nil, fmt.Errorf("render %s: %w", path, err)
	}
	return buf.Bytes(), nil
}

// RedirectorScript is the iPXE script served via TFTP as
// autoexec.ipxe. It's a one-shot chain to our HTTP per-MAC endpoint.
// iPXE substitutes ${net0/mac:hexhyp} at runtime, so the same TFTP
// bytes work for every client.
//
// Why TFTP can't do per-MAC dispatch itself: pin/tftp doesn't expose
// the remote address in the read handler, and the MAC isn't in the
// TFTP RRQ packet. iPXE's variable substitution gives us a clean
// per-MAC dispatch via the chained HTTP fetch.
//
// IMPORTANT — autoexec.ipxe runs BEFORE iPXE's embed.ipxe runs the
// `dhcp` command. If we don't acquire networking here first, the
// HTTP chain to our server silently fails (iPXE has no IP yet)
// and iPXE falls through to its embedded netboot.xyz chain. This
// was the bug behind "v0.4.0 still loaded netboot.xyz" — a real
// PXE boot has UEFI's SNP already configured by the firmware, but
// iPXE itself needs `dhcp` to pull that config into its own state.
func RedirectorScript(advertisedIP string, httpPort int) []byte {
	return []byte(fmt.Sprintf(`#!ipxe
# pxe-beacon: per-machine override redirector. iPXE substitutes the
# client's MAC into the URL below and fetches the per-machine
# autoexec.ipxe from our HTTP server, which does the real dispatch.
#
# DHCP first — autoexec.ipxe runs before iPXE's embed.ipxe runs
# its own dhcp, so we need to acquire networking ourselves before
# chaining HTTP. Without this the chain silently fails and iPXE
# falls back to its embedded netboot.xyz chain (this was the v0.4.0
# bug).
dhcp || echo pxe-beacon: dhcp failed in autoexec; falling back to embed
chain --replace --autofree http://%s:%d/autoinstall/${net0/mac:hexhyp}/autoexec.ipxe || \
echo pxe-beacon: HTTP override unreachable; falling back to embed
exit
`, advertisedIP, httpPort))
}
