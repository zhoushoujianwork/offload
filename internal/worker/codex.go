package worker

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/zhoushoujianwork/offload/internal/text"
)

const codexPrompt = `You are a worker agent delegated a task by a supervising agent. Work autonomously; do not wait for interactive answers. Report blockers to the supervisor.
Complete the assigned task directly. Do not launch offload or delegate to other workers.
Finish with a concise report: what you did or found, exact file:line references, commands run with their results, and anything unverified. No preamble.
Do not commit, push, or change unrelated files. Nobody can approve permission prompts; work within the configured sandbox.`

func codexBin() (string, error) {
	if b := os.Getenv("OFFLOAD_CODEX"); b != "" {
		return b, nil
	}
	return exec.LookPath("codex")
}

func buildCodexArgs(o Opts) []string {
	sandbox := "read-only"
	if o.Write {
		sandbox = "workspace-write"
	}
	// Config overrides also work with exec resume, which has no --sandbox flag.
	args := []string{"exec", "-c", `approval_policy="never"`, "-c", `sandbox_mode="` + sandbox + `"`}
	if o.Profile.ReasoningEffort != "" {
		args = append(args, "-c", "model_reasoning_effort="+strconv.Quote(o.Profile.ReasoningEffort))
	}
	if o.Profile.ServiceTier != "" {
		args = append(args, "-c", "service_tier="+strconv.Quote(o.Profile.ServiceTier))
	}
	if o.Resume != "" {
		args = append(args, "resume")
	}
	args = append(args, "--json", "--skip-git-repo-check")
	if o.Model != "" {
		args = append(args, "-m", o.Model)
	}
	if o.Resume != "" {
		args = append(args, o.Resume)
	}
	return append(args, "-") // instructions and task go through stdin, never argv
}

// parseCodexStream normalizes Codex JSONL events into the shared progress/result model.
// Codex reports token usage, not a USD cost; leave cost unavailable.
func parseCodexStream(out []byte) streamSummary {
	var s streamSummary
	var report string
	seen := make(map[string]bool)
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var e struct {
			Type     string `json:"type"`
			ThreadID string `json:"thread_id"`
			Message  string `json:"message"`
			Error    struct {
				Message string `json:"message"`
			} `json:"error"`
			Item struct {
				ID      string `json:"id"`
				Type    string `json:"type"`
				Text    string `json:"text"`
				Command string `json:"command"`
				Tool    string `json:"tool"`
			} `json:"item"`
		}
		if dec.Decode(&e) != nil {
			break
		}
		s.Events++
		if e.ThreadID != "" {
			s.Session = e.ThreadID
		}
		switch e.Type {
		case "item.started", "item.completed":
			if e.Item.Type == "agent_message" && e.Type == "item.completed" {
				s.LastText = e.Item.Text
				report = e.Item.Text
			}
			switch e.Item.Type {
			case "command_execution", "mcp_tool_call", "file_change", "web_search":
				if !seen[e.Item.ID] {
					seen[e.Item.ID] = true
					s.ToolCalls++
					s.LastTools = append(s.LastTools, text.Truncate(text.OneLine(strings.TrimSpace(e.Item.Type+" "+e.Item.Command+" "+e.Item.Tool)), 120))
					if len(s.LastTools) > lastToolsKept {
						s.LastTools = s.LastTools[1:]
					}
				}
			}
		case "turn.completed":
			s.Result = &claudeResult{Result: report, SessionID: s.Session, NumTurns: s.ToolCalls}
			if strings.TrimSpace(report) == "" {
				s.Result.IsError = true
				s.Result.Errors = []string{"Codex completed without a report"}
			}
		case "error":
			// Error events can be transient (e.g. a reconnect); only turn.failed is terminal.
			if e.Message != "" {
				s.LastText = e.Message
			}
		case "turn.failed":
			message := firstNonEmpty(e.Error.Message, "Codex turn failed")
			s.Result = &claudeResult{IsError: true, Result: message, Errors: []string{message}, SessionID: s.Session, NumTurns: s.ToolCalls}
		}
	}
	return s
}
