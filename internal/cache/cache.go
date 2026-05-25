// Package cache manages pxe-beacon's data-dir layout: where extracted
// distro assets (Ubuntu casper kernel/initrd/squashfs) live, how
// they're fetched, and how the HTTP server resolves /assets/ paths to
// files on disk.
//
// Layout under DataDir:
//
//	<DataDir>/
//	  ubuntu-22.04/
//	    vmlinuz                 (~14 MB) — Subiquity kernel
//	    initrd                  (~74 MB) — initramfs with casper
//	    filesystem.squashfs     (~1.3 GB) — root filesystem
//	    .pxe-beacon-fetched     marker file (JSON: source URL + timestamp + sha256)
//	  ubuntu-24.04/
//	    ... same layout ...
//
// One target = one subdir. The marker file lets `pxe-beacon fetch` be
// idempotent — re-running for an already-populated target is a no-op
// unless `-force` is passed.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// TargetSpec describes how to fetch one target.
type TargetSpec struct {
	// Name is the boot target name (e.g. "ubuntu-22.04").
	Name string
	// ISOURL is the upstream live-server ISO URL.
	ISOURL string
	// ISOPath maps each destination path (relative to the target dir,
	// '/'-separated, may be nested) → the file's absolute path inside
	// the ISO. Ubuntu live-server lays assets flat under the target
	// dir (vmlinuz, initrd, filesystem.squashfs). SystemRescue
	// preserves its native archiso tree (sysresccd/x86_64/airootfs.sfs)
	// because the boot firmware constructs that URL itself from
	// archiso_http_srv + archisobasedir — we don't get to rename it.
	ISOPath map[string]string
}

// Dests returns the destination paths this spec extracts, sorted for
// deterministic iteration (download/extract order, manifest listing).
func (s TargetSpec) Dests() []string {
	out := make([]string, 0, len(s.ISOPath))
	for d := range s.ISOPath {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// Targets is the registry of supported fetch targets.
var Targets = map[string]TargetSpec{
	"ubuntu-22.04": {
		Name:   "ubuntu-22.04",
		ISOURL: "https://releases.ubuntu.com/22.04/ubuntu-22.04.5-live-server-amd64.iso",
		ISOPath: map[string]string{
			"vmlinuz":             "/casper/vmlinuz",
			"initrd":              "/casper/initrd",
			"filesystem.squashfs": "/casper/ubuntu-server-minimal.squashfs",
		},
	},
	"ubuntu-24.04": {
		Name:   "ubuntu-24.04",
		ISOURL: "https://releases.ubuntu.com/24.04/ubuntu-24.04.4-live-server-amd64.iso",
		ISOPath: map[string]string{
			"vmlinuz":             "/casper/vmlinuz",
			"initrd":              "/casper/initrd",
			"filesystem.squashfs": "/casper/ubuntu-server-minimal.squashfs",
		},
	},
	// SystemRescue (Arch/archiso) for the rescue boot target. Unlike
	// the casper distros, archiso fetches the squashfs itself at
	// ${archiso_http_srv}${archisobasedir}/x86_64/airootfs.sfs, so we
	// MUST preserve the native paths — the dest keys mirror the ISO
	// layout exactly and are served verbatim under /assets/systemrescue/.
	// No airootfs.sha512 / `checksum` cmdline: keeps the asset set to
	// the three files we can rely on, and integrity over a trusted LAN
	// rescue boot isn't load-bearing.
	"systemrescue": {
		Name:   "systemrescue",
		ISOURL: "https://fastly-cdn.system-rescue.org/releases/13.00/systemrescue-13.00-amd64.iso",
		ISOPath: map[string]string{
			"sysresccd/boot/x86_64/vmlinuz":       "/sysresccd/boot/x86_64/vmlinuz",
			"sysresccd/boot/x86_64/sysresccd.img": "/sysresccd/boot/x86_64/sysresccd.img",
			"sysresccd/x86_64/airootfs.sfs":       "/sysresccd/x86_64/airootfs.sfs",
		},
	},
}

// Manifest records what's in a populated target dir. Lives next to
// the extracted files as .pxe-beacon-fetched (JSON).
type Manifest struct {
	Target    string           `json:"target"`
	Source    string           `json:"source"`
	FetchedAt time.Time        `json:"fetched_at"`
	Files     map[string]Asset `json:"files"`
}

type Asset struct {
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Cache is the on-disk data dir.
type Cache struct {
	Root string
}

// New returns a Cache rooted at dir. dir is created if missing.
func New(dir string) (*Cache, error) {
	if dir == "" {
		return nil, errors.New("cache: empty data dir")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("cache: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("cache: mkdir %s: %w", abs, err)
	}
	return &Cache{Root: abs}, nil
}

// TargetDir returns the directory for `target` (created if missing).
// Returns "" + error if target name is invalid (contains traversal,
// has slashes, etc.).
func (c *Cache) TargetDir(target string) (string, error) {
	if !safeTargetName(target) {
		return "", fmt.Errorf("invalid target name %q", target)
	}
	p := filepath.Join(c.Root, target)
	if err := os.MkdirAll(p, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", p, err)
	}
	return p, nil
}

// AssetPath returns the absolute path to a target's asset file
// (without checking it exists). The file may be a nested, '/'-separated
// path (e.g. sysresccd/x86_64/airootfs.sfs); every segment is validated
// to reject path traversal. Returns "" + error on invalid input.
func (c *Cache) AssetPath(target, file string) (string, error) {
	if !safeTargetName(target) {
		return "", fmt.Errorf("invalid target name %q", target)
	}
	if !safeAssetPath(file) {
		return "", fmt.Errorf("invalid asset name %q", file)
	}
	return filepath.Join(c.Root, target, filepath.FromSlash(file)), nil
}

// IsPopulated reports whether a target has all expected files (per its
// TargetSpec) plus a valid manifest. Returns (true, manifest) if so.
func (c *Cache) IsPopulated(target string) (bool, *Manifest) {
	spec, ok := Targets[target]
	if !ok {
		return false, nil
	}
	tdir := filepath.Join(c.Root, target)
	m, err := readManifest(tdir)
	if err != nil {
		return false, nil
	}
	for _, dest := range spec.Dests() {
		fi, err := os.Stat(filepath.Join(tdir, filepath.FromSlash(dest)))
		if err != nil || fi.Size() == 0 {
			return false, nil
		}
	}
	return true, m
}

// WriteManifest serializes m next to the extracted files.
func (c *Cache) WriteManifest(target string, m *Manifest) error {
	tdir, err := c.TargetDir(target)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(tdir, ".pxe-beacon-fetched.tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(tdir, ".pxe-beacon-fetched"))
}

func readManifest(dir string) (*Manifest, error) {
	b, err := os.ReadFile(filepath.Join(dir, ".pxe-beacon-fetched"))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// safeTargetName rejects anything path-traversal-y. Only allow
// alphanumerics, dashes, dots, underscores; no slashes, no leading dot.
// safeAssetPath validates each segment of a nested asset path with it.
func safeTargetName(s string) bool {
	if s == "" || strings.HasPrefix(s, ".") {
		return false
	}
	for _, r := range s {
		if !(r == '-' || r == '_' || r == '.' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
			return false
		}
	}
	return true
}

// safeAssetPath validates a '/'-separated relative asset path. Each
// segment must pass safeTargetName (which rejects "..", leading dots,
// and embedded slashes), so the joined result can't escape the target
// dir. Rejects absolute paths and empty input.
func safeAssetPath(s string) bool {
	if s == "" || strings.HasPrefix(s, "/") {
		return false
	}
	for _, seg := range strings.Split(s, "/") {
		if !safeTargetName(seg) {
			return false
		}
	}
	return true
}

// SHA256File returns the hex SHA-256 of a file. Used by the
// manifest so re-runs can detect tampering / corruption.
func SHA256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
