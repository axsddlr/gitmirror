package web

import (
	"net"
	"net/http"
	"sync"
	"time"
)

const (
	failureWindow     = 15 * time.Minute
	maxFailuresPerIP  = 5
	maxFailuresGlobal = 50
	maxTrackedIPs     = 10000
)

// limiter counts failed sign-in (or setup) attempts in a sliding window,
// per client IP and globally. It is in memory and bounded in size.
type limiter struct {
	now func() time.Time

	mu     sync.Mutex
	ips    map[string][]time.Time // failure times within the window, oldest first
	global []time.Time
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

// allow reports whether ip may make another attempt.
func (l *limiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := l.now().Add(-failureWindow)
	l.global = recent(l.global, cutoff)
	if len(l.global) >= maxFailuresGlobal {
		return false
	}
	ts := recent(l.ips[ip], cutoff)
	if len(ts) == 0 {
		delete(l.ips, ip)
	} else {
		l.ips[ip] = ts
	}
	return len(ts) < maxFailuresPerIP
}

// fail records a failed attempt by ip.
func (l *limiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	cutoff := now.Add(-failureWindow)
	if _, ok := l.ips[ip]; !ok && len(l.ips) >= maxTrackedIPs {
		for k, ts := range l.ips {
			if ts = recent(ts, cutoff); len(ts) == 0 {
				delete(l.ips, k)
			}
		}
		for k := range l.ips { // still full: forget a tenth, arbitrarily
			if len(l.ips) < maxTrackedIPs*9/10 {
				break
			}
			delete(l.ips, k)
		}
	}
	ts := append(recent(l.ips[ip], cutoff), now)
	if len(ts) > maxFailuresPerIP {
		ts = ts[len(ts)-maxFailuresPerIP:]
	}
	l.ips[ip] = ts
	l.global = append(recent(l.global, cutoff), now)
	if len(l.global) > maxFailuresGlobal {
		l.global = l.global[len(l.global)-maxFailuresGlobal:]
	}
}

// succeed forgets the failures of ip after a successful attempt.
func (l *limiter) succeed(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.ips, ip)
}

// clientIP is the host of r.RemoteAddr. Forwarding headers such as
// X-Forwarded-For are deliberately ignored: they are set by the client.
func clientIP(r *http.Request) string {
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return h
	}
	return r.RemoteAddr
}
