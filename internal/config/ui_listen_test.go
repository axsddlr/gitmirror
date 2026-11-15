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
