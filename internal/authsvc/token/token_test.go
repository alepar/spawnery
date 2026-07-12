package token

import (
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	authv1 "spawnery/gen/auth/v1"
)

func TestValidateSessionBody(t *testing.T) {
	now := time.Unix(1_770_000_000, 0)
	valid := &authv1.SessionTokenBody{IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Minute).Unix()}
	if err := ValidateSessionBody(valid, now); err != nil {
		t.Fatalf("valid body: %v", err)
	}
	for name, tc := range map[string]struct {
		body *authv1.SessionTokenBody
		want error
	}{
		"nil":     {body: nil, want: ErrMalformed},
		"expired": {body: &authv1.SessionTokenBody{IssuedAt: now.Add(-time.Hour).Unix(), ExpiresAt: now.Unix()}, want: ErrExpired},
		"future":  {body: &authv1.SessionTokenBody{IssuedAt: now.Add(issuedAtSkew + time.Second).Unix(), ExpiresAt: now.Add(time.Hour).Unix()}, want: ErrNotYet},
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateSessionBody(tc.body, now); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestSessionKeyHash(t *testing.T) {
	input := []byte("DER SPKI")
	want := sha256.Sum256(input)
	if got := SessionKeyHash(input); string(got) != string(want[:]) {
		t.Fatalf("hash = %x, want %x", got, want)
	}
}
