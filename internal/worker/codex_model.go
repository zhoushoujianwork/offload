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

func codexHome(scoped ...string) string {
	if len(scoped) > 0 && scoped[0] != "" {
		return scoped[0]
	}
	if home := os.Getenv("CODEX_HOME"); home != "" {
		return home
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex")
}

func resolveCodexModel(rec *history.Record, explicit string, scoped ...string) {
	rec.ModelUnavailable = ""
	if explicit != "" {
		rec.Model, rec.ModelSource = explicit, "profile"
		return
	}
	if model := codexSessionModel(rec.Session, scoped...); model != "" {
		rec.Model, rec.ModelSource = model, "session"
		return
	}
	if model := codexConfigModel(scoped...); model != "" {
		rec.Model, rec.ModelSource = model, "config"
		return
	}
	rec.Model, rec.ModelSource = "", "unknown"
	rec.ModelUnavailable = "no explicit model; session model missing or unreadable; config model missing, unreadable or unsupported"
}

func codexSessionModel(session string, scoped ...string) string {
	path, err := codexRollout(session, scoped...)
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

// Read only the root model, excluding nested provider and profile settings.
var codexModelSetting = regexp.MustCompile(`^(?:model|"model"|'model')\s*=\s*(.*)$`)

func codexConfigModel(scoped ...string) string {
	home := codexHome(scoped...)
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
	multiline := ""
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		inMultiline := multiline != ""
		multiline = codexConfigMultiline(line, multiline)
		if inMultiline {
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			break
		}
		m := codexModelSetting.FindStringSubmatch(strings.TrimLeft(sc.Text(), " \t"))
		if m == nil {
			continue
		}
		if model != "" {
			return ""
		}
		value := m[1]
		for multiline != "" && sc.Scan() {
			line := sc.Text()
			value += "\n" + line
			multiline = codexConfigMultiline(line, multiline)
		}
		var ok bool
		model, ok = codexConfigString(value)
		if !ok {
			return ""
		}
	}
	if sc.Err() != nil || multiline != "" {
		return ""
	}
	return model
}

// Decode TOML basic and literal strings, including multiline forms and continuations.
func codexConfigString(value string) (string, bool) {
	if len(value) == 0 || (value[0] != '"' && value[0] != '\'') {
		return "", false
	}
	quote := value[0]
	delimiter := string(quote)
	multiline := strings.HasPrefix(value, strings.Repeat(delimiter, 3))
	if multiline {
		delimiter = strings.Repeat(delimiter, 3)
	}
	value = value[len(delimiter):]
	if multiline {
		value = strings.TrimPrefix(value, "\n")
	}
	var result strings.Builder
	for i := 0; i < len(value); {
		if strings.HasPrefix(value[i:], delimiter) {
			i += len(delimiter)
			if multiline {
				for n := 0; n < 2 && i < len(value) && value[i] == quote; n++ {
					result.WriteByte(quote)
					i++
				}
			}
			rest := strings.TrimSpace(value[i:])
			return result.String(), rest == "" || strings.HasPrefix(rest, "#")
		}
		if quote == '"' && value[i] == '\\' {
			start := i
			i++
			if i == len(value) {
				return "", false
			}
			if multiline {
				end := i
				for end < len(value) && (value[end] == ' ' || value[end] == '\t') {
					end++
				}
				if end < len(value) && value[end] == '\n' {
					i = end + 1
					for i < len(value) && strings.ContainsRune(" \t\n", rune(value[i])) {
						i++
					}
					continue
				}
			}
			switch value[i] {
			case 'b', 't', 'n', 'f', 'r', '"', '\\':
				i++
			case 'u':
				i += 5
			case 'U':
				i += 9
			default:
				return "", false
			}
			if i > len(value) {
				return "", false
			}
			decoded, err := strconv.Unquote(`"` + value[start:i] + `"`)
			if err != nil {
				return "", false
			}
			result.WriteString(decoded)
			continue
		}
		if value[i] < 0x20 && value[i] != '\t' && !(multiline && value[i] == '\n') {
			return "", false
		}
		result.WriteByte(value[i])
		i++
	}
	return "", false
}

func codexConfigMultiline(line, delimiter string) string {
	for i := 0; i < len(line); {
		if delimiter != "" {
			if delimiter == `"""` && line[i] == '\\' {
				i += 2
			} else if strings.HasPrefix(line[i:], delimiter) {
				quote := delimiter[0]
				i += 3
				for i < len(line) && line[i] == quote {
					i++
				}
				delimiter = ""
			} else {
				i++
			}
			continue
		}
		switch line[i] {
		case '#':
			return ""
		case '\'', '"':
			quote := line[i]
			if i+2 < len(line) && line[i+1] == quote && line[i+2] == quote {
				delimiter = line[i : i+3]
				i += 3
				continue
			}
			i++
			for i < len(line) {
				if quote == '"' && line[i] == '\\' {
					i += 2
				} else if line[i] == quote {
					i++
					break
				} else {
					i++
				}
			}
		default:
			i++
		}
	}
	return delimiter
}

// Native authentication belongs to Codex; inspect only the authentication mode,
// never copy credentials into the run record or diagnostics.
func codexSubscription(scoped ...string) bool {
	home := codexHome(scoped...)
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
