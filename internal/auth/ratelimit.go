package auth

import (
	"sync"
	"time"
)

// Buckets rate limiting is applied to. Each attempt is counted against both the
// client address and whatever it named, so one address cannot grind through
// accounts and one account cannot be ground at from many addresses.
const (
	BucketLogin  = "login"
	BucketReset  = "reset"
	BucketInvite = "invite"
)

type limit struct {
	n      int
	window time.Duration
}

var limitsByBucket = map[string]limit{
	BucketLogin:  {n: 10, window: 5 * time.Minute},
	BucketReset:  {n: 5, window: time.Hour},
	BucketInvite: {n: 20, window: time.Hour},
}

// ponytail: in-memory, so the limits reset on restart and are per process. One
// process is the whole deployment; a second one would need the counts in SQLite.
type limiters struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

func newLimiters() *limiters { return &limiters{hits: map[string][]time.Time{}} }

// Allow counts one attempt against every key and reports whether all of them are
// still under the bucket's limit. An empty key is ignored.
func (a *Auth) Allow(bucket string, keys ...string) bool {
	l, ok := limitsByBucket[bucket]
	if !ok {
		return true
	}
	now := a.Now()
	cutoff := now.Add(-l.window)

	a.limits.mu.Lock()
	defer a.limits.mu.Unlock()

	allowed := true
	for _, k := range keys {
		if k == "" {
			continue
		}
		k = bucket + "\x00" + k
		kept := a.limits.hits[k][:0]
		for _, t := range a.limits.hits[k] {
			if t.After(cutoff) {
				kept = append(kept, t)
			}
		}
		if len(kept) >= l.n {
			allowed = false
		}
		a.limits.hits[k] = append(kept, now)
	}
	return allowed
}

// ResetLimits clears the counters for one set of keys, called after a successful
// sign-in so a person who mistyped twice is not held back.
func (a *Auth) ResetLimits(bucket string, keys ...string) {
	a.limits.mu.Lock()
	defer a.limits.mu.Unlock()
	for _, k := range keys {
		delete(a.limits.hits, bucket+"\x00"+k)
	}
}
