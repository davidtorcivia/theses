package notify

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/davidtorcivia/theses/internal/notify/channel"
	"github.com/davidtorcivia/theses/internal/store"
)

func TestChannelValidate(t *testing.T) {
	tests := []struct {
		name string
		c    Channel
		ok   bool
	}{
		{"email needs nothing", Channel{Kind: KindEmail}, true},
		{"pushover needs a user key", Channel{Kind: KindPushover}, false},
		{"pushover with a key", Channel{Kind: KindPushover, Config: Config{UserKey: "k"}}, true},
		{"ntfy needs a topic", Channel{Kind: KindNtfy}, false},
		{"an ntfy topic is one word", Channel{Kind: KindNtfy, Config: Config{Topic: "a/b"}}, false},
		{"ntfy with a topic", Channel{Kind: KindNtfy, Config: Config{Topic: "alerts"}}, true},
		{"a webhook needs a URL", Channel{Kind: KindWebhook}, false},
		{"a webhook URL is http", Channel{Kind: KindWebhook, Config: Config{URL: "ftp://example.com"}}, false},
		{"a webhook URL", Channel{Kind: KindWebhook, Config: Config{URL: "https://example.com/h"}}, true},
		{"an unknown kind", Channel{Kind: "carrier pigeon"}, false},
		{"quiet hours that are not times", Channel{Kind: KindEmail, QuietFrom: "night", QuietTo: "07:00"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.c.Validate(); (err == nil) != tt.ok {
				t.Errorf("Validate = %v", err)
			}
		})
	}
}

func TestChannelLabelShowsNoSecret(t *testing.T) {
	secret := "abcdefgh1234"
	c := Channel{Kind: KindPushover, Config: Config{UserKey: secret}}
	label := c.Label("")
	if strings.Contains(label, secret) {
		t.Fatalf("the label carries the key: %q", label)
	}
	if !strings.HasSuffix(label, "1234") {
		t.Errorf("label = %q, want the last four characters", label)
	}
	if got := (Channel{Kind: KindNtfy, Config: Config{Topic: "alerts"}}).Label(""); got != "ntfy.sh/alerts" {
		t.Errorf("ntfy label = %q", got)
	}
	if got := (Channel{Kind: KindEmail}).Label("ada@example.com"); got != "ada@example.com" {
		t.Errorf("email label = %q", got)
	}
}

func TestConfigIsEncryptedAtRest(t *testing.T) {
	f := newFixture(t)
	grace := f.user(t, "grace")
	saved := f.channel(t, Channel{UserID: grace, Kind: KindNtfy,
		Config: Config{Topic: "alerts", Token: "tk_secret_value"}})

	var stored string
	if err := f.db.QueryRowContext(context.Background(),
		`SELECT config_json FROM notification_channels WHERE id = ?`, saved.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, "tk_secret_value") || strings.Contains(stored, "alerts") {
		t.Fatalf("the config is in the clear: %q", stored)
	}
	back, err := GetChannel(context.Background(), f.db, f.set, saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if back.Config.Token != "tk_secret_value" || back.Config.Topic != "alerts" {
		t.Errorf("read back %+v", back.Config)
	}
}

func TestAChannelBelongsToOneAccount(t *testing.T) {
	f := newFixture(t)
	ada := f.user(t, "ada")
	grace := f.user(t, "grace")
	c := f.channel(t, Channel{UserID: grace, Kind: KindNtfy, Config: Config{Topic: "t"}})

	if err := DeleteChannel(context.Background(), f.db, c.ID, ada); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Ada deleted Grace's channel: %v", err)
	}
	stolen := c
	stolen.UserID = ada
	if _, err := SaveChannel(context.Background(), f.db, f.set, stolen); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Ada wrote to Grace's channel: %v", err)
	}
	if err := DeleteChannel(context.Background(), f.db, c.ID, grace); err != nil {
		t.Errorf("Grace could not delete her own: %v", err)
	}
}

func TestDeletingAChannelTakesItsRulesAndItsQueue(t *testing.T) {
	f := newFixture(t)
	grace := f.user(t, "grace")
	c := f.channel(t, Channel{UserID: grace, Kind: KindNtfy, Config: Config{Topic: "t"}}, "moved")
	f.queueOne(t, c)

	if err := DeleteChannel(context.Background(), f.db, c.ID, grace); err != nil {
		t.Fatal(err)
	}
	if got := f.outbox(t); len(got) != 0 {
		t.Errorf("%d queued messages outlived their channel", len(got))
	}
	rules, err := Rules(context.Background(), f.db, grace)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 0 {
		t.Errorf("rules = %v, want none", rules)
	}
}

func TestStartingChannelsFollowTheOwnersDefaults(t *testing.T) {
	f := newFixture(t)
	if err := f.set.Set(context.Background(), "notify.defaults",
		[]string{"assigned\nmentioned\nnot an event"}, 0); err != nil {
		t.Fatal(err)
	}
	grace := f.user(t, "grace")
	if err := StartingChannels(context.Background(), f.db, f.set, grace); err != nil {
		t.Fatal(err)
	}
	channels, err := ListChannels(context.Background(), f.db, f.set, grace)
	if err != nil {
		t.Fatal(err)
	}
	if len(channels) != 1 || channels[0].Kind != KindEmail || !channels[0].Verified() {
		t.Fatalf("channels = %+v, want one verified email channel", channels)
	}
	rules, err := Rules(context.Background(), f.db, grace)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 2 || rules["assigned"] == nil || rules["mentioned"] == nil {
		t.Errorf("rules = %v, want the two the owner named and not the one that is not an event", rules)
	}
}

func TestSetRulesRefusesWhatIsNotOnTheMatrix(t *testing.T) {
	f := newFixture(t)
	ada := f.user(t, "ada")
	grace := f.user(t, "grace")
	mine := f.channel(t, Channel{UserID: grace, Kind: KindNtfy, Config: Config{Topic: "t"}})
	hers := f.channel(t, Channel{UserID: ada, Kind: KindNtfy, Config: Config{Topic: "t"}})

	ctx := context.Background()
	if err := SetRules(ctx, f.db, f.set, grace, map[string][]int64{"nonsense": {mine.ID}}); err == nil {
		t.Error("an event nobody can tick was stored")
	}
	if err := SetRules(ctx, f.db, f.set, grace, map[string][]int64{"moved": {hers.ID}}); err == nil {
		t.Error("Grace ticked a cell on Ada's channel")
	}
	if err := SetRules(ctx, f.db, f.set, grace, map[string][]int64{"moved": {mine.ID}}); err != nil {
		t.Fatal(err)
	}
	rules, err := Rules(ctx, f.db, grace)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules["moved"]) != 1 {
		t.Errorf("rules = %v", rules)
	}
	// A matrix with nothing ticked is a matrix with nothing ticked.
	if err := SetRules(ctx, f.db, f.set, grace, map[string][]int64{}); err != nil {
		t.Fatal(err)
	}
	if rules, _ := Rules(ctx, f.db, grace); len(rules) != 0 {
		t.Errorf("rules = %v, want none", rules)
	}
}

func TestEveryEventHasALabelAndAKey(t *testing.T) {
	seen := map[string]bool{}
	for _, e := range Events {
		if e.Key == "" || e.Label == "" {
			t.Errorf("event %+v is missing a key or a label", e)
		}
		if seen[e.Key] {
			t.Errorf("%q is in the table twice", e.Key)
		}
		seen[e.Key] = true
		if e.Priority < -1 || e.Priority > 1 {
			t.Errorf("%q has priority %d, which no channel understands", e.Key, e.Priority)
		}
	}
	if _, ok := LookupEvent(digestEvent); ok {
		t.Error("the digest is a row of the matrix, and it should not be")
	}
}

func TestSenderForNeedsTheWorkspacePushoverToken(t *testing.T) {
	f := newFixture(t)
	c := Channel{Kind: KindPushover, Config: Config{UserKey: "k"}}
	if _, err := f.s.senderFor(context.Background(), c, ""); err == nil {
		t.Fatal("a Pushover channel worked with no application token")
	}
	if err := f.set.Set(context.Background(), "notify.pushover_token", []string{"app"}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.senderFor(context.Background(), c, ""); err != nil {
		t.Errorf("senderFor = %v", err)
	}
	// An ntfy channel with no server of its own takes the workspace's.
	if err := f.set.Set(context.Background(), "notify.ntfy_server",
		[]string{"https://ntfy.example.com"}, 0); err != nil {
		t.Fatal(err)
	}
	snd, err := f.s.senderFor(context.Background(), Channel{Kind: KindNtfy, Config: Config{Topic: "t"}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := snd.(channel.Ntfy); !ok || got.Server != "https://ntfy.example.com" {
		t.Errorf("sender = %+v, want the workspace server", snd)
	}
}
