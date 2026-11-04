package worker

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zhoushoujianwork/offload/internal/text"
)

const codexAppServerProtocol = "codex-app-server"

type appServerMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   json.RawMessage `json:"error"`
}

type appServerParser struct {
	mu              sync.Mutex
	w               io.WriteCloser
	nextID          int
	pending         map[string]chan appServerMessage
	done            chan struct{}
	doneOnce        sync.Once
	err             error
	s               streamSummary
	threadID        string
	turnID          string
	report          strings.Builder
	completedReport string
	o               Opts
}

func newAppServerParser(w io.WriteCloser, o Opts) *appServerParser {
	return &appServerParser{w: w, pending: make(map[string]chan appServerMessage), done: make(chan struct{}), o: o}
}

func (p *appServerParser) summary() streamSummary {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.s.clone()
}

func (p *appServerParser) feed(line []byte) {
	var m appServerMessage
	if len(line) > maxLineKept || json.Unmarshal(line, &m) != nil || m.JSONRPC != "" && m.JSONRPC != "2.0" {
		p.fail(errors.New("Codex app-server emitted a malformed JSON-RPC frame"))
		return
	}
	if len(m.ID) > 0 && string(m.ID) != "null" {
		if m.Method != "" {
			p.rejectServerRequest(m)
			return
		}
		if (len(m.Result) > 0) == (len(m.Error) > 0) {
			p.fail(errors.New("Codex app-server response must contain exactly one result or error"))
			return
		}
		key := string(m.ID)
		p.mu.Lock()
		ch := p.pending[key]
		delete(p.pending, key)
		p.mu.Unlock()
		if ch == nil {
			p.fail(errors.New("Codex app-server returned an unknown JSON-RPC id"))
			return
		}
		ch <- m
		return
	}
	if m.Method == "" {
		p.fail(errors.New("Codex app-server emitted a frame without method or id"))
		return
	}
	p.notification(m.Method, m.Params)
}

func (p *appServerParser) rejectServerRequest(m appServerMessage) {
	_ = p.write(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(m.ID), "error": map[string]any{"code": -32601, "message": "interactive server requests are not supported"}})
	p.fail(fmt.Errorf("Codex app-server requested unsupported interaction %q", m.Method))
}

func (p *appServerParser) notification(method string, raw json.RawMessage) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.s.Events++
	switch method {
	case "thread/started":
		var v struct {
			Thread struct{ ID, Model string } `json:"thread"`
		}
		if json.Unmarshal(raw, &v) == nil && v.Thread.ID != "" {
			p.s.Session, p.threadID = v.Thread.ID, v.Thread.ID
			if v.Thread.Model != "" {
				p.s.ObservedModel = v.Thread.Model
			}
		}
	case "turn/started":
		var v struct {
			ThreadID string `json:"threadId"`
			Turn     struct {
				ID string `json:"id"`
			} `json:"turn"`
		}
		if json.Unmarshal(raw, &v) == nil && v.ThreadID == p.threadID {
			p.turnID = v.Turn.ID
		}
	case "item/started":
		var v struct {
			ThreadID, TurnID string
			Item             struct{ Type, ID string }
		}
		if json.Unmarshal(raw, &v) == nil && v.ThreadID == p.threadID && v.TurnID == p.turnID {
			switch v.Item.Type {
			case "commandExecution", "fileChange", "mcpToolCall", "webSearch":
				p.s.ToolCalls++
				p.s.LastTools = append(p.s.LastTools, v.Item.Type)
				if len(p.s.LastTools) > lastToolsKept {
					p.s.LastTools = p.s.LastTools[1:]
				}
			}
		}
	case "item/agentMessage/delta":
		var v struct{ ThreadID, TurnID, Delta string }
		if json.Unmarshal(raw, &v) == nil && v.ThreadID == p.threadID && v.TurnID == p.turnID {
			p.report.WriteString(v.Delta)
		}
	case "item/completed":
		var v struct {
			ThreadID, TurnID string
			Item             struct{ Type, Text string }
		}
		if json.Unmarshal(raw, &v) == nil && v.ThreadID == p.threadID && v.TurnID == p.turnID && v.Item.Type == "agentMessage" && strings.TrimSpace(v.Item.Text) != "" {
			p.completedReport = v.Item.Text
		}
	case "model/rerouted":
		var v struct{ ThreadID, TurnID, ToModel string }
		if json.Unmarshal(raw, &v) == nil && v.ThreadID == p.threadID && v.TurnID == p.turnID && v.ToModel != "" {
			p.s.ObservedModel = v.ToModel
		}
	case "turn/completed":
		var v struct {
			ThreadID string `json:"threadId"`
			Turn     struct {
				ID, Status string
				Error      json.RawMessage
				Items      []struct{ Type, Text string }
			} `json:"turn"`
		}
		if json.Unmarshal(raw, &v) != nil || v.ThreadID != p.threadID || v.Turn.ID != p.turnID {
			return
		}
		report := strings.TrimSpace(firstNonEmpty(p.completedReport, p.report.String()))
		for _, item := range v.Turn.Items {
			if item.Type == "agentMessage" && strings.TrimSpace(item.Text) != "" {
				report = item.Text
			}
		}
		res := &claudeResult{Result: report, SessionID: p.threadID, NumTurns: p.s.ToolCalls}
		if report == "" {
			res.IsError = true
			res.Errors = append(res.Errors, "Codex app-server completed without a report")
		}
		if v.Turn.Status != "completed" {
			res.IsError = true
			res.Subtype = v.Turn.Status
			message := "Codex app-server turn " + v.Turn.Status
			if diagnostic := appServerTurnError(v.Turn.Error); diagnostic != "" {
				message += ": " + diagnostic
			}
			res.Errors = append(res.Errors, message)
		}
		if p.o.Native.JSONSchema != "" && report != "" {
			var structured json.RawMessage
			if json.Unmarshal([]byte(report), &structured) == nil {
				res.StructuredOutput = structured
			} else {
				res.IsError = true
				res.Errors = append(res.Errors, "Codex app-server returned invalid structured output")
			}
		}
		p.s.LastText, p.s.Result = report, res
		p.finishLocked(nil)
	}
}

func appServerTurnError(raw json.RawMessage) string {
	var v struct{ Message, AdditionalDetails string }
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	message := strings.TrimSpace(v.Message)
	if details := strings.TrimSpace(v.AdditionalDetails); details != "" && details != message {
		message = strings.TrimSpace(message + ": " + details)
	}
	return text.Truncate(text.OneLine(message), 500)
}

func (p *appServerParser) fail(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.finishLocked(err)
}

func (p *appServerParser) finishLocked(err error) {
	if p.err == nil {
		p.err = err
	}
	p.doneOnce.Do(func() { close(p.done); _ = p.w.Close() })
}

func (p *appServerParser) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b) > maxLineKept {
		return errors.New("Codex app-server request exceeds frame limit")
	}
	b = append(b, '\n')
	p.mu.Lock()
	defer p.mu.Unlock()
	_, err = p.w.Write(b)
	return err
}

func (p *appServerParser) request(method string, params any) (json.RawMessage, error) {
	p.mu.Lock()
	p.nextID++
	id := strconv.Itoa(p.nextID)
	ch := make(chan appServerMessage, 1)
	p.pending[id] = ch
	requestID := p.nextID
	p.mu.Unlock()
	if err := p.write(map[string]any{"jsonrpc": "2.0", "id": requestID, "method": method, "params": params}); err != nil {
		return nil, err
	}
	select {
	case m := <-ch:
		if len(m.Error) > 0 && string(m.Error) != "null" {
			return nil, fmt.Errorf("Codex app-server %s failed: %s", method, m.Error)
		}
		if len(m.Result) == 0 {
			return nil, fmt.Errorf("Codex app-server %s response has no result", method)
		}
		return m.Result, nil
	case <-p.done:
		p.mu.Lock()
		err := p.err
		p.mu.Unlock()
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
}

func (p *appServerParser) start() {
	go func() {
		if err := p.initialize(); err != nil {
			p.fail(err)
		}
	}()
}

func (p *appServerParser) initialize() error {
	if _, err := p.request("initialize", map[string]any{"clientInfo": map[string]any{"name": "offload", "title": "Offload", "version": "1"}, "capabilities": map[string]any{"experimentalApi": true}}); err != nil {
		return err
	}
	if err := p.write(map[string]any{"jsonrpc": "2.0", "method": "initialized", "params": map[string]any{}}); err != nil {
		return err
	}
	skillRoots := append([]string{}, p.o.Native.SkillRoots...)
	if _, err := p.request("skills/extraRoots/set", map[string]any{"extraRoots": skillRoots}); err != nil {
		return err
	}
	listed, err := p.request("skills/list", map[string]any{"cwds": []string{p.o.Dir}, "forceReload": true})
	if err != nil {
		return err
	}
	if err := validateAppServerSkills(listed, p.o.Native.SkillRoots); err != nil {
		return err
	}
	config := appServerConfig(p.o.Native.MCPServers)
	roots := append([]string{p.o.Dir}, p.o.AddDirs...)
	params := map[string]any{"cwd": p.o.Dir, "developerInstructions": string(p.o.Instructions), "approvalPolicy": "never", "sandbox": "danger-full-access", "runtimeWorkspaceRoots": roots, "config": config}
	if p.o.Model != "" {
		params["model"] = p.o.Model
	}
	var threadRaw json.RawMessage
	if p.o.Resume != "" {
		params["threadId"] = p.o.Resume
		threadRaw, err = p.request("thread/resume", params)
	} else {
		threadRaw, err = p.request("thread/start", params)
	}
	if err != nil {
		return err
	}
	var thread struct {
		Thread struct{ ID, Model string } `json:"thread"`
		Model  string                     `json:"model"`
	}
	if json.Unmarshal(threadRaw, &thread) != nil || thread.Thread.ID == "" {
		return errors.New("Codex app-server returned an invalid thread")
	}
	p.mu.Lock()
	p.threadID, p.s.Session = thread.Thread.ID, thread.Thread.ID
	p.s.ObservedModel = firstNonEmpty(thread.Model, thread.Thread.Model)
	p.mu.Unlock()
	turn := map[string]any{"threadId": thread.Thread.ID, "input": []any{map[string]any{"type": "text", "text": p.o.Task}}, "cwd": p.o.Dir, "runtimeWorkspaceRoots": roots}
	if p.o.Native.JSONSchema != "" {
		var schema any
		_ = json.Unmarshal([]byte(p.o.Native.JSONSchema), &schema)
		turn["outputSchema"] = schema
	}
	turnRaw, err := p.request("turn/start", turn)
	if err != nil {
		return err
	}
	var started struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if json.Unmarshal(turnRaw, &started) != nil || started.Turn.ID == "" {
		return errors.New("Codex app-server returned an invalid turn")
	}
	p.mu.Lock()
	p.turnID = started.Turn.ID
	p.mu.Unlock()
	return nil
}

func appServerConfig(servers []NativeMCPServer) map[string]any {
	mcp := make(map[string]any, len(servers))
	for _, s := range servers {
		mcp[s.Name] = map[string]any{"command": s.Command, "args": s.Args, "required": true}
	}
	return map[string]any{"mcp_servers": mcp, "plugins": map[string]any{}, "apps": map[string]any{"_default": map[string]any{"enabled": false}}, "skills": map[string]any{"bundled": map[string]any{"enabled": false}}, "project_doc_max_bytes": 0, "allow_login_shell": false}
}

func validateAppServerSkills(raw json.RawMessage, roots []string) error {
	approved := make([]string, len(roots))
	for i, root := range roots {
		st, err := os.Stat(root)
		if err != nil || !st.IsDir() {
			return errors.New("approved Skill root must exist and be a directory")
		}
		approved[i], err = filepath.EvalSymlinks(root)
		if err != nil {
			return errors.New("approved Skill root cannot be resolved")
		}
	}
	var list struct {
		Data []struct {
			Errors []any
			Skills []struct {
				Enabled  bool
				Path     string
				PluginID json.RawMessage `json:"pluginId"`
			}
		}
	}
	if json.Unmarshal(raw, &list) != nil {
		return errors.New("Codex app-server returned an invalid Skills list")
	}
	for _, entry := range list.Data {
		if len(entry.Errors) > 0 {
			return errors.New("Codex app-server reported a Skill discovery error")
		}
		for _, skill := range entry.Skills {
			if !skill.Enabled {
				continue
			}
			if len(skill.PluginID) > 0 && string(skill.PluginID) != "null" {
				return errors.New("plugin-provided Skills are not allowed")
			}
			if !filepath.IsAbs(skill.Path) {
				return errors.New("enabled Skill path is not absolute")
			}
			path, err := filepath.EvalSymlinks(skill.Path)
			if err != nil {
				return errors.New("enabled Skill path is unreadable")
			}
			inside := false
			for _, root := range approved {
				rel, e := filepath.Rel(root, path)
				if e == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
					inside = true
					break
				}
			}
			if !inside {
				return fmt.Errorf("enabled Skill is outside approved roots: %s", skill.Path)
			}
		}
	}
	return nil
}

func (p *appServerParser) interruptAndWait(wait time.Duration) {
	p.mu.Lock()
	threadID, turnID := p.threadID, p.turnID
	p.mu.Unlock()
	if threadID == "" || turnID == "" {
		return
	}
	p.mu.Lock()
	p.nextID++
	id := p.nextID
	key := strconv.Itoa(id)
	response := make(chan appServerMessage, 1)
	p.pending[key] = response
	p.mu.Unlock()
	if err := p.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": "turn/interrupt", "params": map[string]any{"threadId": threadID, "turnId": turnID}}); err != nil {
		p.fail(err)
		return
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	select {
	case m := <-response:
		if len(m.Error) > 0 && string(m.Error) != "null" {
			p.fail(fmt.Errorf("Codex app-server turn/interrupt failed: %s", m.Error))
			return
		}
	case <-p.done:
		return
	case <-deadline.C:
		return
	}
	select {
	case <-p.done:
	case <-deadline.C:
	}
}
