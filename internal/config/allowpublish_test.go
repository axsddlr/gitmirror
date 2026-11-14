package config

import "testing"

func TestAllowPublishRuleSetting(t *testing.T) {
	y := "forges:\n  home: {type: gitea, url: 'https://g.example', token: x}\nrules:\n" +
		"  - name: on\n    adopt: true\n    allow_publish: true\n    visibility: match-source\n    from: {forge: home, owner: a}\n    to: {forge: home, owner: b}\n" +
		"  - name: off\n    from: {forge: home, owner: c}\n    to: {forge: home, owner: d}\n"
	cfg := mustParse(t, y, nil, nil)
	got := map[string]bool{}
	for _, r := range cfg.Rules {
		got[r.Name] = r.Effective(cfg.Defaults).AllowPublish
	}
	if !got["on"] || got["off"] {
		t.Fatalf("AllowPublish = %v", got)
	}
}
