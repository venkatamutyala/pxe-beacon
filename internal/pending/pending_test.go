package pending

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

func TestDeploy_ThenIsPending(t *testing.T) {
	now := time.Now()
	s := newWithClock(15*time.Minute, &now)

	if s.IsPending(testMAC) {
		t.Fatal("fresh Store: IsPending should be false")
	}
	exp, err := s.Deploy(testMAC)
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if exp.IsZero() {
		t.Fatal("expected non-zero expiry with positive ttl")
	}
	if !s.IsPending(testMAC) {
		t.Fatal("after Deploy: IsPending should be true")
	}
	a, _, _, ok := s.Status(testMAC)
	if !ok || a != ActionDeploy {
		t.Fatalf("Status after Deploy: action=%v ok=%v", a, ok)
	}
}

func TestRescue_ThenIsPending(t *testing.T) {
	now := time.Now()
	s := newWithClock(15*time.Minute, &now)

	if _, err := s.Rescue(testMAC); err != nil {
		t.Fatal(err)
	}
	a, _, _, ok := s.Status(testMAC)
	if !ok || a != ActionRescue {
		t.Fatalf("Status after Rescue: action=%v ok=%v", a, ok)
	}
}

func TestExpiryLapse(t *testing.T) {
	now := time.Now()
	s := newWithClock(15*time.Minute, &now)
	if _, err := s.Deploy(testMAC); err != nil {
		t.Fatal(err)
	}
	// One second before expiry — still pending.
	now = now.Add(15*time.Minute - time.Second)
	if !s.IsPending(testMAC) {
		t.Fatal("just before ttl: should still be pending")
	}
	// One second past expiry — no longer pending.
	now = now.Add(2 * time.Second)
	if s.IsPending(testMAC) {
		t.Fatal("past ttl: should be reaped")
	}
}

func TestCancel(t *testing.T) {
	now := time.Now()
	s := newWithClock(15*time.Minute, &now)

	if s.Cancel(testMAC) {
		t.Fatal("Cancel on empty: should return false")
	}
	if _, err := s.Deploy(testMAC); err != nil {
		t.Fatal(err)
	}
	if !s.Cancel(testMAC) {
		t.Fatal("Cancel on pending: should return true")
	}
	if s.IsPending(testMAC) {
		t.Fatal("after Cancel: IsPending should be false")
	}
}

func TestDeploy_ResetsTimer(t *testing.T) {
	now := time.Now()
	s := newWithClock(15*time.Minute, &now)

	if _, err := s.Deploy(testMAC); err != nil {
		t.Fatal(err)
	}
	now = now.Add(15*time.Minute - time.Second)
	if _, err := s.Deploy(testMAC); err != nil {
		t.Fatal(err)
	}
	now = now.Add(15*time.Minute - time.Second)
	if !s.IsPending(testMAC) {
		t.Fatal("re-Deploy should reset the timer")
	}
}

func TestActionSwap_RescueOverridesDeploy(t *testing.T) {
	// Issuing Rescue after Deploy replaces the pending action (no
	// queue of multiple actions per MAC; "last writer wins").
	s := New(15 * time.Minute)
	if _, err := s.Deploy(testMAC); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Rescue(testMAC); err != nil {
		t.Fatal(err)
	}
	a, _, _, ok := s.Status(testMAC)
	if !ok || a != ActionRescue {
		t.Fatalf("expected Rescue to override Deploy, got %v ok=%v", a, ok)
	}
}

func TestNoExpiry(t *testing.T) {
	now := time.Now()
	s := newWithClock(0, &now)
	if _, err := s.Deploy(testMAC); err != nil {
		t.Fatal(err)
	}
	now = now.Add(365 * 24 * time.Hour)
	if !s.IsPending(testMAC) {
		t.Fatal("ttl=0 should disable expiry")
	}
}

func TestInvalidMAC(t *testing.T) {
	s := New(15 * time.Minute)
	if _, err := s.Deploy("not a mac"); err == nil {
		t.Fatal("Deploy of invalid MAC should error")
	}
	if _, err := s.Rescue("not a mac"); err == nil {
		t.Fatal("Rescue of invalid MAC should error")
	}
	if s.Cancel("not a mac") {
		t.Fatal("Cancel of invalid MAC should return false")
	}
	if s.IsPending("not a mac") {
		t.Fatal("IsPending of invalid MAC should return false")
	}
}

func TestMACCanonicalization(t *testing.T) {
	s := New(15 * time.Minute)
	if _, err := s.Deploy("58-47-CA-70-C7-C9"); err != nil {
		t.Fatal(err)
	}
	if !s.IsPending("58:47:ca:70:c7:c9") {
		t.Fatal("canonicalization should unify hyphen and colon forms")
	}
	if !s.Cancel("58:47:CA:70:c7:c9") {
		t.Fatal("Cancel with different case should find the entry")
	}
}

func TestConcurrency(t *testing.T) {
	// Run with -race.
	s := New(15 * time.Minute)
	macs := []string{
		"58:47:ca:70:c7:c9",
		"aa:bb:cc:dd:ee:01",
		"11:22:33:44:55:66",
	}
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		for _, m := range macs {
			wg.Add(3)
			go func(mac string) { defer wg.Done(); _, _ = s.Deploy(mac) }(m)
			go func(mac string) { defer wg.Done(); _, _ = s.Rescue(mac) }(m)
			go func(mac string) {
				defer wg.Done()
				_ = s.IsPending(mac)
				s.Cancel(mac)
			}(m)
		}
	}
	wg.Wait()
}
