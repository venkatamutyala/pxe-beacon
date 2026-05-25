package fleet

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// AddOrUpdate inserts or replaces a machine entry in the in-memory
// fleet. mac is canonicalized first. Validation runs before commit;
// on validation failure the in-memory state is unchanged.
func (f *Fleet) AddOrUpdate(m Machine) error {
	canon, err := CanonicalMAC(m.MAC)
	if err != nil {
		return fmt.Errorf("invalid mac %q: %w", m.MAC, err)
	}
	if err := validateProfile(m.Profile, fmt.Sprintf("machine %q", m.Profile.Name)); err != nil {
		return err
	}
	f.mu.Lock()
	if f.machines == nil {
		f.machines = make(map[string]Profile)
	}
	f.machines[canon] = m.Profile
	f.mu.Unlock()
	return nil
}

// Remove deletes a machine entry by MAC. Returns true if the entry
// existed and was removed.
func (f *Fleet) Remove(mac string) (bool, error) {
	canon, err := CanonicalMAC(mac)
	if err != nil {
		return false, fmt.Errorf("invalid mac %q: %w", mac, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.machines[canon]; !ok {
		return false, nil
	}
	delete(f.machines, canon)
	return true, nil
}

// Save serializes the current in-memory fleet to f.path as YAML.
// Atomic via temp-file rename. Comments in the previous on-disk
// content are LOST — this is the documented trade-off for UI-driven
// edits. Operators who want comments preserved should hand-edit
// fleet.yaml and SIGHUP-reload instead.
//
// The serializer relativizes paths back to the fleet.yaml directory
// so disk output stays diff-friendly across edits.
func (f *Fleet) Save() error {
	if f.path == "" {
		return fmt.Errorf("fleet: Save on Empty fleet (no -config)")
	}

	f.mu.RLock()
	cfg := configFile{
		Defaults: defaultsEntry{
			Boot:       f.defaults.Boot,
			CloudInit:  relativize(f.baseDir, f.defaults.CloudInit),
			Preseed:    relativize(f.baseDir, f.defaults.Preseed),
			Kickstart:  relativize(f.baseDir, f.defaults.Kickstart),
			IPXEScript: relativize(f.baseDir, f.defaults.IPXEScript),
			Params:     f.defaults.Params,
		},
	}
	for canon, p := range f.machines {
		cfg.Machines = append(cfg.Machines, machineYAML{
			MAC:        canon,
			Name:       p.Name,
			Boot:       p.Boot,
			CloudInit:  relativize(f.baseDir, p.CloudInit),
			Preseed:    relativize(f.baseDir, p.Preseed),
			Kickstart:  relativize(f.baseDir, p.Kickstart),
			IPXEScript: relativize(f.baseDir, p.IPXEScript),
			// p.Params here is the machine's OWN params (Lookup does
			// the defaults-merge on read; the stored map is own-only),
			// so this round-trips without denormalizing defaults.
			Params: p.Params,
		})
	}
	f.mu.RUnlock()

	// Sort machines by name (then MAC) for stable diffs.
	sort.SliceStable(cfg.Machines, func(i, j int) bool {
		if cfg.Machines[i].Name != cfg.Machines[j].Name {
			return cfg.Machines[i].Name < cfg.Machines[j].Name
		}
		return cfg.Machines[i].MAC < cfg.Machines[j].MAC
	})

	var buf bytes.Buffer
	buf.WriteString("# fleet.yaml — written by pxe-beacon (admin UI).\n")
	buf.WriteString("# Hand-edits are fine; SIGHUP $(pgrep -x pxe-beacon) to reload.\n")
	buf.WriteString("# Note: UI saves overwrite comments. Edit by hand to preserve them.\n\n")
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&cfg); err != nil {
		return fmt.Errorf("encode fleet: %w", err)
	}
	_ = enc.Close()

	dir := filepath.Dir(f.path)
	tmp, err := os.CreateTemp(dir, ".fleet.yaml.*.tmp")
	if err != nil {
		return fmt.Errorf("create tmp: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close tmp: %w", err)
	}
	if err := os.Rename(tmpName, f.path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("rename tmp -> %s: %w", f.path, err)
	}
	f.log.Infof("fleet: saved %d machines to %s", len(cfg.Machines), f.path)
	return nil
}

// relativize returns p relative to base if p is under base; otherwise
// p unchanged. Keeps fleet.yaml diff-friendly when fields point at
// files alongside it (the common case).
func relativize(base, p string) string {
	if p == "" || base == "" {
		return p
	}
	rel, err := filepath.Rel(base, p)
	if err != nil || strings.HasPrefix(rel, "..") {
		return p
	}
	return "./" + rel
}

// ListMachines returns all configured machines in stable order
// (by name, then MAC). Used by the admin UI for the fleet table.
func (f *Fleet) ListMachines() []Machine {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]Machine, 0, len(f.machines))
	for mac, p := range f.machines {
		out = append(out, Machine{MAC: mac, Profile: p})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Profile.Name != out[j].Profile.Name {
			return out[i].Profile.Name < out[j].Profile.Name
		}
		return out[i].MAC < out[j].MAC
	})
	return out
}
