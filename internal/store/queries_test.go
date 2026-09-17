package store

import (
	"context"
	"errors"
	"testing"
)

func newUser(t *testing.T, db *DB, handle string) int64 {
	t.Helper()
	id, err := CreateUser(context.Background(), db, &User{
		Handle: handle, Email: handle + "@example.com", Name: handle,
		Initials: "XX", Colour: "#111", Role: "owner", PasswordHash: "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestClaimTOTPStepRejectsReplay(t *testing.T) {
	ctx := context.Background()
	db := OpenTemp(t)
	id := newUser(t, db, "dt")

	if ok, err := ClaimTOTPStep(ctx, db, id, 58000000); err != nil || !ok {
		t.Fatalf("first claim: ok=%v err=%v", ok, err)
	}
	if ok, err := ClaimTOTPStep(ctx, db, id, 58000000); err != nil || ok {
		t.Fatalf("replay of the same step accepted: ok=%v err=%v", ok, err)
	}
	if ok, err := ClaimTOTPStep(ctx, db, id, 57999999); err != nil || ok {
		t.Fatalf("earlier step accepted: ok=%v err=%v", ok, err)
	}
	if ok, err := ClaimTOTPStep(ctx, db, id, 58000001); err != nil || !ok {
		t.Fatalf("next step refused: ok=%v err=%v", ok, err)
	}
}

func TestSessionLookupFollowsEpochAndExpiry(t *testing.T) {
	ctx := context.Background()
	db := OpenTemp(t)
	id := newUser(t, db, "dt")
	hmac := []byte("session-hmac")

	if err := CreateSession(ctx, db, id, hmac, 1, 2_000_000_000, "test"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := SessionByHMAC(ctx, db, hmac, 1_000_000_000); err != nil {
		t.Fatalf("fresh session not found: %v", err)
	}
	if _, _, err := SessionByHMAC(ctx, db, hmac, 2_000_000_001); !errors.Is(err, ErrNotFound) {
		t.Errorf("expired session returned %v", err)
	}
	if err := BumpSessionEpoch(ctx, db, id); err != nil {
		t.Fatal(err)
	}
	if _, _, err := SessionByHMAC(ctx, db, hmac, 1_000_000_000); !errors.Is(err, ErrNotFound) {
		t.Errorf("session survived sign out everywhere: %v", err)
	}
}

func TestPasswordResetIsSingleUse(t *testing.T) {
	ctx := context.Background()
	db := OpenTemp(t)
	id := newUser(t, db, "dt")
	hash := []byte("reset-hash")

	if err := CreatePasswordReset(ctx, db, id, hash, 2_000_000_000); err != nil {
		t.Fatal(err)
	}
	r, err := PasswordResetByTokenHash(ctx, db, hash)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := UsePasswordReset(ctx, db, r.ID); err != nil || !ok {
		t.Fatalf("first use: ok=%v err=%v", ok, err)
	}
	if ok, err := UsePasswordReset(ctx, db, r.ID); err != nil || ok {
		t.Fatalf("second use accepted: ok=%v err=%v", ok, err)
	}
}
