// Package callbacktoken mints and verifies the short-lived, self-contained
// bearer tokens that authenticate pxe-beacon's public phone-home callbacks
// (POST /autoinstall/{mac}/done and /log).
//
// A token is:
//
//	<expUnix> "." <hex(HMAC_SHA256(secret, canonMAC + "." + expUnix))>
//
// It carries its own expiry and is bound to one MAC, so it's stateless to
// verify (no server-side store) and a token minted for MAC A can't be
// replayed against MAC B. It travels in the cloud-init phone_home URL over
// plaintext LAN HTTP — so it is a bearer token, NOT crypto-grade auth absent
// TLS. It raises the bar from "any LAN host can spoof any MAC's callback" to
// "you must have seen that MAC's served config", which matches pxe-beacon's
// trusted-LAN threat model.
package callbacktoken

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Sentinel errors returned by Verify. Callers map all of them to 403.
var (
	ErrTokenMalformed = errors.New("callbacktoken: malformed token")
	ErrTokenExpired   = errors.New("callbacktoken: token expired")
	ErrTokenMismatch  = errors.New("callbacktoken: signature mismatch")
)

// Signer mints + verifies tokens against a fixed secret and TTL.
type Signer struct {
	secret []byte
	ttl    time.Duration
	now    func() time.Time // injectable for tests
}

// New returns a Signer. secret must be non-empty; ttl is how long a minted
// token stays valid.
func New(secret []byte, ttl time.Duration) *Signer {
	return &Signer{secret: secret, ttl: ttl, now: time.Now}
}

// Mint returns a token for canonMAC that expires ttl from now. canonMAC must
// already be canonicalized (colon form) by the caller — Verify recomputes the
// HMAC over the exact same string, so any mismatch in form fails closed.
func (s *Signer) Mint(canonMAC string) string {
	exp := s.now().Add(s.ttl).Unix()
	return fmt.Sprintf("%d.%s", exp, s.sign(canonMAC, exp))
}

// Verify checks token for canonMAC: well-formed, not expired, signature
// matches. Returns one of the sentinel errors on failure, nil on success.
func (s *Signer) Verify(canonMAC, token string) error {
	dot := strings.IndexByte(token, '.')
	if dot <= 0 || dot == len(token)-1 {
		return ErrTokenMalformed
	}
	exp, err := strconv.ParseInt(token[:dot], 10, 64)
	if err != nil {
		return ErrTokenMalformed
	}
	// Constant-time signature check first, then expiry — both must hold.
	want := s.sign(canonMAC, exp)
	if !hmac.Equal([]byte(token[dot+1:]), []byte(want)) {
		return ErrTokenMismatch
	}
	if !s.now().Before(time.Unix(exp, 0)) {
		return ErrTokenExpired
	}
	return nil
}

func (s *Signer) sign(canonMAC string, exp int64) string {
	h := hmac.New(sha256.New, s.secret)
	h.Write([]byte(canonMAC + "." + strconv.FormatInt(exp, 10)))
	return hex.EncodeToString(h.Sum(nil))
}
