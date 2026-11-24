package config

import "testing"

// disable_actions is off unless a rule asks for it.
func TestDisableActions(t *testing.T) {
	c := mustParse(t, doc("", okRule), nil, nil)
	if c.Rules[0].Effective(c.Defaults).DisableActions {
		t.Fatal("disable_actions is on by default")
	}
	c = mustParse(t, doc("", okRule+"    disable_actions: true\n"), nil, nil)
	if !c.Rules[0].Effective(c.Defaults).DisableActions {
		t.Fatal("disable_actions: true was not read")
	}
}
