package nativeusage

import (
	"encoding/json"
	"errors"
	"strings"
	"time"
)

func (p *parser) codex(raw []byte) error {
	var e struct {
		Type      string    `json:"type"`
		Timestamp time.Time `json:"timestamp"`
		Payload   struct {
			Type    string `json:"type"`
			ID      string `json:"id"`
			Session string `json:"session_id"`
			Thread  string `json:"thread_id"`
			Turn    string `json:"turn_id"`
			CWD     string `json:"cwd"`
			Model   string `json:"model"`
			Role    string `json:"role"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			TurnUsage   json.RawMessage `json:"turn_token_usage"`
			ThreadUsage json.RawMessage `json:"thread_token_usage"`
			Settings    struct {
				Model string `json:"model"`
				Tier  string `json:"service_tier"`
			} `json:"thread_settings"`
		} `json:"payload"`
	}
	if json.Unmarshal(raw, &e) != nil {
		return errors.New("corrupt native event")
	}
	v := e.Payload
	if e.Type == "session_meta" {
		if v.ID != p.session {
			return errors.New("native session mismatch")
		}
		p.verified = true
		return nil
	}
	if e.Type == "event_msg" && v.Type == "task_started" {
		if p.current != nil && p.current.id == v.Turn {
			return nil
		}
		p.finish()
		p.current = &segment{start: e.Timestamp, id: v.Turn}
	}
	s := p.current
	if s == nil {
		return nil
	}
	if e.Type == "turn_context" && v.Turn == s.id {
		s.cwd, s.model = v.CWD, v.Model
	}
	if e.Type == "event_msg" && v.Type == "thread_settings_applied" {
		if v.Thread != "" && v.Thread != p.session {
			s.invalid = true
			return nil
		}
		if v.Settings.Model != "" {
			s.model = v.Settings.Model
		}
		s.tier = v.Settings.Tier
	}
	if e.Type == "response_item" && v.Type == "message" && v.Role == "user" {
		var parts []string
		for _, c := range v.Content {
			parts = append(parts, c.Text)
		}
		if s.prompt != "" {
			s.prompt += "\n"
		}
		s.prompt += strings.Join(parts, "\n")
	}
	if e.Type == "token_usage_record" && v.Turn == s.id {
		if (v.Session != "" && v.Session != p.session) || (v.Thread != "" && v.Thread != p.session) {
			s.invalid = true
			return nil
		}
		u := counters(v.TurnUsage, false)
		cum := counters(v.ThreadUsage, false)
		if u == nil || cum == nil {
			s.invalid = true
			return nil
		}
		if s.usage != nil && (u.Input < s.usage.Input || u.Cached < s.usage.Cached || u.CacheWrite < s.usage.CacheWrite || u.Output < s.usage.Output) {
			s.invalid = true
		}
		s.usage, s.cumulative, s.end = u, cum, e.Timestamp
	}
	if e.Type == "event_msg" && v.Type == "task_complete" && v.Turn == s.id {
		s.complete, s.end = true, e.Timestamp
	}
	return nil
}
