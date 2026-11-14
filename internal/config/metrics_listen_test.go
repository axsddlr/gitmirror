package config

import (
	"strings"
	"testing"
)

func TestMetricsListen(t *testing.T) {
	if c := mustParse(t, doc("", okRule), nil, nil); c.Server.MetricsListen != "127.0.0.1:9090" {
		t.Errorf("default = %q", c.Server.MetricsListen)
	}
	for in, want := range map[string]string{
		":9090":          ":9090",
		"[::1]:9100":     "[::1]:9100",
		"off":            "off",
		"OFF":            "off",
		"127.0.0.1:9091": "127.0.0.1:9091",
	} {
		c := mustParse(t, doc("server: {metrics_listen: '"+in+"'}", okRule), nil, nil)
		if c.Server.MetricsListen != want {
			t.Errorf("%q -> %q, want %q", in, c.Server.MetricsListen, want)
		}
	}
	_, err := parse(t, doc("server: {metrics_listen: 'nonsense'}", okRule), nil, nil)
	if err == nil || !strings.Contains(err.Error(), "server.metrics_listen") {
		t.Fatalf("err = %v", err)
	}
}
