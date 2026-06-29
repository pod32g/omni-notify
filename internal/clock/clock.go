// Package clock provides a small time abstraction so that code depending on the
// current time (dedup windows, retry backoff) can be tested deterministically.
package clock

import (
	"sync"
	"time"
)

// Clock reports the current time. Production code uses Real; tests use Fake.
type Clock interface {
	Now() time.Time
}

// Real is the production clock backed by time.Now (UTC).
type Real struct{}

// Now returns the current UTC time.
func (Real) Now() time.Time { return time.Now().UTC() }

// Fake is a controllable clock for tests. It is safe for concurrent use.
type Fake struct {
	mu sync.Mutex
	t  time.Time
}

// NewFake returns a Fake clock set to t.
func NewFake(t time.Time) *Fake { return &Fake{t: t.UTC()} }

// Now returns the fake clock's current time.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

// Advance moves the fake clock forward by d.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}

// Set moves the fake clock to t.
func (f *Fake) Set(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = t.UTC()
}
