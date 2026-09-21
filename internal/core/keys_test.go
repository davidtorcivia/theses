package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/store"
)

// keyed is a service with two people in it, and a command that makes one
// proposition and says how many times it has actually run.
type keyed struct {
	s   *Service
	ran int
}

func newKeyed(t *testing.T) (*keyed, Actor, Actor) {
	t.Helper()
	ctx := context.Background()
	db := store.OpenTemp(t)
	k := &keyed{s: New(db, NewBus())}
	who := func(handle, name string) Actor {
		id, err := store.CreateUser(ctx, db, &store.User{
			Handle: handle, Email: handle + "@example.com", Name: name,
			Initials: "XX", Colour: "#111", Role: auth.RoleOwner, PasswordHash: "x",
		})
		if err != nil {
			t.Fatal(err)
		}
		return Actor{Kind: KindUser, ID: id, Name: name}
	}
	return k, who("ada", "Ada Lovelace"), who("grace", "Grace Hopper")
}

// make writes one proposition through Do, the way every real command does.
func (k *keyed) make(ctx context.Context, a Actor, title string) (Event, error) {
	return k.s.Do(ctx, a, 0, auth.CanEdit, func(ctx context.Context, tx *sql.Tx) (Change, error) {
		k.ran++
		res, err := tx.ExecContext(ctx, `INSERT INTO propositions
			(number, title, status, position, created_at)
			VALUES ((SELECT coalesce(max(number), 0) + 1 FROM propositions), ?, 'idea', 'V', 0)`, title)
		if err != nil {
			return Change{}, err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return Change{}, err
		}
		return Change{Entity: "proposition", EntityID: id, Action: "create",
			After: map[string]any{"id": id, "title": title}}, nil
	})
}

func (k *keyed) count(t *testing.T, table string) int {
	t.Helper()
	var n int
	if err := k.s.DB.QueryRowContext(context.Background(),
		`SELECT count(*) FROM `+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestWithKeyRefusesWhatIsNotAKey(t *testing.T) {
	long := ""
	for len(long) <= maxKey {
		long += "a"
	}
	for _, tt := range []struct {
		name string
		key  string
		ok   bool
	}{
		{name: "a uuid the browser makes", key: "3f0a1b2c-4d5e-6f70-8192-a3b4c5d6e7f8", ok: true},
		{name: "letters, digits and an underscore", key: "Tab_7", ok: true},
		{name: "one character", key: "x", ok: true},
		{name: "the numbered key a second command takes", key: "abc#2"},
		{name: "empty", key: ""},
		{name: "a space", key: "two words"},
		{name: "a colon, which the outbox folds on", key: "card.title:12"},
		{name: "longer than a key may be", key: long},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, err := WithKey(context.Background(), tt.key)
			if tt.ok {
				if err != nil {
					t.Fatalf("WithKey(%q) refused it: %v", tt.key, err)
				}
				if ctx == nil {
					t.Fatal("WithKey returned no context")
				}
				return
			}
			if err != ErrKey {
				t.Fatalf("WithKey(%q) returned %v, want ErrKey", tt.key, err)
			}
		})
	}
}

// The whole point: the same key twice is one change, and the second answer is
// the first one's event with Replayed on it.
func TestTheSameKeyTwiceAppliesOnce(t *testing.T) {
	k, ada, _ := newKeyed(t)
	ctx, err := WithKey(context.Background(), "aaaa-bbbb")
	if err != nil {
		t.Fatal(err)
	}
	first, err := k.make(ctx, ada, "One")
	if err != nil {
		t.Fatal(err)
	}
	// A replay is a fresh context with the same key: the run's counter starts
	// again, which is what lines the second attempt up with the first.
	again, err := WithKey(context.Background(), "aaaa-bbbb")
	if err != nil {
		t.Fatal(err)
	}
	second, err := k.make(again, ada, "One")
	if err != nil {
		t.Fatal(err)
	}

	if k.ran != 1 {
		t.Errorf("the command ran %d times, want 1", k.ran)
	}
	if n := k.count(t, "propositions"); n != 1 {
		t.Errorf("%d propositions, want 1", n)
	}
	if n := k.count(t, "activity"); n != 1 {
		t.Errorf("%d activity rows, want 1", n)
	}
	if !second.Replayed {
		t.Error("the second answer does not say it was replayed")
	}
	if first.Replayed {
		t.Error("the first answer says it was replayed")
	}
	if second.Seq != first.Seq || second.EntityID != first.EntityID ||
		second.Entity != first.Entity || second.Action != first.Action ||
		second.At != first.At || second.Actor.ID != first.Actor.ID ||
		second.Actor.Name != first.Actor.Name {
		t.Errorf("the replayed event is %+v, want the first one %+v", second, first)
	}
	if string(second.After) != string(first.After) {
		t.Errorf("the replayed payload is %s, want %s", second.After, first.After)
	}
}

func TestAReplayChecksTheOriginalPropositionAgain(t *testing.T) {
	k, ada, _ := newKeyed(t)
	ctx := context.Background()
	if _, err := k.s.DB.ExecContext(ctx, `UPDATE users SET role = ? WHERE id = ?`, auth.RoleEditor, ada.ID); err != nil {
		t.Fatal(err)
	}
	makeProposition := func(title string) int64 {
		t.Helper()
		res, err := k.s.DB.ExecContext(ctx, `INSERT INTO propositions
			(number, title, status, position, created_at)
			VALUES ((SELECT coalesce(max(number), 0) + 1 FROM propositions), ?, 'idea', ?, 0)`, title, title)
		if err != nil {
			t.Fatal(err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := k.s.DB.ExecContext(ctx,
			`INSERT INTO proposition_members (proposition_id, user_id) VALUES (?, ?)`, id, ada.ID); err != nil {
			t.Fatal(err)
		}
		return id
	}
	one, two := makeProposition("One"), makeProposition("Two")
	write := func(ctx context.Context, proposition int64) (Event, error) {
		return k.s.Do(ctx, ada, proposition, auth.CanEdit,
			func(context.Context, *sql.Tx) (Change, error) {
				return Change{Entity: "proposition", EntityID: proposition, Action: "edit",
					Before: map[string]any{"title": "before"}, After: map[string]any{"title": "after"}}, nil
			})
	}

	keyed, err := WithKey(ctx, "old-proposition")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := write(keyed, one); err != nil {
		t.Fatal(err)
	}
	if _, err := k.s.DB.ExecContext(ctx,
		`DELETE FROM proposition_members WHERE proposition_id = ? AND user_id = ?`, one, ada.ID); err != nil {
		t.Fatal(err)
	}

	again, err := WithKey(ctx, "old-proposition")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := write(again, two); !errors.Is(err, ErrForbidden) {
		t.Fatalf("replaying an event from a proposition the actor left gave %v, want ErrForbidden", err)
	}
}

func TestAReplayOfADetachedDeleteStillNeedsDeleteStanding(t *testing.T) {
	k, ada, _ := newKeyed(t)
	ctx := context.Background()
	deleted := func(ctx context.Context, need string) (Event, error) {
		return k.s.Do(ctx, ada, 0, need, func(context.Context, *sql.Tx) (Change, error) {
			return Change{Entity: "proposition", EntityID: 7, Action: "delete",
				Before: map[string]any{"id": 7}, Detached: true}, nil
		})
	}

	keyed, err := WithKey(ctx, "owner-delete")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deleted(keyed, auth.CanDelete); err != nil {
		t.Fatal(err)
	}
	if _, err := k.s.DB.ExecContext(ctx, `UPDATE users SET role = ? WHERE id = ?`, auth.RoleEditor, ada.ID); err != nil {
		t.Fatal(err)
	}

	again, err := WithKey(ctx, "owner-delete")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deleted(again, auth.CanEdit); !errors.Is(err, ErrForbidden) {
		t.Fatalf("replaying an owner delete after demotion gave %v, want ErrForbidden", err)
	}
}

// A key belongs to the actor that spent it, so two people typing at two devices
// cannot take each other's changes away by colliding on one.
func TestTwoActorsDoNotShareAKey(t *testing.T) {
	k, ada, grace := newKeyed(t)
	for _, a := range []Actor{ada, grace} {
		ctx, err := WithKey(context.Background(), "same-key")
		if err != nil {
			t.Fatal(err)
		}
		e, err := k.make(ctx, a, "One")
		if err != nil {
			t.Fatal(err)
		}
		if e.Replayed {
			t.Fatalf("%s was told their own key had been spent", a.Name)
		}
	}
	if n := k.count(t, "propositions"); n != 2 {
		t.Errorf("%d propositions, want 2", n)
	}
}

// A command the browser calls one command can be several: a paste of three
// paragraphs is three inserts. Replaying it must line each one up with its own
// original rather than all three with the first.
func TestAMultipleCommandReplaysWhole(t *testing.T) {
	k, ada, _ := newKeyed(t)
	three := func(ctx context.Context) []Event {
		out := []Event{}
		if err := k.s.Together(ctx, func(ctx context.Context) error {
			for _, title := range []string{"One", "Two", "Three"} {
				e, err := k.make(ctx, ada, title)
				if err != nil {
					return err
				}
				out = append(out, e)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}

	ctx, err := WithKey(context.Background(), "paste")
	if err != nil {
		t.Fatal(err)
	}
	first := three(ctx)
	again, err := WithKey(context.Background(), "paste")
	if err != nil {
		t.Fatal(err)
	}
	second := three(again)

	if k.ran != 3 {
		t.Errorf("the command ran %d times, want 3", k.ran)
	}
	if n := k.count(t, "propositions"); n != 3 {
		t.Errorf("%d propositions, want 3", n)
	}
	for i := range first {
		if !second[i].Replayed {
			t.Errorf("event %d of the replay does not say it was replayed", i)
		}
		if second[i].EntityID != first[i].EntityID {
			t.Errorf("event %d of the replay is entity %d, want %d",
				i, second[i].EntityID, first[i].EntityID)
		}
	}
	// Nothing is published a second time: a replay applied nothing, so there is
	// nothing for anybody watching to draw.
	if n := k.count(t, "client_keys"); n != 3 {
		t.Errorf("%d keys remembered, want 3", n)
	}
}

// A run that rolls back leaves no key behind, or the retry that follows would
// be told its change had already happened when it never did.
func TestARolledBackRunLeavesNoKey(t *testing.T) {
	k, ada, _ := newKeyed(t)
	ctx, err := WithKey(context.Background(), "rolled-back")
	if err != nil {
		t.Fatal(err)
	}
	refused := errorString("no")
	if err := k.s.Together(ctx, func(ctx context.Context) error {
		if _, err := k.make(ctx, ada, "One"); err != nil {
			return err
		}
		return refused
	}); err != refused {
		t.Fatalf("Together returned %v", err)
	}
	if n := k.count(t, "client_keys"); n != 0 {
		t.Errorf("%d keys left behind by a refused run", n)
	}
	// The retry gets through, because nothing says it already happened.
	again, err := WithKey(context.Background(), "rolled-back")
	if err != nil {
		t.Fatal(err)
	}
	e, err := k.make(again, ada, "One")
	if err != nil {
		t.Fatal(err)
	}
	if e.Replayed {
		t.Error("the retry was told a rolled back change had happened")
	}
}

type errorString string

func (e errorString) Error() string { return string(e) }

// The key is remembered for a day. Older ones go on the sweep that already
// clears up after abandoned uploads.
func TestPruneKeysDropsOnlyTheOldOnes(t *testing.T) {
	k, ada, _ := newKeyed(t)
	now := time.Now()
	k.s.Now = func() time.Time { return now }

	old, err := WithKey(context.Background(), "yesterday")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.make(old, ada, "One"); err != nil {
		t.Fatal(err)
	}
	k.s.Now = func() time.Time { return now.Add(KeyLife + time.Minute) }
	fresh, err := WithKey(context.Background(), "today")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.make(fresh, ada, "Two"); err != nil {
		t.Fatal(err)
	}

	if err := k.s.PruneKeys(context.Background()); err != nil {
		t.Fatal(err)
	}
	var left string
	if err := k.s.DB.QueryRowContext(context.Background(),
		`SELECT coalesce(group_concat(key), '') FROM client_keys`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != "today" {
		t.Errorf("the prune left %q, want \"today\"", left)
	}
}

// The key points at the activity row it produced, so a log that is compacted
// one day takes the keys with it and a command arriving under one of them is
// applied again, which is the safe answer that late.
func TestDeletingTheActivityRowForgetsTheKey(t *testing.T) {
	k, ada, _ := newKeyed(t)
	ctx, err := WithKey(context.Background(), "cascade")
	if err != nil {
		t.Fatal(err)
	}
	e, err := k.make(ctx, ada, "One")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.s.DB.ExecContext(context.Background(),
		`DELETE FROM activity WHERE id = ?`, e.Seq); err != nil {
		t.Fatal(err)
	}
	if n := k.count(t, "client_keys"); n != 0 {
		t.Errorf("%d keys survived the activity row they name", n)
	}
}

// What that cascade costs. SQLite looks the child rows up by the column the
// foreign key names, and with no index on it that is a scan of every key in
// the table for each activity row deleted: one at a time nobody would notice,
// but a compaction deleting the log in bulk would hold the write lock for as
// long as that takes, once per row. The plan is asserted rather than the
// index's existence, because an index the planner never chooses would pass
// that and fail this.
func TestTheCascadeLooksKeysUpByAnIndex(t *testing.T) {
	k, ada, _ := newKeyed(t)
	ctx, err := WithKey(context.Background(), "planned")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.make(ctx, ada, "One"); err != nil {
		t.Fatal(err)
	}

	rows, err := k.s.DB.QueryContext(context.Background(),
		`EXPLAIN QUERY PLAN DELETE FROM activity WHERE id = 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var id, parent, aux int64
		var detail string
		if err := rows.Scan(&id, &parent, &aux, &detail); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(detail, "client_keys") {
			continue
		}
		found = true
		if !strings.Contains(detail, "client_keys_activity") {
			t.Errorf("the cascade plans %q, want a search of client_keys_activity", detail)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	// No line about the keys at all would mean foreign keys are off on this
	// connection, which would take the cascade this design leans on with them.
	if !found {
		t.Error("deleting an activity row plans no lookup in client_keys at all")
	}
}

// The watcher that imports hand edits from the markdown mirror is not a client
// and has nothing to retry, so a key in its context is not spent on it.
func TestOnlyAPersonSpendsAKey(t *testing.T) {
	k, _, _ := newKeyed(t)
	ctx, err := WithKey(context.Background(), "file-actor")
	if err != nil {
		t.Fatal(err)
	}
	file := Actor{Kind: KindFile, Name: "the mirror"}
	for i := 0; i < 2; i++ {
		if _, err := k.make(ctx, file, "One"); err != nil {
			t.Fatal(err)
		}
	}
	if k.ran != 2 {
		t.Errorf("the file actor's command ran %d times, want 2", k.ran)
	}
	if n := k.count(t, "client_keys"); n != 0 {
		t.Errorf("%d keys remembered for an actor that is not a person", n)
	}
}

// The event a replay answers with is the one the log holds, decodable as the
// row it carried, so a caller that reads the payload back gets what it made.
func TestAReplayedEventCarriesTheRowItMade(t *testing.T) {
	k, ada, _ := newKeyed(t)
	ctx, err := WithKey(context.Background(), "payload")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.make(ctx, ada, "One"); err != nil {
		t.Fatal(err)
	}
	again, err := WithKey(context.Background(), "payload")
	if err != nil {
		t.Fatal(err)
	}
	e, err := k.make(again, ada, "One")
	if err != nil {
		t.Fatal(err)
	}
	var row struct {
		ID    int64  `json:"id"`
		Title string `json:"title"`
	}
	if err := json.Unmarshal(e.After, &row); err != nil {
		t.Fatal(err)
	}
	if row.Title != "One" || row.ID != e.EntityID {
		t.Errorf("the replayed payload is %+v, want the row the first run made", row)
	}
}

// The key rides the event, which is how a client that drew a row before the
// server had one knows the real row when it sees it: on its own answer, on the
// copy broadcast to everybody in the room, and on the stream a tab reads when
// it has been away. A run of commands carries the numbered key each one spent.
func TestTheEventCarriesTheKeyItWasAppliedUnder(t *testing.T) {
	k, ada, _ := newKeyed(t)
	ctx := context.Background()

	// What the room is sent is what a second tab matches its own drawing
	// against, so the published copies are read as well as the answers.
	room := k.s.Bus.Subscribe(0)
	defer room.Close()

	plain, err := k.make(ctx, ada, "No name on it")
	if err != nil {
		t.Fatal(err)
	}
	if plain.Key != "" {
		t.Errorf("a command sent under no key answers with %q", plain.Key)
	}

	named, err := WithKey(ctx, "first")
	if err != nil {
		t.Fatal(err)
	}
	one, err := k.make(named, ada, "Named")
	if err != nil {
		t.Fatal(err)
	}
	if one.Key != "first" {
		t.Errorf("the event carries %q, want first", one.Key)
	}
	// The second command under one key spends the numbered form, and that is
	// what its event says: the client that sent the run can tell the rows of it
	// apart.
	two, err := k.make(named, ada, "Named again")
	if err != nil {
		t.Fatal(err)
	}
	if two.Key != "first#2" {
		t.Errorf("the second event carries %q, want first#2", two.Key)
	}

	// A replay applies nothing and answers with the first event, which says
	// which command is being answered.
	back, err := WithKey(ctx, "first")
	if err != nil {
		t.Fatal(err)
	}
	replay, err := k.make(back, ada, "Named")
	if err != nil {
		t.Fatal(err)
	}
	if !replay.Replayed || replay.Key != "first" {
		t.Errorf("the replay is %+v, want replayed under first", replay)
	}

	for i, want := range []string{"", "first", "first#2"} {
		select {
		case e := <-room.C:
			if e.Key != want {
				t.Errorf("published event %d carries %q, want %q", i, e.Key, want)
			}
		default:
			t.Fatalf("only %d events were published, want 3", i)
		}
	}
	// A replay applied nothing, so it published nothing for anybody to draw.
	select {
	case e := <-room.C:
		t.Errorf("the replay published %+v", e)
	default:
	}
}

// The stream a tab reads after being away carries the keys too, read back
// beside the rows rather than kept in them: a tab that missed the broadcast
// still recognizes what it drew itself.
func TestTheStreamCarriesTheKey(t *testing.T) {
	k, ada, _ := newKeyed(t)
	ctx := context.Background()
	made, err := k.make(ctx, ada, "Something to file under")
	if err != nil {
		t.Fatal(err)
	}
	proposition := made.EntityID
	// A command filed under that proposition, which is what the stream reads.
	note := func(ctx context.Context) (Event, error) {
		return k.s.Do(ctx, ada, proposition, auth.CanEdit, func(context.Context, *sql.Tx) (Change, error) {
			return Change{Entity: "block", EntityID: 1, Action: "insert"}, nil
		})
	}
	named, err := WithKey(ctx, "streamed")
	if err != nil {
		t.Fatal(err)
	}
	keyed, err := note(named)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := note(ctx); err != nil {
		t.Fatal(err)
	}

	events, err := k.s.Since(ctx, proposition, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("the stream has %d events, want 2", len(events))
	}
	if events[0].Seq != keyed.Seq || events[0].Key != "streamed" {
		t.Errorf("the first event of the stream is %+v, want seq %d under streamed", events[0], keyed.Seq)
	}
	if events[1].Key != "" {
		t.Errorf("the second event of the stream carries %q, want nothing", events[1].Key)
	}
}
