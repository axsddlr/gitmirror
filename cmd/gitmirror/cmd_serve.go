package main

import (
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"gitmirror/internal/app"
	"gitmirror/internal/config"
	"gitmirror/internal/server"
	"gitmirror/internal/web"
)

// resetAdminEnv lets a stack manager such as Dockhand reset a forgotten web UI
// password without a shell: set it to a new value and restart serve.
const resetAdminEnv = "GITMIRROR_RESET_ADMIN"

func (c *cli) serveCmd() *cobra.Command {
	var lockTimeout, shutdownTimeout time.Duration
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the daemon: scheduled syncs, webhooks, /healthz, /readyz and /metrics (own listener)",
		Long: `Run gitmirror as a long-running daemon. Every rule is synced on its interval
and webhooks (POST /webhook/<forge>) trigger targeted syncs. The server listens
on server.listen and also serves GET /healthz and /readyz. GET /metrics is served
only on the separate server.metrics_listen address (default 127.0.0.1:9090; "off"
disables it). The web UI is served on server.ui_listen (default 127.0.0.1:8090;
"off" disables it); on first start it logs a one-time setup code.

On SIGINT/SIGTERM it stops scheduling, lets running syncs finish for up to
--shutdown-timeout, then aborts them and exits 0.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := c.loadConfigOrExit()
			if err != nil {
				return err
			}
			opts := c.options()
			opts.LockTimeout = lockTimeout
			a, err := app.Build(cmd.Context(), cfg, opts)
			if err != nil {
				return c.buildError(err)
			}
			if len(a.Config().Rules) == 0 {
				c.logger.Warn(config.ErrNoRules.Error() + "; idling until rules are added")
			}
			defer a.Close()
			if tok, ok := c.env(resetAdminEnv); ok {
				reset, err := web.ResetAdminOnce(cmd.Context(), a.Store, tok)
				if err != nil {
					return &exitError{exitFailed, fmt.Errorf("%s: %w", resetAdminEnv, err)}
				}
				if reset {
					c.logger.Warn(resetAdminEnv + " is set: the web UI password was removed and everyone was signed out. " +
						"Choose a new password with the setup code below. This value is now used up; leaving it set does not reset again " +
						"(set a different value to reset again).")
				} else if v := strings.TrimSpace(tok); v != "" && v != "0" && !strings.EqualFold(v, "false") {
					c.logger.Info(resetAdminEnv + " value already used, so the web UI password was not reset again; set a different value to reset again, or remove it")
				}
			}
			ln, err := net.Listen("tcp", cfg.Server.Listen)
			if err != nil {
				return &exitError{exitFailed, fmt.Errorf("listen on %s: %w", cfg.Server.Listen, err)}
			}
			if sec, _ := a.WebhookSecret(); sec == "" {
				c.logger.Warn("no webhook secret configured (server.webhook_secret or the web UI's Webhooks page); webhooks are disabled")
			}
			c.logger.Info("listening", "addr", ln.Addr().String())
			s := &server.Server{App: a, Logger: c.logger, ShutdownTimeout: shutdownTimeout}
			if ml := cfg.Server.MetricsListen; ml != "" && ml != "off" {
				mln, err := net.Listen("tcp", ml)
				if err != nil {
					ln.Close()
					return &exitError{exitFailed, fmt.Errorf("listen for metrics on %s: %w", ml, err)}
				}
				s.MetricsListener = mln
			} else {
				c.logger.Info("metrics are disabled (server.metrics_listen: off)")
			}
			if ul := cfg.Server.UIListen; ul != "" && ul != "off" {
				uln, err := net.Listen("tcp", ul)
				if err != nil {
					ln.Close()
					if s.MetricsListener != nil {
						s.MetricsListener.Close()
					}
					return &exitError{exitFailed, fmt.Errorf("listen for the web UI on %s: %w", ul, err)}
				}
				base := "http://" + uln.Addr().String()
				c.logger.Info("web UI: " + base)
				ui := &web.UI{App: a, Logger: c.logger, BaseURL: base, SyncNow: s.SyncNow, SyncAll: s.SyncAll}
				s.UIListener, s.UIHandler = uln, ui.Handler()
			} else {
				c.logger.Info("web UI is disabled (server.ui_listen: off)")
			}
			if err := s.Run(cmd.Context(), ln); err != nil {
				return &exitError{exitFailed, err}
			}
			return nil
		},
	}
	fl := cmd.Flags()
	fl.DurationVar(&lockTimeout, "lock-timeout", 0, "wait this long for another gitmirror process to release the state dir (0 = fail at once)")
	fl.DurationVar(&shutdownTimeout, "shutdown-timeout", server.DefaultShutdownTimeout, "how long running syncs may finish after a shutdown signal before they are aborted")
	return cmd
}
