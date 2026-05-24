#!/usr/bin/env bash
# debug.sh — one-shot pxe-beacon diagnostic for paste-back. v0.5.1.
#
# Usage:
#   1. Make sure pxe-beacon is running in another terminal:
#        sudo ./pxe-beacon -config ./fleet.yaml -data-dir ./data
#   2. Run this script:
#        sudo ./debug.sh
#   3. Power-cycle the PXE client SOON after starting — tcpdump runs
#      for $TCPDUMP_SEC (default 45s).
#   4. Paste pxe-beacon-debug.txt back.
#
# v0.5.1 fixes:
#   - macOS portable timeout (GNU `timeout` isn't on macOS by default)
#   - drops local-TFTP probe (BSD tftp hangs talking to its own
#     non-loopback IP on macOS 26.x); uses HTTP /debug/tftp/autoexec.ipxe
#     instead — same bytes, no hang
#   - tests external-host reachability (dig + curl HEAD to deb.debian.org)
#     since the failure mode in v0.5.0 was kernel-fetch from external HTTP

set -u  # not -e — keep going even if probes fail
OUT="${OUT:-pxe-beacon-debug.txt}"
PCAP="${PCAP:-pxe-beacon-debug.pcap}"
INTERFACE="${INTERFACE:-en0}"
MAC="${MAC:-58:47:ca:70:c7:c9}"
ADV_IP="${ADV_IP:-10.69.69.218}"
PORT="${PORT:-8080}"
TCPDUMP_SEC="${TCPDUMP_SEC:-45}"

note() { printf '\n=== %s ===\n' "$*" >> "$OUT"; }
run()  { printf '$ %s\n' "$*" >> "$OUT"; eval "$@" >> "$OUT" 2>&1 || true; }

# Portable timeout — GNU `timeout` doesn't exist on macOS by default.
# Spawns the command, kills it after $1 seconds. Returns the command's
# exit code, or 124 if killed by the watchdog.
xtimeout() {
  local secs=$1; shift
  ( "$@" ) &
  local pid=$!
  ( sleep "$secs"; kill -TERM "$pid" 2>/dev/null ) &
  local watcher=$!
  wait "$pid" 2>/dev/null
  local rc=$?
  kill -TERM "$watcher" 2>/dev/null
  wait "$watcher" 2>/dev/null
  return $rc
}

: > "$OUT"
date -u +"=== pxe-beacon-debug %Y-%m-%dT%H:%M:%SZ ===" >> "$OUT"
note "host"
run "uname -a"
run "sw_vers 2>/dev/null || lsb_release -a 2>/dev/null || true"

note "pxe-beacon binary"
run "command -v pxe-beacon || command -v ./pxe-beacon"
run "./pxe-beacon -version 2>/dev/null || pxe-beacon -version 2>/dev/null || echo '(binary not in PATH)'"

note "fleet.yaml on disk"
if   [ -f ./fleet.yaml ];               then run "cat ./fleet.yaml"
elif [ -f /etc/pxe-beacon/fleet.yaml ]; then run "cat /etc/pxe-beacon/fleet.yaml"
else echo "(no fleet.yaml found in ./ or /etc/pxe-beacon/)" >> "$OUT"
fi

note "side-files referenced by fleet.yaml"
run "ls -la ./data/templates/ 2>/dev/null || echo '(no data-dir/templates — embedded defaults in use)'"

note "ports bound (need to be pxe-beacon, not another tftpd)"
run "sudo lsof -nP -iUDP:67 -iUDP:69 -iUDP:4011 -iTCP:${PORT} 2>&1"

note "macOS firewall state"
if [ -x /usr/libexec/ApplicationFirewall/socketfilterfw ]; then
  run "/usr/libexec/ApplicationFirewall/socketfilterfw --getglobalstate"
  run "/usr/libexec/ApplicationFirewall/socketfilterfw --getstealthmode"
fi
run "sudo pfctl -s rules 2>&1 | head -10 || true"

note "macOS tftpd state — should be DISABLED so it doesn't race for port 69"
run "sudo launchctl print-disabled system | grep -i tftp 2>&1 || true"
echo "  If com.apple.tftpd shows 'enabled', re-disable with:" >> "$OUT"
echo "    sudo launchctl disable system/com.apple.tftpd" >> "$OUT"
echo "    sudo launchctl bootout system/com.apple.tftpd 2>/dev/null || true" >> "$OUT"

note "curl /status.json"
run "curl -sS -m 5 http://${ADV_IP}:${PORT}/status.json || echo '(unreachable)'"

note "curl /admin (HTML, first 20 lines)"
run "curl -sS -m 5 http://127.0.0.1:${PORT}/admin 2>&1 | head -20 || true"

HYP=$(echo "$MAC" | tr ':' '-')

note "curl /debug/tftp/autoexec.ipxe (v0.5.0+ dispatch script — same bytes TFTP serves)"
run "curl -sS -m 5 http://${ADV_IP}:${PORT}/debug/tftp/autoexec.ipxe || echo '(unreachable — pxe-beacon < v0.5.1 lacks this route)'"

note "curl /autoinstall/<mac>/preseed.cfg (the d-i preseed)"
run "curl -sS -m 5 http://${ADV_IP}:${PORT}/autoinstall/${HYP}/preseed.cfg 2>&1 | head -40 || true"

# External reachability — what the PXE client needs in order for kernel
# fetch from deb.debian.org to succeed in v0.5.0 dispatch mode. If THIS
# host can't reach it, the PXE client almost certainly can't either.
note "external DNS — can THIS host resolve deb.debian.org?"
run "dig +time=3 +tries=1 deb.debian.org A 2>&1 | head -15"

note "external HTTP — can THIS host fetch from deb.debian.org over HTTP?"
run "curl -sS -m 8 -I http://deb.debian.org/debian/dists/bookworm/main/installer-amd64/current/images/netboot/debian-installer/amd64/linux 2>&1 | head -10"

note "arp table — confirms client got an IP from real DHCP"
run "arp -an 2>&1 | grep -iE \"$(echo ${MAC} | tr -d ':')|${MAC}\" || echo '(client not yet in arp cache)'"

note "tcpdump — capturing ${TCPDUMP_SEC}s of relevant traffic NOW"
echo "  -> Power-cycle the PXE client now, or kick it via its BMC." >> "$OUT"
echo "  -> tcpdump will run for ${TCPDUMP_SEC}s then write transcript below." >> "$OUT"
echo "(starting tcpdump on ${INTERFACE} for ${TCPDUMP_SEC}s — power-cycle client now)" >&2
xtimeout "${TCPDUMP_SEC}" sudo tcpdump -i "${INTERFACE}" -n -tttt \
  -w "${PCAP}" \
  "ether host ${MAC} or port ${PORT}" \
  >>"$OUT" 2>&1 || true

note "tcpdump transcript (text — first 300 lines)"
sudo tcpdump -r "${PCAP}" -n -vv 2>/dev/null \
  | head -300 >> "$OUT" || true

note "pxe-beacon log"
echo "(if you redirected pxe-beacon output to a file, tail it here):" >> "$OUT"
echo "  e.g.   tail -200 /tmp/pxe-beacon.log" >> "$OUT"
echo "(otherwise: copy the most recent 50 lines from your pxe-beacon terminal)" >> "$OUT"

note "complete"
echo "Wrote ${OUT} (and ${PCAP})." >&2
echo "Paste ${OUT} back. PCAP is binary; attach separately if helpful." >&2
