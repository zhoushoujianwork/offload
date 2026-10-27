package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/offload/internal/config"
	"github.com/zhoushoujianwork/offload/internal/history"
)

func TestCodexModelResolution(t *testing.T) {
	for _, tc := range []struct {
		name, fixture, explicit, model, source string
		config                                 bool
	}{
		{"session", "session.jsonl", "", "gpt-6-astra", "session", true},
		{"config", "no-model.jsonl", "", "gpt-6.1-sol", "config", true},
		{"unknown", "no-model.jsonl", "", "", "unknown", false},
		{"override", "session.jsonl", "explicit-model", "explicit-model", "profile", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata", "codex-model", tc.fixture))
			if err != nil {
				t.Fatal(err)
			}
			writeBaseline(t, string(data))
			if tc.config {
				data, err = os.ReadFile("testdata/codex-model/config.toml")
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(filepath.Join(codexHome(), "config.toml"), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			rec := history.Record{Session: usageSession}
			resolveCodexModel(&rec, tc.explicit)
			if rec.Model != tc.model || rec.ModelSource != tc.source || (rec.ModelUnavailable != "") != (tc.source == "unknown") {
				t.Fatalf("%+v", rec)
			}
		})
	}
}

func TestCodexModelRejectsWrongSessionAndCorruptRollout(t *testing.T) {
	for _, contents := range []string{
		`{"type":"session_meta","payload":{"id":"other"}}` + "\n" + `{"type":"turn_context","payload":{"model":"wrong"}}`,
		`{"type":"turn_context","payload":{"model":"unverified"}}`,
		baselineLog(freshUsage) + `{"type":"turn_context","payload":{"model":"partial"}}` + "\n{",
	} {
		writeBaseline(t, contents)
		if model := codexSessionModel(usageSession); model != "" {
			t.Fatal(model)
		}
	}
	if model := codexSessionModel("../../invalid"); model != "" {
		t.Fatal(model)
	}
}

func TestCodexConfigDefaultHome(t *testing.T) {
	t.Setenv("CODEX_HOME", "")
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(codexHome(), 0700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ content, want string }{
		{"model = \"gpt-6-astra\" # comment\n", "gpt-6-astra"},
		{"[profiles.other]\nmodel = 'wrong'\n", ""},
		{"model = 123\n", ""},
		{"instructions = '''\nmodel = 'wrong'\n'''\n", ""},
		{"model = 'valid'\ninstructions = '''\nunterminated\n", ""},
	} {
		if err := os.WriteFile(filepath.Join(codexHome(), "config.toml"), []byte(tc.content), 0600); err != nil {
			t.Fatal(err)
		}
		if got := codexConfigModel(); got != tc.want {
			t.Fatalf("%q: %q", tc.content, got)
		}
	}
}

func TestCodexConfigMultilineSettings(t *testing.T) {
	for _, tc := range []struct{ name, content string }{
		{"inline comment", "model = \"config-model\" # documentation: \"\"\"\n"},
		{"following instructions", "model = 'config-model'\ndeveloper_instructions = \"\"\"\nBe concise.\n\"\"\"\n"},
		{"preceding instructions", "developer_instructions = '''\nmodel = 'wrong'\n[profiles.other]\n'''\nmodel = 'config-model'\n"},
		{"quoted delimiter", "instructions = \"literal ''' delimiter\"\nmodel = 'config-model'\n"},
		{"same line delimiters", "instructions = \"\"\"Be concise.\"\"\" # '''\nmodel = 'config-model'\n"},
		{"escaped delimiter", "instructions = \"\"\"\nescaped \\\"\"\"\nmodel = 'wrong'\n\"\"\"\nmodel = 'config-model'\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CODEX_HOME", t.TempDir())
			if err := os.WriteFile(filepath.Join(codexHome(), "config.toml"), []byte(tc.content), 0600); err != nil {
				t.Fatal(err)
			}
			rec := history.Record{}
			resolveCodexModel(&rec, "")
			if rec.Model != "config-model" || rec.ModelSource != "config" || rec.ModelUnavailable != "" {
				t.Fatalf("%+v", rec)
			}
		})
	}
}

func TestCodexSubscriptionNeverEstimatesCost(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	for _, auth := range []string{`{"auth_mode":"chatgpt"}`, `{"tokens":{"access_token":"fixture"},"OPENAI_API_KEY":null}`} {
		if err := os.WriteFile(filepath.Join(codexHome(), "auth.json"), []byte(auth), 0600); err != nil {
			t.Fatal(err)
		}
		for _, completed := range []bool{true, false} {
			rec := history.Record{}
			applyCodexAccounting(&rec, parseCodexStream(usageStream(freshUsage)), &history.TokenUsage{}, "", Opts{Model: "gpt-6.1-sol", Profile: config.Profile{ServiceTier: "default"}}, completed)
			if rec.CostUSD != nil || rec.CostIsEstimate || rec.Pricing != nil || !strings.Contains(rec.CostUnavailable, "subscription") {
				t.Fatalf("%+v", rec)
			}
		}
	}
}
