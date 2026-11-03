// Package nativeusage reads accounting evidence from native session files. It
// never invokes a model, edits a native file, or derives billing from text.
package nativeusage

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/zhoushoujianwork/offload/internal/history"
)

var sessionID = regexp.MustCompile(`^[a-fA-F0-9]{8}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{12}$`)

type segment struct {
	start, end                   time.Time
	prompt, cwd, model, tier, id string
	report                       string
	usage, cumulative            *history.TokenUsage
	cost, cumulativeCost         *float64
	complete, invalid            bool
}

type session struct {
	segments []segment
	digest   string
	err      error
}

// Reader caches only within one repair pass. Every worker fallback gets a fresh
// reader so a resumed session's new entries cannot be hidden by a stale cache.
type Reader struct {
	indexes  map[string]map[string][]string
	sessions map[string]session
}

func NewReader() *Reader {
	return &Reader{indexes: map[string]map[string][]string{}, sessions: map[string]session{}}
}

func home(backend string, r history.Record) string {
	if backend == "codex" {
		if r.CodexHome != "" {
			return r.CodexHome
		}
		if h := os.Getenv("CODEX_HOME"); h != "" {
			return h
		}
	} else {
		if r.ClaudeHome != "" {
			return r.ClaudeHome
		}
		if h := os.Getenv("CLAUDE_CONFIG_DIR"); h != "" {
			return h
		}
	}
	h, _ := os.UserHomeDir()
	if backend == "codex" {
		return filepath.Join(h, ".codex")
	}
	return filepath.Join(h, ".claude")
}

func (rd *Reader) find(backend string, r history.Record) (string, error) {
	root := filepath.Join(home(backend, r), "projects")
	if backend == "codex" {
		root = filepath.Join(home(backend, r), "sessions")
	}
	index, ok := rd.indexes[root]
	if !ok {
		index = map[string][]string{}
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			name := d.Name()
			if d.Type().IsRegular() && strings.HasSuffix(name, ".jsonl") && len(name) >= 42 {
				id := name[len(name)-42 : len(name)-6]
				if sessionID.MatchString(id) {
					index[id] = append(index[id], path)
				}
			}
			return nil
		})
		if err != nil {
			return "", errors.New("native log directory unavailable")
		}
		rd.indexes[root] = index
	}
	paths := index[r.Session]
	if len(paths) != 1 {
		return "", errors.New("native session missing or ambiguous")
	}
	return paths[0], nil
}

func Needed(r history.Record) bool {
	return r.Usage == nil || r.UsageScope == "partial_run" || r.CostUSD == nil || (r.Backend != "codex" && r.CumulativeCostUSD == nil)
}

// SamePromptOverlap rejects two history entries that could both claim the same
// native invocation. Distinct prompts still have independent native turn ids.
func SamePromptOverlap(a, b history.Record) bool {
	if a.Session == "" || a.Session != b.Session || strings.TrimSpace(a.Task) != strings.TrimSpace(b.Task) {
		return false
	}
	if a.Backend != "" && b.Backend != "" && a.Backend != b.Backend {
		return false
	}
	if a.CodexHome != "" && b.CodexHome != "" && a.CodexHome != b.CodexHome {
		return false
	}
	if a.ClaudeHome != "" && b.ClaudeHome != "" && a.ClaudeHome != b.ClaudeHome {
		return false
	}
	as, ok := history.ParseTS(a.TS, time.Local)
	if !ok {
		return false
	}
	bs, ok := history.ParseTS(b.TS, time.Local)
	if !ok {
		return false
	}
	if a.ElapsedMs == nil || b.ElapsedMs == nil {
		return as.Equal(bs)
	}
	const maxMS = int64((365 * 24 * time.Hour) / time.Millisecond)
	if *a.ElapsedMs < 0 || *b.ElapsedMs < 0 || *a.ElapsedMs > maxMS || *b.ElapsedMs > maxMS {
		return true
	}
	ae := as.Add(time.Duration(*a.ElapsedMs)*time.Millisecond + time.Second)
	be := bs.Add(time.Duration(*b.ElapsedMs)*time.Millisecond + time.Second)
	return as.Before(be) && bs.Before(ae)
}

// Recover requires a unique prompt, matching session, cwd and invocation window.
// It refuses to replace a conflicting measured value. Interrupted native records
// are retained as a lower bound (partial_run), never promoted to full-run totals.
func (rd *Reader) Recover(r history.Record) (history.Record, bool, string) {
	if !Needed(r) {
		return r, false, "already covered"
	}
	if r.ErrorKind == "configuration" || r.ErrorKind == "launch" {
		return r, false, "worker did not launch a native invocation"
	}
	if !sessionID.MatchString(r.Session) {
		return r, false, "no valid native session"
	}
	if r.Salvaged {
		return r, false, "salvage spans multiple attempts"
	}
	if r.Cleanup != "" {
		return r, false, "worker cleanup unresolved"
	}
	start, ok := history.ParseTS(r.TS, time.Local)
	if !ok || r.ElapsedMs == nil || *r.ElapsedMs < 0 || *r.ElapsedMs > int64((365*24*time.Hour)/time.Millisecond) {
		return r, false, "invocation window unavailable"
	}
	end := start.Add(time.Duration(*r.ElapsedMs)*time.Millisecond + time.Second)
	if r.WaitMs != nil && *r.WaitMs >= 0 && *r.WaitMs <= *r.ElapsedMs {
		// TS was saved at whole-second precision. Exclude queue wait at that
		// same precision, so another invocation while queued cannot be claimed.
		start = start.Add(time.Duration(*r.WaitMs) * time.Millisecond).Truncate(time.Second)
	}
	backends := []string{r.Backend}
	if r.Backend == "" {
		backends = []string{"codex", "claude"}
	}
	var found *segment
	var backend, digest string
	missingReason := "native session missing or ambiguous"
	sawFile, sawValid := false, false
	for _, b := range backends {
		if b != "codex" && b != "claude" {
			continue
		}
		path, err := rd.find(b, r)
		if err != nil {
			missingReason = err.Error()
			continue
		}
		sawFile = true
		s, cached := rd.sessions[path]
		if !cached {
			s = readSession(path, b, r.Session)
			rd.sessions[path] = s
		}
		if s.err != nil {
			continue
		}
		sawValid = true
		for i := range s.segments {
			v := &s.segments[i]
			if v.invalid || v.id == "" || v.usage == nil || v.cwd != r.Dir || v.start.Before(start) || v.start.After(end) {
				continue
			}
			task, prompt := strings.TrimSpace(r.Task), strings.TrimSpace(v.prompt)
			if task == "" || !(prompt == task || strings.HasSuffix(prompt, "\n"+task)) {
				continue
			}
			if !v.end.IsZero() && v.end.After(end) {
				continue
			}
			if v.complete && (v.end.IsZero() || v.end.Before(v.start)) {
				continue
			}
			if found != nil {
				return r, false, "ambiguous native invocation"
			}
			found, backend, digest = v, b, s.digest
		}
	}
	if found == nil {
		if !sawFile {
			return r, false, missingReason
		}
		if !sawValid {
			return r, false, "native session unreadable or corrupt"
		}
		return r, false, "no verified native invocation"
	}
	trustedCost := r.CostUSD != nil && (r.Backend == "codex" || r.CumulativeCostUSD != nil)
	// Older Claude logs have no cost-state footer. A successful stored terminal
	// report that exactly matches the last native assistant response also proves
	// the invocation ended; it does not supply a missing cost snapshot.
	complete := found.complete || (backend == "claude" && r.Status == "ok" && strings.TrimSpace(r.Result) != "" && strings.TrimSpace(r.Result) == strings.TrimSpace(found.report))
	if !complete && ((r.Usage != nil && r.UsageScope != "partial_run") || trustedCost) {
		return r, false, "native invocation incomplete"
	}
	if r.Usage != nil && r.UsageScope != "partial_run" && *r.Usage != *found.usage {
		return r, false, "native usage conflicts with recorded usage"
	}
	if found.cost != nil && r.CumulativeCostUSD != nil && r.CostUSD != nil && math.Abs(*found.cost-*r.CostUSD) > 1e-8 {
		return r, false, "native cost conflicts with recorded cost"
	}
	r.Backend = backend
	r.Usage, r.CumulativeUsage = found.usage, found.cumulative
	r.UsageScope, r.UsageSource = "run", "native_"+backend+"_session"
	r.AccountingEvidence = "sha256:" + digest + "; segment:" + found.id
	r.UsageUnavailable = ""
	if found.model != "" {
		r.Model, r.ModelSource = found.model, "native_session_turn"
		r.ModelUnavailable = ""
	}
	if found.tier != "" {
		r.ServiceTier = found.tier
	}
	if !complete {
		r.UsageScope, r.CostUSD, r.Pricing = "partial_run", nil, nil
		r.CostIsEstimate = false
		r.UsageUnavailable = "native log contains only observed usage; full-run usage unavailable"
		r.CostUnavailable = "native invocation did not finish; cost unavailable"
		return r, true, "partial native usage"
	}
	if backend == "codex" {
		if !trustedCost {
			r.CostUSD, r.Pricing = nil, nil
			history.EstimateCodexCost(&r)
		}
	} else if found.cost != nil {
		r.CostUSD, r.CumulativeCostUSD = found.cost, found.cumulativeCost
		r.CostBasis, r.CostUnavailable = "native_worker_estimate", ""
	} else if r.CumulativeCostUSD == nil {
		// A legacy value might be a session total. Never keep it as a run cost.
		r.CostUSD = nil
		r.CostUnavailable = "native cost snapshots do not verify this invocation's cost"
	}
	r.CostIsEstimate = r.CostUSD != nil
	return r, true, "recovered"
}

func readSession(path, backend, id string) session {
	f, err := os.Open(path)
	if err != nil {
		return session{err: err}
	}
	defer f.Close()
	p := parser{backend: backend, session: id, messages: map[string]messageUsage{}}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 64<<20)
	hash := sha256.New()
	for sc.Scan() {
		line := sc.Bytes()
		hash.Write(line)
		hash.Write([]byte{'\n'})
		if err := p.line(line); err != nil {
			return session{err: err}
		}
	}
	if err := sc.Err(); err != nil {
		return session{err: err}
	}
	p.finish()
	if !p.verified {
		return session{err: errors.New("native session identity unavailable")}
	}
	return session{segments: p.segments, digest: hex.EncodeToString(hash.Sum(nil))}
}

type messageUsage struct {
	usage history.TokenUsage
	model string
}
type parser struct {
	backend, session string
	verified         bool
	segments         []segment
	current          *segment
	messages         map[string]messageUsage
	baseUsage        *history.TokenUsage
	baseCost         *float64
	nextUsage        *history.TokenUsage
	nextCost         *float64
	lastTime         time.Time
}

func (p *parser) finish() {
	if p.current == nil {
		return
	}
	if p.backend == "claude" {
		var sum history.TokenUsage
		for _, m := range p.messages {
			if !add(&sum, m.usage) {
				p.current.invalid = true
				break
			}
			if p.current.model == "" {
				p.current.model = m.model
			} else if p.current.model != m.model {
				p.current.model = "mixed"
			}
		}
		if len(p.messages) > 0 {
			p.current.usage = &sum
		}
	}
	p.segments = append(p.segments, *p.current)
	p.current = nil
	p.messages = map[string]messageUsage{}
}

func add(a *history.TokenUsage, b history.TokenUsage) bool {
	for _, pair := range [][2]*int64{{&a.Input, &b.Input}, {&a.Cached, &b.Cached}, {&a.CacheWrite, &b.CacheWrite}, {&a.Output, &b.Output}} {
		if *pair[1] < 0 || *pair[0] > math.MaxInt64-*pair[1] {
			return false
		}
		*pair[0] += *pair[1]
	}
	return a.Valid()
}

func counters(raw json.RawMessage, claude bool) *history.TokenUsage {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	read := func(name string, required bool) (*int64, bool) {
		v, ok := m[name]
		if !ok && !required {
			z := int64(0)
			return &z, true
		}
		var n *int64
		if json.Unmarshal(v, &n) != nil || n == nil || *n < 0 {
			return nil, false
		}
		return n, true
	}
	i, a := read("input_tokens", true)
	o, b := read("output_tokens", true)
	cacheKey, writeKey := "cached_input_tokens", "cache_write_input_tokens"
	if claude {
		cacheKey, writeKey = "cache_read_input_tokens", "cache_creation_input_tokens"
	}
	c, d := read(cacheKey, !claude)
	w, e := read(writeKey, false)
	if !a || !b || !d || !e {
		return nil
	}
	u := &history.TokenUsage{Input: *i, Cached: *c, CacheWrite: *w, Output: *o}
	if claude {
		if *i > math.MaxInt64-*c || *i+*c > math.MaxInt64-*w {
			return nil
		}
		u.Input = *i + *c + *w
	}
	if !u.Valid() {
		return nil
	}
	return u
}

func (p *parser) line(raw []byte) error {
	if p.backend == "codex" {
		return p.codex(raw)
	}
	return p.claude(raw)
}
