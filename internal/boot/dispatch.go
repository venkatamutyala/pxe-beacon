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
	w("echo pxe-beacon: dispatch for ${net0/mac:hexhyp}")
	w("")

	machines := []fleet.Machine{}
	if f != nil {
		for mac, p := range f.Machines() {
			machines = append(machines, fleet.Machine{MAC: mac, Profile: p})
		}
	}
	// Stable order — sort by MAC for diff-ability.
	sort.Slice(machines, func(i, j int) bool { return machines[i].MAC < machines[j].MAC })

	// Dispatch table.
	w("# ----- per-MAC dispatch -----")
	for _, m := range machines {
		label := labelOf(m.MAC, m.Profile.Name)
		// One `iseq` per machine. Trailing `||` is INTENTIONAL — iPXE
		// chains them so the next condition is tried on miss. After
		// the last `iseq`, fall through to the default arm.
		fmt.Fprintf(&buf, "iseq ${net0/mac:hexhyp} %s && goto %s ||\n",
			strings.ReplaceAll(m.MAC, ":", "-"), label)
	}
	w("goto target_default")
	w("")

	// Per-MAC blocks.
	for _, m := range machines {
		writeMachineBlock(&buf, m, ctx)
	}

	// Default arm — fall back to netboot.xyz embed.
	w("# ----- default arm: machine not in fleet.yaml -----")
	w(":target_default")
	w("echo pxe-beacon: ${net0/mac:hexhyp} not in fleet.yaml — chaining netboot.xyz menu")
	w("dhcp || echo pxe-beacon: dhcp failed in default arm")
	w("chain --replace --autofree https://boot.netboot.xyz/menu.ipxe ||")
	w("echo pxe-beacon: netboot.xyz chain failed; dropping to iPXE shell in 3s")
	w("sleep 3")
	w("shell")

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
	w("dhcp || echo pxe-beacon: dhcp failed; cannot fetch kernel && sleep 3 && shell")
	w("imgfree")

	switch m.Profile.Boot {
	case "debian-12":
		mirror := "http://deb.debian.org/debian/dists/bookworm/main/installer-amd64/current/images/netboot/debian-installer/amd64"
		fmt.Fprintf(buf, "echo pxe-beacon: fetching Debian 12 d-i kernel from %s/linux\n", mirror)
		fmt.Fprintf(buf,
			"kernel --name linux %s/linux auto=true priority=critical ip=dhcp url=%s %s ---\n",
			mirror, preseedURL, consoleArgs)
		fmt.Fprintf(buf, "initrd --name initrd.gz %s/initrd.gz ||\n", mirror)
		w("echo pxe-beacon: initrd fetch failed; check HTTP to deb.debian.org && sleep 3 && shell")
		fmt.Fprintf(buf, "boot ||\n")
		w("echo pxe-beacon: boot failed; check preseed URL reachability && sleep 3 && shell")

	case "debian-13":
		mirror := "http://deb.debian.org/debian/dists/trixie/main/installer-amd64/current/images/netboot/debian-installer/amd64"
		fmt.Fprintf(buf, "echo pxe-beacon: fetching Debian 13 d-i kernel from %s/linux\n", mirror)
		fmt.Fprintf(buf,
			"kernel --name linux %s/linux auto=true priority=critical ip=dhcp url=%s %s ---\n",
			mirror, preseedURL, consoleArgs)
		fmt.Fprintf(buf, "initrd --name initrd.gz %s/initrd.gz ||\n", mirror)
		w("echo pxe-beacon: initrd fetch failed; check HTTP to deb.debian.org && sleep 3 && shell")
		fmt.Fprintf(buf, "boot ||\n")
		w("echo pxe-beacon: boot failed; check preseed URL reachability && sleep 3 && shell")

	case "ubuntu-22.04", "ubuntu-24.04":
		assets := assetsBase(m.Profile.Boot)
		fmt.Fprintf(buf, "echo pxe-beacon: fetching Ubuntu %s kernel from %s/vmlinuz\n",
			strings.TrimPrefix(m.Profile.Boot, "ubuntu-"), assets)
		fmt.Fprintf(buf, "echo pxe-beacon: (requires `pxe-beacon fetch %s` to have populated data-dir)\n", m.Profile.Boot)
		// `autoinstall ---` separator is REQUIRED on 22.04.3+; without
		// it Subiquity prompts. Order: cmdline args, then ---, then
		// initrd.
		fmt.Fprintf(buf,
			"kernel --name vmlinuz %s/vmlinuz initrd=initrd ip=dhcp ipv6.disable=1 boot=casper url=%s/filesystem.squashfs %s autoinstall ds=nocloud-net\\;s=%s ---\n",
			assets, assets, consoleArgs, autoinstallBase)
		fmt.Fprintf(buf, "initrd --name initrd %s/initrd ||\n", assets)
		w("echo pxe-beacon: initrd fetch failed; run `pxe-beacon fetch " + m.Profile.Boot + "` to populate data-dir && sleep 3 && shell")
		fmt.Fprintf(buf, "boot ||\n")
		w("echo pxe-beacon: boot failed; check Subiquity cmdline + /assets/ reachability && sleep 3 && shell")

	case "custom":
		// We can't inline an arbitrary operator script — but we CAN
		// chain to the HTTP route that serves the templated custom
		// script. Custom is the one path that legitimately needs HTTP
		// chain (no other way to deliver an operator-defined script).
		customURL := fmt.Sprintf("http://%s:%d/autoinstall/%s/autoexec.ipxe",
			ctx.AdvertisedIP, ctx.HTTPPort,
			strings.ReplaceAll(m.MAC, ":", "-"))
		fmt.Fprintf(buf, "echo pxe-beacon: chaining operator-supplied script %s\n", customURL)
		fmt.Fprintf(buf, "chain --replace --autofree %s ||\n", customURL)
		w("echo pxe-beacon: custom script chain failed; check HTTP reachability && sleep 3 && shell")

	case "menu":
		fmt.Fprintf(buf, "echo pxe-beacon: chaining netboot.xyz menu\n")
		w("chain --replace --autofree https://boot.netboot.xyz/menu.ipxe ||")
		w("chain --replace --autofree http://boot.netboot.xyz/menu.ipxe ||")
		w("echo pxe-beacon: netboot.xyz chain failed && sleep 3 && shell")

	default:
		fmt.Fprintf(buf, "echo pxe-beacon: unknown boot target %q for %s; falling through\n", m.Profile.Boot, name)
		w("goto target_default")
	}
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
