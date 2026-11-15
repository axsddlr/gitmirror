package web

import (
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// parallelWrong sends n wrong-password POSTs to path at once from one IP
// and counts the answers by status code.
func parallelWrong(t *testing.T, c *client, path string, form url.Values, n int) map[int]int {
	t.Helper()
	var (
		mu    sync.Mutex
		codes = map[int]int{}
		wg    sync.WaitGroup
		start = make(chan struct{})
	)
	body := form.Encode()
	for range n {
		wg.Go(func() {
			req, err := http.NewRequest(http.MethodPost, c.h.srv.URL+path, strings.NewReader(body))
			if err != nil {
				t.Error(err)
				return
			}
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			<-start
			res, err := c.c.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			res.Body.Close()
			mu.Lock()
			codes[res.StatusCode]++
			mu.Unlock()
		})
	}
	close(start)
	wg.Wait()
	return codes
}

func TestLoginRateLimitParallel(t *testing.T) {
	h := newHarness(t)
	h.admin()
	c := h.client()
	tok := c.csrf("/login")
	codes := parallelWrong(t, c, "/login", url.Values{"csrf": {tok}, "password": {"wrong password!!"}}, 30)
	if codes[http.StatusUnauthorized] > maxFailuresPerIP || codes[http.StatusUnauthorized]+codes[http.StatusTooManyRequests] != 30 {
		t.Fatalf("30 parallel wrong passwords: %v; want at most %d checked, the rest 429", codes, maxFailuresPerIP)
	}
	if r := c.login(testPassword); r.code != http.StatusTooManyRequests {
		t.Fatalf("right password after the burst = %d", r.code)
	}
}

func TestChangePasswordRateLimitParallel(t *testing.T) {
	h := newHarness(t)
	c := h.admin()
	tok := c.csrf("/settings")
	codes := parallelWrong(t, c, "/settings/password", url.Values{"csrf": {tok}, "current": {"wrong password!!"},
		"password": {"new password 123"}, "confirm": {"new password 123"}}, 30)
	if codes[http.StatusUnauthorized] > maxFailuresPerIP || codes[http.StatusUnauthorized]+codes[http.StatusTooManyRequests] != 30 {
		t.Fatalf("30 parallel wrong passwords: %v; want at most %d checked, the rest 429", codes, maxFailuresPerIP)
	}
}

func TestLimiterSuccessClears(t *testing.T) {
	h := newHarness(t)
	h.admin()
	c := h.client()
	for range maxFailuresPerIP - 1 {
		if r := c.login("wrong password!!"); r.code != http.StatusUnauthorized {
			t.Fatalf("wrong = %d", r.code)
		}
	}
	if r := c.login(testPassword); r.code != http.StatusSeeOther {
		t.Fatalf("right = %d", r.code)
	}
	c = h.client()
	for i := range maxFailuresPerIP {
		if r := c.login("wrong password!!"); r.code != http.StatusUnauthorized {
			t.Fatalf("wrong %d after a success = %d", i, r.code)
		}
	}
}
