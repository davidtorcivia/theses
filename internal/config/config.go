// Package config reads the bootstrap environment variables. Everything else the
// app knows is configured in the UI and stored in the database.
package config

import (
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
)

const MinKeyLen = 32

type Config struct {
	Bind       string
	DataDir    string
	BaseURL    string
	SecretKey  []byte // encrypts settings secrets at rest
	SessionKey []byte // signs session cookies, CSRF tokens and token lookups
	TrustProxy bool
	Dev        bool
	LogLevel   slog.Level

	// CookieSecure follows the base URL scheme so http://localhost works.
	CookieSecure bool
}

// Load reads the environment through lookup, which is os.LookupEnv in production.
func Load(lookup func(string) (string, bool)) (*Config, error) {
	get := func(key, def string) string {
		if v, ok := lookup(key); ok && v != "" {
			return v
		}
		return def
	}

	c := &Config{
		Bind:    get("THESES_BIND", ":8080"),
		DataDir: get("THESES_DATA_DIR", "data"),
	}

	base := get("THESES_BASE_URL", "")
	if base == "" {
		return nil, fmt.Errorf("THESES_BASE_URL is required: the absolute public URL of this install, for example https://theses.example.com")
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("THESES_BASE_URL must be an absolute http or https URL, got %q", base)
	}
	c.BaseURL = strings.TrimRight(base, "/")
	c.CookieSecure = u.Scheme == "https"

	if c.SecretKey, err = key(lookup, "THESES_SECRET_KEY"); err != nil {
		return nil, err
	}
	if c.SessionKey, err = key(lookup, "THESES_SESSION_KEY"); err != nil {
		return nil, err
	}

	if c.TrustProxy, err = boolVar(lookup, "THESES_TRUST_PROXY"); err != nil {
		return nil, err
	}
	if c.Dev, err = boolVar(lookup, "THESES_DEV"); err != nil {
		return nil, err
	}

	switch strings.ToLower(get("THESES_LOG_LEVEL", "info")) {
	case "debug":
		c.LogLevel = slog.LevelDebug
	case "info":
		c.LogLevel = slog.LevelInfo
	case "warn", "warning":
		c.LogLevel = slog.LevelWarn
	case "error":
		c.LogLevel = slog.LevelError
	default:
		return nil, fmt.Errorf("THESES_LOG_LEVEL must be debug, info, warn or error")
	}

	return c, nil
}

func LoadEnv() (*Config, error) { return Load(os.LookupEnv) }

// key accepts hex or raw bytes and refuses anything a generated key would not be.
func key(lookup func(string) (string, bool), name string) ([]byte, error) {
	v, _ := lookup(name)
	if v == "" {
		return nil, fmt.Errorf("%s is required: generate one with `openssl rand -hex 32`", name)
	}
	b := []byte(v)
	if decoded, err := hex.DecodeString(v); err == nil && len(decoded) >= MinKeyLen {
		b = decoded
	}
	if len(b) < MinKeyLen {
		return nil, fmt.Errorf("%s is too short: %d bytes, need at least %d; generate one with `openssl rand -hex 32`", name, len(b), MinKeyLen)
	}
	if distinct(b) < 8 {
		return nil, fmt.Errorf("%s looks like a placeholder rather than a random key; generate one with `openssl rand -hex 32`", name)
	}
	return b, nil
}

func distinct(b []byte) int {
	var seen [256]bool
	n := 0
	for _, c := range b {
		if !seen[c] {
			seen[c] = true
			n++
		}
	}
	return n
}

func boolVar(lookup func(string) (string, bool), name string) (bool, error) {
	v, ok := lookup(name)
	if !ok || v == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s must be true or false, got %q", name, v)
	}
	return b, nil
}
