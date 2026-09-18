package search

import (
	"context"
	"strconv"
	"testing"

	"github.com/davidtorcivia/theses/internal/store"
)

func TestFTSQuery(t *testing.T) {
	cases := []struct{ in, want string }{
		{"debt", `"debt"*`},
		{"student debt", `"student" "debt"*`},
		{`"student debt"`, `"student" "debt"*`},
		{"debt*", `"debt"*`},
		{"debt NEAR crisis", `"debt" "NEAR" "crisis"*`},
		{"debt AND (crisis OR ruin)", `"debt" "AND" "crisis" "OR" "ruin"*`},
		{"title:debt", `"title" "debt"*`},
		{"debt^2", `"debt" "2"*`},
		{"!?*.,", ""},
		{"", ""},
		{"   ", ""},
	}
	for _, c := range cases {
		if got := ftsQuery(c.in); got != c.want {
			t.Errorf("ftsQuery(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestLikePatternEscapes(t *testing.T) {
	cases := []struct{ in, want string }{
		{`100% of _it_`, `%100\% of \_it\_%`},
		{`a\b`, `%a\\b%`},
		{`trailing\`, `%trailing\\%`},
	}
	for _, c := range cases {
		if got := likePattern(c.in); got != c.want {
			t.Errorf("likePattern(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// A backslash in the query is a backslash to match, not an escape that eats the
// character after it or dangles at the end of the pattern.
func TestSearchMatchesABackslash(t *testing.T) {
	db := store.OpenTemp(t)
	if _, err := db.ExecContext(context.Background(), `INSERT INTO users
		(id, handle, email, name, initials, colour, role, password_hash, created_at)
		VALUES (1, 'dos', 'dos@example.com', 'C:\ Drive', 'CD', '#fff', 'guest', 'x', 1)`); err != nil {
		t.Fatal(err)
	}
	groups, err := Search(context.Background(), db, `C:\ Dri`, 0, Everything())
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].Kind != KindUser {
		t.Fatalf("groups = %v", kinds(groups))
	}
}

// seed fills every searchable table with one row about student debt, through
// plain inserts so that the FTS triggers in the schema do the indexing.
func seed(t *testing.T, db *store.DB) {
	t.Helper()
	ctx := context.Background()
	stmts := []string{
		`INSERT INTO users (id, handle, email, name, initials, colour, role, password_hash, created_at)
		 VALUES (1, 'nora', 'nora@example.com', 'Nora Student', 'NS', '#fff', 'editor', 'x', 1)`,
		`INSERT INTO propositions (id, number, title, statement, status, position, created_at)
		 VALUES (1, 10, 'Student debt is a policy choice', 'It was designed', 'idea', 1, 1)`,
		`INSERT INTO columns (id, proposition_id, name, position) VALUES (1, 1, 'Research', 1)`,
		`INSERT INTO cards (id, proposition_id, column_id, position, title, description_md, created_at)
		 VALUES (1, 1, 1, 1, 'Find the debt numbers', 'Federal loan totals by year', 1)`,
		`INSERT INTO documents (id, proposition_id, name, slug, position, created_at)
		 VALUES (1, 1, 'Research', 'research', 1, 1)`,
		`INSERT INTO blocks (id, document_id, position, text, updated_at)
		 VALUES (1, 1, 'a0', 'Outstanding student debt passed a trillion dollars', 1)`,
		`INSERT INTO blocks (id, document_id, position, text, updated_at, deleted_at)
		 VALUES (2, 1, 'a1', 'A deleted line about debt', 1, 2)`,
		`INSERT INTO links (id, proposition_id, url, title, created_at)
		 VALUES (1, 1, 'https://example.com/debt', 'The debt trap', 1)`,
		`INSERT INTO files (id, proposition_id, name, object_key, state, created_at)
		 VALUES (1, 1, 'debt-tape.wav', 'k/1', 'ready', 1)`,
		`INSERT INTO files (id, proposition_id, name, object_key, state, created_at)
		 VALUES (2, 1, 'debt-draft.wav', 'k/2', 'uploading', 1)`,
		`INSERT INTO comments (id, card_id, user_id, body_md, created_at)
		 VALUES (1, 1, 1, 'The debt figures are in the appendix', 1)`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			t.Fatalf("seed: %v\n%s", err, s)
		}
	}
}

func TestSearchGroupsEveryKind(t *testing.T) {
	db := store.OpenTemp(t)
	seed(t, db)

	groups, err := Search(context.Background(), db, "debt", 0, Everything())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{KindProposition, KindCard, KindBlock, KindLink, KindFile, KindComment}
	if len(groups) != len(want) {
		t.Fatalf("groups = %v, want %v", kinds(groups), want)
	}
	for i, k := range want {
		if groups[i].Kind != k {
			t.Fatalf("groups = %v, want %v", kinds(groups), want)
		}
		if len(groups[i].Hits) != 1 {
			t.Errorf("%s: %d hits, want 1", k, len(groups[i].Hits))
		}
	}
	block := groups[2].Hits[0]
	if block.ID != 1 {
		t.Errorf("the deleted block was returned: %+v", block)
	}
	if block.Title != "Research" || block.Snippet == "" || block.PropositionID != 1 {
		t.Errorf("block hit = %+v", block)
	}
	if file := groups[4].Hits[0]; file.Title != "debt-tape.wav" {
		t.Errorf("an unfinished upload was returned: %+v", file)
	}
}

func TestSearchMatchesUsersByName(t *testing.T) {
	db := store.OpenTemp(t)
	seed(t, db)

	// A name is matched anywhere inside it and nowhere else: "nora" is in no FTS
	// table and in no proposition title.
	groups, err := Search(context.Background(), db, "ora Stud", 0, Everything())
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].Kind != KindUser || groups[0].Hits[0].ID != 1 {
		t.Fatalf("groups = %v", kinds(groups))
	}
}

func TestSearchSurvivesFTSSyntax(t *testing.T) {
	db := store.OpenTemp(t)
	seed(t, db)

	for _, q := range []string{`"debt`, `debt*`, `debt NEAR crisis`, `debt AND (`, `^debt`, `debt OR OR`} {
		if _, err := Search(context.Background(), db, q, 0, Everything()); err != nil {
			t.Errorf("Search(%q): %v", q, err)
		}
	}
}

func TestSearchOfPunctuationIsEmpty(t *testing.T) {
	db := store.OpenTemp(t)
	seed(t, db)

	groups, err := Search(context.Background(), db, `*"()^ -`, 0, Everything())
	if err != nil {
		t.Fatal(err)
	}
	if groups != nil {
		t.Fatalf("groups = %v, want none", kinds(groups))
	}
}

func kinds(groups []Group) []string {
	var out []string
	for _, g := range groups {
		out = append(out, g.Kind)
	}
	return out
}

// The membership test belongs inside the query, not over its results: the
// limit has to count hits the reader may see. Filtering afterwards lets one
// busy proposition they are not a member of fill the page and hide their own.
func TestABusyPropositionDoesNotHideTheReadersOwnHits(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTemp(t)
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	mustExec(`INSERT INTO users (id, handle, email, name, initials, colour, role, password_hash, created_at)
		VALUES (1, 'grace', 'grace@example.com', 'Grace Hopper', 'GH', '#111', 'editor', 'x', 0)`)
	for _, p := range []int64{1, 2} {
		mustExec(`INSERT INTO propositions (id, number, title, status, position, created_at)
			VALUES (?, ?, 'Prop', 'idea', 'V', 0)`, p, p)
		mustExec(`INSERT INTO columns (id, proposition_id, name, position) VALUES (?, ?, 'Research', 'V')`, p, p)
	}
	// The reader is a member of the second one only.
	mustExec(`INSERT INTO proposition_members (proposition_id, user_id) VALUES (2, 1)`)

	// Far more matches in the proposition they cannot read than any page holds.
	for i := 0; i < 55; i++ {
		mustExec(`INSERT INTO cards (id, proposition_id, column_id, position, title, created_at)
			VALUES (?, 1, 1, ?, 'tidal survey', 0)`, i+1, "V"+strconv.Itoa(i))
	}
	mustExec(`INSERT INTO cards (id, proposition_id, column_id, position, title, created_at)
		VALUES (100, 2, 2, 'V', 'tidal gauge', 0)`)

	groups, err := Search(ctx, db, "tidal", 0, Member(1))
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, g := range groups {
		for _, h := range g.Hits {
			if h.Kind != KindCard {
				continue
			}
			if h.PropositionID != 2 {
				t.Errorf("a card of proposition %d reached a reader who is not a member", h.PropositionID)
			}
			titles = append(titles, h.Title)
		}
	}
	if len(titles) != 1 || titles[0] != "tidal gauge" {
		t.Errorf("the reader's own card is %v, want the one in their proposition", titles)
	}

	// An owner still reads all fifty six.
	groups, err = Search(ctx, db, "tidal", MaxLimit, Everything())
	if err != nil {
		t.Fatal(err)
	}
	cards := 0
	for _, g := range groups {
		if g.Kind == KindCard {
			cards = len(g.Hits)
		}
	}
	if cards != MaxLimit {
		t.Errorf("an owner asking for %d cards got %d", MaxLimit, cards)
	}
}
