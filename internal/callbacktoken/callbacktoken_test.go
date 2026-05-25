package callbacktoken

import (
	"errors"
	"testing"
	"time"
)

func atTime(s *Signer, t time.Time) { s.now = func() time.Time { return t } }

func TestMintVerify_RoundTrip(t *testing.T) {
	s := New([]byte("secret"), time.Hour)
	mac := "58:47:ca:70:c7:c9"
	tok := s.Mint(mac)
	if err := s.Verify(mac, tok); err != nil {
		t.Fatalf("Verify of freshly minted token: %v", err)
	}
}

func TestVerify_Expired(t *testing.T) {
	s := New([]byte("secret"), time.Hour)
	base := time.Unix(1_700_000_000, 0)
	atTime(s, base)
	tok := s.Mint("58:47:ca:70:c7:c9")
	atTime(s, base.Add(time.Hour+time.Second))
	if err := s.Verify("58:47:ca:70:c7:c9", tok); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("want ErrTokenExpired, got %v", err)
	}
}

func TestVerify_MACMismatch(t *testing.T) {
	// A token minted for MAC A must not validate for MAC B.
	s := New([]byte("secret"), time.Hour)
	tok := s.Mint("58:47:ca:70:c7:c9")
	if err := s.Verify("aa:bb:cc:dd:ee:ff", tok); !errors.Is(err, ErrTokenMismatch) {
		t.Fatalf("want ErrTokenMismatch for wrong MAC, got %v", err)
	}
}

func TestVerify_WrongSecret(t *testing.T) {
	a := New([]byte("secret-a"), time.Hour)
	b := New([]byte("secret-b"), time.Hour)
	tok := a.Mint("58:47:ca:70:c7:c9")
	if err := b.Verify("58:47:ca:70:c7:c9", tok); !errors.Is(err, ErrTokenMismatch) {
		t.Fatalf("want ErrTokenMismatch across secrets, got %v", err)
	}
}

func TestVerify_Malformed(t *testing.T) {
	s := New([]byte("secret"), time.Hour)
	for _, bad := range []string{"", "noseparator", "notanumber.abcd", ".abcd", "123."} {
		if err := s.Verify("58:47:ca:70:c7:c9", bad); !errors.Is(err, ErrTokenMalformed) {
			t.Errorf("Verify(%q): want ErrTokenMalformed, got %v", bad, err)
		}
	}
}
