package config

import "testing"

func TestEndpointValidation(t *testing.T) {
	t.Setenv("OFFLOAD_PROFILE", "")
	claude := func(endpoint string) Profile {
		return Profile{BaseURL: "https://gateway", Model: "model", Endpoint: endpoint}
	}
	for _, tc := range []struct {
		name    string
		profile Profile
		wantErr string
	}{
		{"unset", claude(""), ""},
		{"cc", claude(EndpointCC), ""},
		{"llm", claude(EndpointLLM), ""},
		{"unknown", claude("foo"), `profile "test" has unknown endpoint "foo" (want cc or llm)`},
		{"codex with endpoint", Profile{Backend: "codex", Endpoint: "cc"}, `profile "test": endpoint applies only to claude profiles`},
		{"codex without endpoint", Profile{Backend: "codex"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Config{Default: "test", Profiles: map[string]Profile{"test": tc.profile}}
			_, _, err := c.Resolve("")
			if tc.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr != "" && (err == nil || err.Error() != tc.wantErr) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
	for s, want := range map[string]bool{"": true, "cc": true, "llm": true, "foo": false, "CC": false} {
		if got := ValidEndpoint(s); got != want {
			t.Errorf("ValidEndpoint(%q) = %v, want %v", s, got, want)
		}
	}
}

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
