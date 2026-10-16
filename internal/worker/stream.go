package worker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/zhoushoujianwork/offload/internal/text"
)

// streamSummary is what survives of a `claude -p --output-format stream-json` run. The final
// "result" event only exists when the worker finishes by itself; a worker killed by the timeout
// leaves just the events below, which is still enough to resume its session.
type streamSummary struct {
	Session   string
	Events    int
	ToolCalls int
	Denials   int
	LastText  string
	LastTools []string
	Result    *claudeResult
}

const lastToolsKept = 5

// parseStream reads newline-delimited events and stops at the first undecodable one, which for a
// killed worker is a truncated final line.
func parseStream(out []byte) streamSummary {
	var s streamSummary
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var raw json.RawMessage
		if dec.Decode(&raw) != nil {
			break
		}
		var head struct {
			Type      string          `json:"type"`
			Subtype   string          `json:"subtype"`
			SessionID string          `json:"session_id"`
			Message   json.RawMessage `json:"message"`
		}
		if json.Unmarshal(raw, &head) != nil {
			continue
		}
		s.Events++
		if s.Session == "" {
			s.Session = head.SessionID
		}
		switch head.Type {
		case "result":
			var r claudeResult
			if json.Unmarshal(raw, &r) == nil {
				s.Result = &r
			}
		case "system":
			if head.Subtype == "permission_denied" {
				s.Denials++
			}
		case "assistant":
			s.addAssistant(head.Message)
		}
	}
	return s
}

func (s *streamSummary) addAssistant(message json.RawMessage) {
	var m struct {
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	}
	if json.Unmarshal(message, &m) != nil {
		return
	}
	for _, b := range m.Content {
		switch b.Type {
		case "text":
			if t := strings.TrimSpace(b.Text); t != "" {
				s.LastText = t
			}
		case "tool_use":
			s.ToolCalls++
			s.LastTools = append(s.LastTools, b.Name+" "+text.Truncate(text.OneLine(string(b.Input)), 100))
			if len(s.LastTools) > lastToolsKept {
				s.LastTools = s.LastTools[1:]
			}
		}
	}
}

// resumeHint tells a supervising agent how to recover the work of an interrupted run.
func (s streamSummary) resumeHint() string {
	if s.Session == "" {
		return ""
	}
	return fmt.Sprintf(`; resume with: offload -r %s "stop exploring and report what you already found"`, s.Session)
}

// progress describes what the worker did before it stopped, so an interrupted run is not a blank.
func (s streamSummary) progress() string {
	if s.Events == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\nBefore stopping the worker made %d tool calls and had %d permission denials.", s.ToolCalls, s.Denials)
	if s.LastText != "" {
		fmt.Fprintf(&b, "\nLast worker text:\n%s", text.Truncate(s.LastText, 2000))
	}
	if len(s.LastTools) > 0 {
		b.WriteString("\nLast tool calls:")
		for _, t := range s.LastTools {
			fmt.Fprintf(&b, "\n- %s", t)
		}
	}
	return b.String()
}
