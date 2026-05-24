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
func RenderDispatch(f *fleet.Fleet, ctx DispatchContext) []byte {
	var buf bytes.Buffer
	w := func(s string) { buf.WriteString(s); buf.WriteByte('\n') }

	w("#!ipxe")
	w("# pxe-beacon dispatch — generated per request from fleet.yaml.")
	w("# Each machine matches by MAC and kernel-boots its target OS")
	w("# directly. No HTTP chain dependency.")
	w("")
	w("echo ==============================================")
	w("echo pxe-beacon dispatch (v0.5.5)")
	w("echo   net0/mac       = ${net0/mac}")
	w("echo   net0/mac:hxhyp = ${net0/mac:hexhyp}")
	w("echo   net1/mac:hxhyp = ${net1/mac:hexhyp}")
	w("echo ==============================================")
	w("")
	// v0.5.5: phone-home FIRST — no dhcp prerequisite. iPXE inherits
	// the firmware DHCP state (it just used it to TFTP autoexec.ipxe
	// from us), so HTTP to the same pxe-beacon address works without
	// re-running dhcp. v0.5.4 put dhcp before the chain and got stuck
	// in 30s+ of iPXE DHCP-retry before the phone-home fired.
	fmt.Fprintf(&buf, "echo pxe-beacon: phoning home to %s:%d/debug/iPXE-state\n",
		ctx.AdvertisedIP, ctx.HTTPPort)
	fmt.Fprintf(&buf,
		"chain --autofree http://%s:%d/debug/iPXE-state?stage=before-iseq&net0=${net0/mac:hexhyp}&net1=${net1/mac:hexhyp}&net2=${net2/mac:hexhyp}&net3=${net3/mac:hexhyp}&ip=${ip}&gateway=${gateway}&dns=${dns}&platform=${platform}&buildarch=${buildarch}&version=${version} || echo pxe-beacon: phone-home failed (HTTP to pxe-beacon unreachable from this NIC)\n",
		ctx.AdvertisedIP, ctx.HTTPPort)
	w("")

	machines := []fleet.Machine{}
	if f != nil {
		for mac, p := range f.Machines() {
			machines = append(machines, fleet.Machine{MAC: mac, Profile: p})
		}
	}
	// Stable order — sort by MAC for diff-ability.
	sort.Slice(machines, func(i, j int) bool { return machines[i].MAC < machines[j].MAC })

	// Dispatch table. We compare against each NIC iPXE has discovered
	// (net0..net3). The dropped `${mac:hexhyp}` variant from v0.5.2
	// turned out to not be a standard iPXE setting in some builds —
	// it expanded to empty and the iseq fell through silently or
	// parse-errored. The /debug/iPXE-state phone-home logs which
	// variant actually carries the booting MAC.
	w("# ----- per-MAC dispatch (multi-NIC safe via net0..net3) -----")
	for _, m := range machines {
		label := labelOf(m.MAC, m.Profile.Name)
		hyp := strings.ReplaceAll(m.MAC, ":", "-")
		for _, nic := range []string{"net0/mac:hexhyp", "net1/mac:hexhyp", "net2/mac:hexhyp", "net3/mac:hexhyp"} {
			fmt.Fprintf(&buf, "iseq ${%s} %s && goto %s ||\n", nic, hyp, label)
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
	w("echo pxe-beacon: if you EXPECTED a match, check that fleet.yaml's mac matches ${mac:hexhyp} above")
	w("sleep 8")
	w("dhcp || goto target_default_fail_dhcp")
	w("echo pxe-beacon: chaining https://boot.netboot.xyz/menu.ipxe ...")
	w("chain --replace --autofree https://boot.netboot.xyz/menu.ipxe || goto target_default_fail_chain")
	// Error blocks. chain success replaces iPXE (never returns), so
	// fallthrough only happens on goto.
	w("")
	w(":target_default_fail_dhcp")
	w("echo pxe-beacon: DHCP FAILED in default arm — rebooting in 30s")
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

	// DHCP: needed for kernel/initrd HTTP fetch. (PXE expert fix #1.)
	fmt.Fprintf(buf, "dhcp || goto %s_fail_dhcp\n", label)
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

// labelOf produces a stable iPXE label for a machine. iPXE labels are
// ASCII, no spaces. Use "m_" prefix + name (if it parses) or MAC-hyp
// fallback.
func labelOf(mac, name string) string {
	id := name
	if id == "" {
		id = strings.ReplaceAll(mac, ":", "-")
	}
	// iPXE labels: alnum + underscore + dot + dash. Sanitize.
	out := make([]byte, 0, len(id)+2)
	out = append(out, 'm', '_')
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '_' || c == '-' || c == '.':
			out = append(out, c)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}
