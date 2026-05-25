package fleet

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
)

// v0.9.0 transactional CRUD + ETag support.
//
// The API (unlike the single-operator admin form) is a concurrent
// writer: a Terraform provider can fire parallel PUT/DELETE calls.
// The pre-v0.9.0 AddOrUpdate+Save pattern had an unsynchronized gap —
// AddOrUpdate mutated the in-memory map under f.mu, Save serialized it
// under a separate f.mu.RLock, and a SIGHUP reload landing between the
// two could silently revert the change. These methods close that gap
// with a dedicated saveMu held across the entire compare→mutate→persist
// sequence, with rollback if Save fails.

// Sentinel errors so httpd can map to HTTP status codes without string
// matching.
var (
	ErrMACExists            = errors.New("fleet: machine already exists")
	ErrMACAbsent            = errors.New("fleet: machine not found")
	ErrPreconditionFailed   = errors.New("fleet: etag precondition failed")
	ErrPreconditionRequired = errors.New("fleet: if-match precondition required")
)

// ETag returns the weak entity tag for the machine and whether it
// exists. The tag is a content hash over the canonical MAC + every
// Profile field in fixed order, so one machine's tag is independent of
// any other's (unlike a whole-file mtime). Weak form: W/"<hex>".
func (f *Fleet) ETag(mac string) (etag string, exists bool) {
	canon, err := CanonicalMAC(mac)
	if err != nil {
		return "", false
	}
	f.mu.RLock()
	p, ok := f.machines[canon]
	f.mu.RUnlock()
	if !ok {
		return "", false
	}
	return profileETag(canon, p), true
}

// profileETag computes the weak ETag for one entry. Fixed field order;
// NUL-separated so no field-boundary ambiguity. Params are hashed with
// keys in sorted order so the tag is deterministic across map iteration
// (and so editing a param changes the tag — required for If-Match).
func profileETag(canonMAC string, p Profile) string {
	h := sha256.New()
	for _, s := range []string{
		canonMAC, p.Boot, p.Name,
		p.CloudInit, p.Preseed, p.Kickstart, p.IPXEScript,
	} {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	keys := make([]string, 0, len(p.Params))
	for k := range p.Params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{0})
		h.Write([]byte(p.Params[k]))
		h.Write([]byte{0})
	}
	sum := h.Sum(nil)
	return `W/"` + hex.EncodeToString(sum[:16]) + `"`
}

// CreateAndSave inserts a brand-new machine. Returns ErrMACExists if
// the MAC is already present (use UpdateAndSave to modify). On success
// returns the new entry's ETag. Atomic against concurrent writers and
// reloads via saveMu; rolls the in-memory map back if Save fails.
func (f *Fleet) CreateAndSave(m Machine) (etag string, err error) {
	canon, err := CanonicalMAC(m.MAC)
	if err != nil {
		return "", fmt.Errorf("invalid mac %q: %w", m.MAC, err)
	}
	if err := validateProfile(m.Profile, fmt.Sprintf("machine %q", m.Profile.Name)); err != nil {
		return "", err
	}

	f.saveMu.Lock()
	defer f.saveMu.Unlock()

	f.mu.Lock()
	if _, exists := f.machines[canon]; exists {
		f.mu.Unlock()
		return "", ErrMACExists
	}
	if f.machines == nil {
		f.machines = make(map[string]Profile)
	}
	f.machines[canon] = m.Profile
	f.mu.Unlock()

	if err := f.Save(); err != nil {
		// Roll back the in-memory insert so live state matches disk.
		f.mu.Lock()
		delete(f.machines, canon)
		f.mu.Unlock()
		return "", err
	}
	return profileETag(canon, m.Profile), nil
}

// UpdateAndSave modifies an existing machine. Returns ErrMACAbsent if
// it doesn't exist (use CreateAndSave for new). If-Match semantics:
//   - ifMatch == ""           → ErrPreconditionRequired (428)
//   - ifMatch != current ETag → ErrPreconditionFailed   (412)
//
// On success returns the new ETag. Atomic + rollback like CreateAndSave.
func (f *Fleet) UpdateAndSave(m Machine, ifMatch string) (etag string, err error) {
	canon, err := CanonicalMAC(m.MAC)
	if err != nil {
		return "", fmt.Errorf("invalid mac %q: %w", m.MAC, err)
	}
	if err := validateProfile(m.Profile, fmt.Sprintf("machine %q", m.Profile.Name)); err != nil {
		return "", err
	}
	if ifMatch == "" {
		return "", ErrPreconditionRequired
	}

	f.saveMu.Lock()
	defer f.saveMu.Unlock()

	f.mu.Lock()
	prior, exists := f.machines[canon]
	if !exists {
		f.mu.Unlock()
		return "", ErrMACAbsent
	}
	if profileETag(canon, prior) != ifMatch {
		f.mu.Unlock()
		return "", ErrPreconditionFailed
	}
	f.machines[canon] = m.Profile
	f.mu.Unlock()

	if err := f.Save(); err != nil {
		f.mu.Lock()
		f.machines[canon] = prior // restore
		f.mu.Unlock()
		return "", err
	}
	return profileETag(canon, m.Profile), nil
}

// DeleteAndSave removes a machine. Idempotent: a missing MAC returns
// existed=false, nil error (not an error — repeat deletes succeed).
// If ifMatch is non-empty it is enforced against the current entry
// (ErrPreconditionFailed on mismatch); empty ifMatch skips the check.
// Atomic + rollback like the others.
func (f *Fleet) DeleteAndSave(mac, ifMatch string) (existed bool, err error) {
	canon, err := CanonicalMAC(mac)
	if err != nil {
		return false, fmt.Errorf("invalid mac %q: %w", mac, err)
	}

	f.saveMu.Lock()
	defer f.saveMu.Unlock()

	f.mu.Lock()
	prior, exists := f.machines[canon]
	if !exists {
		f.mu.Unlock()
		return false, nil // idempotent no-op
	}
	if ifMatch != "" && profileETag(canon, prior) != ifMatch {
		f.mu.Unlock()
		return true, ErrPreconditionFailed
	}
	delete(f.machines, canon)
	f.mu.Unlock()

	if err := f.Save(); err != nil {
		f.mu.Lock()
		f.machines[canon] = prior // restore
		f.mu.Unlock()
		return true, err
	}
	return true, nil
}
