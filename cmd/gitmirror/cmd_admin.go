package main

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"gitmirror/internal/app"
	"gitmirror/internal/web"
)

func (c *cli) resetAdminCmd() *cobra.Command {
	var lockTimeout time.Duration
	cmd := &cobra.Command{
		Use:   "reset-admin",
		Short: "Forget the web UI admin password and sign everyone out",
		Long: `Remove the web UI admin password and every web UI session. The next start
of gitmirror serve logs a new one-time setup code ("web UI setup: ..."), so a
new password can be chosen on the setup page. Sites, mirrors and the rest of
the state are kept.

gitmirror serve must be stopped first: reset-admin takes the state dir lock
and refuses while another gitmirror process holds it (see --lock-timeout).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := c.loadConfigOrExit()
			if err != nil {
				return err
			}
			opts := c.options()
			opts.LockTimeout = lockTimeout
			a, err := app.Build(cmd.Context(), cfg, opts)
			if errors.Is(err, app.ErrLocked) {
				return &exitError{exitFailed, fmt.Errorf("%w. Stop gitmirror serve (or wait for the other command to finish), then run reset-admin again", err)}
			}
			if err != nil {
				return c.buildError(err)
			}
			defer a.Close()
			if err := web.ResetAdmin(cmd.Context(), a.Store); err != nil {
				return &exitError{exitFailed, err}
			}
			fmt.Fprintln(c.out, "The web UI admin password was removed and everyone was signed out.")
			fmt.Fprintln(c.out, `Start gitmirror serve again: it logs a new setup code (look for "web UI setup") to choose a new password.`)
			return nil
		},
	}
	cmd.Flags().DurationVar(&lockTimeout, "lock-timeout", 0, "wait this long for another gitmirror process to release the state dir (0 = fail at once)")
	return cmd
}
