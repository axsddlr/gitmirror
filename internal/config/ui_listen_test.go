package config

import (
	"strings"
	"testing"
)

func TestUIListen(t *testing.T) {
	if c := mustParse(t, doc("", okRule), nil, nil); c.Server.UIListen != "127.0.0.1:8090" {
		t.Errorf("default = %q", c.Server.UIListen)
	}
	for in, want := range map[string]string{
		":8091":          ":8091",
		"[::1]:8092":     "[::1]:8092",
		"off":            "off",
		" Off ":          "off",
		"127.0.0.1:8093": "127.0.0.1:8093",
	} {
		c := mustParse(t, doc("server: {ui_listen: '"+in+"'}", okRule), nil, nil)
		if c.Server.UIListen != want {
			t.Errorf("%q -> %q, want %q", in, c.Server.UIListen, want)
		}
	}
	for _, bad := range []string{
		"server: {ui_listen: 'nonsense'}",
		"server: {ui_listen: '127.0.0.1:8000', listen: '127.0.0.1:8000'}",
		"server: {ui_listen: '127.0.0.1:9100', metrics_listen: '127.0.0.1:9100'}",
		"server: {ui_listen: ':8000', listen: '127.0.0.1:8000'}",
		"server: {ui_listen: '127.0.0.1:8000', listen: ':8000'}",
		"server: {listen: '127.0.0.1:8090'}", // clashes with the default ui_listen
	} {
		_, err := parse(t, doc(bad, okRule), nil, nil)
		if err == nil || !strings.Contains(err.Error(), "server.ui_listen") {
			t.Errorf("%s: err = %v", bad, err)
		}
	}
	// Same port on different explicit hosts, port 0, and off never clash.
	for _, ok := range []string{
		"server: {ui_listen: '127.0.0.1:8000', listen: '127.0.0.2:8000'}",
		"server: {ui_listen: '127.0.0.1:0', listen: '127.0.0.1:0', metrics_listen: '127.0.0.1:0'}",
		"server: {ui_listen: 'off', listen: '127.0.0.1:8090'}",
	} {
		mustParse(t, doc(ok, okRule), nil, nil)
	}
}

func TestUIListenEnvPrecedence(t *testing.T) {
	const key = "GITMIRROR_UI_LISTEN"
	tests := []struct {
		name, top string
		env       map[string]string
		want      string
	}{
		{"default", "", nil, "127.0.0.1:8090"},
		{"config", "server: {ui_listen: '127.0.0.1:8095'}", nil, "127.0.0.1:8095"},
		{"env over default", "", map[string]string{key: ":8090"}, ":8090"},
		{"env over config", "server: {ui_listen: '127.0.0.1:8095'}", map[string]string{key: ":8096"}, ":8096"},
		{"env off", "", map[string]string{key: " OFF "}, "off"},
		{"empty env ignored", "server: {ui_listen: '127.0.0.1:8095'}", map[string]string{key: ""}, "127.0.0.1:8095"},
		{"empty env default", "", map[string]string{key: ""}, "127.0.0.1:8090"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := mustParse(t, doc(tt.top, okRule), tt.env, nil)
			if c.Server.UIListen != tt.want {
				t.Fatalf("UIListen = %q, want %q", c.Server.UIListen, tt.want)
			}
		})
	}
	// The env value is validated like the file value.
	_, err := parse(t, doc("", okRule), map[string]string{key: "nonsense"}, nil)
	if err == nil || !strings.Contains(err.Error(), "server.ui_listen") {
		t.Errorf("bad env value: err = %v", err)
	}
}

// TestUIListenEnvWithoutConfigFile covers the no-config-file path (Parse of
// an empty document), which the Docker image relies on.
func TestUIListenEnvWithoutConfigFile(t *testing.T) {
	c, err := Parse(nil, envFrom(map[string]string{"GITMIRROR_UI_LISTEN": ":8090"}), filesFrom(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Server.UIListen != ":8090" {
		t.Fatalf("UIListen = %q", c.Server.UIListen)
	}
}

func TestExternalURL(t *testing.T) {
	if c := mustParse(t, doc("", okRule), nil, nil); c.Server.ExternalURL != "" {
		t.Errorf("default = %q", c.Server.ExternalURL)
	}
	for in, want := range map[string]string{
		"https://mirror.example.com":        "https://mirror.example.com",
		"https://mirror.example.com/":       "https://mirror.example.com",
		"http://10.0.0.5:8090/gitmirror/":   "http://10.0.0.5:8090/gitmirror",
		" HTTPS://Mirror.Example.com:8443 ": "HTTPS://Mirror.Example.com:8443",
	} {
		c := mustParse(t, doc("server: {external_url: '"+in+"'}", okRule), nil, nil)
		if c.Server.ExternalURL != want {
			t.Errorf("%q -> %q, want %q", in, c.Server.ExternalURL, want)
		}
	}
	for _, bad := range []string{
		"mirror.example.com",
		"/relative",
		"ftp://mirror.example.com",
		"https://",
		"https://mirror.example.com/?a=b",
		"https://mirror.example.com/#frag",
		"https://user:pw@mirror.example.com",
		"https:// bad host",
	} {
		_, err := parse(t, doc("server: {external_url: '"+bad+"'}", okRule), nil, nil)
		if err == nil || !strings.Contains(err.Error(), "server.external_url") {
			t.Errorf("%q: err = %v", bad, err)
		}
	}
}
