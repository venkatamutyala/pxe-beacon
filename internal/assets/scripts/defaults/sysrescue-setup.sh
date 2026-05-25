#!/bin/sh
# pxe-beacon SystemRescue autorun script — injects an operator SSH key
# into the live rescue environment and ensures sshd is running.
#
# Referenced from sysrescue.yaml's autorun.exec block, fetched over HTTP
# at boot. pxe-beacon Go-templates this; Vars: Name, MAC, MACHyp,
# AdvertisedIP, HTTPPort, Params. Only meaningful when
# params.ssh_authorized_key is set.
set -e

{{- with index .Params "ssh_authorized_key"}}
mkdir -p /root/.ssh
chmod 700 /root/.ssh
printf '%s\n' "{{.}}" >> /root/.ssh/authorized_keys
chmod 600 /root/.ssh/authorized_keys
{{- end}}

# SystemRescue ships sshd; make sure it's up so the key is usable.
systemctl enable --now sshd 2>/dev/null || rc-service sshd start 2>/dev/null || true
