package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"gitmirror/internal/app"
	"gitmirror/internal/config"
)

// appOptions supplies the app.Options used by commands that build the App.
// Tests override it to inject fake providers.
var appOptions = func() app.Options { return app.Options{} }

// Exit codes.
const (
	exitOK      = 0
	exitFailed  = 1
	exitUsage   = 2
	exitPartial = 3
)

const rootLong = `gitmirror mirrors repositories one-way between git forges (GitHub, Gitea).

Exit codes:
  0  everything ok
  1  any pair failed or was aborted, or a fatal runtime error
  2  usage or configuration error
  3  partial: some pair was partial or a rule had errors, but nothing failed`

// exitError carries a process exit code out of a command. err may be nil when
// the command already printed everything it wanted to say.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string {
	if e.err == nil {
		return ""
	}
	return e.err.Error()
}

func (e *exitError) Unwrap() error { return e.err }

// redactor masks configured secrets in everything written through it.
type redactor struct {
	w       io.Writer
	mu      sync.Mutex
	secrets []string
}

func (r *redactor) add(secrets ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range secrets {
		if len(s) >= 4 {
			r.secrets = append(r.secrets, s)
		}
	}
}

func (r *redactor) Write(p []byte) (int, error) {
	r.mu.Lock()
	secrets := r.secrets
	r.mu.Unlock()
	if len(secrets) == 0 {
		return r.w.Write(p)
	}
	s := string(p)
	for _, sec := range secrets {
		s = strings.ReplaceAll(s, sec, "[REDACTED]")
	}
	if _, err := io.WriteString(r.w, s); err != nil {
		return 0, err
	}
	return len(p), nil
}

type cli struct {
	configPath string
	logLevel   string
	logFormat  string
	json       bool

	out, errw *redactor
	env       func(string) (string, bool)
	logger    *slog.Logger
}

// run executes gitmirror with args and returns the process exit code.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, env func(string) (string, bool)) int {
	c := &cli{
		out:  &redactor{w: stdout},
		errw: &redactor{w: stderr},
		env:  env,
	}
	root := c.root()
	root.SetArgs(args)
	root.SetOut(c.out)
	root.SetErr(c.errw)
	err := root.ExecuteContext(ctx)
	if err == nil {
		return exitOK
	}
	var ee *exitError
	if errors.As(err, &ee) {
		if ee.err != nil {
			fmt.Fprintf(c.errw, "gitmirror: %v\n", ee.err)
		}
		return ee.code
	}
	// Anything cobra itself rejected (unknown command or flag, bad args).
	fmt.Fprintf(c.errw, "gitmirror: %v\nRun 'gitmirror --help' for usage.\n", err)
	return exitUsage
}

func (c *cli) root() *cobra.Command {
	root := &cobra.Command{
		Use:           "gitmirror",
		Short:         "Mirror repositories between git forges",
		Long:          rootLong,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			return c.setupLogger()
		},
	}
	root.CompletionOptions.DisableDefaultCmd = true
	def := "config.yaml"
	if v, ok := c.env("GITMIRROR_CONFIG"); ok && v != "" {
		def = v
	}
	f := root.PersistentFlags()
	f.StringVarP(&c.configPath, "config", "c", def, "path to the config file (env GITMIRROR_CONFIG)")
	f.StringVar(&c.logLevel, "log-level", "info", "log level: debug, info, warn, error")
	f.StringVar(&c.logFormat, "log-format", "text", "log format: text or json")
	f.BoolVar(&c.json, "json", false, "machine-readable JSON output on stdout")
	root.AddCommand(c.syncCmd(), c.planCmd(), c.validateCmd(), c.statusCmd(), c.versionCmd())
	return root
}

func (c *cli) setupLogger() error {
	var lvl slog.Level
	switch c.logLevel {
	case "debug":
		lvl = slog.LevelDebug
	case "info":
		lvl = slog.LevelInfo
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		return &exitError{exitUsage, fmt.Errorf("invalid --log-level %q (want debug, info, warn or error)", c.logLevel)}
	}
	opts := &slog.HandlerOptions{Level: lvl}
	switch c.logFormat {
	case "text":
		c.logger = slog.New(slog.NewTextHandler(c.errw, opts))
	case "json":
		c.logger = slog.New(slog.NewJSONHandler(c.errw, opts))
	default:
		return &exitError{exitUsage, fmt.Errorf("invalid --log-format %q (want text or json)", c.logFormat)}
	}
	return nil
}

// flatten splits a (possibly joined) error into one message per problem.
func flatten(err error) []string {
	var out []string
	var walk func(error)
	walk = func(e error) {
		if j, ok := e.(interface{ Unwrap() []error }); ok {
			for _, s := range j.Unwrap() {
				walk(s)
			}
			return
		}
		for _, l := range strings.Split(e.Error(), "\n") {
			if l = strings.TrimSpace(l); l != "" {
				out = append(out, l)
			}
		}
	}
	walk(err)
	return out
}

// loadConfig reads the config and registers its secrets with the output
// redactors. On failure it returns the load error unprinted.
func (c *cli) loadConfig() (*config.Config, error) {
	cfg, err := config.Load(c.configPath)
	if err != nil {
		return nil, err
	}
	for _, f := range cfg.Forges {
		c.out.add(f.Token)
		c.errw.add(f.Token)
	}
	c.out.add(cfg.Server.WebhookSecret)
	c.errw.add(cfg.Server.WebhookSecret)
	return cfg, nil
}

// loadConfigOrExit is loadConfig for commands that treat a bad config as a
// usage error: problems go to stderr, one per line.
func (c *cli) loadConfigOrExit() (*config.Config, error) {
	cfg, err := c.loadConfig()
	if err != nil {
		for _, l := range flatten(err) {
			fmt.Fprintf(c.errw, "gitmirror: config: %s\n", l)
		}
		return nil, &exitError{code: exitUsage}
	}
	return cfg, nil
}

func (c *cli) writeJSON(v any) error {
	enc := json.NewEncoder(c.out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-3]) + "..."
}
