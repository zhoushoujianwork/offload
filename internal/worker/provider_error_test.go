package worker

import (
	"encoding/json"
	"testing"
)

func TestProviderErrorFixtures(t *testing.T) {
	for _, tc := range []struct{ name, payload, kind string }{
		{"status capacity", `"status_code":529,"message":"PRIVATE"`, "capacity"},
		{"status rate", `"http_status_code":429,"message":"Selected model is at capacity"`, "rate_limit"},
		{"overload code", `"type":"overloaded_error","message":"PRIVATE"`, "capacity"},
		{"rate code", `"code":"rate_limit_exceeded","message":"PRIVATE"`, "rate_limit"},
		{"unknown code", `"code":"invalid_request_error","message":"Selected model is at capacity"`, "execution"},
		{"unknown status", `"status":400,"message":"Overloaded"`, "execution"},
		{"capacity prose", `"message":"Selected model is at capacity. PRIVATE"`, "capacity"},
		{"overload prose", `"message":"Overloaded"`, "capacity"},
		{"rate prose", `"message":"Rate limit exceeded: PRIVATE"`, "rate_limit"},
		{"unrelated", `"message":"authentication failed"`, ""},
		{"incidental", `"message":"Test failed: Selected model is at capacity"`, ""},
		{"word boundary", `"message":"OverloadedWidget failed"`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			codex := parseCodexStream([]byte(`{"type":"turn.failed","error":{` + tc.payload + `}}`))
			// Claude result prose is separate from its optional provider error envelope.
			claude := parseStream([]byte(`{"type":"result","is_error":true,"error":{` + tc.payload + `},"errors":[` + messageJSON(tc.payload) + `]}`))
			for backend, s := range map[string]streamSummary{"codex": codex, "claude": claude} {
				if s.Result == nil || s.Result.ErrorKind != tc.kind {
					t.Fatalf("%s: %+v", backend, s.Result)
				}
			}
		})
	}
}

func messageJSON(payload string) string {
	var obj map[string]json.RawMessage
	_ = json.Unmarshal([]byte("{"+payload+"}"), &obj)
	return string(obj["message"])
}

func TestProviderErrorRecovery(t *testing.T) {
	for _, tc := range []struct {
		stream string
		codex  bool
		kind   string
	}{
		{`{"type":"error","code":"overloaded_error"}
{"type":"turn.failed","error":{"message":"failed"}}`, true, "capacity"},
		{`{"type":"assistant","error":"rate_limit"}
{"type":"result","is_error":true,"errors":["failed"]}`, false, "rate_limit"},
		{`{"type":"assistant","error":"overloaded_error"}
{"type":"result","is_error":false,"result":"success"}`, false, ""},
		{`{"type":"error","code":"overloaded_error"}
{"type":"item.completed","item":{"type":"agent_message","text":"success"}}
{"type":"turn.completed"}`, true, ""},
	} {
		s := parseStream([]byte(tc.stream))
		if tc.codex {
			s = parseCodexStream([]byte(tc.stream))
		}
		if s.Result == nil || s.Result.ErrorKind != tc.kind {
			t.Fatalf("%+v", s.Result)
		}
	}
}
