package installlog

import (
	"bytes"
	"strings"
	"testing"
)

func TestAppendGet(t *testing.T) {
	s := New()
	mac := "58:47:ca:70:c7:c9"
	s.Append(mac, []byte("hello "))
	s.Append(mac, []byte("world"))
	if got := string(s.Get(mac)); got != "hello world" {
		t.Fatalf("Get = %q, want %q", got, "hello world")
	}
	// Canonicalization: hyphen form resolves to the same entry.
	if got := string(s.Get("58-47-ca-70-c7-c9")); got != "hello world" {
		t.Fatalf("hyphen-form Get = %q", got)
	}
}

func TestAppend_TrimsToCap(t *testing.T) {
	s := New()
	mac := "58:47:ca:70:c7:c9"
	s.Append(mac, bytes.Repeat([]byte("a"), MaxPerMAC))
	s.Append(mac, []byte("TAIL"))
	got := s.Get(mac)
	if len(got) != MaxPerMAC {
		t.Fatalf("len = %d, want %d (capped)", len(got), MaxPerMAC)
	}
	if !strings.HasSuffix(string(got), "TAIL") {
		t.Errorf("retained tail should end in TAIL, got ...%q", got[len(got)-8:])
	}
}

func TestGet_ReturnsCopy(t *testing.T) {
	s := New()
	mac := "58:47:ca:70:c7:c9"
	s.Append(mac, []byte("abc"))
	got := s.Get(mac)
	got[0] = 'X'
	if string(s.Get(mac)) != "abc" {
		t.Error("Get must return a copy; mutating it leaked into the store")
	}
}

func TestRetainOnly(t *testing.T) {
	s := New()
	s.Append("58:47:ca:70:c7:c9", []byte("keep"))
	s.Append("aa:bb:cc:dd:ee:ff", []byte("drop"))
	removed := s.RetainOnly(func(mac string) bool { return mac == "58:47:ca:70:c7:c9" })
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if s.Get("aa:bb:cc:dd:ee:ff") != nil {
		t.Error("dropped MAC should have no log")
	}
	if string(s.Get("58:47:ca:70:c7:c9")) != "keep" {
		t.Error("retained MAC lost its log")
	}
}
