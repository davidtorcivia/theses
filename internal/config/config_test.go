package config

import (
	"log/slog"
	"strings"
	"testing"
)

func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

const (
	hexKey  = "d3f0a1b2c4e5968778695a4b3c2d1e0fa9b8c7d6e5f4031223344556678899aa"
	rawKey  = "correct horse battery staple, thirty-eight bytes"
	otherHx = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

func good() map[string]string {
	return map[string]string{
		"THESES_BASE_URL":    "https://theses.example.com/",
		"THESES_SECRET_KEY":  hexKey,
		"THESES_SESSION_KEY": rawKey,
	}
}

func TestLoadDefaults(t *testing.T) {
	c, err := Load(env(good()))
	if err != nil {
		t.Fatal(err)
	}
	if c.Bind != ":8080" || c.DataDir != "data" {
		t.Errorf("bind %q data dir %q", c.Bind, c.DataDir)
	}
	if c.BaseURL != "https://theses.example.com" {
		t.Errorf("trailing slash not stripped: %q", c.BaseURL)
	}
	if !c.CookieSecure {
		t.Error("https base URL should keep cookie Secure on")
	}
	if len(c.SecretKey) != 32 {
		t.Errorf("hex key should decode to 32 bytes, got %d", len(c.SecretKey))
	}
	if string(c.SessionKey) != rawKey {
		t.Error("non-hex key should be used raw")
	}
	if c.TrustProxy || c.Dev {
		t.Error("booleans default to false")
	}
	if c.LogLevel != slog.LevelInfo {
		t.Errorf("log level %v", c.LogLevel)
	}
}

func TestHTTPBaseURLTurnsOffSecureCookies(t *testing.T) {
	m := good()
	m["THESES_BASE_URL"] = "http://localhost:8080"
	c, err := Load(env(m))
	if err != nil {
		t.Fatal(err)
	}
	if c.CookieSecure {
		t.Error("http base URL must turn cookie Secure off")
	}
}

func TestLoadRejects(t *testing.T) {
	cases := []struct {
		name, key, value, want string
	}{
		{"missing base URL", "THESES_BASE_URL", "", "THESES_BASE_URL is required"},
		{"relative base URL", "THESES_BASE_URL", "/theses", "absolute http or https URL"},
		{"ftp base URL", "THESES_BASE_URL", "ftp://example.com", "absolute http or https URL"},
		{"missing secret key", "THESES_SECRET_KEY", "", "THESES_SECRET_KEY is required"},
		{"short secret key", "THESES_SECRET_KEY", "too short", "too short"},
		{"short hex key", "THESES_SECRET_KEY", "abcdef", "too short"},
		{"placeholder key", "THESES_SESSION_KEY", strings.Repeat("changeme", 5), "placeholder"},
		{"missing session key", "THESES_SESSION_KEY", "", "THESES_SESSION_KEY is required"},
		{"bad bool", "THESES_TRUST_PROXY", "yes please", "must be true or false"},
		{"bad level", "THESES_LOG_LEVEL", "chatty", "debug, info, warn or error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := good()
			m[tc.key] = tc.value
			_, err := Load(env(m))
			if err == nil {
				t.Fatalf("%s=%q accepted", tc.key, tc.value)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestKeysMayDiffer(t *testing.T) {
	m := good()
	m["THESES_SESSION_KEY"] = otherHx
	c, err := Load(env(m))
	if err != nil {
		t.Fatal(err)
	}
	if string(c.SecretKey) == string(c.SessionKey) {
		t.Error("keys should not be conflated")
	}
}
