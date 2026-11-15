package web

import (
	"net"
	"net/http"
	"slices"
	"sync"
	"time"
)

const (
	failureWindow    = 15 * time.Minute
	maxFailuresPerIP = 5
	// maxFailuresGlobal is far above maxFailuresPerIP: one IP can use at
	// most maxFailuresPerIP of it, so it takes many addresses to lock
	// everyone out (see the package doc).
	maxFailuresGlobal = 100
	maxTrackedIPs     = 10000
)

// limiter counts sign-in (or setup) attempts in a sliding window, per
// client IP and globally. It is in memory and bounded in size.
//
// allow reserves the attempt under the lock before the password is
// checked, so parallel requests cannot all get through before their
// failures are recorded: an attempt counts as a failure from the moment it
// is allowed until succeed (or release) takes it back.
type limiter struct {
	now func() time.Time

	mu     sync.Mutex
	ips    map[string][]time.Time // attempt times within the window, oldest first
	global []attempt
}

type attempt struct {
	at time.Time
	ip string
}

func newLimiter(now func() time.Time) *limiter {
	return &limiter{now: now, ips: map[string][]time.Time{}}
}

func recent(ts []time.Time, cutoff time.Time) []time.Time {
	i := 0
	for i < len(ts) && !ts[i].After(cutoff) {
		i++
	}
	return ts[i:]
}

// allow reports whether ip may make another attempt and, if so, reserves
// it: it counts as a failure until succeed or release is called.
func (l *limiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	cutoff := now.Add(-failureWindow)
	i := 0
	for i < len(l.global) && !l.global[i].at.After(cutoff) {
		i++
	}
	l.global = l.global[i:]
	if len(l.global) >= maxFailuresGlobal {
		return false
	}
	ts := recent(l.ips[ip], cutoff)
	if len(ts) >= maxFailuresPerIP {
		l.ips[ip] = ts
		return false
	}
	if _, ok := l.ips[ip]; !ok && len(l.ips) >= maxTrackedIPs {
		l.evict(cutoff)
	}
	l.ips[ip] = append(ts, now)
	l.global = append(l.global, attempt{now, ip})
	return true
}

// evict makes room in ips: expired entries go first, then a tenth,
// arbitrarily. Called with mu held.
func (l *limiter) evict(cutoff time.Time) {
	for k, ts := range l.ips {
		if ts = recent(ts, cutoff); len(ts) == 0 {
			delete(l.ips, k)
		}
	}
	for k := range l.ips {
		if len(l.ips) < maxTrackedIPs*9/10 {
			break
		}
		delete(l.ips, k)
	}
}

// fail records that ip's reserved attempt failed. The reservation made by
// allow already counts as the failure, so there is nothing to change; it
// is kept so that callers state every outcome.
func (l *limiter) fail(string) {}

// succeed forgets ip's failures and reserved attempts after a successful
// attempt (also in the global count).
func (l *limiter) succeed(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.ips, ip)
	l.global = slices.DeleteFunc(l.global, func(a attempt) bool { return a.ip == ip })
}

// release takes back one reserved attempt of ip that ended without a
// verdict (an internal error).
func (l *limiter) release(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if ts := l.ips[ip]; len(ts) > 0 {
		if len(ts) == 1 {
			delete(l.ips, ip)
		} else {
			l.ips[ip] = ts[:len(ts)-1]
		}
	}
	for i := len(l.global) - 1; i >= 0; i-- {
		if l.global[i].ip == ip {
			l.global = slices.Delete(l.global, i, i+1)
			break
		}
	}
}

// clientIP is the host of r.RemoteAddr. Forwarding headers such as
// X-Forwarded-For are deliberately ignored: they are set by the client.
func clientIP(r *http.Request) string {
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return h
	}
	return r.RemoteAddr
}
