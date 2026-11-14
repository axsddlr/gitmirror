package main

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"gitmirror/internal/app"
	"gitmirror/internal/forge"
	"gitmirror/internal/gitx"
	"gitmirror/internal/resolve"
)

type jsonForgeCheck struct {
	Name  string `json:"name"`
	OK    bool   `json:"ok"`
	Login string `json:"login,omitempty"`
	Error string `json:"error,omitempty"`
}

type jsonRuleCheck struct {
	Name     string   `json:"name"`
	Complete bool     `json:"complete"`
	Pairs    int      `json:"pairs"`
	Errors   []string `json:"errors"`
}

type jsonValidateDoc struct {
	Valid     bool             `json:"valid"`
	Problems  []string         `json:"problems,omitempty"`
	Git       string           `json:"git,omitempty"`
	GitLFS    string           `json:"git_lfs,omitempty"`
	GitError  string           `json:"git_error,omitempty"`
	Forges    []jsonForgeCheck `json:"forges,omitempty"`
	Rules     []jsonRuleCheck  `json:"rules,omitempty"`
	ExitCode  int              `json:"exit_code"`
	Offline   bool             `json:"offline"`
	NumForges int              `json:"forge_count,omitempty"`
	NumRules  int              `json:"rule_count,omitempty"`
}

func (c *cli) validateCmd() *cobra.Command {
	var offline bool
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Check the config, git, forge credentials and rule listings",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			code, err := c.validate(cmd.Context(), offline)
			if err != nil {
				return err
			}
			if code != exitOK {
				return &exitError{code: code}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&offline, "offline", false, "only check the config and local tools; no network")
	return cmd
}

func (c *cli) validate(ctx context.Context, offline bool) (int, error) {
	doc := jsonValidateDoc{Offline: offline}
	finish := func(code int) (int, error) {
		doc.ExitCode = code
		if c.json {
			if err := c.writeJSON(doc); err != nil {
				return exitFailed, err
			}
		}
		return code, nil
	}
	say := func(format string, a ...any) {
		if !c.json {
			fmt.Fprintf(c.out, format+"\n", a...)
		}
	}

	cfg, err := c.loadConfig()
	if err != nil {
		doc.Problems = flatten(err)
		for _, p := range doc.Problems {
			say("config: %s", p)
		}
		return finish(exitUsage)
	}
	doc.Valid = true
	doc.NumForges, doc.NumRules = len(cfg.Forges), len(cfg.Rules)
	say("config: ok (%d forges, %d rules)", len(cfg.Forges), len(cfg.Rules))

	opts := appOptions()
	failed := false
	v, err := (&gitx.Runner{Env: opts.GitEnv, UseHostConfig: cfg.Git.UseHostConfig}).Check(ctx)
	if err != nil {
		doc.GitError = err.Error()
		say("git: %v", err)
		return finish(exitFailed)
	}
	doc.Git, doc.GitLFS = v.Git, v.LFS
	say("git: %s", v.Git)
	if v.LFS == "" {
		say("git-lfs: not installed")
	} else {
		say("git-lfs: %s", v.LFS)
	}
	if offline {
		return finish(exitOK)
	}

	provs, err := app.BuildProviders(cfg, opts)
	if err != nil {
		return exitFailed, err
	}
	names := make([]string, 0, len(provs))
	for n := range provs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fc := jsonForgeCheck{Name: n}
		id, ok := provs[n].(forge.Identity)
		if !ok {
			fc.OK = true
			say("forge %s: ok (identity check not supported)", n)
		} else {
			lctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			login, err := id.Login(lctx)
			cancel()
			if err != nil {
				fc.Error = err.Error()
				failed = true
				say("forge %s: FAILED: %v", n, err)
			} else {
				fc.OK, fc.Login = true, login
				say("forge %s: ok as %s", n, login)
			}
		}
		doc.Forges = append(doc.Forges, fc)
	}

	res := resolve.Resolve(ctx, cfg, provs)
	pairs := map[string]int{}
	for _, p := range res.Pairs {
		pairs[p.Rule]++
	}
	errs := map[string][]string{}
	for _, e := range res.Errors {
		errs[e.Rule] = append(errs[e.Rule], e.Err.Error())
	}
	for _, r := range cfg.Rules {
		rc := jsonRuleCheck{Name: r.Name, Complete: res.Complete[r.Name], Pairs: pairs[r.Name], Errors: append([]string{}, errs[r.Name]...)}
		yn := "yes"
		if !rc.Complete {
			yn = "no"
			failed = true
		}
		say("rule %s: complete=%s pairs=%d", r.Name, yn, rc.Pairs)
		for _, e := range rc.Errors {
			say("  error: %s", firstLine(e))
		}
		doc.Rules = append(doc.Rules, rc)
	}
	if failed {
		return finish(exitFailed)
	}
	return finish(exitOK)
}
