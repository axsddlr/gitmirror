package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"gitmirror/internal/app"
	"gitmirror/internal/engine"
	"gitmirror/internal/state"
)

func (c *cli) syncCmd() *cobra.Command {
	var rules, repos []string
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Mirror every pair now",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return c.runSync(cmd, rules, repos, dryRun, false)
		},
	}
	fl := cmd.Flags()
	fl.StringArrayVar(&rules, "rule", nil, "only sync this rule (repeatable)")
	fl.StringArrayVar(&repos, "repo", nil, "only sync this source repo, OWNER/NAME (repeatable; skips reconcile)")
	fl.BoolVar(&dryRun, "dry-run", false, "compute and print what would change without writing anything")
	return cmd
}

func (c *cli) planCmd() *cobra.Command {
	var rules, repos []string
	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Show what a sync would do, without changing anything",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return c.runSync(cmd, rules, repos, true, true)
		},
	}
	fl := cmd.Flags()
	fl.StringArrayVar(&rules, "rule", nil, "only plan this rule (repeatable)")
	fl.StringArrayVar(&repos, "repo", nil, "only plan this source repo, OWNER/NAME (repeatable)")
	return cmd
}

func (c *cli) runSync(cmd *cobra.Command, rules, repos []string, dryRun, plan bool) error {
	cfg, err := c.loadConfigOrExit()
	if err != nil {
		return err
	}
	opts := appOptions()
	opts.Logger = c.logger
	a, err := app.Build(cmd.Context(), cfg, opts)
	if err != nil {
		return &exitError{exitFailed, err}
	}
	defer a.Close()
	res, err := a.Sync(cmd.Context(), rules, repos, dryRun, "cli")
	if err != nil {
		if errors.Is(err, app.ErrNoMatch) {
			return &exitError{exitUsage, err}
		}
		return &exitError{exitFailed, err}
	}
	code := syncExitCode(res)
	if c.json {
		if err := c.writeJSON(syncJSON(res, dryRun, code)); err != nil {
			return &exitError{exitFailed, err}
		}
	} else {
		c.printSync(res, dryRun, plan)
	}
	if code != exitOK {
		return &exitError{code: code}
	}
	return nil
}

func syncExitCode(res *app.SyncResult) int {
	code := exitOK
	if len(res.Resolve.Errors) > 0 {
		code = exitPartial
	}
	for _, list := range [][]engine.Report{res.Reports, res.Reconciled} {
		for _, r := range list {
			switch r.Status {
			case state.RunFailed, state.RunAborted:
				return exitFailed
			case state.RunPartial:
				code = exitPartial
			}
		}
	}
	return code
}

func pairText(k state.PairKey) string {
	return fmt.Sprintf("%s:%s → %s:%s", k.SrcForge, k.SrcRepo, k.DstForge, k.DstRepo)
}

func stepSummary(r engine.Report) string {
	parts := make([]string, 0, len(r.Steps))
	for _, s := range r.Steps {
		parts = append(parts, fmt.Sprintf("%s:%s", s.Step, s.Status))
	}
	return strings.Join(parts, " ")
}

func reportDuration(r engine.Report) time.Duration {
	var d time.Duration
	for _, s := range r.Steps {
		d += s.Duration
	}
	return d.Round(time.Millisecond)
}

func (c *cli) printReport(r engine.Report, label string, withActions bool) {
	status := string(r.Status)
	if label != "" {
		status = label
	}
	line := fmt.Sprintf("%-8s %s", status, pairText(r.Key))
	if s := stepSummary(r); s != "" {
		line += "  " + s
	}
	line += "  " + reportDuration(r).String()
	fmt.Fprintln(c.out, line)
	if r.Err != nil {
		fmt.Fprintf(c.out, "    error: %s\n", firstLine(r.Err.Error()))
	}
	for _, s := range r.Steps {
		if s.Status == state.StepFailed && s.Detail != "" {
			fmt.Fprintf(c.out, "    %s: %s\n", s.Step, firstLine(s.Detail))
		}
	}
	if withActions {
		for _, a := range r.Actions {
			fmt.Fprintf(c.out, "    - %s\n", a)
		}
	}
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return l
}

func (c *cli) printSync(res *app.SyncResult, dryRun, plan bool) {
	counts := map[state.RunStatus]int{}
	for _, r := range res.Reports {
		counts[r.Status]++
		c.printReport(r, "", plan)
	}
	if len(res.Resolve.Errors) > 0 {
		fmt.Fprintln(c.out, "rule errors:")
		for _, e := range res.Resolve.Errors {
			fmt.Fprintf(c.out, "  %s\n", firstLine(e.Error()))
		}
	}
	if len(res.Reconciled) > 0 {
		fmt.Fprintln(c.out, "reconcile:")
		for _, r := range res.Reconciled {
			c.printReport(r, "", true)
		}
	}
	sum := fmt.Sprintf("%d pairs: %d ok, %d partial, %d failed, %d aborted; %d rule errors; %d reconcile",
		len(res.Reports), counts[state.RunOK], counts[state.RunPartial], counts[state.RunFailed], counts[state.RunAborted],
		len(res.Resolve.Errors), len(res.Reconciled))
	if dryRun {
		sum += " (dry run)"
	}
	fmt.Fprintln(c.out, sum)
}

type jsonStep struct {
	Step       string `json:"step"`
	Status     string `json:"status"`
	Detail     string `json:"detail,omitempty"`
	DurationMS int64  `json:"duration_ms"`
}

type jsonReport struct {
	Rule        string     `json:"rule"`
	Source      string     `json:"source"`
	Destination string     `json:"destination"`
	Status      string     `json:"status"`
	RunID       int64      `json:"run_id,omitempty"`
	Steps       []jsonStep `json:"steps"`
	Error       string     `json:"error,omitempty"`
	Actions     []string   `json:"actions"`
}

type jsonRuleError struct {
	Rule  string `json:"rule"`
	Error string `json:"error"`
}

type jsonSyncDoc struct {
	DryRun     bool            `json:"dry_run"`
	ExitCode   int             `json:"exit_code"`
	Pairs      []jsonReport    `json:"pairs"`
	RuleErrors []jsonRuleError `json:"rule_errors"`
	Reconcile  []jsonReport    `json:"reconcile"`
}

func toJSONReports(list []engine.Report) []jsonReport {
	out := make([]jsonReport, 0, len(list))
	for _, r := range list {
		jr := jsonReport{
			Rule:        r.Key.Rule,
			Source:      r.Key.SrcForge + ":" + r.Key.SrcRepo,
			Destination: r.Key.DstForge + ":" + r.Key.DstRepo,
			Status:      string(r.Status),
			RunID:       r.RunID,
			Steps:       make([]jsonStep, 0, len(r.Steps)),
			Actions:     append([]string{}, r.Actions...),
		}
		if r.Err != nil {
			jr.Error = r.Err.Error()
		}
		for _, s := range r.Steps {
			jr.Steps = append(jr.Steps, jsonStep{Step: s.Step, Status: string(s.Status), Detail: s.Detail, DurationMS: s.Duration.Milliseconds()})
		}
		out = append(out, jr)
	}
	return out
}

func syncJSON(res *app.SyncResult, dryRun bool, code int) jsonSyncDoc {
	doc := jsonSyncDoc{
		DryRun:     dryRun,
		ExitCode:   code,
		Pairs:      toJSONReports(res.Reports),
		RuleErrors: make([]jsonRuleError, 0, len(res.Resolve.Errors)),
		Reconcile:  toJSONReports(res.Reconciled),
	}
	for _, e := range res.Resolve.Errors {
		doc.RuleErrors = append(doc.RuleErrors, jsonRuleError{Rule: e.Rule, Error: e.Err.Error()})
	}
	return doc
}
