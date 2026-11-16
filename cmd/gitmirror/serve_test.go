package main

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"gitmirror/internal/app"
	"gitmirror/internal/config"
)

// syncBuffer is a goroutine-safe bytes.Buffer.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// holdLock builds a locked App on the test env's state dir, as another
// process would.
func (e *testEnv) holdLock() *app.App {
	e.t.Helper()
	cfg, err := config.Load(e.cfgPath)
	if err != nil {
		e.t.Fatal(err)
	}
	a, err := app.Build(context.Background(), cfg, appOptions())
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { a.Close() })
	return a
}

// withServer appends a server section to the test config.
func (e *testEnv) withServer(listen, secret string) {
	e.t.Helper()
	e.withServerMetrics(listen, secret, "off")
}

// withServerMetrics is withServer with an explicit server.metrics_listen.
func (e *testEnv) withServerMetrics(listen, secret, metrics string) {
	e.t.Helper()
	e.withServerUI(listen, secret, metrics, "off")
}

// withServerUI is withServerMetrics with an explicit server.ui_listen.
func (e *testEnv) withServerUI(listen, secret, metrics, ui string) {
	e.t.Helper()
	b, err := os.ReadFile(e.cfgPath)
	if err != nil {
		e.t.Fatal(err)
	}
	s := string(b) + "server:\n  listen: '" + listen + "'\n  metrics_listen: '" + metrics + "'\n  ui_listen: '" + ui + "'\n"
	if secret != "" {
		s += "  webhook_secret: '" + secret + "'\n"
	}
	if err := os.WriteFile(e.cfgPath, []byte(s), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

func TestHelpListsServe(t *testing.T) {
	code, out, errOut := runCLI([]string{"--help"}, nil)
	wantCode(t, code, 0, out, errOut)
	if !strings.Contains(out, "serve") {
		t.Fatalf("help lacks serve:\n%s", out)
	}
	code, out, errOut = runCLI([]string{"serve", "--help"}, nil)
	wantCode(t, code, 0, out, errOut)
	for _, f := range []string{"--lock-timeout", "--shutdown-timeout"} {
		if !strings.Contains(out, f) {
			t.Fatalf("serve help lacks %s:\n%s", f, out)
		}
	}
	code, out, errOut = runCLI([]string{"sync", "--help"}, nil)
	wantCode(t, code, 0, out, errOut)
	if !strings.Contains(out, "--lock-timeout") {
		t.Fatalf("sync help lacks --lock-timeout:\n%s", out)
	}
}

func TestSyncWhileLockedExits1(t *testing.T) {
	e := newTestEnv(t)
	e.addSource("alice", "a")
	e.holdLock()
	code, out, errOut := e.cli("sync")
	wantCode(t, code, 1, out, errOut)
	if !strings.Contains(errOut, "another gitmirror process is using state dir") {
		t.Fatalf("stderr = %q", errOut)
	}
}

func TestSyncLockTimeoutWaits(t *testing.T) {
	e := newTestEnv(t)
	e.addSource("alice", "a")
	a := e.holdLock()
	go func() {
		time.Sleep(5 * time.Millisecond)
		a.Close()
	}()
	code, out, errOut := e.cli("sync", "--lock-timeout", "30s")
	wantCode(t, code, 0, out, errOut)
}

func TestPlanWhileLockedSucceeds(t *testing.T) {
	e := newTestEnv(t)
	e.addSource("alice", "a")
	e.holdLock()
	code, out, errOut := e.cli("plan")
	wantCode(t, code, 0, out, errOut)
}

func TestServeWhileLockedExits1(t *testing.T) {
	e := newTestEnv(t)
	e.withServer("127.0.0.1:0", "")
	e.holdLock()
	code, out, errOut := e.cli("serve")
	wantCode(t, code, 1, out, errOut)
	if !strings.Contains(errOut, "another gitmirror process is using state dir") {
		t.Fatalf("stderr = %q", errOut)
	}
}

func TestServeBadConfigExits2(t *testing.T) {
	code, out, errOut := runCLI([]string{"--config", "does-not-exist.yaml", "serve"}, nil)
	wantCode(t, code, 2, out, errOut)
}

var addrRe = regexp.MustCompile(`addr=(127\.0\.0\.1:\d+)`)

func TestServeEndToEnd(t *testing.T) {
	e := newTestEnv(t)
	e.addSource("alice", "a")
	e.withServer("127.0.0.1:0", "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var so, se syncBuffer
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{"--config", e.cfgPath, "serve", "--shutdown-timeout", "30s"}, &so, &se, func(string) (string, bool) { return "", false })
	}()

	var addr string
	deadline := time.Now().Add(30 * time.Second)
	for addr == "" {
		if m := addrRe.FindStringSubmatch(se.String()); m != nil {
			addr = m[1]
			break
		}
		select {
		case code := <-done:
			t.Fatalf("serve exited early with %d\nstderr:\n%s", code, se.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("no listen address logged\nstderr:\n%s", se.String())
		}
		time.Sleep(2 * time.Millisecond)
	}
	resp, err := http.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz = %d", resp.StatusCode)
	}
	if !strings.Contains(se.String(), "webhooks are disabled") {
		t.Fatalf("no webhook warning\nstderr:\n%s", se.String())
	}

	cancel()
	select {
	case code := <-done:
		wantCode(t, code, 0, so.String(), se.String())
	case <-time.After(60 * time.Second):
		t.Fatal("serve did not exit after cancel")
	}
	if strings.Contains(se.String()+so.String(), sentinel) {
		t.Fatal("token printed")
	}
}

var metricsAddrRe = regexp.MustCompile(`metrics listening.*addr=(127\.0\.0\.1:\d+)`)

func TestServeMetricsOnSeparateListener(t *testing.T) {
	e := newTestEnv(t)
	e.addSource("alice", "a")
	e.withServerMetrics("127.0.0.1:0", "", "127.0.0.1:0")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var so, se syncBuffer
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{"--config", e.cfgPath, "serve", "--shutdown-timeout", "30s"}, &so, &se, func(string) (string, bool) { return "", false })
	}()
	var mainAddr, metrics string
	deadline := time.Now().Add(30 * time.Second)
	for mainAddr == "" || metrics == "" {
		if m := addrRe.FindStringSubmatch(se.String()); m != nil {
			mainAddr = m[1]
		}
		if m := metricsAddrRe.FindStringSubmatch(se.String()); m != nil {
			metrics = m[1]
		}
		if time.Now().After(deadline) {
			t.Fatalf("addresses not logged\nstderr:\n%s", se.String())
		}
		time.Sleep(2 * time.Millisecond)
	}
	code := func(url string) int {
		resp, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := code("http://" + metrics + "/metrics"); c != http.StatusOK {
		t.Fatalf("metrics listener /metrics = %d", c)
	}
	if c := code("http://" + mainAddr + "/metrics"); c != http.StatusNotFound {
		t.Fatalf("main listener /metrics = %d", c)
	}
	cancel()
	select {
	case c := <-done:
		wantCode(t, c, 0, so.String(), se.String())
	case <-time.After(60 * time.Second):
		t.Fatal("serve did not exit after cancel")
	}
}

var uiAddrRe = regexp.MustCompile(`web UI: http://(127\.0\.0\.1:\d+)`)

// startServe runs serve until the test ends and waits until it started.
func (e *testEnv) startServe() (stderr *syncBuffer, stop func() int) {
	e.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var so, se syncBuffer
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{"--config", e.cfgPath, "serve", "--shutdown-timeout", "30s"}, &so, &se, func(string) (string, bool) { return "", false })
	}()
	stop = func() int {
		cancel()
		select {
		case c := <-done:
			done <- c
			return c
		case <-time.After(60 * time.Second):
			e.t.Fatal("serve did not exit after cancel")
			return -1
		}
	}
	e.t.Cleanup(func() { stop() })
	deadline := time.Now().Add(30 * time.Second)
	for !strings.Contains(se.String(), "server: started") {
		select {
		case code := <-done:
			e.t.Fatalf("serve exited early with %d\nstderr:\n%s", code, se.String())
		default:
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("serve did not start\nstderr:\n%s", se.String())
		}
		time.Sleep(2 * time.Millisecond)
	}
	return &se, stop
}

func TestServeWebUI(t *testing.T) {
	e := newTestEnv(t)
	e.withServerUI("127.0.0.1:0", "", "off", "127.0.0.1:0")
	se, stop := e.startServe()
	m := uiAddrRe.FindStringSubmatch(se.String())
	if m == nil {
		t.Fatalf("web UI address not logged\nstderr:\n%s", se.String())
	}
	ui := m[1]
	if !strings.Contains(se.String(), "web UI setup: open http://"+ui+"/setup and enter setup code ") {
		t.Fatalf("no setup code message\nstderr:\n%s", se.String())
	}
	resp, err := http.Get("http://" + ui + "/setup")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Security-Policy") == "" {
		t.Fatalf("GET /setup = %d %v", resp.StatusCode, resp.Header)
	}
	if c := stop(); c != 0 {
		t.Fatalf("serve exit = %d\nstderr:\n%s", c, se.String())
	}
}

func TestServeWebUIOff(t *testing.T) {
	e := newTestEnv(t)
	e.withServer("127.0.0.1:0", "")
	se, _ := e.startServe()
	if !strings.Contains(se.String(), "web UI is disabled") || strings.Contains(se.String(), "setup code") {
		t.Fatalf("stderr:\n%s", se.String())
	}
}
