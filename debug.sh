#!/usr/bin/env bash
# debug.sh — one-shot pxe-beacon diagnostic for paste-back.
#
# Usage:
#   1. Make sure pxe-beacon is running in another terminal:
#        sudo ./pxe-beacon -config ./fleet.yaml -data-dir ./data
#   2. Run this script (no args needed):
#        sudo ./debug.sh
#   3. Power-cycle your PXE client SOON after starting this — the
#      script tcpdumps for 60 seconds.
#   4. Paste the resulting pxe-beacon-debug.txt back.
#
# Captures: pxe-beacon version, fleet.yaml, /status.json, autoexec +
# preseed dumps, lsof on the privileged ports, 60s tcpdump (PCAP +
# text), macOS firewall state, recent pxe-beacon log lines.

set -u  # not -e — we want to keep going even if individual probes fail
OUT="${OUT:-pxe-beacon-debug.txt}"
PCAP="${PCAP:-pxe-beacon-debug.pcap}"
INTERFACE="${INTERFACE:-en0}"
MAC="${MAC:-58:47:ca:70:c7:c9}"
ADV_IP="${ADV_IP:-10.69.69.218}"
PORT="${PORT:-8080}"
TCPDUMP_SEC="${TCPDUMP_SEC:-60}"

note() { printf '\n=== %s ===\n' "$*" >> "$OUT"; }
run() { printf '$ %s\n' "$*" >> "$OUT"; eval "$@" >> "$OUT" 2>&1 || true; }

: > "$OUT"
date -u +"=== pxe-beacon-debug %Y-%m-%dT%H:%M:%SZ ===" >> "$OUT"
note "host"; run "uname -a"; run "sw_vers 2>/dev/null || lsb_release -a 2>/dev/null || true"

note "pxe-beacon binary"
run "command -v pxe-beacon || command -v ./pxe-beacon"
run "./pxe-beacon -version 2>/dev/null || pxe-beacon -version 2>/dev/null || echo '(binary not in PATH)'"

note "fleet.yaml on disk"
if [ -f ./fleet.yaml ]; then run "cat ./fleet.yaml"
elif [ -f /etc/pxe-beacon/fleet.yaml ]; then run "cat /etc/pxe-beacon/fleet.yaml"
else echo "(no fleet.yaml found in ./ or /etc/pxe-beacon/)" >> "$OUT"
fi

note "side-files referenced by fleet.yaml"
run "ls -la ./examples/*.{cfg,yaml,ipxe} 2>/dev/null || true"
run "ls -la ./data/templates/ 2>/dev/null || echo '(no data-dir/templates — embedded defaults in use)'"

note "ports bound (need to be pxe-beacon, not another tftpd)"
run "sudo lsof -nP -iUDP:67 -iUDP:69 -iUDP:4011 -iTCP:${PORT} 2>&1"

note "macOS firewall state"
if [ -x /usr/libexec/ApplicationFirewall/socketfilterfw ]; then
  run "/usr/libexec/ApplicationFirewall/socketfilterfw --getglobalstate"
  run "/usr/libexec/ApplicationFirewall/socketfilterfw --getstealthmode"
fi
run "sudo pfctl -s rules 2>&1 | head -10 || true"

note "macOS tftpd state (the v0.1.x problem — confirm not interfering)"
run "sudo launchctl print-disabled system | grep -i tftp 2>&1 || true"

note "curl /status.json"
run "curl -sS -m 5 http://${ADV_IP}:${PORT}/status.json || echo '(unreachable)'"

note "curl /admin (HTML, first 30 lines)"
run "curl -sS -m 5 http://127.0.0.1:${PORT}/admin 2>&1 | head -30 || true"

note "curl /autoinstall/<mac>/autoexec.ipxe (HTTP fallback route)"
HYP=$(echo "$MAC" | tr ':' '-')
run "curl -sS -m 5 http://${ADV_IP}:${PORT}/autoinstall/${HYP}/autoexec.ipxe 2>&1 | head -20 || true"

note "curl /autoinstall/<mac>/preseed.cfg (default + bridge)"
run "curl -sS -m 5 http://${ADV_IP}:${PORT}/autoinstall/${HYP}/preseed.cfg 2>&1 | head -40 || true"

note "TFTP autoexec.ipxe (the dispatch script v0.5.0 serves)"
TMPFILE=$(mktemp)
run "tftp ${ADV_IP} -c get autoexec.ipxe ${TMPFILE} 2>&1 || true"
note "  (dispatch script contents)"
run "cat ${TMPFILE} 2>/dev/null || true"

note "arp table — confirms client got an IP from real DHCP"
run "arp -an 2>&1 | grep -iE \"$(echo ${MAC} | tr -d ':')|${MAC}\" || echo '(client not yet in arp cache)'"

note "tcpdump — capturing ${TCPDUMP_SEC}s of relevant traffic NOW"
echo "  -> Power-cycle the PXE client now, or kick it via its BMC." >> "$OUT"
echo "  -> tcpdump will run for ${TCPDUMP_SEC}s then write transcript below." >> "$OUT"
echo "(starting tcpdump on ${INTERFACE} for ${TCPDUMP_SEC}s...)" >&2
sudo timeout "${TCPDUMP_SEC}" tcpdump -i "${INTERFACE}" -n -tttt \
  -w "${PCAP}" \
  "ether host ${MAC} or port ${PORT}" \
  2>>"$OUT" || true

note "tcpdump transcript (text)"
sudo tcpdump -r "${PCAP}" -n -vv 2>/dev/null \
  | grep -E 'BOOTP|TFTP|tcp|http|\.${PORT}|\.67|\.68|\.69|\.4011' \
  | head -300 >> "$OUT" || true

note "pxe-beacon log (last 200 lines from journalctl OR your shell history)"
echo "(if you redirected pxe-beacon output to a file, cat the tail here):" >> "$OUT"
echo "  e.g.   tail -200 /tmp/pxe-beacon.log" >> "$OUT"

note "complete"
echo "Wrote ${OUT} (and ${PCAP})." >&2
echo "Paste ${OUT} back. PCAP is binary; attach separately if needed." >&2
