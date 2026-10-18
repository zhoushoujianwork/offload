package config

import "testing"

func TestBackendValidation(t *testing.T) {
	t.Setenv("OFFLOAD_PROFILE", "")
	for _, tc := range []struct {
		name    string
		profile Profile
		valid   bool
	}{
		{"legacy Claude", Profile{BaseURL: "https://gateway", Model: "model"}, true},
		{"explicit Claude", Profile{Backend: "claude", BaseURL: "https://gateway", Model: "model"}, true},
		{"native Codex", Profile{Backend: "codex"}, true},
		{"unknown backend", Profile{Backend: "unknown"}, false},
		{"missing Claude config", Profile{Backend: "claude"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Config{Default: "test", Profiles: map[string]Profile{"test": tc.profile}}
			_, _, err := c.Resolve("")
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
}
