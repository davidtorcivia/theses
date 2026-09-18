package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
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

// Several owners all demoting themselves at once. The guard is one statement,
// so whatever order they land in, the last one is refused.
func TestConcurrentDemotionsLeaveAnOwner(t *testing.T) {
	ctx := context.Background()
	for round := 0; round < 20; round++ {
		db := OpenTemp(t)
		const owners = 8
		ids := make([]int64, owners)
		for i := range ids {
			ids[i] = newOwner(t, db, fmt.Sprintf("owner%d", i))
		}

		start := make(chan struct{})
		var wg sync.WaitGroup
		for _, id := range ids {
			wg.Add(1)
			go func(id int64) {
				defer wg.Done()
				<-start
				if _, err := SetUserRoleKeepingAnOwner(ctx, db, id, "editor"); err != nil {
					t.Error(err)
				}
			}(id)
		}
		close(start)
		wg.Wait()

		if left := countOwners(t, db); left != 1 {
			t.Fatalf("round %d left %d owners, want 1", round, left)
		}
		db.Close()
	}
}

// The same for deletion.
func TestConcurrentDeletionsLeaveAnOwner(t *testing.T) {
	ctx := context.Background()
	for round := 0; round < 20; round++ {
		db := OpenTemp(t)
		const owners = 8
		ids := make([]int64, owners)
		for i := range ids {
			ids[i] = newOwner(t, db, fmt.Sprintf("owner%d", i))
		}

		start := make(chan struct{})
		var wg sync.WaitGroup
		for _, id := range ids {
			wg.Add(1)
			go func(id int64) {
				defer wg.Done()
				<-start
				if _, err := DeleteUserKeepingAnOwner(ctx, db, id); err != nil {
					t.Error(err)
				}
			}(id)
		}
		close(start)
		wg.Wait()

		if left := countOwners(t, db); left != 1 {
			t.Fatalf("round %d left %d owners, want 1", round, left)
		}
		db.Close()
	}
}

func TestTheGuardRefusesTheOnlyOwner(t *testing.T) {
	ctx := context.Background()
	db := OpenTemp(t)
	only := newOwner(t, db, "dt")

	if ok, err := SetUserRoleKeepingAnOwner(ctx, db, only, "editor"); err != nil || ok {
		t.Errorf("demoting the only owner: ok=%v err=%v", ok, err)
	}
	if ok, err := DeleteUserKeepingAnOwner(ctx, db, only); err != nil || ok {
		t.Errorf("deleting the only owner: ok=%v err=%v", ok, err)
	}
	if owners := countOwners(t, db); owners != 1 {
		t.Errorf("%d owners left, want the one that was refused", owners)
	}
}

func TestTheGuardLetsGoWhenAnotherOwnerRemains(t *testing.T) {
	ctx := context.Background()
	db := OpenTemp(t)
	newOwner(t, db, "dt")
	b := newOwner(t, db, "mara")

	if ok, err := SetUserRoleKeepingAnOwner(ctx, db, b, "editor"); err != nil || !ok {
		t.Fatalf("demoting the second owner: ok=%v err=%v", ok, err)
	}
	if ok, err := DeleteUserKeepingAnOwner(ctx, db, b); err != nil || !ok {
		t.Fatalf("deleting a non-owner: ok=%v err=%v", ok, err)
	}
	if owners := countOwners(t, db); owners != 1 {
		t.Errorf("%d owners left", owners)
	}
}

func newOwner(t *testing.T, db *DB, handle string) int64 {
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

func countOwners(t *testing.T, db *DB) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM users WHERE role = 'owner'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
