package armstate

import (
	"sync"
	"testing"
	"time"
)

const testMAC = "58:47:ca:70:c7:c9"

func newWithClock(ttl time.Duration, now *time.Time) *Store {
	s := New(ttl)
	s.now = func() time.Time { return *now }
	return s
}

func TestArm_ThenIsArmed(t *testing.T) {
	now := time.Now()
	s := newWithClock(15*time.Minute, &now)

	if s.IsArmed(testMAC) {
		t.Fatal("fresh Store: IsArmed should be false")
	}
	exp, err := s.Arm(testMAC)
	if err != nil {
		t.Fatalf("Arm: %v", err)
	}
	if exp.IsZero() {
		t.Fatal("expected non-zero expiry with positive ttl")
	}
	if !s.IsArmed(testMAC) {
		t.Fatal("after Arm: IsArmed should be true")
	}
}

func TestArm_ExpiryLapse(t *testing.T) {
	now := time.Now()
	s := newWithClock(15*time.Minute, &now)
	if _, err := s.Arm(testMAC); err != nil {
		t.Fatal(err)
	}
	// One second before expiry — still armed.
	now = now.Add(15*time.Minute - time.Second)
	if !s.IsArmed(testMAC) {
		t.Fatal("just before ttl: should still be armed")
	}
	// One second past expiry — no longer armed.
	now = now.Add(2 * time.Second)
	if s.IsArmed(testMAC) {
		t.Fatal("past ttl: should be disarmed")
	}
}

func TestDisarm(t *testing.T) {
	now := time.Now()
	s := newWithClock(15*time.Minute, &now)

	if s.Disarm(testMAC) {
		t.Fatal("disarming an unarmed MAC should return false")
	}
	if _, err := s.Arm(testMAC); err != nil {
		t.Fatal(err)
	}
	if !s.Disarm(testMAC) {
		t.Fatal("disarming an armed MAC should return true")
	}
	if s.IsArmed(testMAC) {
		t.Fatal("after Disarm: IsArmed should be false")
	}
}

func TestArm_ResetsTimer(t *testing.T) {
	now := time.Now()
	s := newWithClock(15*time.Minute, &now)

	if _, err := s.Arm(testMAC); err != nil {
		t.Fatal(err)
	}
	// Advance to ttl-1s. Re-arm — should reset the clock so we have
	// another full ttl from this moment.
	now = now.Add(15*time.Minute - time.Second)
	if _, err := s.Arm(testMAC); err != nil {
		t.Fatal(err)
	}
	// Move forward by ttl-1s again — still armed because the second
	// Arm reset the timer.
	now = now.Add(15*time.Minute - time.Second)
	if !s.IsArmed(testMAC) {
		t.Fatal("re-arm should reset the timer; expected still armed")
	}
}

func TestNoExpiry(t *testing.T) {
	now := time.Now()
	s := newWithClock(0, &now)
	if _, err := s.Arm(testMAC); err != nil {
		t.Fatal(err)
	}
	// Jump a year forward.
	now = now.Add(365 * 24 * time.Hour)
	if !s.IsArmed(testMAC) {
		t.Fatal("ttl=0 should disable expiry")
	}
}

func TestStatus(t *testing.T) {
	now := time.Now()
	s := newWithClock(15*time.Minute, &now)

	at, exp, armed := s.Status(testMAC)
	if armed || !at.IsZero() || !exp.IsZero() {
		t.Fatalf("unarmed status: want zero-zero-false, got %v %v %v", at, exp, armed)
	}
	if _, err := s.Arm(testMAC); err != nil {
		t.Fatal(err)
	}
	at, exp, armed = s.Status(testMAC)
	if !armed {
		t.Fatal("armed status: want true")
	}
	if at.IsZero() || exp.IsZero() {
		t.Fatal("armed status: timestamps should be non-zero")
	}
	if !exp.Equal(at.Add(15 * time.Minute)) {
		t.Fatalf("expiry off: at=%v exp=%v want at+15min", at, exp)
	}

	// Expired entries should report armed=false even though they're
	// still in the map.
	now = now.Add(20 * time.Minute)
	at, exp, armed = s.Status(testMAC)
	if armed {
		t.Fatal("expired status: want armed=false")
	}
	if !at.IsZero() || !exp.IsZero() {
		t.Fatal("expired status: timestamps should be zero")
	}
}

func TestInvalidMAC(t *testing.T) {
	s := New(15 * time.Minute)
	if _, err := s.Arm("not a mac"); err == nil {
		t.Fatal("Arm of invalid MAC should error")
	}
	if s.Disarm("not a mac") {
		t.Fatal("Disarm of invalid MAC should return false")
	}
	if s.IsArmed("not a mac") {
		t.Fatal("IsArmed of invalid MAC should return false")
	}
	if _, _, armed := s.Status("not a mac"); armed {
		t.Fatal("Status of invalid MAC should report armed=false")
	}
}

func TestMACCanonicalization(t *testing.T) {
	// Hyphen and colon forms of the same MAC must hit the same entry.
	s := New(15 * time.Minute)
	if _, err := s.Arm("58-47-CA-70-C7-C9"); err != nil {
		t.Fatal(err)
	}
	if !s.IsArmed("58:47:ca:70:c7:c9") {
		t.Fatal("expected MAC canonicalization to unify hyphen + colon forms")
	}
	if !s.Disarm("58:47:CA:70:c7:c9") {
		t.Fatal("Disarm with different case should find the entry")
	}
}

func TestConcurrency(t *testing.T) {
	// Run with -race to catch data races.
	s := New(15 * time.Minute)
	macs := []string{
		"58:47:ca:70:c7:c9",
		"aa:bb:cc:dd:ee:01",
		"11:22:33:44:55:66",
	}
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		for _, m := range macs {
			wg.Add(2)
			go func(mac string) {
				defer wg.Done()
				_, _ = s.Arm(mac)
			}(m)
			go func(mac string) {
				defer wg.Done()
				_ = s.IsArmed(mac)
				s.Disarm(mac)
			}(m)
		}
	}
	wg.Wait()
}
