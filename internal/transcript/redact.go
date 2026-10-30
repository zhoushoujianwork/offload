package transcript

import (
	"encoding/json"
	"regexp"
	"strings"
	"sync"
)

// Redacted replaces every removed secret.
const Redacted = "[REDACTED]"

// minSecretLen keeps short or empty configured values from blanking ordinary text.
const minSecretLen = 8

// secretPatterns match credential shapes that offload does not know by value. The list is
// deliberately small and conservative: redaction is best effort, and the documentation says so.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?(?:-----END [A-Z ]*PRIVATE KEY-----|$)`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}`),
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}`),
	regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`),
	regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`),
	regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{16,}`),
}

// assignment matches `name=value` and `name: value` for credential-like names. Only the value
// is replaced.
var assignment = regexp.MustCompile(`(?i)\b((?:[a-z0-9_-]*(?:api[_-]?key|token|secret|passw(?:or)?d|authorization))["']?\s*[:=]\s*["']?)([^\s"'<>,;]{8,})`)

// Redactor removes known credential values and common credential shapes from text.
type Redactor struct {
	mu      sync.RWMutex
	secrets []string // literal values and their JSON-escaped forms
}

// NewRedactor returns a redactor that removes the given literal values. Values shorter than
// eight bytes are ignored.
func NewRedactor(values ...string) *Redactor {
	r := &Redactor{}
	r.Add(values...)
	return r
}

// Add registers more literal values.
func (r *Redactor) Add(values ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, v := range values {
		v = strings.TrimSpace(v)
		if len(v) < minSecretLen {
			continue
		}
		r.secrets = appendUnique(r.secrets, v)
		if b, err := json.Marshal(v); err == nil && len(b) > 2 {
			r.secrets = appendUnique(r.secrets, string(b[1:len(b)-1]))
		}
	}
}

func appendUnique(list []string, v string) []string {
	for _, s := range list {
		if s == v {
			return list
		}
	}
	return append(list, v)
}

// Apply returns s with credentials removed and the number of replacements. It must run before any
// truncation so that a secret cut in half is never left behind.
func (r *Redactor) Apply(s string) (string, int) {
	if r == nil || s == "" {
		return s, 0
	}
	n := 0
	r.mu.RLock()
	for _, secret := range r.secrets {
		if c := strings.Count(s, secret); c > 0 {
			s = strings.ReplaceAll(s, secret, Redacted)
			n += c
		}
	}
	r.mu.RUnlock()
	for _, re := range secretPatterns {
		s = re.ReplaceAllStringFunc(s, func(string) string { n++; return Redacted })
	}
	s = assignment.ReplaceAllStringFunc(s, func(m string) string {
		sub := assignment.FindStringSubmatch(m)
		if sub[2] == Redacted || strings.HasPrefix(sub[2], "[REDACTED") {
			return m
		}
		n++
		return sub[1] + Redacted
	})
	return s, n
}

// EnvSecrets returns the values of environment variables whose names look like credentials. The
// worker inherits (Codex) or is given (Claude) credentials through its environment, so these are
// the values most likely to be echoed by a command the worker runs.
func EnvSecrets(env []string) []string {
	var out []string
	for _, kv := range env {
		name, value, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		u := strings.ToUpper(name)
		for _, w := range []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "PASSWD", "CREDENTIAL"} {
			if strings.Contains(u, w) {
				out = append(out, value)
				break
			}
		}
	}
	return out
}
