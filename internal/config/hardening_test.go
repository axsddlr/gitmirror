package config

import (
	"os"
	"strings"
	"testing"
)

func TestEmptyInterpolatedSecretNamesVariable(t *testing.T) {
	gitea := func(tok string) string {
		return "forges:\n  home: {type: gitea, url: 'https://g.example', token: \"" + tok + "\"}\nrules:\n" +
			"  - name: r\n    from: {forge: home, owner: a}\n    to: {forge: home, owner: b}\n"
	}
	for _, val := range []string{"", "   "} {
		_, err := parse(t, gitea("${GH_TOK}"), map[string]string{"GH_TOK": val}, nil)
		if err == nil || !strings.Contains(err.Error(), "forges.home.token: ${GH_TOK} resolved to an empty value") {
			t.Fatalf("val %q: err = %v", val, err)
		}
	}
	// a literal empty token is a public, read-only site, so the rule that
	// copies to it is refused for that reason rather than for being empty
	if _, err := parse(t, gitea(""), nil, nil); err == nil || !strings.Contains(err.Error(), "can only be copied from") {
		t.Fatalf("literal empty: %v", err)
	}
	// non-empty works
	mustParse(t, gitea("${GH_TOK}"), map[string]string{"GH_TOK": "x"}, nil)
}

func TestEmptyInterpolatedWebhookSecret(t *testing.T) {
	d := doc("server: {webhook_secret: \"${WH}\"}", okRule)
	_, err := parse(t, d, map[string]string{"WH": ""}, nil)
	if err == nil || !strings.Contains(err.Error(), "server.webhook_secret: ${WH} resolved to an empty value") {
		t.Fatalf("err = %v", err)
	}
}

func TestStateDirEnvPrecedence(t *testing.T) {
	const key = "GITMIRROR_STATE_DIR"
	tests := []struct {
		name, top string
		env       map[string]string
		want      string
	}{
		{"default", "", nil, "./data"},
		{"config", "state_dir: /cfg", nil, "/cfg"},
		{"env over default", "", map[string]string{key: "/env"}, "/env"},
		{"env over config", "state_dir: /cfg", map[string]string{key: "/env"}, "/env"},
		{"empty env ignored", "state_dir: /cfg", map[string]string{key: ""}, "/cfg"},
		{"empty env default", "", map[string]string{key: ""}, "./data"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := mustParse(t, doc(tt.top, okRule), tt.env, nil)
			if c.StateDir != tt.want {
				t.Fatalf("StateDir = %q, want %q", c.StateDir, tt.want)
			}
		})
	}
}

func TestLoadHonoursStateDirEnv(t *testing.T) {
	path := t.TempDir() + "/c.yaml"
	if err := os.WriteFile(path, []byte(doc("state_dir: /cfg", okRule)), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITMIRROR_STATE_DIR", "/from-env")
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.StateDir != "/from-env" {
		t.Fatalf("StateDir = %q", c.StateDir)
	}
}
