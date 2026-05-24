// Package fleet loads the fleet.yaml config and exposes per-MAC
// boot-profile lookups for the rest of pxe-beacon. It also owns the
// in-memory status tracker (see status.go).
//
// Design: this package has no dependencies on any other internal/*
// package. proxydhcp/tftp/httpd import fleet, not the other way
// around. That keeps cycles impossible and makes the fleet config
// independently testable.
package fleet

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/venkatamutyala/pxe-beacon/internal/narrlog"
)

// ValidBootTargets is the set of `boot:` values recognized by v0.2.0.
// Adding a new built-in target = add it here + add a template under
// internal/assets/scripts/autoexec/. `custom` means "serve the
// user-provided iPXE script verbatim."
var ValidBootTargets = map[string]bool{
	"menu":         true,
	"ubuntu-22.04": true,
	"ubuntu-24.04": true,
	"debian-12":    true,
	"debian-13":    true,
	"custom":       true,
}

// Profile is the resolved per-MAC boot configuration. It's what
// callers get back from Fleet.Lookup. Plain values, no pointers — safe
// to copy.
type Profile struct {
	// Boot target name (see ValidBootTargets).
	Boot string

	// Name is the operator-friendly machine name (e.g. "kube-1").
	// Empty for unknown MACs falling through to defaults.
	Name string

	// CloudInit is an absolute path to the user-data file. Empty
	// when boot==menu or boot==custom.
	CloudInit string

	// IPXEScript is an absolute path to the raw iPXE script the
	// operator wants served verbatim. Only meaningful when
	// boot==custom; empty otherwise.
	IPXEScript string

	// IsDefault is true when this profile came from `defaults:` (i.e.
	// the MAC is unknown to the fleet config). Useful for logging.
	IsDefault bool
}

// configFile is the on-disk YAML shape.
type configFile struct {
	Defaults defaultsEntry `yaml:"defaults"`
	Machines []machineYAML `yaml:"machines"`
}

type defaultsEntry struct {
	Boot       string `yaml:"boot"`
	CloudInit  string `yaml:"cloud_init"`
	IPXEScript string `yaml:"ipxe_script"`
}

type machineYAML struct {
	MAC        string `yaml:"mac"`
	Name       string `yaml:"name"`
	Boot       string `yaml:"boot"`
	CloudInit  string `yaml:"cloud_init"`
	IPXEScript string `yaml:"ipxe_script"`
}

// Fleet is the live in-memory store. It satisfies the lookup
// interface proxydhcp + tftp + httpd consume.
type Fleet struct {
	mu       sync.RWMutex
	path     string             // path to fleet.yaml; empty for Empty()
	baseDir  string             // directory of path (for resolving relative cloud_init / ipxe_script)
	machines map[string]Profile // canonical-MAC → resolved Profile
	defaults Profile
	log      *narrlog.Logger
}

// Empty returns a Fleet with no machines and a "menu" default — used
// when -config isn't passed. Preserves v0.1.3 behavior exactly: every
// client gets the netboot.xyz menu.
func Empty(log *narrlog.Logger) *Fleet {
	if log == nil {
		log = narrlog.New("fleet", narrlog.LevelInfo, nil)
	}
	return &Fleet{
		machines: map[string]Profile{},
		defaults: Profile{Boot: "menu", IsDefault: true},
		log:      log.With("fleet"),
	}
}

// Load reads, parses, and validates a fleet.yaml from disk. Returns
// a populated Fleet on success. Validation errors include the
// machine name / line context where possible.
func Load(path string, log *narrlog.Logger) (*Fleet, error) {
	if log == nil {
		log = narrlog.New("fleet", narrlog.LevelInfo, nil)
	}
	f := &Fleet{
		path: path,
		log:  log.With("fleet"),
	}
	if err := f.reload(); err != nil {
		return nil, err
	}
	return f, nil
}

// Reload re-reads the underlying file. Safe to call concurrently with
// Lookup — writers hold the write lock, readers hold the read lock.
func (f *Fleet) Reload() error {
	if f.path == "" {
		return errors.New("fleet: Reload on Empty fleet (no -config)")
	}
	return f.reload()
}

func (f *Fleet) reload() error {
	raw, err := os.ReadFile(f.path)
	if err != nil {
		return fmt.Errorf("read %s: %w", f.path, err)
	}
	var cfg configFile
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("parse %s: %w", f.path, err)
	}
	baseDir, _ := filepath.Abs(filepath.Dir(f.path))

	// Defaults validation. Allow defaults to be absent → "menu".
	if cfg.Defaults.Boot == "" {
		cfg.Defaults.Boot = "menu"
	}
	if !ValidBootTargets[cfg.Defaults.Boot] {
		return fmt.Errorf("defaults.boot %q is not a recognized target (known: %s)",
			cfg.Defaults.Boot, strings.Join(knownTargetsSorted(), ", "))
	}
	defProfile := Profile{
		Boot:       cfg.Defaults.Boot,
		CloudInit:  resolvePath(baseDir, cfg.Defaults.CloudInit),
		IPXEScript: resolvePath(baseDir, cfg.Defaults.IPXEScript),
		IsDefault:  true,
	}
	if err := validateProfile(defProfile, "defaults"); err != nil {
		return err
	}

	// Machines.
	machines := make(map[string]Profile, len(cfg.Machines))
	for i, m := range cfg.Machines {
		ctx := fmt.Sprintf("machines[%d]", i)
		if m.Name != "" {
			ctx = fmt.Sprintf("machine %q", m.Name)
		}
		canon, err := CanonicalMAC(m.MAC)
		if err != nil {
			return fmt.Errorf("%s: invalid mac: %w", ctx, err)
		}
		if _, dup := machines[canon]; dup {
			return fmt.Errorf("%s: duplicate mac %s", ctx, canon)
		}
		if m.Boot == "" {
			m.Boot = "menu"
		}
		if !ValidBootTargets[m.Boot] {
			return fmt.Errorf("%s: boot %q is not a recognized target", ctx, m.Boot)
		}
		p := Profile{
			Boot:       m.Boot,
			Name:       m.Name,
			CloudInit:  resolvePath(baseDir, m.CloudInit),
			IPXEScript: resolvePath(baseDir, m.IPXEScript),
		}
		if err := validateProfile(p, ctx); err != nil {
			return err
		}
		machines[canon] = p
	}

	// Atomic swap.
	f.mu.Lock()
	f.baseDir = baseDir
	f.machines = machines
	f.defaults = defProfile
	f.mu.Unlock()

	f.log.Infof("fleet: loaded %d machines from %s (default boot=%s)",
		len(machines), f.path, defProfile.Boot)
	return nil
}

// validateProfile enforces invariants that depend on the boot target.
func validateProfile(p Profile, ctx string) error {
	switch p.Boot {
	case "custom":
		if p.IPXEScript == "" {
			return fmt.Errorf("%s: boot=custom requires ipxe_script", ctx)
		}
		if _, err := os.Stat(p.IPXEScript); err != nil {
			return fmt.Errorf("%s: ipxe_script %s: %w", ctx, p.IPXEScript, err)
		}
	case "ubuntu-22.04", "ubuntu-24.04", "debian-12", "debian-13":
		// Autoinstall targets need a cloud-init user-data file.
		// Refusing to start is intentional — see PROGRESS.md / the
		// roadmap: we don't ship a default credential.
		if p.CloudInit == "" {
			return fmt.Errorf("%s: boot=%s requires cloud_init (a user-data file with a credential / SSH key)",
				ctx, p.Boot)
		}
		if _, err := os.Stat(p.CloudInit); err != nil {
			return fmt.Errorf("%s: cloud_init %s: %w", ctx, p.CloudInit, err)
		}
	case "menu":
		// No required side-files.
	}
	return nil
}

// Lookup resolves a MAC (in any common format) to a Profile. Unknown
// MACs get the defaults profile. Lock-protected; cheap to call.
func (f *Fleet) Lookup(mac string) Profile {
	canon, err := CanonicalMAC(mac)
	if err != nil {
		f.mu.RLock()
		defer f.mu.RUnlock()
		return f.defaults
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	if p, ok := f.machines[canon]; ok {
		return p
	}
	return f.defaults
}

// Machines returns a snapshot copy of every configured machine
// (canonical-MAC → Profile). Used by the status page to enumerate
// the known fleet. Excludes the defaults entry.
func (f *Fleet) Machines() map[string]Profile {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make(map[string]Profile, len(f.machines))
	for k, v := range f.machines {
		out[k] = v
	}
	return out
}

// Defaults returns the fallthrough profile for unknown MACs.
func (f *Fleet) Defaults() Profile {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.defaults
}

// CanonicalMAC normalizes a MAC string to lowercase colon-separated
// form ("58:47:ca:70:c7:c9"). Accepts colon, hyphen, dot, or no
// separator; case-insensitive. Returns error on anything that
// doesn't parse as exactly 6 octets.
func CanonicalMAC(s string) (string, error) {
	if s == "" {
		return "", errors.New("empty MAC")
	}
	// net.ParseMAC accepts colon, hyphen, and dot-quad separators
	// for EUI-48; reject 8-byte EUI-64 by length.
	hw, err := net.ParseMAC(s)
	if err != nil {
		// Try inserting colons every 2 chars for the no-separator
		// form (e.g. "5847ca70c7c9").
		clean := strings.ReplaceAll(strings.ReplaceAll(s, " ", ""), "_", "")
		if len(clean) == 12 {
			withColons := fmt.Sprintf("%s:%s:%s:%s:%s:%s",
				clean[0:2], clean[2:4], clean[4:6], clean[6:8], clean[8:10], clean[10:12])
			if hw2, err2 := net.ParseMAC(withColons); err2 == nil {
				hw = hw2
			} else {
				return "", err
			}
		} else {
			return "", err
		}
	}
	if len(hw) != 6 {
		return "", fmt.Errorf("expected EUI-48 (6 bytes), got %d", len(hw))
	}
	return strings.ToLower(hw.String()), nil
}

func resolvePath(base, p string) string {
	if p == "" {
		return ""
	}
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(base, p)
}

func knownTargetsSorted() []string {
	out := make([]string, 0, len(ValidBootTargets))
	for k := range ValidBootTargets {
		out = append(out, k)
	}
	// stable-ish output; doesn't matter much
	return out
}
