package config

import "testing"

func TestValidName(t *testing.T) {
	for _, ok := range []string{"github", "git-example-com", "a_b", "x1"} {
		if !ValidName(ok) {
			t.Errorf("ValidName(%q) = false", ok)
		}
	}
	for _, bad := range []string{"", "-a", "A", "a.b", "a b", "ä"} {
		if ValidName(bad) {
			t.Errorf("ValidName(%q) = true", bad)
		}
	}
}

func TestValidateExternalURL(t *testing.T) {
	for _, ok := range []string{"https://mirror.example.com", "http://10.0.0.1:8080/gm"} {
		if err := ValidateExternalURL(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"mirror.example.com", "ftp://x", "https://", "https://u:p@x", "https://x/?a=1", "https://x/#f"} {
		if err := ValidateExternalURL(bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}
