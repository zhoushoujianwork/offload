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
Nobody can answer interactive prompts. offload imposes no command, file or network restriction on you; the task text and the supervisor's authorization define what you may do, so stay inside that scope. The host operating system, network and credentials still apply and may make a step fail; report such a failure instead of looping on it.`

func codexBin() (string, error) {
	if b := os.Getenv("OFFLOAD_CODEX"); b != "" {
		return b, nil
	}
	return exec.LookPath("codex")
}

func buildCodexArgs(o Opts) []string {
	// The explicit bypass works for both exec and exec resume. Write is task intent only.
	args := []string{"exec", "--dangerously-bypass-approvals-and-sandbox"}
	if o.Profile.ReasoningEffort != "" {
		args = append(args, "-c", "model_reasoning_effort="+strconv.Quote(o.Profile.ReasoningEffort))
	}
	if o.Profile.ServiceTier != "" {
		args = append(args, "-c", "service_tier="+strconv.Quote(o.Profile.ServiceTier))
	}
	// exec accepts --add-dir only before the resume subcommand.
	for _, d := range o.AddDirs {
		args = append(args, "--add-dir", d)
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
// Usage is session-cumulative; run accounting is applied after the process exits.
func parseCodexStream(out []byte) streamSummary {
	var s streamSummary
	var report string
	seen := make(map[string]bool)
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var e struct {
			Type     string          `json:"type"`
			Usage    json.RawMessage `json:"usage"`
			ThreadID string          `json:"thread_id"`
			Message  string          `json:"message"`
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
		case "turn.started":
			s.Result, report = nil, ""
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
			s.Result.CumulativeUsage = decodeCodexUsage(e.Usage)
			if s.Result.CumulativeUsage == nil {
				s.Result.UsageUnavailable = "missing or invalid terminal usage"
			}
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
