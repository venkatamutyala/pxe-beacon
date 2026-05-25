// Package sightings records unknown MACs that PXE-boot on the segment so
// operators can enroll them without hand-typing MAC addresses.
//
// proxyDHCP sees every DISCOVER, including from machines not in fleet.yaml.
// Today those fall through to the netboot.xyz menu and are forgotten; this
// store captures them (deduped by MAC, with arch + vendor + first/last-seen
// + count) for the discovery feed at /api/v1/discovered and the /admin
// "Discovered" panel.
//
// Like internal/pending and internal/installlog it is RAM-only — cleared on
// restart, bounded in size, and pruned to drop MACs that become known.
package sightings

import (
	"sort"
	"sync"
	"time"

	"github.com/venkatamutyala/pxe-beacon/internal/fleet"
)

// MaxEntries caps how many distinct MACs we retain. When full, the
// least-recently-seen entry is evicted so a busy segment can't grow the
// store without bound.
const MaxEntries = 256

// Sighting is the public view of one discovered MAC.
type Sighting struct {
	MAC         string    `json:"mac"`
	Arch        string    `json:"arch"`         // human label, e.g. "x86_64 UEFI"
	Vendor      string    `json:"vendor"`       // from the OUI table; "" if unknown
	VendorClass string    `json:"vendor_class"` // raw DHCP option-60 string
	FirstSeen   time.Time `json:"first_seen"`
	LastSeen    time.Time `json:"last_seen"`
	Count       int       `json:"count"`
}

type entry struct {
	arch        string
	vendorClass string
	firstSeen   time.Time
	lastSeen    time.Time
	count       int
}

// Store holds per-MAC sightings. Safe for concurrent use.
type Store struct {
	mu      sync.Mutex
	entries map[string]*entry // canonical MAC -> entry
	now     func() time.Time  // injectable for tests
}

// New returns an empty Store.
func New() *Store {
	return &Store{entries: map[string]*entry{}, now: time.Now}
}

// Note records a DISCOVER from mac. Repeated sightings bump count +
// last_seen rather than adding rows. Invalid MACs are ignored. When the
// store is at capacity and mac is new, the least-recently-seen entry is
// evicted.
func (s *Store) Note(mac, archLabel, vendorClass string) {
	canon, err := fleet.CanonicalMAC(mac)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.now()
	if e, ok := s.entries[canon]; ok {
		e.lastSeen = t
		e.count++
		// Keep the freshest arch/vendor-class we've seen.
		if archLabel != "" {
			e.arch = archLabel
		}
		if vendorClass != "" {
			e.vendorClass = vendorClass
		}
		return
	}
	if len(s.entries) >= MaxEntries {
		s.evictOldestLocked()
	}
	s.entries[canon] = &entry{
		arch:        archLabel,
		vendorClass: vendorClass,
		firstSeen:   t,
		lastSeen:    t,
		count:       1,
	}
}

// List returns all sightings, newest-last-seen first. Vendor is resolved
// from the MAC's OUI at read time.
func (s *Store) List() []Sighting {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Sighting, 0, len(s.entries))
	for mac, e := range s.entries {
		out = append(out, Sighting{
			MAC:         mac,
			Arch:        e.arch,
			Vendor:      VendorForMAC(mac),
			VendorClass: e.vendorClass,
			FirstSeen:   e.firstSeen,
			LastSeen:    e.lastSeen,
			Count:       e.count,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastSeen.After(out[j].LastSeen) })
	return out
}

// Forget drops the sighting for mac. Returns true if it existed.
func (s *Store) Forget(mac string) bool {
	canon, err := fleet.CanonicalMAC(mac)
	if err != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.entries[canon]
	delete(s.entries, canon)
	return ok
}

// RetainOnly drops sightings whose MAC the `keep` predicate rejects. Used
// to forget MACs that have become fleet members (a discovered box should
// vanish from the feed once enrolled). Returns the number dropped.
func (s *Store) RetainOnly(keep func(mac string) bool) (removed int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for mac := range s.entries {
		if !keep(mac) {
			delete(s.entries, mac)
			removed++
		}
	}
	return removed
}

// Len returns the number of distinct MACs retained.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// evictOldestLocked removes the entry with the oldest last_seen. Caller
// must hold s.mu.
func (s *Store) evictOldestLocked() {
	var oldestMAC string
	var oldest time.Time
	for mac, e := range s.entries {
		if oldestMAC == "" || e.lastSeen.Before(oldest) {
			oldestMAC, oldest = mac, e.lastSeen
		}
	}
	if oldestMAC != "" {
		delete(s.entries, oldestMAC)
	}
}
