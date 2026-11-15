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

func TestResetAdminRefusedWhileServeRuns(t *testing.T) {
	e := newTestEnv(t)
	e.holdLock()
	code, out, errOut := e.cli("reset-admin")
	wantCode(t, code, 1, out, errOut)
	if !strings.Contains(errOut, "another gitmirror process is using state dir") || !strings.Contains(errOut, "Stop gitmirror serve") {
		t.Fatalf("stderr = %q", errOut)
	}
}
