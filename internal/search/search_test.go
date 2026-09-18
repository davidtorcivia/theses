package search

import (
	"context"
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
	if got := likePattern(`100% of _it_`); got != `%100\% of \_it\_%` {
		t.Errorf("likePattern = %q", got)
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

	groups, err := Search(context.Background(), db, "debt", 0)
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
	groups, err := Search(context.Background(), db, "ora Stud", 0)
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
		if _, err := Search(context.Background(), db, q, 0); err != nil {
			t.Errorf("Search(%q): %v", q, err)
		}
	}
}

func TestSearchOfPunctuationIsEmpty(t *testing.T) {
	db := store.OpenTemp(t)
	seed(t, db)

	groups, err := Search(context.Background(), db, `*"()^ -`, 0)
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
