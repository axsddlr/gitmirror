package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitmirror/internal/app"
	"gitmirror/internal/config"
)

const noRulesMsg = "no mirror rules configured: add one in the web UI or in the config file"

// writeNoRules replaces the test config with one that has forges but no rules.
func (e *testEnv) writeNoRules() {
	e.t.Helper()
	y := "state_dir: '" + filepath.ToSlash(e.stateDir) + "'\nforges:\n  src: {type: fake, token: " + sentinel +
		"}\n  dst: {type: fake, token: " + sentinel + "}\n"
	if err := os.WriteFile(e.cfgPath, []byte(y), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

// addUIRule stores a web UI rule mirroring src owner alice to dst owner
// uimirrors, using the config at cfg.
func (e *testEnv) addUIRule(cfg *config.Config) {
	e.t.Helper()
	o := appOptions()
	a, err := app.Build(context.Background(), cfg, o)
	if err != nil {
		e.t.Fatal(err)
	}
	defer a.Close()
	if err := a.UpdateUI(context.Background(), func(u *app.UIConfig) error {
		if cfg.ForgeOrigin("src") == "" {
			if err := u.PutForge("src", config.Forge{Type: "fake", Token: sentinel}); err != nil {
				return err
			}
			if err := u.PutForge("dst", config.Forge{Type: "fake", Token: sentinel}); err != nil {
				return err
			}
		}
		return u.PutRule(config.Rule{Name: "ui1", From: config.Source{Forge: "src", Owner: "alice"}, To: config.Dest{Forge: "dst", Owner: "uimirrors"}})
	}); err != nil {
		e.t.Fatal(err)
	}
}

func TestSyncAndPlanWithoutRulesExit2(t *testing.T) {
	e := newTestEnv(t)
	e.writeNoRules()
	for _, cmd := range []string{"sync", "plan"} {
		code, out, errOut := e.cli(cmd)
		wantCode(t, code, 2, out, errOut)
		if !strings.Contains(errOut, noRulesMsg) {
			t.Fatalf("%s stderr = %q", cmd, errOut)
		}
	}
	code, out, errOut := e.cli("validate")
	wantCode(t, code, 2, out, errOut)
	if !strings.Contains(out, noRulesMsg) {
		t.Fatalf("validate stdout = %q", out)
	}
}

func TestSyncUsesUIRules(t *testing.T) {
	e := newTestEnv(t)
	e.addSource("alice", "a")
	e.writeNoRules()
	cfg, err := config.Load(e.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	e.addUIRule(cfg)
	code, out, errOut := e.cli("sync", "--rule", "ui1")
	wantCode(t, code, 0, out, errOut)
	if _, err := e.dst.GetRepo(context.Background(), "uimirrors", "a"); err != nil {
		t.Fatalf("UI rule not synced: %v\n%s", err, out)
	}
	code, out, errOut = e.cli("validate")
	wantCode(t, code, 0, out, errOut)
	if !strings.Contains(out, "rule ui1:") {
		t.Fatalf("validate does not check the UI rule:\n%s", out)
	}
	code, out, errOut = e.cli("validate", "--offline")
	wantCode(t, code, 0, out, errOut)
	if !strings.Contains(out, "not checked offline") {
		t.Fatalf("validate --offline does not say UI entries are unchecked:\n%s", out)
	}
}

func TestMissingDefaultConfigUsesDefaults(t *testing.T) {
	e := newTestEnv(t)
	e.addSource("alice", "a")
	t.Chdir(t.TempDir())
	t.Setenv("GITMIRROR_STATE_DIR", e.stateDir)

	code, out, errOut := runCLI([]string{"sync"}, nil)
	wantCode(t, code, 2, out, errOut)
	if !strings.Contains(errOut, noRulesMsg) {
		t.Fatalf("sync without config.yaml: stderr = %q", errOut)
	}

	cfg, err := config.Parse(nil, os.LookupEnv, os.ReadFile)
	if err != nil {
		t.Fatal(err)
	}
	e.addUIRule(cfg)
	code, out, errOut = runCLI([]string{"sync"}, nil)
	wantCode(t, code, 0, out, errOut)
	if _, err := e.dst.GetRepo(context.Background(), "uimirrors", "a"); err != nil {
		t.Fatalf("UI rule not synced without a config file: %v", err)
	}
	if _, err := os.Stat("config.yaml"); err == nil {
		t.Fatal("a config file was created")
	}
}

func TestExplicitMissingConfigStillErrors(t *testing.T) {
	t.Chdir(t.TempDir())
	missing := filepath.Join(t.TempDir(), "nope.yaml")
	for _, tc := range []struct {
		args []string
		env  map[string]string
	}{
		{[]string{"-c", missing, "sync"}, nil},
		{[]string{"--config", "config.yaml", "sync"}, nil},
		{[]string{"sync"}, map[string]string{"GITMIRROR_CONFIG": missing}},
	} {
		code, out, errOut := runCLI(tc.args, tc.env)
		wantCode(t, code, 2, out, errOut)
		if !strings.Contains(errOut, "read config") {
			t.Fatalf("%v: stderr = %q", tc.args, errOut)
		}
	}
}

func TestServeWithoutRulesIdles(t *testing.T) {
	e := newTestEnv(t)
	e.writeNoRules()
	e.withServer("127.0.0.1:0", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var so, se syncBuffer
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{"--config", e.cfgPath, "serve"}, &so, &se, func(string) (string, bool) { return "", false })
	}()
	deadline := time.Now().Add(30 * time.Second)
	for addrRe.FindStringSubmatch(se.String()) == nil {
		select {
		case code := <-done:
			t.Fatalf("serve exited early with %d\nstderr:\n%s", code, se.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("serve did not start\nstderr:\n%s", se.String())
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("serve exit = %d\nstderr:\n%s", code, se.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatal("serve did not stop")
	}
}
