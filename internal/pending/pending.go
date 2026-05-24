// Package pending tracks per-machine pending actions for pxe-beacon.
//
// An operator queues an action against a MAC via the REST API:
//   - POST /api/v1/machines/{mac}/deploy  → Action == ActionDeploy
//   - POST /api/v1/machines/{mac}/rescue  → Action == ActionRescue (v0.7.1 stub)
//   - POST /api/v1/machines/{mac}/cancel  → clears any pending action
//
// proxyDHCP consults IsPending(mac) before responding: no pending action,
// no OFFER, and the client falls through to local-disk boot.
//
// Storage is in-memory only — restart clears every pending action. That
// matches the v0.7.0 design rationale: a forgotten action can't survive
// a power blip and accidentally reinstall a box days later. The TTL
// (default 15 min) is a second layer of the same protection.
//
// The naming follows MaaS's verb model: `deploy` and `rescue` are
// peer actions, `cancel` is the universal antonym. See PROGRESS for
// the v0.7.1 TPM review that landed on this vocabulary.
package pending

import (
	"sync"
	"time"

	"github.com/venkatamutyala/pxe-beacon/internal/fleet"
)

// Action names the kind of operation queued against a machine. Stable
// strings — they appear in JSON, log lines, and the admin UI.
type Action string

const (
	ActionDeploy Action = "deploy"
	ActionRescue Action = "rescue"
)

// entry is the stored value: which action was queued and when.
type entry struct {
	action      Action
	requestedAt time.Time
}

// Store holds the per-MAC queue. Safe for concurrent use.
type Store struct {
	mu      sync.RWMutex
	entries map[string]entry // canonical MAC → entry
	ttl     time.Duration
	now     func() time.Time
}

// New returns a Store. ttl is the auto-expiry duration; pass 0 or
// negative to disable expiry (entries stay until manual Cancel or
// successful install).
func New(ttl time.Duration) *Store {
	return &Store{
		entries: map[string]entry{},
		ttl:     ttl,
		now:     time.Now,
	}
}

// Deploy queues a deploy action for mac. Re-queueing resets the timer.
// Returns the resulting expiry time (zero when ttl <= 0).
func (s *Store) Deploy(mac string) (time.Time, error) {
	return s.queue(mac, ActionDeploy)
}

// Rescue queues a rescue action for mac. Same semantics as Deploy.
func (s *Store) Rescue(mac string) (time.Time, error) {
	return s.queue(mac, ActionRescue)
}

func (s *Store) queue(mac string, action Action) (time.Time, error) {
	canon, err := fleet.CanonicalMAC(mac)
	if err != nil {
		return time.Time{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.now()
	s.entries[canon] = entry{action: action, requestedAt: t}
	if s.ttl > 0 {
		return t.Add(s.ttl), nil
	}
	return time.Time{}, nil
}

// Cancel removes the pending action for mac. Returns true if there
// was anything pending. Invalid MACs return false.
func (s *Store) Cancel(mac string) bool {
	canon, err := fleet.CanonicalMAC(mac)
	if err != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, existed := s.entries[canon]
	delete(s.entries, canon)
	return existed
}

// IsPending reports whether mac has a non-expired pending action.
// Invalid MACs return false. Read-only; expired entries are reaped
// lazily by writes (Deploy/Rescue/Cancel), not here.
func (s *Store) IsPending(mac string) bool {
	_, _, _, ok := s.Status(mac)
	return ok
}

// Status returns the current pending state for mac. When ok is false,
// the other return values are zero.
func (s *Store) Status(mac string) (action Action, requestedAt, expiresAt time.Time, ok bool) {
	canon, err := fleet.CanonicalMAC(mac)
	if err != nil {
		return "", time.Time{}, time.Time{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, present := s.entries[canon]
	if !present {
		return "", time.Time{}, time.Time{}, false
	}
	if s.ttl > 0 {
		expiresAt = e.requestedAt.Add(s.ttl)
		if !s.now().Before(expiresAt) {
			return "", time.Time{}, time.Time{}, false
		}
	}
	return e.action, e.requestedAt, expiresAt, true
}

// TTL returns the configured expiry duration. 0 means no expiry.
func (s *Store) TTL() time.Duration {
	return s.ttl
}
