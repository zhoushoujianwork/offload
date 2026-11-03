package nativeusage

import (
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/zhoushoujianwork/offload/internal/history"
)

func modelCounters(raw json.RawMessage) *history.TokenUsage {
	var models map[string]struct {
		Input  *int64 `json:"inputTokens"`
		Output *int64 `json:"outputTokens"`
		Read   int64  `json:"cacheReadInputTokens"`
		Write  int64  `json:"cacheCreationInputTokens"`
	}
	if json.Unmarshal(raw, &models) != nil || len(models) == 0 {
		return nil
	}
	var sum history.TokenUsage
	for _, m := range models {
		if m.Input == nil || m.Output == nil {
			return nil
		}
		raw, _ := json.Marshal(map[string]int64{"input_tokens": *m.Input, "output_tokens": *m.Output, "cache_read_input_tokens": m.Read, "cache_creation_input_tokens": m.Write})
		u := counters(raw, true)
		if u == nil || !add(&sum, *u) {
			return nil
		}
	}
	return &sum
}

func (p *parser) messageTotal() *history.TokenUsage {
	if len(p.messages) == 0 {
		return nil
	}
	var sum history.TokenUsage
	for _, m := range p.messages {
		if !add(&sum, m.usage) {
			return nil
		}
	}
	return &sum
}

func (p *parser) claude(raw []byte) error {
	var e struct {
		Type           string    `json:"type"`
		Timestamp      time.Time `json:"timestamp"`
		Session        string    `json:"sessionId"`
		UUID           string    `json:"uuid"`
		Parent         *string   `json:"parentUuid"`
		Sidechain      bool      `json:"isSidechain"`
		Meta           bool      `json:"isMeta"`
		CompactSummary bool      `json:"isCompactSummary"`
		APIError       bool      `json:"isApiErrorMessage"`
		CWD            string    `json:"cwd"`
		Message        struct {
			ID      string          `json:"id"`
			Model   string          `json:"model"`
			Content json.RawMessage `json:"content"`
			Usage   json.RawMessage `json:"usage"`
		} `json:"message"`
		Cost        *float64        `json:"totalCostUSD"`
		Models      json.RawMessage `json:"modelUsage"`
		UnknownCost bool            `json:"hasUnknownModelCost"`
	}
	if json.Unmarshal(raw, &e) != nil {
		return errors.New("corrupt native event")
	}
	if e.Session != "" && e.Session != p.session {
		return errors.New("native session mismatch")
	}
	if e.Session == p.session {
		p.verified = true
	}
	if e.Sidechain {
		return nil
	}
	if !e.Timestamp.IsZero() {
		p.lastTime = e.Timestamp
	}
	if e.Type == "user" && !e.Meta && !e.CompactSummary {
		if prompt, ok := userPrompt(e.Message.Content); ok {
			p.finish()
			p.baseUsage, p.baseCost = p.nextUsage, p.nextCost
			p.nextUsage, p.nextCost = nil, nil
			if len(p.segments) == 0 && e.Parent == nil {
				p.baseUsage = &history.TokenUsage{}
				z := 0.0
				p.baseCost = &z
			}
			p.current = &segment{start: e.Timestamp, cwd: e.CWD, prompt: prompt, id: e.UUID}
		}
	}
	s := p.current
	if s == nil {
		return nil
	}
	if e.Type == "assistant" && !e.APIError && e.Message.Model != "<synthetic>" {
		u := counters(e.Message.Usage, true)
		if u == nil || e.Message.ID == "" {
			s.invalid = true
			return nil
		}
		// Streaming chunks share a message id and carry cumulative counters.
		// Keep its latest counters once instead of summing each content block.
		if old, ok := p.messages[e.Message.ID]; ok && (u.Input < old.usage.Input || u.Output < old.usage.Output || u.Cached < old.usage.Cached || u.CacheWrite < old.usage.CacheWrite) {
			s.invalid = true
		}
		p.messages[e.Message.ID] = messageUsage{*u, e.Message.Model}
		if text, ok := userPrompt(e.Message.Content); ok {
			s.report = text
		}
		s.end = e.Timestamp
	}
	if e.Type == "cost-state" {
		s.end = p.lastTime
		u := modelCounters(e.Models)
		if u == nil {
			return nil
		}
		s.cumulative, p.nextUsage = u, u
		if e.Cost != nil && history.ValidCost(*e.Cost) && !e.UnknownCost {
			s.cumulativeCost, p.nextCost = e.Cost, e.Cost
		}
		measured := p.messageTotal()
		if measured == nil || p.baseUsage == nil {
			return nil
		}
		base := p.baseUsage
		delta := history.TokenUsage{Input: u.Input - base.Input, Cached: u.Cached - base.Cached, CacheWrite: u.CacheWrite - base.CacheWrite, Output: u.Output - base.Output}
		if !delta.Valid() || delta != *measured {
			s.invalid = true
			return nil
		}
		s.complete = true
		if p.nextCost == nil || p.baseCost == nil || *e.Cost < *p.baseCost {
			return nil
		}
		cost := math.Round((*e.Cost-*p.baseCost)*1e12) / 1e12
		s.cost = &cost
	}
	return nil
}

// Native SDK input uses content blocks; tool-result messages also have type
// "user", but must never begin a new invocation or reset its usage counters.
func userPrompt(raw json.RawMessage) (string, bool) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, text != ""
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return "", false
	}
	for _, b := range blocks {
		if b.Type == "tool_result" {
			return "", false
		}
		if b.Type == "text" {
			if text != "" {
				text += "\n"
			}
			text += b.Text
		}
	}
	return text, text != ""
}
