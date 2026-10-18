package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSetModelPreservesOtherSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	original := `{"default":"a","delegation_mode":"basic","future":true,"profiles":{"a":{"backend":"claude","base_url":"https://gateway","model":"old","token":"test-secret","token_cmd":"printenv TOKEN","future":{"keep":true}},"b":{"backend":"codex","model":"other"}}}`
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	if err := SetModel(path, "a", " new-model "); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	var got, want map[string]any
	json.Unmarshal(b, &got)
	json.Unmarshal([]byte(original), &want)
	want["profiles"].(map[string]any)["a"].(map[string]any)["model"] = "new-model"
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if !bytes.Equal(gotJSON, wantJSON) {
		t.Fatal("model update changed unrelated settings")
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm()&0077 != 0 {
		t.Fatal("config is not private")
	}
}

func TestSetModelValidationAndNativeDefault(t *testing.T) {
	for _, tc := range []struct {
		name, contents, profile, model string
		valid                          bool
	}{
		{"native default", `{"profiles":{"a":{"backend":"codex","model":"old"}}}`, "a", "", true},
		{"repair empty model", `{"profiles":{"a":{"base_url":"https://gateway"}}}`, "a", "new", true},
		{"empty Claude", `{"profiles":{"a":{"base_url":"https://gateway","model":"old"}}}`, "a", "  ", false},
		{"unknown profile", `{"profiles":{}}`, "unknown", "new", false},
		{"empty profile", `{"profiles":{}}`, "", "new", false},
		{"unknown backend", `{"profiles":{"a":{"backend":"other"}}}`, "a", "new", false},
		{"bad config", `{`, "a", "new", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(tc.contents), 0600); err != nil {
				t.Fatal(err)
			}
			err := SetModel(path, tc.profile, tc.model)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
			if !tc.valid {
				after, _ := os.ReadFile(path)
				if string(after) != tc.contents {
					t.Fatal("invalid model selection changed config")
				}
			}
		})
	}
}
