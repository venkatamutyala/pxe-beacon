// Package assets bundles the iPXE binaries and chain scripts into the
// pxe-beacon binary via go:embed so the tool has zero runtime
// dependencies.
package assets

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync/atomic"
)

//go:embed ipxe/netboot.xyz.efi ipxe/netboot.xyz-snponly.efi ipxe/netboot.xyz-arm64.efi ipxe/netboot.xyz.kpxe
//go:embed scripts/boot.ipxe
//go:embed scripts/autoexec/menu.ipxe
//go:embed scripts/autoexec/ubuntu-22.04.ipxe
//go:embed scripts/autoexec/ubuntu-24.04.ipxe
//go:embed scripts/autoexec/debian-12.ipxe
//go:embed scripts/autoexec/debian-13.ipxe
//go:embed scripts/defaults/debian-preseed.cfg
//go:embed scripts/defaults/cloud-init.yaml
var fsys embed.FS

// FS returns the embedded filesystem rooted at the package's
// `internal/assets` directory.
func FS() fs.FS {
	return fsys
}

// overrideDir is the on-disk directory where operator-edited template
// overrides live. When set (via SetOverrideDir from main at startup),
// every Read* function checks <overrideDir>/templates/<rel-path>
// first and falls back to the embedded baseline.
//
// Stored as atomic.Value so SIGHUP-style reloads (and the admin UI)
// can swap it at runtime without locking the read path.
var overrideDir atomic.Value // string

// SetOverrideDir configures the on-disk override location. Pass an
// absolute path. Empty string disables override lookups.
func SetOverrideDir(dir string) {
	overrideDir.Store(dir)
}

// OverrideDir returns the configured override path (or "" if unset).
func OverrideDir() string {
	d, _ := overrideDir.Load().(string)
	return d
}

// resolveOverride returns the disk-override bytes for a relative
// asset path if a file exists there; ok=false means caller should
// fall back to embedded.
func resolveOverride(rel string) ([]byte, bool) {
	d, _ := overrideDir.Load().(string)
	if d == "" {
		return nil, false
	}
	p := filepath.Join(d, "templates", rel)
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, false
	}
	return b, true
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

// ReadAutoexec returns the autoexec.ipxe template for a given boot
// target (e.g. "menu", "ubuntu-22.04"). Disk override takes
// precedence over the embedded baseline.
func ReadAutoexec(target string) ([]byte, error) {
	if target == "" {
		return nil, fmt.Errorf("empty target")
	}
	rel := "autoexec/" + target + ".ipxe"
	if b, ok := resolveOverride(rel); ok {
		return b, nil
	}
	b, err := fs.ReadFile(fsys, "scripts/"+rel)
	if err != nil {
		return nil, fmt.Errorf("read embedded autoexec/%s.ipxe: %w", target, err)
	}
	return b, nil
}

// ReadDefault returns the "out-of-the-box" side-file for a fleet
// entry that doesn't supply its own. Known names:
//
//	debian-preseed.cfg  — used when a debian-12/13 entry omits preseed:
//	cloud-init.yaml     — used when an autoinstall entry omits cloud_init:
//
// Disk override takes precedence over the embedded baseline.
func ReadDefault(name string) ([]byte, error) {
	if name == "" {
		return nil, fmt.Errorf("empty default name")
	}
	rel := "defaults/" + name
	if b, ok := resolveOverride(rel); ok {
		return b, nil
	}
	b, err := fs.ReadFile(fsys, "scripts/"+rel)
	if err != nil {
		return nil, fmt.Errorf("read embedded defaults/%s: %w", name, err)
	}
	return b, nil
}

// ListEditableTemplates returns the set of template relative paths
// that operators can edit (via the admin UI or by dropping a file in
// <override-dir>/templates/<rel>). Order is stable for UI rendering.
func ListEditableTemplates() []string {
	return []string{
		"defaults/debian-preseed.cfg",
		"defaults/cloud-init.yaml",
		"autoexec/menu.ipxe",
		"autoexec/ubuntu-22.04.ipxe",
		"autoexec/ubuntu-24.04.ipxe",
		"autoexec/debian-12.ipxe",
		"autoexec/debian-13.ipxe",
	}
}

// ReadEmbedded returns the bytes baked into the binary for a given
// template path, ignoring any disk override. Used by the admin UI
// to show the operator "what would be served if I deleted my override."
func ReadEmbedded(rel string) ([]byte, error) {
	b, err := fs.ReadFile(fsys, "scripts/"+rel)
	if err != nil {
		return nil, fmt.Errorf("read embedded %s: %w", rel, err)
	}
	return b, nil
}
