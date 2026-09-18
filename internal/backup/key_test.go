package backup

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"filippo.io/age"
)

// The identity has to parse, which is what proves the bech32 encoding: age
// checks the checksum over the whole string before it looks at the scalar.
func TestIdentityIsDerivedAndRoundTrips(t *testing.T) {
	key := []byte("an environment key of at least thirty-two bytes")
	id, err := identity(key)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(id.String(), "AGE-SECRET-KEY-1") {
		t.Fatalf("the identity is not an age secret key: %s", id.String())
	}

	var archive bytes.Buffer
	w, err := age.Encrypt(&archive, id.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, "the archive"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	again, err := identity(key)
	if err != nil {
		t.Fatal(err)
	}
	r, err := age.Decrypt(&archive, again)
	if err != nil {
		t.Fatalf("the same environment key does not decrypt it: %v", err)
	}
	plain, err := io.ReadAll(r)
	if err != nil || string(plain) != "the archive" {
		t.Fatalf("decrypted %q, %v", plain, err)
	}
}

func TestADifferentEnvironmentKeyIsADifferentIdentity(t *testing.T) {
	a, err := identity([]byte("an environment key of at least thirty-two bytes"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := identity([]byte("another environment key of at least thirty-two"))
	if err != nil {
		t.Fatal(err)
	}
	if a.String() == b.String() {
		t.Fatal("two environment keys derived the same identity")
	}
}
