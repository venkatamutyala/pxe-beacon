package fleet

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTrackerForTest(t *testing.T) (*Tracker, *Fleet) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "u.yaml"), []byte("#cloud-config"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fleet.yaml"), []byte(`
machines:
  - mac: 58:47:ca:70:c7:c9
    name: kube-1
    boot: ubuntu-22.04
    cloud_init: ./u.yaml
  - mac: aa:bb:cc:dd:ee:01
    name: kube-2
    boot: menu
`), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := Load(filepath.Join(dir, "fleet.yaml"), newLog())
	if err != nil {
		t.Fatal(err)
	}
	tr := NewTracker(f, 30*time.Second)
	return tr, f
}

func TestTracker_NoteAndSnapshot(t *testing.T) {
	tr, _ := newTrackerForTest(t)
	tr.now = func() time.Time { return time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC) }

	tr.Note("58:47:ca:70:c7:c9", EventFirmwareDHCP)
	tr.Note("58-47-CA-70-C7-C9", EventFirmwareFetched) // alt format, same MAC
	tr.Note("58:47:ca:70:c7:c9", EventInstallerDone)

	snap := tr.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot len = %d, want 2 (both configured machines)", len(snap))
	}

	// kube-1 should reflect installer-done
	var kube1, kube2 *Status
	for i := range snap {
		switch snap[i].Name {
		case "kube-1":
			kube1 = &snap[i]
		case "kube-2":
			kube2 = &snap[i]
		}
	}
	if kube1 == nil || kube2 == nil {
		t.Fatalf("missing kube-1 or kube-2 in snapshot: %+v", snap)
	}
	if kube1.State != EventInstallerDone {
		t.Errorf("kube-1 state = %q, want %q", kube1.State, EventInstallerDone)
	}
	if len(kube1.Events) != 3 {
		t.Errorf("kube-1 events = %d, want 3 (dhcp, fetched, done)", len(kube1.Events))
	}
	if kube2.State != "" {
		t.Errorf("kube-2 state = %q, want empty (pending)", kube2.State)
	}
}

func TestTracker_StateMonotonicity(t *testing.T) {
	tr, _ := newTrackerForTest(t)
	tr.Note("58:47:ca:70:c7:c9", EventIPXEDHCP)
	tr.Note("58:47:ca:70:c7:c9", EventFirmwareDHCP) // older event, should not regress State
	snap := tr.Snapshot()
	for _, s := range snap {
		if s.Name == "kube-1" {
			if s.State != EventIPXEDHCP {
				t.Errorf("State regressed to %q, want %q (later events shouldn't lower the state)",
					s.State, EventIPXEDHCP)
			}
		}
	}
}

func TestTracker_StalledFlag(t *testing.T) {
	tr, _ := newTrackerForTest(t)
	base := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	tr.now = func() time.Time { return base }
	tr.Note("58:47:ca:70:c7:c9", EventIPXEDHCP)
	// Advance time past stalledAfter (30s)
	tr.now = func() time.Time { return base.Add(45 * time.Second) }
	snap := tr.Snapshot()
	for _, s := range snap {
		if s.Name == "kube-1" {
			if !s.Stalled {
				t.Errorf("kube-1 should be flagged stalled at 45s with stalledAfter=30s")
			}
		}
	}
}

func TestTracker_InstallerDoneNotStalled(t *testing.T) {
	tr, _ := newTrackerForTest(t)
	base := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	tr.now = func() time.Time { return base }
	tr.Note("58:47:ca:70:c7:c9", EventInstallerDone)
	tr.now = func() time.Time { return base.Add(2 * time.Hour) }
	snap := tr.Snapshot()
	for _, s := range snap {
		if s.Name == "kube-1" {
			if s.Stalled {
				t.Error("installer-done machines should never be flagged stalled")
			}
		}
	}
}

func TestTracker_UnknownMacAppears(t *testing.T) {
	tr, _ := newTrackerForTest(t)
	tr.Note("00:11:22:33:44:55", EventFirmwareDHCP)
	snap := tr.Snapshot()
	var found bool
	for _, s := range snap {
		if s.MAC == "00:11:22:33:44:55" {
			found = true
			if s.Name != "" {
				t.Errorf("unknown MAC should have empty Name, got %q", s.Name)
			}
		}
	}
	if !found {
		t.Errorf("unknown MAC missing from snapshot: %+v", snap)
	}
}
