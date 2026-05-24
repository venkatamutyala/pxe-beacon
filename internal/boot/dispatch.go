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
	w("# v0.6.5: verbose, screen-debuggable. Every stage transition is")
	w("# echoed with a ===== header, settings are dumped, and 2-second")
	w("# sleeps separate sections so a human at the console can see")
	w("# the boot story unfold.")
	w("")
	w("echo ============================================================")
	w("echo  pxe-beacon dispatch (v0.6.5) — verbose mode")
	w("echo ============================================================")
	w("echo")
	w("echo [stage 0/5] iPXE settings BEFORE dhcp")
	w("echo   net0/mac        = ${net0/mac}")
	w("echo   net0/mac:hexhyp = ${net0/mac:hexhyp}")
	w("echo   ip              = ${ip}")
	w("echo   netmask         = ${netmask}")
	w("echo   gateway         = ${gateway}")
	w("echo   dns             = ${dns}")
	w("echo   platform        = ${platform}")
	w("echo   buildarch       = ${buildarch}")
	w("sleep 2")
	w("")
	w("echo [stage 1/5] running dhcp...")
	w("dhcp || goto top_fail_dhcp")
	w("echo   dhcp ok. assigned:")
	w("echo     ip      = ${ip}")
	w("echo     netmask = ${netmask}")
	w("echo     gateway = ${gateway}")
	w("echo     dns     = ${dns}")
	w("sleep 2")
	w("")
	if ctx.ClientNetmask != "" {
		w("echo [stage 2/5] widening netmask for cross-subnet routing")
		fmt.Fprintf(&buf, "echo   from %s -> %s\n", "${netmask}", ctx.ClientNetmask)
		fmt.Fprintf(&buf, "set net0/netmask %s\n", ctx.ClientNetmask)
		w("echo   net0/netmask now: ${netmask}")
		w("sleep 2")
		w("")
	} else {
		w("echo [stage 2/5] no -client-netmask flag set; using DHCP-supplied netmask")
		w("sleep 1")
		w("")
	}

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
	w("echo [stage 3/5] per-MAC iseq dispatch")
	w("echo   comparing ${net0/mac:hexhyp} against fleet.yaml entries...")
	w("sleep 1")
	w("")
	w("# ----- per-MAC dispatch (one iseq per line; covers net0..net3) -----")
	for _, m := range machines {
		label := labelOf(m.MAC, m.Profile.Name)
		hyp := strings.ReplaceAll(m.MAC, ":", "-")
		fmt.Fprintf(&buf, "echo   trying %s (%s)...\n", m.Profile.Name, hyp)
		for _, nic := range []string{"net0/mac:hexhyp", "net1/mac:hexhyp", "net2/mac:hexhyp", "net3/mac:hexhyp"} {
			fmt.Fprintf(&buf, "iseq ${%s} %s && goto %s\n", nic, hyp, label)
		}
	}
	w("echo   no iseq matched — falling through to target_default")
	w("sleep 1")
	w("goto target_default")
	w("")

	// Per-MAC blocks.
	for _, m := range machines {
		writeMachineBlock(&buf, m, ctx)
	}

	// Default arm — fall back to netboot.xyz embed.
	w("# ----- default arm: machine not in fleet.yaml -----")
	w(":target_default")
	w("echo")
	w("echo ===== NO FLEET MATCH =====")
	w("echo   ${net0/mac:hexhyp} is not in fleet.yaml")
	w("echo   check fleet.yaml for a matching `mac:` entry")
	w("echo   (compare against the value above — case + hyphens matter)")
	w("echo   falling back to netboot.xyz menu in 8s")
	w("sleep 8")
	w("goto menu_netbootxyz")
	w("")
	w("# ----- :menu_netbootxyz — clean chain to netboot.xyz hosted menu -----")
	w(":menu_netbootxyz")
	w("echo")
	w("echo ===== CHAIN TO NETBOOT.XYZ =====")
	w("echo   target: https://boot.netboot.xyz/menu.ipxe")
	w("echo   (this REPLACES iPXE; you should see netboot.xyz's menu next)")
	w("sleep 2")
	w("chain --replace --autofree https://boot.netboot.xyz/menu.ipxe || goto menu_netbootxyz_fail")
	w("")
	// Fail blocks.
	w("# ----- top-level fail blocks -----")
	w(":top_fail_dhcp")
	w("echo")
	w("echo ===== TOP-LEVEL DHCP FAILED =====")
	w("echo   iPXE could not get an IP from the DHCP server")
	w("echo   nothing further is possible; rebooting in 30s")
	w("sleep 30")
	w("reboot")
	w("")
	w(":menu_netbootxyz_fail")
	w("echo")
	w("echo ===== NETBOOT.XYZ CHAIN FAILED =====")
	w("echo   HTTPS chain to boot.netboot.xyz failed")
	w("echo   check outbound HTTPS reachability from this NIC")
	w("echo   rebooting in 30s")
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

	// v0.6.0: when -client-netmask was used to widen iPXE's netmask
	// for cross-/24 routing to pxe-beacon, we also need to pass the
	// widened netmask through to the Linux kernel. d-i / Subiquity
	// re-DHCP after kernel boot and would otherwise get the same
	// broken /24 from the DHCP server, making /preseed.cfg fetch
	// from pxe-beacon (off the /24) fail.
	//
	// Kernel `ip=` cmdline syntax per
	// Documentation/admin-guide/nfs/nfsroot.rst:
	//   ip=client-ip:server-ip:gw:netmask:hostname:device:autoconf
	//
	// We use iPXE variable substitution (${ip}, ${gateway}) so the
	// values are the ones iPXE actually resolved at boot. autoconf
	// is `none` (static), so the kernel doesn't re-DHCP and we keep
	// the widened netmask.
	ipArg := "ip=dhcp"
	if ctx.ClientNetmask != "" {
		ipArg = fmt.Sprintf("ip=${ip}::${gateway}:%s:::none", ctx.ClientNetmask)
	}

	w("")
	fmt.Fprintf(buf, ":%s\n", label)
	w("echo")
	fmt.Fprintf(buf, "echo ===== [stage 4/5] MATCHED ARM: %s =====\n", name)
	fmt.Fprintf(buf, "echo   fleet target: %s\n", m.Profile.Boot)
	fmt.Fprintf(buf, "echo   mac: %s\n", m.MAC)
	w("sleep 2")
	w("")

	// v0.6.3: 30-second interactive boot menu. Default = fleet target
	// (auto-selected for unattended boots). Operator at the console
	// can press a letter key to override:
	//   b — fleet target (default — also auto-selected at timeout)
	//   m — netboot.xyz menu (pick a different OS interactively)
	//   s — iPXE shell (debug)
	//
	// Letter keys instead of numeric: some snponly UEFI keyboard
	// stacks don't register number keys reliably. Letters are
	// consistently handled.
	//
	// `goto menu_netbootxyz` (not `target_default`) — the latter emits
	// "NO MATCH" text and sleeps 8s, which is the right UX for
	// iseq-miss but wrong for menu-driven choice.
	fmt.Fprintf(buf, "menu pxe-beacon — %s (%s)\n", name, m.MAC)
	fmt.Fprintf(buf, "item --gap fleet config: boot=%s\n", m.Profile.Boot)
	fmt.Fprintf(buf, "item --default --key b %s_boot         Boot fleet target (default — auto in 30s): %s\n",
		label, m.Profile.Boot)
	fmt.Fprintf(buf, "item            --key m menu_netbootxyz   netboot.xyz menu (manual OS picker)\n")
	fmt.Fprintf(buf, "item            --key s %s_shell       iPXE shell (debug)\n", label)
	fmt.Fprintf(buf, "echo pxe-beacon: press 'b' to boot %s now, 'm' for netboot.xyz menu, 's' for shell — or wait 30s\n", m.Profile.Boot)
	fmt.Fprintf(buf, "choose --timeout 30000 --default %s_boot %s_menu_choice ||\n", label, label)
	fmt.Fprintf(buf, "echo pxe-beacon: choose returned error (Ctrl+C?), defaulting to boot fleet target\n")
	fmt.Fprintf(buf, "echo pxe-beacon: ===== menu choice: ${%s_menu_choice} =====\n", label)
	w("sleep 2")
	fmt.Fprintf(buf, "goto ${%s_menu_choice}\n", label)
	w("")
	fmt.Fprintf(buf, ":%s_shell\n", label)
	w("echo pxe-beacon: dropping to iPXE shell. Type 'exit' to return to the menu.")
	w("shell")
	fmt.Fprintf(buf, "goto %s\n", label)
	w("")
	fmt.Fprintf(buf, ":%s_boot\n", label)
	w("echo")
	fmt.Fprintf(buf, "echo ===== [stage 5/5] BOOTING %s =====\n", m.Profile.Boot)
	w("echo   ip      = ${ip}")
	w("echo   netmask = ${netmask}")
	w("echo   gateway = ${gateway}")
	w("echo   dns     = ${dns}")
	w("sleep 2")

	// v0.5.13: dhcp + netmask widening are done ONCE at the top of
	// the script. Doing them again here would overwrite the widened
	// netmask with the DHCP-supplied /24, breaking the cross-subnet
	// fix.
	w("imgfree")

	switch m.Profile.Boot {
	case "debian-12":
		mirror := "http://deb.debian.org/debian/dists/bookworm/main/installer-amd64/current/images/netboot/debian-installer/amd64"
		fmt.Fprintf(buf, "echo pxe-beacon: ip=${ip} gw=${gateway} dns=${dns}\n")
		fmt.Fprintf(buf, "echo pxe-beacon: fetching Debian 12 d-i kernel: %s/linux\n", mirror)
		fmt.Fprintf(buf,
			"kernel --name linux %s/linux auto=true priority=critical %s url=%s %s --- || goto %s_fail_kernel\n",
			mirror, ipArg, preseedURL, consoleArgs, label)
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
			"kernel --name linux %s/linux auto=true priority=critical %s url=%s %s --- || goto %s_fail_kernel\n",
			mirror, ipArg, preseedURL, consoleArgs, label)
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
			"kernel --name vmlinuz %s/vmlinuz initrd=initrd %s ipv6.disable=1 boot=casper url=%s/filesystem.squashfs %s autoinstall ds=nocloud-net\\;s=%s --- || goto %s_fail_kernel\n",
			assets, ipArg, assets, consoleArgs, autoinstallBase, label)
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
	w("echo")
	w("echo ===== DHCP FAILED =====")
	fmt.Fprintf(buf, "echo   target: %s\n", target)
	w("echo   iPXE could not get an IP from the DHCP server in the matched arm")
	w("echo   nothing further is possible; rebooting in 30s")
	w("sleep 30")
	w("reboot")

	fmt.Fprintf(buf, "\n:%s_fail_kernel\n", label)
	w("echo")
	w("echo ===== KERNEL FETCH FAILED =====")
	fmt.Fprintf(buf, "echo   target:    %s\n", target)
	fmt.Fprintf(buf, "echo   tried URL: %s/linux\n", srcURL)
	w("echo   verify DNS + outbound HTTP from this NIC reach the URL above")
	w("echo   rebooting in 30s")
	w("sleep 30")
	w("reboot")

	fmt.Fprintf(buf, "\n:%s_fail_initrd\n", label)
	w("echo")
	w("echo ===== INITRD FETCH FAILED =====")
	fmt.Fprintf(buf, "echo   target:    %s\n", target)
	fmt.Fprintf(buf, "echo   tried URL: %s/initrd.gz\n", srcURL)
	w("echo   kernel loaded fine; initrd download failed")
	w("echo   rebooting in 30s")
	w("sleep 30")
	w("reboot")

	fmt.Fprintf(buf, "\n:%s_fail_boot\n", label)
	w("echo")
	w("echo ===== BOOT FAILED =====")
	fmt.Fprintf(buf, "echo   target: %s\n", target)
	w("echo   kernel + initrd loaded but `boot` returned error")
	w("echo   likely a kernel cmdline / arch mismatch")
	w("echo   rebooting in 30s")
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
