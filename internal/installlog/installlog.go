// Package installlog keeps a small, in-memory, per-MAC ring of the most
// recent diagnostic output an installer posted to pxe-beacon (kernel ring
// buffer + cloud-init / installer logs). It exists to close the "an
// unattended install failed and I have no idea why" gap without standing up
// any persistence — like internal/pending, it's RAM-only and cleared on
// restart, and pruned to the live fleet on SIGHUP reload.
package installlog

import (
	"sync"

	"github.com/venkatamutyala/pxe-beacon/internal/fleet"
)

// MaxPerMAC caps how much we retain per machine. Logs are append-mostly and
// we only care about the tail (the failure is at the end), so once a MAC
// exceeds this we keep the last MaxPerMAC bytes.
const MaxPerMAC = 64 << 10 // 64 KiB

// Store holds the per-MAC log tails. Safe for concurrent use.
type Store struct {
	mu      sync.RWMutex
	entries map[string][]byte // canonical MAC -> last <=MaxPerMAC bytes
}

// New returns an empty Store.
func New() *Store {
	return &Store{entries: map[string][]byte{}}
}

// Append adds b to mac's log, trimming from the front so the retained tail
// never exceeds MaxPerMAC. Invalid MACs are dropped silently (the caller has
// already authenticated against the URL MAC).
func (s *Store) Append(mac string, b []byte) {
	canon, err := fleet.CanonicalMAC(mac)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := append(s.entries[canon], b...)
	if len(cur) > MaxPerMAC {
		// Keep the tail. Copy so we don't pin the larger backing array.
		tail := make([]byte, MaxPerMAC)
		copy(tail, cur[len(cur)-MaxPerMAC:])
		cur = tail
	}
	s.entries[canon] = cur
}

// Get returns a copy of mac's retained log tail (nil if none).
func (s *Store) Get(mac string) []byte {
	canon, err := fleet.CanonicalMAC(mac)
	if err != nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	b := s.entries[canon]
	if b == nil {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

// Len returns the number of MACs with retained logs. For the /readyz probe.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.entries)
}

// RetainOnly drops logs for MACs the `known` predicate rejects. Called from
// the SIGHUP reload path so removing a machine from fleet.yaml also frees its
// logs. Returns the number of MACs dropped.
func (s *Store) RetainOnly(known func(mac string) bool) (removed int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for mac := range s.entries {
		if !known(mac) {
			delete(s.entries, mac)
			removed++
		}
	}
	return removed
}
