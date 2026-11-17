package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"gitmirror/internal/state"
)

func TestHelpListsResetAdmin(t *testing.T) {
	code, out, errOut := runCLI([]string{"--help"}, nil)
	wantCode(t, code, 0, out, errOut)
	if !strings.Contains(out, "reset-admin") {
		t.Fatalf("help lacks reset-admin:\n%s", out)
	}
}

func TestResetAdmin(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	st := e.openState()
	if err := st.PutSetting(ctx, "admin_password", []byte("pbkdf2-sha256$1$x$y")); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := st.CreateUISession(ctx, state.UISession{IDHash: []byte("0123456789abcdef0123456789abcdef"), CSRF: "c", Created: now, LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutSetting(ctx, "keep_me", []byte("1")); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := e.cli("reset-admin")
	wantCode(t, code, 0, out, errOut)
	if !strings.Contains(out, "setup code") {
		t.Fatalf("stdout = %q", out)
	}
	if _, ok, err := st.Setting(ctx, "admin_password"); err != nil || ok {
		t.Fatalf("admin password still set (err %v)", err)
	}
	if _, ok, err := st.UISession(ctx, []byte("0123456789abcdef0123456789abcdef")); err != nil || ok {
		t.Fatalf("session kept (err %v)", err)
	}
	if _, ok, _ := st.Setting(ctx, "keep_me"); !ok {
		t.Fatal("other settings were removed")
	}
}

// serveOnce starts serve with env, waits until it listens, stops it and
// returns its stderr.
func (e *testEnv) serveOnce(env map[string]string) string {
	e.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var so, se syncBuffer
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{"--config", e.cfgPath, "serve"}, &so, &se, func(k string) (string, bool) {
			v, ok := env[k]
			return v, ok
		})
	}()
	deadline := time.Now().Add(30 * time.Second)
	for addrRe.FindStringSubmatch(se.String()) == nil {
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
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			e.t.Fatalf("serve exit = %d\nstderr:\n%s", code, se.String())
		}
	case <-time.After(30 * time.Second):
		e.t.Fatal("serve did not stop")
	}
	return se.String()
}

func TestServeResetAdminEnv(t *testing.T) {
	e := newTestEnv(t)
	e.writeNoRules()
	e.withServer("127.0.0.1:0", "")
	ctx := context.Background()
	setAdmin := func() {
		t.Helper()
		st := e.openState()
		defer st.Close()
		if err := st.PutSetting(ctx, "admin_password", []byte("pbkdf2-sha256$1$x$y")); err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		if err := st.CreateUISession(ctx, state.UISession{IDHash: []byte("0123456789abcdef0123456789abcdef"), CSRF: "c", Created: now, LastSeen: now}); err != nil {
			t.Fatal(err)
		}
	}
	adminSet := func() bool {
		t.Helper()
		st := e.openState()
		defer st.Close()
		_, ok, err := st.Setting(ctx, "admin_password")
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}

	// Without the variable nothing changes.
	setAdmin()
	e.serveOnce(nil)
	if !adminSet() {
		t.Fatal("password removed without GITMIRROR_RESET_ADMIN")
	}

	// "0" and "false" mean off.
	for _, off := range []string{"0", "false", ""} {
		e.serveOnce(map[string]string{"GITMIRROR_RESET_ADMIN": off})
		if !adminSet() {
			t.Fatalf("GITMIRROR_RESET_ADMIN=%q removed the password", off)
		}
	}

	// A new value removes the password and the sessions, once.
	errOut := e.serveOnce(map[string]string{"GITMIRROR_RESET_ADMIN": "1"})
	if adminSet() {
		t.Fatalf("password kept with GITMIRROR_RESET_ADMIN=1\nstderr:\n%s", errOut)
	}
	if !strings.Contains(errOut, "GITMIRROR_RESET_ADMIN") || !strings.Contains(errOut, "password was removed") {
		t.Fatalf("no reset message in the log:\n%s", errOut)
	}
	st := e.openState()
	if _, ok, err := st.UISession(ctx, []byte("0123456789abcdef0123456789abcdef")); err != nil || ok {
		t.Fatalf("session kept (err %v)", err)
	}
	st.Close()

	// The same value left in place does not reset the new password again.
	setAdmin()
	errOut = e.serveOnce(map[string]string{"GITMIRROR_RESET_ADMIN": "1"})
	if !adminSet() {
		t.Fatal("the same GITMIRROR_RESET_ADMIN value reset the password a second time")
	}
	if !strings.Contains(errOut, "already used") {
		t.Fatalf("no 'already used' message in the log:\n%s", errOut)
	}

	// A different value resets again.
	e.serveOnce(map[string]string{"GITMIRROR_RESET_ADMIN": "2"})
	if adminSet() {
		t.Fatal("a new GITMIRROR_RESET_ADMIN value did not reset the password")
	}
}

func TestResetAdminRefusedWhileServeRuns(t *testing.T) {
	e := newTestEnv(t)
	e.holdLock()
	code, out, errOut := e.cli("reset-admin")
	wantCode(t, code, 1, out, errOut)
	if !strings.Contains(errOut, "another gitmirror process is using state dir") || !strings.Contains(errOut, "Stop gitmirror serve") {
		t.Fatalf("stderr = %q", errOut)
	}
}
