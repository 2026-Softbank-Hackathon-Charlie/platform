package auth

import (
	"strings"
	"sync"
	"time"
)

// LoginLimiter refuses logins for a username once it has max attempts in a
// sliding window. An attempt counts when it starts, so concurrent attempts
// cannot pass the limit, and a successful login clears the username's
// attempts (Reset): what remains counted are failures. Usernames are compared
// in lower case. It is safe for concurrent use.
//
// Memory is bounded: at most max attempts per username and maxKeys
// usernames. When a new username finds it full, the usernames whose attempts
// all left the window are forgotten first, then the one whose newest attempt
// is oldest.
type LoginLimiter struct {
	max     int
	window  time.Duration
	maxKeys int

	mu       sync.Mutex
	attempts map[string][]time.Time // oldest first, all inside the window when last written
}

// NewLoginLimiter returns a limiter allowing max attempts per username in
// window, for at most maxKeys usernames.
func NewLoginLimiter(max int, window time.Duration, maxKeys int) *LoginLimiter {
	return &LoginLimiter{max: max, window: window, maxKeys: maxKeys, attempts: map[string][]time.Time{}}
}

// Attempt records an attempt for username at now and reports true. When the
// username already has max attempts in the window it records nothing and
// reports false with the time until the oldest of them leaves the window.
func (l *LoginLimiter) Attempt(username string, now time.Time) (time.Duration, bool) {
	key := strings.ToLower(username)
	l.mu.Lock()
	defer l.mu.Unlock()
	old, known := l.attempts[key]
	live := l.live(old, now)
	if len(live) >= l.max {
		l.attempts[key] = live
		return live[0].Add(l.window).Sub(now), false
	}
	if !known && len(l.attempts) >= l.maxKeys {
		l.makeRoom(now)
	}
	l.attempts[key] = append(live, now)
	return 0, true
}

// Reset forgets username's attempts.
func (l *LoginLimiter) Reset(username string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.attempts, strings.ToLower(username))
}

// live returns the attempts of ts still inside the window at now.
func (l *LoginLimiter) live(ts []time.Time, now time.Time) []time.Time {
	since := now.Add(-l.window)
	i := 0
	for i < len(ts) && !ts[i].After(since) {
		i++
	}
	return ts[i:]
}

// makeRoom frees at least one username. Call with l.mu held.
func (l *LoginLimiter) makeRoom(now time.Time) {
	var oldestKey string
	var oldest time.Time
	for k, ts := range l.attempts {
		if len(l.live(ts, now)) == 0 {
			delete(l.attempts, k)
			continue
		}
		if newest := ts[len(ts)-1]; oldestKey == "" || newest.Before(oldest) {
			oldestKey, oldest = k, newest
		}
	}
	if len(l.attempts) >= l.maxKeys {
		delete(l.attempts, oldestKey)
	}
}
