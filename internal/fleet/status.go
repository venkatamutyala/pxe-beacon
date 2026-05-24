package fleet

import (
	"sort"
	"sync"
	"time"
)

// Event names align with the status model in the v0.2 plan. They're
// stable strings — the status page + JSON consumers depend on them.
type Event string

const (
	EventFirmwareDHCP    Event = "firmware-dhcp"
	EventFirmwareFetched Event = "firmware-fetched"
	EventIPXEDHCP        Event = "ipxe-dhcp"
	EventUserDataFetched Event = "user-data-fetched"
	EventInstallerDone   Event = "installer-done"
)

// Order defines the canonical progression order for events. Used to
// compute "have we gone backwards" and to label the latest stage.
var eventOrder = map[Event]int{
	EventFirmwareDHCP:    1,
	EventFirmwareFetched: 2,
	EventIPXEDHCP:        3,
	EventUserDataFetched: 4,
	EventInstallerDone:   5,
}

// Status is the public snapshot for one machine.
type Status struct {
	MAC      string    `json:"mac"`
	Name     string    `json:"name,omitempty"`
	Profile  string    `json:"profile"`
	State    Event     `json:"state,omitempty"` // "" → pending
	LastSeen time.Time `json:"last_seen,omitempty"`
	Stalled  bool      `json:"stalled,omitempty"`
	Events   []Event   `json:"events,omitempty"`
}

// Tracker is the in-memory store of per-MAC live status. Safe for
// concurrent use; written by proxydhcp/tftp/httpd, read by the status
// endpoints.
type Tracker struct {
	mu           sync.RWMutex
	fleet        *Fleet
	machines     map[string]*Status // canonical-MAC → live status (only populated once seen)
	stalledAfter time.Duration
	now          func() time.Time // injectable for tests
}

// NewTracker wires up the tracker. `stalledAfter` is the age after
// which a machine that's in-progress (not pending, not done) gets
// flagged "stalled" in snapshots.
func NewTracker(f *Fleet, stalledAfter time.Duration) *Tracker {
	if stalledAfter <= 0 {
		stalledAfter = 5 * time.Minute
	}
	return &Tracker{
		fleet:        f,
		machines:     map[string]*Status{},
		stalledAfter: stalledAfter,
		now:          time.Now,
	}
}

// Note records an event for a MAC. Unknown MACs are tracked too (so
// the status page can show "saw an unconfigured machine"). Idempotent
// for the same event arriving repeatedly.
func (t *Tracker) Note(mac string, ev Event) {
	canon, err := CanonicalMAC(mac)
	if err != nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.machines[canon]
	if !ok {
		s = &Status{MAC: canon}
		t.machines[canon] = s
	}
	// Resolve name + profile from fleet (re-resolve each time —
	// fleet config can change via SIGHUP reload).
	if t.fleet != nil {
		p := t.fleet.Lookup(canon)
		s.Name = p.Name
		s.Profile = p.Boot
	}
	// Only advance the state if this event is at-or-past the
	// current one (avoids regressing on out-of-order packets).
	if rank(ev) >= rank(s.State) {
		s.State = ev
	}
	// Append to event history if it's not a duplicate of the last
	// entry.
	if len(s.Events) == 0 || s.Events[len(s.Events)-1] != ev {
		s.Events = append(s.Events, ev)
	}
	s.LastSeen = t.now()
}

// Snapshot returns the current status for every machine — both
// configured ones (even if never seen) and observed-but-unconfigured
// MACs. Sorted by name (configured first), then MAC. Stalled flag
// computed at snapshot time.
func (t *Tracker) Snapshot() []Status {
	t.mu.RLock()
	defer t.mu.RUnlock()

	out := []Status{}
	now := t.now()
	stallCutoff := now.Add(-t.stalledAfter)

	// 1. Every configured machine — observed or pending.
	if t.fleet != nil {
		for canon, p := range t.fleet.Machines() {
			if live, seen := t.machines[canon]; seen {
				cp := *live
				cp.Profile = p.Boot
				cp.Name = p.Name
				cp.Stalled = isStalled(cp, stallCutoff)
				out = append(out, cp)
			} else {
				out = append(out, Status{
					MAC:     canon,
					Name:    p.Name,
					Profile: p.Boot,
				})
			}
		}
	}

	// 2. Unknown MACs we've observed (no fleet entry).
	knownSet := map[string]bool{}
	if t.fleet != nil {
		for k := range t.fleet.Machines() {
			knownSet[k] = true
		}
	}
	for canon, live := range t.machines {
		if knownSet[canon] {
			continue
		}
		cp := *live
		cp.Stalled = isStalled(cp, stallCutoff)
		out = append(out, cp)
	}

	sort.SliceStable(out, func(i, j int) bool {
		// configured (has name) sorts before unconfigured
		if (out[i].Name != "") != (out[j].Name != "") {
			return out[i].Name != ""
		}
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].MAC < out[j].MAC
	})
	return out
}

func isStalled(s Status, cutoff time.Time) bool {
	if s.State == "" || s.State == EventInstallerDone {
		return false
	}
	if s.LastSeen.IsZero() {
		return false
	}
	return s.LastSeen.Before(cutoff)
}

func rank(ev Event) int {
	if ev == "" {
		return 0
	}
	return eventOrder[ev]
}
