// Package armstate tracks per-machine arming state for pxe-beacon.
//
// A machine is "armed" when an operator has explicitly opted it in for
// a PXE install via POST /api/v1/machines/{mac}/arm. The proxyDHCP
// listener skips OFFER for fleet members that aren't armed, so a
// power-on without arming falls through to local-disk boot.
//
// Arming is in-memory only — restart clears everything. That's
// deliberate: a forgotten arm can't survive a power blip and
// accidentally reinstall a machine days later. The 15-minute (default)
// TTL is a second layer of the same protection.
package armstate

import (
	"sync"
	"time"

	"github.com/venkatamutyala/pxe-beacon/internal/fleet"
)

// Store holds the live arming map. Safe for concurrent use.
type Store struct {
	mu      sync.RWMutex
	armings map[string]time.Time // canonical MAC → armed-at timestamp
	ttl     time.Duration
	now     func() time.Time
}

// New returns a Store that auto-expires armings after ttl. Pass 0 or
// negative to disable expiry (armings stay until manual disarm or
// install completion).
func New(ttl time.Duration) *Store {
	return &Store{
		armings: map[string]time.Time{},
		ttl:     ttl,
		now:     time.Now,
	}
}

// Arm marks mac as armed at the current time. If the MAC was already
// armed, the timestamp resets (re-arming extends the window). Returns
// the resulting expiry time (zero when ttl <= 0).
func (s *Store) Arm(mac string) (expiresAt time.Time, err error) {
	canon, err := fleet.CanonicalMAC(mac)
	if err != nil {
		return time.Time{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.now()
	s.armings[canon] = t
	if s.ttl > 0 {
		return t.Add(s.ttl), nil
	}
	return time.Time{}, nil
}

// Disarm removes mac from the armed set. Returns true if mac was
// armed prior to the call. Invalid MACs return false.
func (s *Store) Disarm(mac string) bool {
	canon, err := fleet.CanonicalMAC(mac)
	if err != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, existed := s.armings[canon]
	delete(s.armings, canon)
	return existed
}

// IsArmed reports whether mac is currently armed (present in the map
// AND not yet expired). Expired entries are not removed by this read —
// the next write call (or Snapshot) reaps them. Invalid MACs return
// false.
func (s *Store) IsArmed(mac string) bool {
	canon, err := fleet.CanonicalMAC(mac)
	if err != nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	armedAt, ok := s.armings[canon]
	if !ok {
		return false
	}
	if s.ttl <= 0 {
		return true
	}
	return s.now().Sub(armedAt) < s.ttl
}

// Status returns the per-machine arming view. When armed is false, the
// timestamps are zero values. Invalid MACs return armed=false.
func (s *Store) Status(mac string) (armedAt, expiresAt time.Time, armed bool) {
	canon, err := fleet.CanonicalMAC(mac)
	if err != nil {
		return time.Time{}, time.Time{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	armedAt, ok := s.armings[canon]
	if !ok {
		return time.Time{}, time.Time{}, false
	}
	if s.ttl > 0 {
		expiresAt = armedAt.Add(s.ttl)
		if !s.now().Before(expiresAt) {
			return time.Time{}, time.Time{}, false
		}
	}
	return armedAt, expiresAt, true
}

// TTL returns the configured expiry duration. 0 means no expiry.
func (s *Store) TTL() time.Duration {
	return s.ttl
}
