package worker

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/zhoushoujianwork/offload/internal/history"
)

func codexHome() string {
	if home := os.Getenv("CODEX_HOME"); home != "" {
		return home
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex")
}

func resolveCodexModel(rec *history.Record, explicit string) {
	rec.ModelUnavailable = ""
	if explicit != "" {
		rec.Model, rec.ModelSource = explicit, "profile"
		return
	}
	if model := codexSessionModel(rec.Session); model != "" {
		rec.Model, rec.ModelSource = model, "session"
		return
	}
	if model := codexConfigModel(); model != "" {
		rec.Model, rec.ModelSource = model, "config"
		return
	}
	rec.Model, rec.ModelSource = "", "unknown"
	rec.ModelUnavailable = "no explicit model; session model missing or unreadable; config model missing, unreadable or unsupported"
}

func codexSessionModel(session string) string {
	path, err := codexRollout(session)
	if err != nil {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	verified, model := false, ""
	for sc.Scan() {
		var e struct {
			Type    string `json:"type"`
			Payload struct {
				ID    string `json:"id"`
				Model string `json:"model"`
			} `json:"payload"`
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			return ""
		}
		switch e.Type {
		case "session_meta":
			if e.Payload.ID != session {
				return ""
			}
			verified = true
		case "turn_context":
			if e.Payload.Model != "" {
				model = e.Payload.Model
			}
		}
	}
	if sc.Err() != nil || !verified {
		return ""
	}
	return model
}

// Read only a root-level single-line TOML string. Never interpret nested provider
// or profile settings as the default model. Unsupported syntax stays unknown.
var codexModelSetting = regexp.MustCompile(`^(?:model|"model"|'model')\s*=\s*("(?:[^"\\]|\\.)*"|'[^']*')\s*(?:#.*)?$`)

func codexConfigModel() string {
	home := codexHome()
	if home == "" {
		return ""
	}
	f, err := os.Open(filepath.Join(home, "config.toml"))
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	model := ""
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			break
		}
		// Avoid reading apparent keys inside multiline string values.
		if strings.Contains(line, `"""`) || strings.Contains(line, "'''") {
			return ""
		}
		m := codexModelSetting.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if model != "" {
			return ""
		}
		if m[1][0] == '\'' {
			model = m[1][1 : len(m[1])-1]
		} else {
			model, err = strconv.Unquote(m[1])
			if err != nil {
				return ""
			}
		}
	}
	if sc.Err() != nil {
		return ""
	}
	return model
}

// Native authentication belongs to Codex; inspect only the authentication mode,
// never copy credentials into the run record or diagnostics.
func codexSubscription() bool {
	home := codexHome()
	if home == "" {
		return false
	}
	b, err := os.ReadFile(filepath.Join(home, "auth.json"))
	if err != nil {
		return false
	}
	var auth struct {
		Mode   string          `json:"auth_mode"`
		Tokens json.RawMessage `json:"tokens"`
		APIKey string          `json:"OPENAI_API_KEY"`
	}
	if json.Unmarshal(b, &auth) != nil {
		return false
	}
	return auth.Mode == "chatgpt" || (auth.Mode == "" && auth.APIKey == "" && len(auth.Tokens) > 0 && string(auth.Tokens) != "null")
}
