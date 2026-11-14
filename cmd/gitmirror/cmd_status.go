package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"gitmirror/internal/state"
)

const timeFmt = "2006-01-02 15:04:05"

type jsonPairState struct {
	Rule                string     `json:"rule"`
	Source              string     `json:"source"`
	Destination         string     `json:"destination"`
	LastSuccess         *time.Time `json:"last_success"`
	LastAttempt         *time.Time `json:"last_attempt"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	LastError           string     `json:"last_error,omitempty"`
}

type jsonRun struct {
	ID       int64      `json:"id"`
	Rule     string     `json:"rule"`
	Source   string     `json:"source"`
	Dest     string     `json:"destination"`
	Trigger  string     `json:"trigger"`
	Started  time.Time  `json:"started"`
	Finished *time.Time `json:"finished"`
	Status   string     `json:"status"`
	Error    string     `json:"error,omitempty"`
}

type jsonAuditEvent struct {
	ID          int64     `json:"id"`
	Time        time.Time `json:"time"`
	Action      string    `json:"action"`
	Rule        string    `json:"rule"`
	Source      string    `json:"source"`
	Destination string    `json:"destination"`
	Detail      string    `json:"detail,omitempty"`
}

type jsonStatusDoc struct {
	Pairs []jsonPairState `json:"pairs"`
	Runs  []jsonRun       `json:"runs"`
	// Audit is present only with --audit.
	Audit []jsonAuditEvent `json:"audit,omitempty"`
}

func tptr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func (c *cli) statusCmd() *cobra.Command {
	var runs, audit int
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the last known state of every pair (no network)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := c.loadConfigOrExit()
			if err != nil {
				return err
			}
			path := filepath.Join(cfg.StateDir, "state.db")
			doc := jsonStatusDoc{Pairs: []jsonPairState{}, Runs: []jsonRun{}}
			if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
				if c.json {
					return c.writeJSON(doc)
				}
				fmt.Fprintln(c.out, "no state yet")
				return nil
			} else if err != nil {
				return &exitError{exitFailed, err}
			}
			st, err := state.Open(cmd.Context(), path)
			if err != nil {
				return &exitError{exitFailed, err}
			}
			defer st.Close()
			pairs, err := st.Pairs(cmd.Context())
			if err != nil {
				return &exitError{exitFailed, err}
			}
			var recent []state.Run
			if runs > 0 {
				if recent, err = st.RecentRuns(cmd.Context(), runs); err != nil {
					return &exitError{exitFailed, err}
				}
			}
			var events []state.Event
			if audit > 0 {
				if events, err = st.AuditLog(cmd.Context(), audit); err != nil {
					return &exitError{exitFailed, err}
				}
			}
			if c.json {
				for _, ev := range events {
					doc.Audit = append(doc.Audit, jsonAuditEvent{
						ID: ev.ID, Time: ev.Time, Action: string(ev.Action), Rule: ev.Key.Rule,
						Source: ev.Key.SrcForge + ":" + ev.Key.SrcRepo, Destination: ev.Key.DstForge + ":" + ev.Key.DstRepo,
						Detail: ev.Detail,
					})
				}
				for _, p := range pairs {
					doc.Pairs = append(doc.Pairs, jsonPairState{
						Rule: p.Key.Rule, Source: p.Key.SrcForge + ":" + p.Key.SrcRepo,
						Destination: p.Key.DstForge + ":" + p.Key.DstRepo,
						LastSuccess: tptr(p.LastSuccess), LastAttempt: tptr(p.LastAttempt),
						ConsecutiveFailures: p.ConsecutiveFailures, LastError: p.LastError,
					})
				}
				for _, r := range recent {
					doc.Runs = append(doc.Runs, jsonRun{
						ID: r.ID, Rule: r.Key.Rule, Source: r.Key.SrcForge + ":" + r.Key.SrcRepo,
						Dest: r.Key.DstForge + ":" + r.Key.DstRepo, Trigger: r.Trigger,
						Started: r.Started, Finished: tptr(r.Finished), Status: string(r.Status), Error: r.Error,
					})
				}
				return c.writeJSON(doc)
			}
			if len(pairs) == 0 {
				fmt.Fprintln(c.out, "no pairs recorded")
			} else {
				tw := tabwriter.NewWriter(c.out, 0, 4, 2, ' ', 0)
				fmt.Fprintln(tw, "PAIR\tLAST SUCCESS\tFAILURES\tLAST ERROR")
				for _, p := range pairs {
					last := "never"
					if !p.LastSuccess.IsZero() {
						last = p.LastSuccess.Local().Format(timeFmt)
					}
					fmt.Fprintf(tw, "%s: %s\t%s\t%d\t%s\n", p.Key.Rule, pairText(p.Key), last, p.ConsecutiveFailures, truncate(p.LastError, 80))
				}
				tw.Flush()
			}
			if runs > 0 {
				fmt.Fprintf(c.out, "\nrecent runs (%d):\n", len(recent))
				tw := tabwriter.NewWriter(c.out, 0, 4, 2, ' ', 0)
				fmt.Fprintln(tw, "ID\tSTARTED\tTRIGGER\tSTATUS\tPAIR\tERROR")
				for _, r := range recent {
					fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s: %s\t%s\n", r.ID, r.Started.Local().Format(timeFmt), r.Trigger, r.Status,
						r.Key.Rule, pairText(r.Key), truncate(r.Error, 80))
				}
				tw.Flush()
			}
			if audit > 0 {
				fmt.Fprintf(c.out, "\naudit log (%d):\n", len(events))
				tw := tabwriter.NewWriter(c.out, 0, 4, 2, ' ', 0)
				fmt.Fprintln(tw, "ID\tTIME\tACTION\tPAIR\tDETAIL")
				for _, ev := range events {
					fmt.Fprintf(tw, "%d\t%s\t%s\t%s: %s\t%s\n", ev.ID, ev.Time.Local().Format(timeFmt), ev.Action,
						ev.Key.Rule, pairText(ev.Key), truncate(ev.Detail, 80))
				}
				tw.Flush()
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&runs, "runs", 0, "also list the N most recent runs")
	cmd.Flags().IntVar(&audit, "audit", 0, "also list the N most recent audit events (destination create, adopt, archive, delete)")
	return cmd
}
