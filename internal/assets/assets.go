// Package assets bundles the iPXE binaries and chain scripts into the
// pxe-beacon binary via go:embed so the tool has zero runtime
// dependencies.
package assets

import (
	"embed"
	"fmt"
	"io/fs"
)

//go:embed ipxe/netboot.xyz.efi ipxe/netboot.xyz-snponly.efi ipxe/netboot.xyz-arm64.efi ipxe/netboot.xyz.kpxe
//go:embed scripts/boot.ipxe
//go:embed scripts/autoexec/menu.ipxe
//go:embed scripts/autoexec/ubuntu-22.04.ipxe
//go:embed scripts/autoexec/ubuntu-24.04.ipxe
//go:embed scripts/autoexec/debian-12.ipxe
var fsys embed.FS

// FS returns the embedded filesystem rooted at the package's
// `internal/assets` directory.
func FS() fs.FS {
	return fsys
}

// IPXEKind identifies which embedded iPXE/netboot.xyz binary to serve.
type IPXEKind int

const (
	// IPXEEFIx64 is the standard UEFI x86_64 iPXE EFI executable.
	IPXEEFIx64 IPXEKind = iota
	// IPXESNPOnly is the SNP-only build (uses firmware's SNP NIC stack).
	IPXESNPOnly
	// IPXEARM64 is the UEFI aarch64 iPXE EFI executable.
	IPXEARM64
	// IPXELegacyBIOS is the legacy BIOS undionly.kpxe build.
	IPXELegacyBIOS
)

func (k IPXEKind) String() string {
	switch k {
	case IPXEEFIx64:
		return "netboot.xyz.efi"
	case IPXESNPOnly:
		return "netboot.xyz-snponly.efi"
	case IPXEARM64:
		return "netboot.xyz-arm64.efi"
	case IPXELegacyBIOS:
		return "netboot.xyz.kpxe"
	}
	return "unknown"
}

// FilePath returns the embed-relative path for a given kind.
func (k IPXEKind) FilePath() string {
	return "ipxe/" + k.String()
}

// ReadIPXE returns the bytes of the requested iPXE binary.
func ReadIPXE(k IPXEKind) ([]byte, error) {
	b, err := fs.ReadFile(fsys, k.FilePath())
	if err != nil {
		return nil, fmt.Errorf("read embedded %s: %w", k, err)
	}
	return b, nil
}

// ReadScript returns the raw boot.ipxe template bytes.
func ReadScript() ([]byte, error) {
	b, err := fs.ReadFile(fsys, "scripts/boot.ipxe")
	if err != nil {
		return nil, fmt.Errorf("read embedded boot.ipxe: %w", err)
	}
	return b, nil
}

// ReadAutoexec returns the embedded autoexec.ipxe template for a
// given boot target (e.g. "menu", "ubuntu-22.04"). Unknown targets
// return an error; callers should validate via fleet.ValidBootTargets
// first.
func ReadAutoexec(target string) ([]byte, error) {
	if target == "" {
		return nil, fmt.Errorf("empty target")
	}
	b, err := fs.ReadFile(fsys, "scripts/autoexec/"+target+".ipxe")
	if err != nil {
		return nil, fmt.Errorf("read embedded autoexec/%s.ipxe: %w", target, err)
	}
	return b, nil
}
