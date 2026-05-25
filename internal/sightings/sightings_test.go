package sightings

import (
	"fmt"
	"testing"
	"time"
)

func TestNote_DedupsByMAC(t *testing.T) {
	s := New()
	mac := "dc:a6:32:11:22:33"
	s.Note(mac, "arm64 UEFI", "PXEClient:Arch:00011")
	s.Note(mac, "arm64 UEFI", "PXEClient:Arch:00011")
	s.Note("DC-A6-32-11-22-33", "arm64 UEFI", "") // hyphen form, same MAC
	list := s.List()
	if len(list) != 1 {
		t.Fatalf("want 1 deduped sighting, got %d", len(list))
	}
	if list[0].Count != 3 {
		t.Errorf("count = %d, want 3", list[0].Count)
	}
	if list[0].Vendor != "Raspberry Pi" {
		t.Errorf("vendor = %q, want Raspberry Pi (from OUI)", list[0].Vendor)
	}
}

func TestList_SortedByLastSeenDesc(t *testing.T) {
	s := New()
	base := time.Unix(1_700_000_000, 0)
	i := 0
	s.now = func() time.Time { return base.Add(time.Duration(i) * time.Second) }
	for _, mac := range []string{"00:00:00:00:00:01", "00:00:00:00:00:02", "00:00:00:00:00:03"} {
		i++
		s.Note(mac, "x86_64 UEFI", "")
	}
	list := s.List()
	if list[0].MAC != "00:00:00:00:00:03" {
		t.Errorf("newest should be first, got %s", list[0].MAC)
	}
}

func TestForgetAndRetainOnly(t *testing.T) {
	s := New()
	s.Note("00:00:00:00:00:01", "x86", "")
	s.Note("00:00:00:00:00:02", "x86", "")
	if !s.Forget("00:00:00:00:00:01") {
		t.Error("Forget should report the entry existed")
	}
	if s.Len() != 1 {
		t.Fatalf("len = %d, want 1 after Forget", s.Len())
	}
	// RetainOnly drops MACs the predicate rejects (e.g. now-enrolled).
	removed := s.RetainOnly(func(mac string) bool { return false })
	if removed != 1 || s.Len() != 0 {
		t.Errorf("RetainOnly removed=%d len=%d, want 1/0", removed, s.Len())
	}
}

func TestNote_EvictsOldestAtCap(t *testing.T) {
	s := New()
	base := time.Unix(1_700_000_000, 0)
	i := 0
	s.now = func() time.Time { i++; return base.Add(time.Duration(i) * time.Second) }
	// Fill past capacity; MAC ...0001 is the oldest and should be evicted.
	for n := 1; n <= MaxEntries+1; n++ {
		s.Note(fmt.Sprintf("02:00:00:00:%02x:%02x", n>>8, n&0xff), "x86", "")
	}
	if s.Len() != MaxEntries {
		t.Fatalf("len = %d, want %d (bounded)", s.Len(), MaxEntries)
	}
}

func TestVendorForMAC(t *testing.T) {
	if v := VendorForMAC("52:54:00:12:34:56"); v != "QEMU/KVM" {
		t.Errorf("QEMU OUI = %q", v)
	}
	if v := VendorForMAC(" ab:cd:ef:00:00:00"); v != "" {
		t.Errorf("unknown OUI should be empty, got %q", v)
	}
}
