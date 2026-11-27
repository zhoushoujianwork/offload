package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"

	"github.com/zhoushoujianwork/offload/internal/config"
	"github.com/zhoushoujianwork/offload/internal/text"
)

const AgyProtocol = "acp"

func agyArgs(o Opts) []string {
	args := []string{"acp"}
	for _, dir := range o.AddDirs {
		args = append(args, "--add-dir", dir)
	}
	return args
}

// ACP allows agent requests while the client is waiting for a response. The
// protocol goroutine owns all stdin writes, leaving the stdout reader free to
// drain notifications even when a request is larger than a pipe buffer.
type agyACP struct {
	ctx         context.Context
	stdin       io.WriteCloser
	inbox       chan jsonRPCMessage
	protocolErr chan error
	processDone chan struct{}
	done        chan error
	nextID      int

	mu     sync.Mutex
	s      streamSummary
	report strings.Builder
}

func newAgyACP(ctx context.Context, stdin io.WriteCloser) *agyACP {
	return &agyACP{
		ctx: ctx, stdin: stdin, inbox: make(chan jsonRPCMessage, 16),
		protocolErr: make(chan error, 1), processDone: make(chan struct{}), done: make(chan error, 1),
	}
}

func (p *agyACP) start(o Opts, prompt string) {
	go func() {
		err := p.run(o, prompt)
		_ = p.stdin.Close()
		p.done <- err
	}()
}

func (p *agyACP) summary() streamSummary {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.s.clone()
}

func (p *agyACP) fail(err error) {
	select {
	case p.protocolErr <- err:
	default:
	}
}

func (p *agyACP) feed(line []byte) {
	var msg jsonRPCMessage
	if json.Unmarshal(line, &msg) != nil || msg.JSONRPC != "2.0" {
		p.fail(errors.New("Antigravity ACP emitted an invalid JSON-RPC frame"))
		return
	}
	p.mu.Lock()
	p.s.Events++
	p.mu.Unlock()
	if len(msg.ID) == 0 || string(msg.ID) == "null" {
		if msg.Method == "session/update" {
			p.update(msg.Params)
		}
		return
	}
	select {
	case p.inbox <- msg:
	default:
		p.fail(errors.New("Antigravity ACP sent too many pending messages"))
	}
}

func (p *agyACP) update(raw json.RawMessage) {
	var params struct {
		SessionID string
		Update    struct {
			SessionUpdate, Title, Kind string
			Content                    struct{ Type, Text string }
			ConfigOptions              []agyConfigOption
		}
	}
	if json.Unmarshal(raw, &params) != nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.s.Session == "" || params.SessionID != p.s.Session {
		return
	}
	u := params.Update
	switch u.SessionUpdate {
	case "agent_message_chunk":
		if u.Content.Type != "text" {
			return
		}
		if len(u.Content.Text) > maxLineKept-p.report.Len() {
			p.fail(errors.New("Antigravity ACP report exceeds the output limit"))
			return
		}
		p.report.WriteString(u.Content.Text)
		if s := strings.TrimSpace(u.Content.Text); s != "" {
			p.s.LastText = s
		}
	case "tool_call":
		p.s.ToolCalls++
		title := text.Truncate(text.OneLine(firstNonEmpty(u.Title, u.Kind)), 100)
		p.s.LastTools = append(p.s.LastTools, title)
		if len(p.s.LastTools) > lastToolsKept {
			p.s.LastTools = p.s.LastTools[1:]
		}
	case "config_option_update":
		p.setModelLocked(u.ConfigOptions, "")
	}
}

func (p *agyACP) send(message map[string]any) error {
	raw, err := json.Marshal(message)
	if err != nil {
		return err
	}
	if len(raw) > maxLineKept {
		return errors.New("Antigravity ACP request exceeds the frame limit")
	}
	_, err = p.stdin.Write(append(raw, '\n'))
	return err
}

func (p *agyACP) receive() (jsonRPCMessage, error) {
	select {
	case err := <-p.protocolErr:
		return jsonRPCMessage{}, err
	default:
	}
	select {
	case msg := <-p.inbox:
		return msg, nil
	default:
	}
	select {
	case msg := <-p.inbox:
		return msg, nil
	case err := <-p.protocolErr:
		return jsonRPCMessage{}, err
	case <-p.ctx.Done():
		return jsonRPCMessage{}, p.ctx.Err()
	case <-p.processDone:
		// The stdout reader may have queued a response immediately before exit.
		select {
		case msg := <-p.inbox:
			return msg, nil
		default:
			return jsonRPCMessage{}, io.ErrUnexpectedEOF
		}
	}
}

func (p *agyACP) request(method string, params any, result any) error {
	p.nextID++
	id := p.nextID
	err := p.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return fmt.Errorf("Antigravity ACP %s: %w", method, err)
	}
	for {
		msg, err := p.receive()
		if err != nil {
			return fmt.Errorf("Antigravity ACP %s: %w", method, err)
		}
		if msg.Method != "" {
			if err := p.answer(msg); err != nil {
				return err
			}
			continue
		}
		if string(msg.ID) != strconv.Itoa(id) || (len(msg.Result) > 0) == (len(msg.Error) > 0) {
			return fmt.Errorf("Antigravity ACP %s returned an invalid response", method)
		}
		if len(msg.Error) > 0 {
			var rpcErr struct {
				Code    int
				Message string
			}
			_ = json.Unmarshal(msg.Error, &rpcErr)
			if rpcErr.Code == -32000 {
				return errors.New("Antigravity ACP authentication required; please sign in with agy acp --login")
			}
			return fmt.Errorf("Antigravity ACP %s failed (%d): %s", method, rpcErr.Code, config.SanitizeReason(rpcErr.Message))
		}
		if string(msg.Result) == "null" || json.Unmarshal(msg.Result, result) != nil {
			return fmt.Errorf("Antigravity ACP %s returned an invalid result", method)
		}
		return nil
	}
}

func (p *agyACP) answer(msg jsonRPCMessage) error {
	reply := map[string]any{"jsonrpc": "2.0", "id": msg.ID}
	switch msg.Method {
	case "session/request_permission":
		p.mu.Lock()
		p.s.Denials++
		p.mu.Unlock()
		// Deny this action without modifying the user's persistent rules.
		outcome := map[string]string{"outcome": "cancelled"}
		var params struct {
			Options []struct{ Kind, OptionID string }
		}
		if json.Unmarshal(msg.Params, &params) == nil {
			for _, option := range params.Options {
				if option.Kind == "reject_once" && option.OptionID != "" {
					outcome = map[string]string{"outcome": "selected", "optionId": option.OptionID}
					break
				}
			}
		}
		reply["result"] = map[string]any{"outcome": outcome}
	default:
		reply["error"] = map[string]any{"code": -32601, "message": "Offload does not support this ACP client method"}
	}
	return p.send(reply)
}

type agyConfigOption struct {
	ID, Category, CurrentValue string
}

func (p *agyACP) setModelLocked(options []agyConfigOption, fallback string) {
	for _, option := range options {
		if option.Category == "model" || option.ID == "model" {
			fallback = firstNonEmpty(option.CurrentValue, fallback)
			break
		}
	}
	p.s.ObservedModel = firstNonEmpty(fallback, p.s.ObservedModel)
}

func (p *agyACP) run(o Opts, prompt string) error {
	var initialize struct {
		ProtocolVersion   int
		AgentCapabilities struct {
			SessionCapabilities struct{ Resume json.RawMessage }
		}
	}
	if err := p.request("initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}, &initialize); err != nil {
		return err
	}
	if initialize.ProtocolVersion != 1 {
		return fmt.Errorf("Antigravity ACP returned unsupported protocol version %d", initialize.ProtocolVersion)
	}
	params := map[string]any{"cwd": o.Dir, "mcpServers": []any{}}
	method := "session/new"
	if o.Resume != "" {
		if len(initialize.AgentCapabilities.SessionCapabilities.Resume) == 0 {
			return errors.New("Antigravity ACP does not support session resume; update agy")
		}
		method, params["sessionId"] = "session/resume", o.Resume
	}
	var session struct {
		SessionID     string
		ConfigOptions []agyConfigOption
		Models        struct{ CurrentModelID string }
	}
	if err := p.request(method, params, &session); err != nil {
		return err
	}
	if o.Resume != "" && session.SessionID != "" && session.SessionID != o.Resume {
		return errors.New("Antigravity ACP returned a different session ID for resume")
	}
	id := firstNonEmpty(session.SessionID, o.Resume)
	if strings.TrimSpace(id) == "" {
		return errors.New("Antigravity ACP returned no session ID")
	}
	p.mu.Lock()
	p.s.Session = id
	p.setModelLocked(session.ConfigOptions, session.Models.CurrentModelID)
	p.mu.Unlock()

	for _, setting := range []struct{ id, value string }{
		{"model", o.Model}, {"thought_level", o.Profile.ReasoningEffort},
	} {
		if setting.value == "" {
			continue
		}
		var applied struct{ ConfigOptions []agyConfigOption }
		if err := p.request("session/set_config_option", map[string]string{
			"sessionId": id, "configId": setting.id, "value": setting.value,
		}, &applied); err != nil {
			return fmt.Errorf("set Antigravity %s: %w", setting.id, err)
		}
		p.mu.Lock()
		p.setModelLocked(applied.ConfigOptions, "")
		p.mu.Unlock()
	}

	var turn struct{ StopReason string }
	if err := p.request("session/prompt", map[string]any{
		"sessionId": id, "prompt": []map[string]string{{"type": "text", "text": prompt}},
	}, &turn); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	report := strings.TrimSpace(p.report.String())
	result := &claudeResult{Result: report, SessionID: id, NumTurns: p.s.ToolCalls}
	if turn.StopReason != "end_turn" {
		result.IsError = true
		result.Errors = []string{"Antigravity ACP stopped before completing the prompt (" + firstNonEmpty(turn.StopReason, "unknown reason") + ")"}
	} else if report == "" {
		result.IsError = true
		result.Errors = []string{"Antigravity ACP completed without a text report"}
	}
	p.s.Result = result
	return nil
}
