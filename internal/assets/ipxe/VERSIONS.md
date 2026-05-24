# Embedded iPXE binaries — vanilla upstream

v0.6.0 swapped from netboot.xyz's iPXE build (which has
`EMBED=embed.ipxe` and silently preempts our autoexec.ipxe
dispatch) to **vanilla upstream iPXE**.

## Source

- Repository: <https://github.com/ipxe/ipxe>
- Commit: `56a4f695d6d17a4a1c93d196c586d481dbe3b934` (2026-05-21)
- Build config: `src/config/local/general.h` enables
  `DOWNLOAD_PROTO_HTTPS`, `CONSOLE_CMD`, `NSLOOKUP_CMD`,
  `PING_CMD`, `REBOOT_CMD`, `POWEROFF_CMD`, `IMAGE_TRUST_CMD`.
- Build env: `SOURCE_DATE_EPOCH=0` for partial reproducibility.

## Binaries

| File | Target | Size | Purpose |
|---|---|---|---|
| `ipxe.efi` | x86_64-efi (full driver) | ~1.2 MB | UEFI x86-64 with native NIC drivers |
| `snponly.efi` | x86_64-efi (SNP) | ~300 KB | UEFI x86-64 using firmware's SNP — most compatible |
| `ipxe-arm64.efi` | arm64-efi (SNP) | ~330 KB | UEFI arm64 |
| `undionly.kpxe` | x86 BIOS (UNDI) | ~100 KB | Legacy BIOS PXE |

## Why vanilla, not netboot.xyz

netboot.xyz builds iPXE with `EMBED=embed.ipxe` — that compiled-in
script is netboot.xyz's main menu loader, and it runs INSTEAD of
iPXE's standard autoboot path. Net effect: pxe-beacon's per-MAC
dispatch script was being fetched (PXE firmware speculatively) but
iPXE never executed it. Vanilla iPXE has no EMBED and respects the
DHCP-supplied boot file URL (which our proxyDHCP serves as
`http://<advertised-ip>:8080/boot.ipxe` for iPXE-stage clients,
where pxe-beacon's HTTP server returns the dispatch script).

## Rebuild

To bump the iPXE pin in a future release:

```sh
git clone https://github.com/ipxe/ipxe.git
cd ipxe/src
# (place config in config/local/general.h — see above)
SOURCE_DATE_EPOCH=0 make -j bin-x86_64-efi/ipxe.efi bin-x86_64-efi/snponly.efi
SOURCE_DATE_EPOCH=0 make -j CROSS=aarch64-linux-gnu- bin-arm64-efi/snponly.efi
SOURCE_DATE_EPOCH=0 make -j bin/undionly.kpxe
# Copy artifacts into internal/assets/ipxe/ and update this file.
```

Build deps on Debian/Ubuntu: `gcc binutils perl mtools liblzma-dev
xorriso isolinux gcc-aarch64-linux-gnu binutils-aarch64-linux-gnu`.
