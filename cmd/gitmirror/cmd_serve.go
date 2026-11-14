package main

import (
	"fmt"
	"net"
	"time"

	"github.com/spf13/cobra"

	"gitmirror/internal/app"
	"gitmirror/internal/server"
)

func (c *cli) serveCmd() *cobra.Command {
	var lockTimeout, shutdownTimeout time.Duration
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the daemon: scheduled syncs, webhooks, /healthz, /readyz and /metrics",
		Long: `Run gitmirror as a long-running daemon. Every rule is synced on its interval
and webhooks (POST /webhook/<forge>) trigger targeted syncs. The server listens
on server.listen and also serves GET /healthz, /readyz and /metrics.

On SIGINT/SIGTERM it stops scheduling, lets running syncs finish for up to
--shutdown-timeout, then aborts them and exits 0.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := c.loadConfigOrExit()
			if err != nil {
				return err
			}
			opts := appOptions()
			opts.Logger = c.logger
			opts.LockTimeout = lockTimeout
			a, err := app.Build(cmd.Context(), cfg, opts)
			if err != nil {
				return &exitError{exitFailed, err}
			}
			defer a.Close()
			ln, err := net.Listen("tcp", cfg.Server.Listen)
			if err != nil {
				return &exitError{exitFailed, fmt.Errorf("listen on %s: %w", cfg.Server.Listen, err)}
			}
			if cfg.Server.WebhookSecret == "" {
				c.logger.Warn("no server.webhook_secret configured; webhooks are disabled")
			}
			c.logger.Info("listening", "addr", ln.Addr().String())
			s := &server.Server{App: a, Logger: c.logger, ShutdownTimeout: shutdownTimeout}
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
