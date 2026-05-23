# iPXE binary provenance

These iPXE/netboot.xyz binaries are embedded into the `pxe-beacon` binary
via `go:embed`. Per the GPLv2+ iPXE license, the source and version of
each embedded binary is recorded here.

| File                        | Source URL                                            | Notes                                       |
|-----------------------------|-------------------------------------------------------|---------------------------------------------|
| netboot.xyz.efi             | https://boot.netboot.xyz/ipxe/netboot.xyz.efi         | UEFI x86_64 — TFTP loader (option 93 0x07)  |
| netboot.xyz-snponly.efi     | https://boot.netboot.xyz/ipxe/netboot.xyz-snponly.efi | UEFI x86_64 — SNP-only (firmware NIC)       |
| netboot.xyz-arm64.efi       | https://boot.netboot.xyz/ipxe/netboot.xyz-arm64.efi   | UEFI arm64 (option 93 0x0b)                 |
| netboot.xyz.kpxe            | https://boot.netboot.xyz/ipxe/netboot.xyz.kpxe        | Legacy BIOS / undionly.kpxe (option 93 0x00)|

These come from netboot.xyz's CDN. They are bundled here so that
`pxe-beacon` is a single static binary with no runtime download
dependency.

iPXE source: https://github.com/ipxe/ipxe
iPXE license: GPLv2+
netboot.xyz: https://netboot.xyz
