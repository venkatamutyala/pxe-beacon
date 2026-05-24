package boot

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"github.com/venkatamutyala/pxe-beacon/internal/fleet"
)

// DispatchContext is the global render context for the dispatch
// script. (Per-MAC values are templated into individual blocks via
// each machine's profile.)
type DispatchContext struct {
	AdvertisedIP string
	HTTPPort     int
	// ClientNetmask, if set, is emitted as `set net0/netmask <value>`
	// in each matched arm right after dhcp. Use when pxe-beacon and
	// the PXE client are on different L3 subnets that share L2 (e.g.
	// Mac on Wi-Fi and client on wired LAN behind the same router).
	// Widening to e.g. 255.255.0.0 makes iPXE treat pxe-beacon's IP
	// as local and use direct ARP/L2 instead of going through the
	// gateway. v0.5.11+.
	ClientNetmask string
}

// RenderDispatch generates the full TFTP-served autoexec.ipxe for a
// fleet. The script does per-MAC dispatch via iPXE's `iseq` + `goto`
// using `${net0/mac:hexhyp}` as the discriminator, then kernel-boots
// directly with the right cmdline per OS target. This bypasses the
// HTTP autoexec.ipxe chain step entirely — iPXE reads one TFTP file
// and goes straight to kernel.
//
// Per the PXE-expert review:
//   - `${net0/mac:hexhyp}` IS populated pre-DHCP (hardware attr), so
//     the iseq dispatch works without networking.
//   - The kernel fetch DOES need IP+DNS, so each `:m_*` block opens
//     with `dhcp` before `kernel`.
//   - d-i kernel/initrd are fetched over plain HTTP (snponly iPXE's
//     TLS state isn't reliable).
//   - Each block emits `echo` narration so a failed kernel fetch
//     doesn't leave the operator at a context-less iPXE shell.
//   - A `sleep 3` precedes `shell` so the error message stays on
//     screen on boards that clear on shell entry.
//
// RenderDispatch generates the per-MAC dispatch autoexec.ipxe script.
// v0.5.11 restores the production dispatch after v0.5.9/v0.5.10's
// diagnostics confirmed iPXE can both run our script AND reach
// pxe-beacon via the IPv4-bound listeners.
func RenderDispatch(f *fleet.Fleet, ctx DispatchContext) []byte {
	return renderDispatchProduction(f, ctx)
}

// renderDispatchProduction is the v0.5.0..v0.5.8 dispatch logic,
// retained here so we can restore it in v0.5.10 once the v0.5.9
// diagnostic settles the autoexec-is-or-is-not-running question.
func renderDispatchProduction(f *fleet.Fleet, ctx DispatchContext) []byte {
	var buf bytes.Buffer
	w := func(s string) { buf.WriteString(s); buf.WriteByte('\n') }

	w("#!ipxe")
	w("# pxe-beacon dispatch — generated per request from fleet.yaml.")
	w("# Each machine matches by MAC and kernel-boots its target OS")
	w("# directly. No HTTP chain dependency.")
	w("")
	w("echo ==============================================")
	w("echo pxe-beacon dispatch (v0.5.13)")
	w("echo   net0/mac       = ${net0/mac}")
	w("echo   net0/mac:hxhyp = ${net0/mac:hexhyp}")
	w("echo ==============================================")
	w("")
	// v0.5.16: 30-second sleep at the top of the script as a
	// diagnostic for the autoboot-vs-shell-chain mismatch. venkat@'s
	// shell test (manually `dhcp`+`set netmask`+`chain autoexec.ipxe`)
	// boots Debian fine, but autoboot of the same script lands at
	// netboot.xyz menu. Likely iPXE's network isn't fully initialized
	// when autoexec.ipxe first runs in autoboot context. Sleeping
	// before any network operation lets it settle. If 30s works we
	// dial back to a more reasonable value in a follow-up.
	w("echo pxe-beacon: settling 30s before network operations (autoboot timing diagnostic)")
	w("sleep 30")
	// v0.5.13: dhcp + optional netmask widening at the TOP of the
	// script, BEFORE iseq dispatch and BEFORE any chain to pxe-beacon.
	// All matched arms inherit the resulting network state — no need
	// to dhcp again per-arm.
	w("dhcp || goto top_fail_dhcp")
	if ctx.ClientNetmask != "" {
		fmt.Fprintf(&buf, "set net0/netmask %s\n", ctx.ClientNetmask)
		fmt.Fprintf(&buf, "echo pxe-beacon: widened net0/netmask to %s for cross-subnet routing\n", ctx.ClientNetmask)
	}
	w("")

	machines := []fleet.Machine{}
	if f != nil {
		for mac, p := range f.Machines() {
			machines = append(machines, fleet.Machine{MAC: mac, Profile: p})
		}
	}
	// Stable order — sort by MAC for diff-ability.
	sort.Slice(machines, func(i, j int) bool { return machines[i].MAC < machines[j].MAC })

	// v0.5.14: dispatch table — one iseq per line, NO chained ||.
	// Multi-line `iseq A B && goto X || iseq C D && goto Y || ...`
	// does NOT work in iPXE the way bash precedence would suggest.
	// Confirmed by venkat@'s shell test: `iseq abc abc && echo P || echo F`
	// works fine in isolation, but my chained form fell through on
	// the very first iseq. Switching to plain one-per-line: if iseq
	// succeeds, && goto jumps; if iseq fails, goto skipped, parser
	// moves to next statement. Falls through to `goto target_default`
	// at the bottom only when all iseqs failed.
	w("# ----- per-MAC dispatch (one iseq per line; covers net0..net3) -----")
	for _, m := range machines {
		label := labelOf(m.MAC, m.Profile.Name)
		hyp := strings.ReplaceAll(m.MAC, ":", "-")
		for _, nic := range []string{"net0/mac:hexhyp", "net1/mac:hexhyp", "net2/mac:hexhyp", "net3/mac:hexhyp"} {
			fmt.Fprintf(&buf, "iseq ${%s} %s && goto %s\n", nic, hyp, label)
		}
	}
	w("goto target_default")
	w("")

	// Per-MAC blocks.
	for _, m := range machines {
		writeMachineBlock(&buf, m, ctx)
	}

	// Default arm — fall back to netboot.xyz embed.
	w("# ----- default arm: machine not in fleet.yaml (or iseq did not match) -----")
	w(":target_default")
	w("echo pxe-beacon: NO MATCH for ${net0/mac:hexhyp} in fleet.yaml")
	w("echo pxe-beacon: if you EXPECTED a match, check that fleet.yaml's mac matches ${net0/mac:hexhyp} above")
	w("sleep 8")
	w("echo pxe-beacon: chaining https://boot.netboot.xyz/menu.ipxe ...")
	w("chain --replace --autofree https://boot.netboot.xyz/menu.ipxe || goto target_default_fail_chain")
	w("")
	// Top-level fail labels — reached if the top-of-script dhcp itself
	// failed. Each machine block goto's its own labeled fail blocks.
	w(":top_fail_dhcp")
	w("echo pxe-beacon: TOP-LEVEL DHCP FAILED — no IP, cannot continue; rebooting in 30s")
	w("sleep 30")
	w("reboot")
	w("")
	w(":target_default_fail_chain")
	w("echo pxe-beacon: netboot.xyz CHAIN FAILED — check HTTPS reachability; rebooting in 30s")
	w("sleep 30")
	w("reboot")

	return buf.Bytes()
}

func writeMachineBlock(buf *bytes.Buffer, m fleet.Machine, ctx DispatchContext) {
	w := func(s string) { buf.WriteString(s); buf.WriteByte('\n') }
	label := labelOf(m.MAC, m.Profile.Name)
	name := m.Profile.Name
	if name == "" {
		name = m.MAC
	}
	preseedURL := fmt.Sprintf("http://%s:%d/autoinstall/%s/preseed.cfg",
		ctx.AdvertisedIP, ctx.HTTPPort,
		strings.ReplaceAll(m.MAC, ":", "-"))
	autoinstallBase := fmt.Sprintf("http://%s:%d/autoinstall/%s/",
		ctx.AdvertisedIP, ctx.HTTPPort,
		strings.ReplaceAll(m.MAC, ":", "-"))
	assetsBase := func(target string) string {
		return fmt.Sprintf("http://%s:%d/assets/%s", ctx.AdvertisedIP, ctx.HTTPPort, target)
	}
	consoleArgs := "console=tty0 console=ttyS0,115200n8"

	w("")
	fmt.Fprintf(buf, ":%s\n", label)
	fmt.Fprintf(buf, "echo pxe-beacon: %s (%s) -> %s\n", name, m.MAC, m.Profile.Boot)

	// v0.5.3: control flow uses explicit goto labels for every error
	// branch. The previous form `cmd || echo X && sleep N && reboot`
	// was a precedence trap — iPXE/bash evaluate it as
	// `(cmd || echo) && sleep && reboot`, so `sleep + reboot` fires
	// after `cmd` SUCCEEDS too, putting the box in a reboot loop that
	// never reaches the kernel.

	// v0.5.13: dhcp + netmask widening are now done ONCE at the top
	// of the script. Doing them again here would overwrite the
	// widened netmask with the DHCP-supplied /24, breaking the
	// cross-subnet fix.
	w("imgfree")

	switch m.Profile.Boot {
	case "debian-12":
		mirror := "http://deb.debian.org/debian/dists/bookworm/main/installer-amd64/current/images/netboot/debian-installer/amd64"
		fmt.Fprintf(buf, "echo pxe-beacon: ip=${ip} gw=${gateway} dns=${dns}\n")
		fmt.Fprintf(buf, "echo pxe-beacon: fetching Debian 12 d-i kernel: %s/linux\n", mirror)
		fmt.Fprintf(buf,
			"kernel --name linux %s/linux auto=true priority=critical ip=dhcp url=%s %s --- || goto %s_fail_kernel\n",
			mirror, preseedURL, consoleArgs, label)
		fmt.Fprintf(buf, "echo pxe-beacon: fetching initrd: %s/initrd.gz\n", mirror)
		fmt.Fprintf(buf, "initrd --name initrd.gz %s/initrd.gz || goto %s_fail_initrd\n", mirror, label)
		w("echo pxe-beacon: handing control to d-i (boot)...")
		fmt.Fprintf(buf, "boot || goto %s_fail_boot\n", label)
		writeMachineErrorBlocks(buf, label, m.Profile.Boot, mirror)

	case "debian-13":
		mirror := "http://deb.debian.org/debian/dists/trixie/main/installer-amd64/current/images/netboot/debian-installer/amd64"
		fmt.Fprintf(buf, "echo pxe-beacon: ip=${ip} gw=${gateway} dns=${dns}\n")
		fmt.Fprintf(buf, "echo pxe-beacon: fetching Debian 13 d-i kernel: %s/linux\n", mirror)
		fmt.Fprintf(buf,
			"kernel --name linux %s/linux auto=true priority=critical ip=dhcp url=%s %s --- || goto %s_fail_kernel\n",
			mirror, preseedURL, consoleArgs, label)
		fmt.Fprintf(buf, "echo pxe-beacon: fetching initrd: %s/initrd.gz\n", mirror)
		fmt.Fprintf(buf, "initrd --name initrd.gz %s/initrd.gz || goto %s_fail_initrd\n", mirror, label)
		w("echo pxe-beacon: handing control to d-i (boot)...")
		fmt.Fprintf(buf, "boot || goto %s_fail_boot\n", label)
		writeMachineErrorBlocks(buf, label, m.Profile.Boot, mirror)

	case "ubuntu-22.04", "ubuntu-24.04":
		assets := assetsBase(m.Profile.Boot)
		fmt.Fprintf(buf, "echo pxe-beacon: fetching Ubuntu %s kernel from %s/vmlinuz\n",
			strings.TrimPrefix(m.Profile.Boot, "ubuntu-"), assets)
		fmt.Fprintf(buf, "echo pxe-beacon: (requires `pxe-beacon fetch %s` to have populated data-dir)\n", m.Profile.Boot)
		// `autoinstall ---` separator is REQUIRED on 22.04.3+; without
		// it Subiquity prompts. Order: cmdline args, then ---, then
		// initrd.
		fmt.Fprintf(buf,
			"kernel --name vmlinuz %s/vmlinuz initrd=initrd ip=dhcp ipv6.disable=1 boot=casper url=%s/filesystem.squashfs %s autoinstall ds=nocloud-net\\;s=%s --- || goto %s_fail_kernel\n",
			assets, assets, consoleArgs, autoinstallBase, label)
		fmt.Fprintf(buf, "initrd --name initrd %s/initrd || goto %s_fail_initrd\n", assets, label)
		w("echo pxe-beacon: handing control to Subiquity (boot)...")
		fmt.Fprintf(buf, "boot || goto %s_fail_boot\n", label)
		writeMachineErrorBlocks(buf, label, m.Profile.Boot, assets)

	case "custom":
		// We can't inline an arbitrary operator script — but we CAN
		// chain to the HTTP route that serves the templated custom
		// script. Custom is the one path that legitimately needs HTTP
		// chain (no other way to deliver an operator-defined script).
		customURL := fmt.Sprintf("http://%s:%d/autoinstall/%s/autoexec.ipxe",
			ctx.AdvertisedIP, ctx.HTTPPort,
			strings.ReplaceAll(m.MAC, ":", "-"))
		fmt.Fprintf(buf, "echo pxe-beacon: chaining operator-supplied script %s\n", customURL)
		fmt.Fprintf(buf, "chain --replace --autofree %s || goto %s_fail_chain\n", customURL, label)
		// Error blocks for custom (only chain can fail; no kernel/initrd here).
		fmt.Fprintf(buf, "\n:%s_fail_dhcp\n", label)
		w("echo pxe-beacon: DHCP FAILED in custom arm — no IP, rebooting in 30s")
		w("sleep 30")
		w("reboot")
		fmt.Fprintf(buf, "\n:%s_fail_chain\n", label)
		w("echo pxe-beacon: custom script CHAIN FAILED — check HTTP reachability to operator URL")
		w("sleep 30")
		w("reboot")

	case "menu":
		fmt.Fprintf(buf, "echo pxe-beacon: chaining netboot.xyz menu\n")
		fmt.Fprintf(buf, "chain --replace --autofree https://boot.netboot.xyz/menu.ipxe || goto %s_fail_chain\n", label)
		fmt.Fprintf(buf, "\n:%s_fail_dhcp\n", label)
		w("echo pxe-beacon: DHCP FAILED in menu arm — no IP, rebooting in 30s")
		w("sleep 30")
		w("reboot")
		fmt.Fprintf(buf, "\n:%s_fail_chain\n", label)
		w("echo pxe-beacon: netboot.xyz menu CHAIN FAILED — check HTTPS reachability")
		w("sleep 30")
		w("reboot")

	default:
		fmt.Fprintf(buf, "echo pxe-beacon: unknown boot target %q for %s; falling through\n", m.Profile.Boot, name)
		w("goto target_default")
	}
}

// writeMachineErrorBlocks emits labeled error handlers for the
// dhcp/kernel/initrd/boot failure paths shared by debian-* and
// ubuntu-* targets. Each block ends in `reboot` which terminates
// the script — no fallthrough between blocks.
func writeMachineErrorBlocks(buf *bytes.Buffer, label, target, srcURL string) {
	w := func(s string) { buf.WriteString(s); buf.WriteByte('\n') }
	fmt.Fprintf(buf, "\n:%s_fail_dhcp\n", label)
	w("echo pxe-beacon: DHCP FAILED — iPXE could not get an IP, cannot fetch kernel")
	w("echo pxe-beacon: rebooting in 30s")
	w("sleep 30")
	w("reboot")

	fmt.Fprintf(buf, "\n:%s_fail_kernel\n", label)
	fmt.Fprintf(buf, "echo pxe-beacon: KERNEL FETCH FAILED for %s\n", target)
	fmt.Fprintf(buf, "echo pxe-beacon: could not reach %s/linux\n", srcURL)
	w("echo pxe-beacon: verify DNS + outbound HTTP from this NIC; rebooting in 30s")
	w("sleep 30")
	w("reboot")

	fmt.Fprintf(buf, "\n:%s_fail_initrd\n", label)
	fmt.Fprintf(buf, "echo pxe-beacon: INITRD FETCH FAILED for %s — kernel loaded but initrd did not\n", target)
	w("echo pxe-beacon: rebooting in 30s")
	w("sleep 30")
	w("reboot")

	fmt.Fprintf(buf, "\n:%s_fail_boot\n", label)
	fmt.Fprintf(buf, "echo pxe-beacon: BOOT FAILED for %s — kernel image rejected (cmdline / arch mismatch?)\n", target)
	w("echo pxe-beacon: rebooting in 30s")
	w("sleep 30")
	w("reboot")
}

// labelOf produces an iPXE-safe label for a machine. v0.5.15: confirmed
// by venkat@'s shell test that iPXE's goto silently no-ops on labels
// containing hyphens (`goto foo-label-that-does-not-exist` produces
// no error, execution falls through). Restrict labels to
// [a-zA-Z0-9_] only — replace every other character (including '-'
// and '.') with '_'.
func labelOf(mac, name string) string {
	id := name
	if id == "" {
		id = strings.ReplaceAll(mac, ":", "_")
	}
	out := make([]byte, 0, len(id)+2)
	out = append(out, 'm', '_')
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '_':
			out = append(out, c)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}
