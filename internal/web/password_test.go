package web

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestPasswordHashRoundTrip(t *testing.T) {
	if defaultIterations != 600000 {
		t.Fatalf("defaultIterations = %d", defaultIterations)
	}
	enc, err := hashPassword(testPassword)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(enc, testPassword) {
		t.Fatal("hash contains the password")
	}
	parts := strings.Split(enc, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" || parts[1] != fmt.Sprint(hashIterations) {
		t.Fatalf("format %q", enc)
	}
	ok, err := checkPassword(enc, testPassword)
	if err != nil || !ok {
		t.Fatalf("check right = %v %v", ok, err)
	}
	if ok, err := checkPassword(enc, testPassword+"x"); err != nil || ok {
		t.Fatalf("check wrong = %v %v", ok, err)
	}
	enc2, _ := hashPassword(testPassword)
	if enc2 == enc {
		t.Fatal("same salt twice")
	}
	// The iteration count is read from the stored value, not the default.
	old := hashIterations
	hashIterations = 2000
	defer func() { hashIterations = old }()
	if ok, _ := checkPassword(enc, testPassword); !ok {
		t.Fatal("stored iteration count ignored")
	}
}

func TestPasswordHashBadInput(t *testing.T) {
	for _, bad := range []string{"", "plain", "md5$1$a$b", "pbkdf2-sha256$x$AAAA$AAAA", "pbkdf2-sha256$0$AAAA$AAAA",
		"pbkdf2-sha256$1000$!!$AAAA", "pbkdf2-sha256$1000$AAAA$", "pbkdf2-sha256$99999999$AAAA$AAAA"} {
		if ok, err := checkPassword(bad, testPassword); ok || err == nil {
			t.Errorf("%q: %v %v", bad, ok, err)
		}
	}
}

func TestSetupCodeFormat(t *testing.T) {
	seen := map[string]bool{}
	for range 50 {
		c, err := newSetupCode()
		if err != nil {
			t.Fatal(err)
		}
		if len(c) != 14 || c[4] != '-' || c[9] != '-' {
			t.Fatalf("code %q", c)
		}
		for _, r := range strings.ReplaceAll(c, "-", "") {
			if !strings.ContainsRune(setupAlphabet, r) {
				t.Fatalf("code %q has %q", c, r)
			}
		}
		seen[c] = true
	}
	if len(seen) < 50 {
		t.Fatal("codes repeat")
	}
	for _, amb := range "0O1IL" {
		if strings.ContainsRune(setupAlphabet, amb) {
			t.Fatalf("ambiguous %q in alphabet", amb)
		}
	}
}

func TestLimiter(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l := newLimiter(func() time.Time { return now })
	for range maxFailuresPerIP {
		if !l.allow("1.1.1.1") {
			t.Fatal("blocked early")
		}
		l.fail("1.1.1.1")
	}
	if l.allow("1.1.1.1") {
		t.Fatal("not blocked after max failures")
	}
	if !l.allow("2.2.2.2") {
		t.Fatal("other IP blocked")
	}
	l.succeed("2.2.2.2")
	// release takes back an attempt without a verdict.
	l2 := newLimiter(func() time.Time { return now })
	for range maxFailuresPerIP * 2 {
		if !l2.allow("3.3.3.3") {
			t.Fatal("released attempts still counted")
		}
		l2.release("3.3.3.3")
	}
	if len(l2.global) != 0 || len(l2.ips) != 0 {
		t.Fatalf("release left %d global, %d ips", len(l2.global), len(l2.ips))
	}
	now = now.Add(failureWindow + time.Second)
	if !l.allow("1.1.1.1") {
		t.Fatal("still blocked after the window")
	}

	// Global cap: many IPs, few failures each. A success takes the
	// IP's attempts out of the global count too.
	l = newLimiter(func() time.Time { return now })
	for range maxFailuresPerIP - 1 {
		l.allow("4.4.4.4")
	}
	l.succeed("4.4.4.4")
	if len(l.global) != 0 {
		t.Fatalf("succeed left %d global attempts", len(l.global))
	}
	l = newLimiter(func() time.Time { return now })
	for i := range maxFailuresGlobal {
		ip := fmt.Sprintf("10.0.%d.%d", i/256, i%256)
		if !l.allow(ip) {
			t.Fatalf("blocked early at %d", i)
		}
		l.fail(ip)
	}
	if l.allow("192.0.2.1") {
		t.Fatal("global cap not applied")
	}
	now = now.Add(failureWindow + time.Second)
	if !l.allow("192.0.2.1") {
		t.Fatal("global cap not lifted after the window")
	}

	// Bounded size.
	l = newLimiter(func() time.Time { return now })
	for i := range maxTrackedIPs * 2 {
		l.allow(fmt.Sprintf("ip-%d", i))
		l.fail(fmt.Sprintf("ip-%d", i))
		l.global = nil // keep the global cap out of the way
	}
	if n := len(l.ips); n > maxTrackedIPs {
		t.Fatalf("tracked %d IPs, max %d", n, maxTrackedIPs)
	}
}
