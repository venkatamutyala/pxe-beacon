# Running `pxe-beacon`

`pxe-beacon` is a single static binary that bundles a proxyDHCP
responder, a TFTP server, and an HTTP server. It needs root (or
`CAP_NET_BIND_SERVICE`) because UDP ports 67, 69, and 4011 are
privileged, and it must run on the **same L2 broadcast segment** as
the PXE client (proxyDHCP is broadcast-based — it cannot cross
routers / cannot be hosted on a VPS / cannot run inside a NAT'd
Docker VM on macOS).

---

## Quick start

```bash
go build -o pxe-beacon ./cmd/pxe-beacon
sudo ./pxe-beacon                            # auto-detect interface
sudo ./pxe-beacon -interface eth0            # pin an interface
sudo ./pxe-beacon -advertise-ip 192.168.1.10 # override advertised IP
```

Flags:

```
-interface       network interface to advertise (auto-detect if empty)
-listen          address to bind UDP sockets on (default 0.0.0.0)
-advertise-ip    override the IPv4 sent to clients (auto from interface)
-http-port       HTTP port for iPXE binary + chain script (default 8080)
-tftp-listen     TFTP listen address (default 0.0.0.0:69)
-chain-url       URL the chain script chainloads (default netboot.xyz)
-ipxe-script     path to a custom boot.ipxe template (override embedded)
-crosscert       emit `set crosscert` in the script (older iPXE + HTTPS)
-loglevel        error|warn|info|debug (default info)
```

---

## The manual M4 validation — first real boot

The PLAN milestone M4 is **"a UEFI client boots through to the
netboot.xyz menu with no other config"**. That has to be run on a
machine you trust to PXE-boot — a VM with bridged networking, or a
real box on a test LAN. We can't fully verify this from CI.

### Path A — QEMU+OVMF on Linux (preferred per PLAN section 5)

You need an existing DHCP server on the test segment (your home/lab
router is fine). pxe-beacon is *proxy*-DHCP — it never assigns IPs.

```bash
# 1) on the server (host machine):
sudo ./pxe-beacon -interface br0 -loglevel info

# 2) on the same host, in another terminal, boot a UEFI VM bridged
#    onto the same network. (br0 must be the bridge the VM uses.)
qemu-system-x86_64 \
  -machine q35 -accel kvm -m 2048 \
  -bios /usr/share/OVMF/OVMF_CODE.fd \
  -netdev bridge,id=net0,br=br0 \
  -device virtio-net-pci,netdev=net0,romfile= \
  -boot n

# Watch pxe-beacon's stdout: you should see, in order:
#   client <mac> arch=0x07(EFI x86-64) ... stage=firmware-TFTP -> decision: serve netboot.xyz.efi via TFTP from <ip>
#   TFTP RRQ "netboot.xyz.efi" -> served netboot.xyz.efi (1171456 bytes) ok
#   client <mac> arch=0x07(EFI x86-64) userclass=iPXE stage=iPXE-script -> decision: serve http://<ip>:8080/boot.ipxe via HTTP from <ip>
#   GET /boot.ipxe -> 200, <n> bytes
# Then iPXE pulls the netboot.xyz menu and renders it.
```

If the VM bridge isn't already configured, the simplest setup on a
Debian/Ubuntu host:

```bash
sudo apt install -y bridge-utils qemu-system-x86 ovmf
sudo brctl addbr br0
sudo ip link set br0 up
# point your VM at br0; have your existing DHCP serve that network.
```

### Path B — Real hardware on a test LAN

1. Plug the test machine into a switch that also has your DHCP server
   and the host running pxe-beacon.
2. Boot the test machine into UEFI firmware setup, enable network
   boot (sometimes called "PXE Boot" or "UEFI Network Stack").
3. Save & exit; on next boot, the firmware will broadcast DISCOVER.
4. Watch pxe-beacon's log for the decision/TFTP/HTTP sequence above.
5. The screen should land on the netboot.xyz menu within ~30 seconds.

If the screen says "PXE-E32: TFTP open timeout" or similar, the
TFTP transfer failed — see the troubleshooting section below.

### Path C — UEFI HTTP boot (option-93 0x10)

Some recent firmware does UEFI HTTP boot natively (no TFTP needed).
The flow is the same; pxe-beacon detects option-93 `0x10` and sends
back an HTTP URL in option 67 instead of a TFTP filename. The log
will say `stage=firmware-HTTP -> decision: serve netboot.xyz.efi via
HTTP from <ip>`.

---

## The universal lens — tcpdump

PLAN section 5 calls this out, repeat here so it's at the operator's
fingertips. Run on the pxe-beacon host:

```bash
sudo tcpdump -i <if> -n 'port 67 or port 68 or port 69 or port 4011 or port 8080'
```

You should see:
1. `IP 0.0.0.0.68 > 255.255.255.255.67: BOOTP/DHCP, Request from <mac>` — the client's DISCOVER.
2. `IP <ip>.67 > 255.255.255.255.68: BOOTP/DHCP, Reply` — our OFFER.
3. `IP <client>.<high> > <ip>.69: TFTP, RRQ "netboot.xyz.efi" octet` — the client fetching.
4. TFTP DATA/ACK on a high port pair.
5. (Then iPXE takes over and you'll see HTTP on 8080.)

If step 2 never appears, pxe-beacon isn't being heard — interface or
firewall issue.
If step 3 never appears, the OFFER reached the client but it didn't
or couldn't follow up — most often a firewall blocking UDP 69 on the
pxe-beacon host, or the advertised IP is unreachable from the
client (different subnet?).

---

## Loopback / Tier-1 smoke test

You can sanity-check the binary on a single host without any VM:

```bash
sudo ./pxe-beacon -listen 127.0.0.1 -tftp-listen 127.0.0.1:6969 \
                  -advertise-ip 127.0.0.1 -http-port 8080 &

# Verify TFTP serves the iPXE binary at the right size:
tftp 127.0.0.1 6969 -c get netboot.xyz.efi /tmp/ipxe.efi
ls -l /tmp/ipxe.efi  # should be 1171456 bytes

# Verify HTTP:
curl -sI http://127.0.0.1:8080/netboot.xyz.efi
curl -s  http://127.0.0.1:8080/boot.ipxe
```

This is what the Tier-1 unit tests do programmatically. It is **not**
a substitute for the M4 boot gate (proxyDHCP cannot be exercised over
loopback without a synthetic client), just a sanity check.

---

## Troubleshooting (PLAN section 0 gotchas, surfaced)

| Symptom                                                                | Likely cause                                                            |
|------------------------------------------------------------------------|-------------------------------------------------------------------------|
| `bind udp/67: permission denied`                                       | Not running as root. `sudo ./pxe-beacon` or set `CAP_NET_BIND_SERVICE`. |
| OFFER sent, TFTP never starts                                          | Same-segment problem, firewall, or advertised IP wrong.                 |
| TFTP timeouts intermittently                                           | WiFi. PLAN section 0 — prefer wired. Listener warns when iface looks wireless. |
| iPXE says "Could not verify: Permission denied"                        | TLS cert / older iPXE. Try `-crosscert`.                                |
| Logs flooded with `missing option 60`                                  | These are benign — labelled `(benign: client already handed off to iPXE)`. |
| `port already in use`                                                  | Another DHCP/TFTP/HTTP on the host. The error names which.              |
| AMI/Phoenix keyboard dead in iPXE                                      | Firmware bug (PLAN section 0). Not pxe-beacon. Reboot.                  |

---

## Logging

Default `info` reads as a story of the boot. Look for:

```
client 58:47:ca:70:c7:c9 arch=0x07(EFI x86-64) userclass=<none> stage=firmware-TFTP -> decision: serve netboot.xyz.efi via TFTP from 192.168.1.10
TFTP RRQ "netboot.xyz.efi" -> served netboot.xyz.efi (1171456 bytes) ok
client 58:47:ca:70:c7:c9 arch=0x07(EFI x86-64) userclass=iPXE stage=iPXE-script -> decision: serve http://192.168.1.10:8080/boot.ipxe via HTTP from 192.168.1.10
GET /boot.ipxe -> 200, 415 bytes (192.168.1.20:34022)
```

`-loglevel debug` adds hex dumps of every packet sent and received, and
prints the full parsed option set. Use it when narration alone isn't
enough.

---

## Cross-compile

```bash
GOOS=linux  GOARCH=amd64 go build -o dist/pxe-beacon-linux-amd64  ./cmd/pxe-beacon
GOOS=linux  GOARCH=arm64 go build -o dist/pxe-beacon-linux-arm64  ./cmd/pxe-beacon
GOOS=darwin GOARCH=arm64 go build -o dist/pxe-beacon-darwin-arm64 ./cmd/pxe-beacon
```

(Windows is intentionally not supported per PLAN.)
