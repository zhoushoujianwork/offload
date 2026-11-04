package worker

import "testing"

func TestClaudeObservedModelRequiresAssistantEvidence(t *testing.T) {
	for _, tc := range []struct{ stream, want string }{
		{`{"type":"system","subtype":"init","model":"requested-alias"}`, ""},
		{`{"type":"assistant","message":{"model":"native-model","content":[]}}`, "native-model"},
		{`{"type":"assistant","message":{"model":"<synthetic>","content":[]}}`, ""},
	} {
		if got := parseStream([]byte(tc.stream)).ObservedModel; got != tc.want {
			t.Fatalf("observed model = %q, want %q", got, tc.want)
		}
	}
}
