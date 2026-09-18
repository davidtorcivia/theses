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
	BucketAPI    = "api"
)

type limit struct {
	n      int
	window time.Duration
}

var limitsByBucket = map[string]limit{
	BucketLogin:  {n: 10, window: 5 * time.Minute},
	BucketReset:  {n: 5, window: time.Hour},
	BucketInvite: {n: 20, window: time.Hour},
	// One token, a few requests a second: enough for an agent working through a
	// document, low enough that a loop cannot hold the one writer connection.
	BucketAPI: {n: 300, window: time.Minute},
}

// longestWindow is how old a key's newest attempt has to be before no bucket
// could still care about it, and so before the sweep may drop it.
var longestWindow = func() time.Duration {
	var d time.Duration
	for _, l := range limitsByBucket {
		if l.window > d {
			d = l.window
		}
	}
	return d
}()

// sweepEvery is how many Allow calls pass between full sweeps. Without one, a
// run of invented account names would leave a key each in the map forever.
const sweepEvery = 256

// ponytail: in-memory, so the limits reset on restart and are per process. One
// process is the whole deployment; a second one would need the counts in SQLite.
type limiters struct {
	mu    sync.Mutex
	hits  map[string][]time.Time
	calls int
}

func newLimiters() *limiters { return &limiters{hits: map[string][]time.Time{}} }

// Allow reports whether every key is still under the bucket's limit, and counts
// one attempt against each only when they all are. A refused attempt records
// nothing, so hammering a limit that is already reached cannot hold it open.
// An empty key is ignored.
func (a *Auth) Allow(bucket string, keys ...string) bool {
	l, ok := limitsByBucket[bucket]
	if !ok {
		return true
	}
	now := a.Now()
	cutoff := now.Add(-l.window)

	a.limits.mu.Lock()
	defer a.limits.mu.Unlock()
	a.limits.sweep(now)

	type counter struct {
		key  string
		kept []time.Time
	}
	var counters []counter
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
		if len(kept) == 0 {
			delete(a.limits.hits, k)
		} else {
			a.limits.hits[k] = kept
		}
		counters = append(counters, counter{k, kept})
	}

	for _, c := range counters {
		if len(c.kept) >= l.n {
			return false
		}
	}
	for _, c := range counters {
		a.limits.hits[c.key] = append(c.kept, now)
	}
	return true
}

// sweep drops keys whose newest attempt is older than any window still cares
// about. It runs on one call in sweepEvery: often enough to bound the map, rare
// enough that walking it does not matter.
func (l *limiters) sweep(now time.Time) {
	l.calls++
	if l.calls < sweepEvery {
		return
	}
	l.calls = 0
	cutoff := now.Add(-longestWindow)
	for k, ts := range l.hits {
		if len(ts) == 0 || !ts[len(ts)-1].After(cutoff) {
			delete(l.hits, k)
		}
	}
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
