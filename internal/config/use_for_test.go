package config

import (
	"strings"
	"testing"
)

func TestNormalizeUseFor(t *testing.T) {
	for _, tc := range []struct {
		input, want string
		invalid     bool
	}{
		{"", "", false}, {"  frontend/UI; long reviews  ", "frontend/UI; long reviews", false},
		{"   ", "", false}, {strings.Repeat("界", 300), strings.Repeat("界", 300), false},
		{strings.Repeat("界", 301), "", true}, {"a\nb", "", true}, {"a\rb", "", true},
		{"a\tb", "", true}, {"\nhint", "", true}, {"a\x00b", "", true}, {"a\x7fb", "", true},
		{"a\u2028b", "", true}, {"a\u2029b", "", true},
	} {
		got, err := NormalizeUseFor(tc.input)
		if (err != nil) != tc.invalid || got != tc.want {
			t.Errorf("%q: got %q, %v", tc.input, got, err)
		}
	}
}

func TestValidateClaudeSettings(t *testing.T) {
	for _, effort := range []string{"", "low", "medium", "high", "xhigh", "max", "ultra", "invalid"} {
		err := (Profile{ReasoningEffort: effort}).ValidateClaudeSettings()
		if (err != nil) != (effort == "ultra" || effort == "invalid") {
			t.Errorf("%q: %v", effort, err)
		}
	}
}
