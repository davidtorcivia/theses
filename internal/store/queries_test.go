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

func TestEmailAddressesAreUniqueIgnoringCase(t *testing.T) {
	ctx := context.Background()
	db := OpenTemp(t)
	ada := &User{
		Handle: "ada", Email: "Ada@Example.com", Name: "Ada",
		Initials: "AL", Colour: "#111", Role: "owner", PasswordHash: "x",
	}
	if _, err := CreateUser(ctx, db, ada); err != nil {
		t.Fatal(err)
	}
	grace := &User{
		Handle: "grace", Email: "ADA@example.COM", Name: "Grace",
		Initials: "GH", Colour: "#222", Role: "editor", PasswordHash: "x",
	}
	if _, err := CreateUser(ctx, db, grace); err == nil {
		t.Fatal("CreateUser accepted a differently-cased copy of an existing email")
	}

	grace.Email = "grace@example.com"
	id, err := CreateUser(ctx, db, grace)
	if err != nil {
		t.Fatalf("create second user with a distinct email: %v", err)
	}
	grace.ID = id
	if err := UpdateProfile(ctx, db, grace.ID, grace.Handle, grace.Name, grace.Initials, grace.Colour,
		"ADA@example.COM"); err == nil {
		t.Fatal("UpdateProfile accepted a differently-cased copy of an existing email")
	}
	after, err := UserByID(ctx, db, grace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Email != grace.Email {
		t.Errorf("failed update changed email to %q, want %q", after.Email, grace.Email)
	}
}

func TestMoreThanOneAccountMayHaveNoEmailAddress(t *testing.T) {
	ctx := context.Background()
	db := OpenTemp(t)
	for _, handle := range []string{"ada", "grace"} {
		if _, err := CreateUser(ctx, db, &User{
			Handle: handle, Email: "", Name: handle,
			Initials: "XX", Colour: "#111", Role: "editor", PasswordHash: "x",
		}); err != nil {
			t.Fatalf("create addressless user %q: %v", handle, err)
		}
	}
}

func TestClaimTOTPStepRejectsReplay(t *testing.T) {
	ctx := context.Background()
	db := OpenTemp(t)
	id := newUser(t, db, "ada")

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
	id := newUser(t, db, "ada")
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
	id := newUser(t, db, "ada")
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
	only := newOwner(t, db, "ada")

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
	newOwner(t, db, "ada")
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
