package execution

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"
)

// ContextStore is an owner-private local journal. Its entries contain receipts,
// reference-only checkpoints and clean native runtime state. Share a store
// between client processes that dispatch the same task. Retention belongs to
// the caller, including removal of any expired native runtime directories.
type ContextStore struct {
	root string
}

func OpenContextStore(root string) (*ContextStore, error) {
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil || !filepath.IsAbs(root) || canonical != root {
		return nil, errors.New("context store requires a canonical absolute directory")
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("context store directory must be private")
	}
	return &ContextStore{root: root}, nil
}

type CheckpointRef struct {
	ID     string `json:"id"`
	Digest string `json:"digest"`
}

type ConfirmedStep struct {
	ID          string `json:"id"`
	EvidenceRef string `json:"evidence_ref"`
}

// TaskCheckpoint contains caller-confirmed facts only as references. Pending
// steps include uncertain outcomes; native output cannot complete a step.
type TaskCheckpoint struct {
	SchemaVersion        string          `json:"schema_version"`
	CheckpointID         string          `json:"checkpoint_id"`
	TenantID             string          `json:"tenant_id"`
	TaskID               string          `json:"task_id"`
	RunID                string          `json:"run_id"`
	ProducerAgentID      string          `json:"producer_agent_id"`
	Audience             string          `json:"audience"`
	ProjectID            string          `json:"project_id"`
	EnvelopeDigest       string          `json:"envelope_digest"`
	AuthorizationVersion string          `json:"authorization_version"`
	CompletedSteps       []ConfirmedStep `json:"completed_steps,omitempty"`
	PendingSteps         []string        `json:"pending_steps,omitempty"`
	ArtifactRefs         []string        `json:"artifact_refs,omitempty"`
	EvidenceRefs         []string        `json:"evidence_refs,omitempty"`
	ToolResultRefs       []string        `json:"tool_result_refs,omitempty"`
	IdempotencyState     []string        `json:"idempotency_state,omitempty"`
	CreatedAt            time.Time       `json:"created_at"`
	ExpiresAt            time.Time       `json:"expires_at"`
}

// SaveCheckpoint publishes caller-confirmed references against a finished
// dispatch. Repeating the exact write returns the same reference. No data from
// the provider report or native transcript is promoted by this method.
func (s *ContextStore) SaveCheckpoint(source SourceRun, checkpoint TaskCheckpoint) (CheckpointRef, error) {
	var empty CheckpointRef
	entry := s.entry(source.TenantID, source.TaskID, source.IdempotencyKey)
	var claim contextClaim
	var done contextCompletion
	if err := readContextFile(filepath.Join(entry, "claim.json"), &claim); err != nil {
		return empty, errors.New("checkpoint source is missing")
	}
	if err := readContextFile(filepath.Join(entry, "receipt.json"), &done); err != nil || done.Receipt.AcceptedEnvelopeDigest == "" {
		return empty, errors.New("checkpoint source has no accepted execution receipt")
	}
	if !validContextVersion(checkpoint.SchemaVersion) || !validIDs(checkpoint.CheckpointID, source.RunID) ||
		checkpoint.TenantID != source.TenantID || checkpoint.TaskID != source.TaskID ||
		checkpoint.RunID != source.RunID || source.RunID != claim.RunID ||
		checkpoint.ProducerAgentID != claim.AgentID || checkpoint.Audience != claim.Audience ||
		checkpoint.ProjectID != claim.ProjectID || checkpoint.EnvelopeDigest != claim.EnvelopeDigest ||
		checkpoint.AuthorizationVersion != claim.AuthorizationVersion ||
		checkpoint.CreatedAt.Before(done.Receipt.CompletedAt) || checkpoint.CreatedAt.After(time.Now()) ||
		!checkpoint.ExpiresAt.After(checkpoint.CreatedAt) {
		return empty, errors.New("checkpoint provenance or lifetime does not match its source")
	}
	if err := validateCheckpointRefs(checkpoint); err != nil {
		return empty, err
	}
	digest, err := ContextDigest(checkpoint)
	if err != nil {
		return empty, err
	}
	path := s.checkpointPath(source.TenantID, source.TaskID, checkpoint.CheckpointID)
	if err := publishContextFile(path, checkpoint); errors.Is(err, os.ErrExist) {
		existing, readErr := s.loadCheckpoint(source.TenantID, source.TaskID, CheckpointRef{checkpoint.CheckpointID, digest})
		if readErr != nil || existing == nil {
			return empty, errors.New("checkpoint ID already holds different content")
		}
	} else if err != nil {
		return empty, err
	}
	return CheckpointRef{checkpoint.CheckpointID, digest}, nil
}

type SourceRun struct {
	TenantID, TaskID, RunID, IdempotencyKey string
}

func validateCheckpointRefs(cp TaskCheckpoint) error {
	seen := make(map[string]bool)
	for _, step := range cp.CompletedSteps {
		if !validIDs(step.ID, step.EvidenceRef) || seen[step.ID] {
			return errors.New("invalid or duplicate confirmed checkpoint step")
		}
		seen[step.ID] = true
	}
	for _, id := range cp.PendingSteps {
		if !validIDs(id) || seen[id] {
			return errors.New("invalid or duplicate pending checkpoint step")
		}
		seen[id] = true
	}
	for _, refs := range [][]string{cp.ArtifactRefs, cp.EvidenceRefs, cp.ToolResultRefs, cp.IdempotencyState} {
		if !validIDs(refs...) {
			return errors.New("invalid checkpoint reference")
		}
	}
	return nil
}

func (s *ContextStore) loadCheckpoint(tenant, task string, ref CheckpointRef) (*TaskCheckpoint, error) {
	var cp TaskCheckpoint
	if err := readContextFile(s.checkpointPath(tenant, task, ref.ID), &cp); err != nil {
		return nil, err
	}
	digest, err := ContextDigest(cp)
	if err != nil || digest != ref.Digest || cp.TenantID != tenant || cp.TaskID != task || cp.CheckpointID != ref.ID {
		return nil, errors.New("checkpoint content changed")
	}
	return &cp, nil
}

type contextClaim struct {
	RunID, EnvelopeDigest, AgentID, Audience, ProjectID, AuthorizationVersion string
}

type contextCompletion struct {
	Receipt     ContextReceipt
	Environment string
}

var errRunClaimed = errors.New("run ID already has a dispatch claim")

func (s *ContextStore) entry(tenant, task, key string) string {
	digest, _ := ContextDigest([]string{tenant, task, key})
	return filepath.Join(s.root, "dispatch-"+digest)
}

func (s *ContextStore) checkpointPath(tenant, task, id string) string {
	digest, _ := ContextDigest([]string{tenant, task, id})
	return filepath.Join(s.root, "checkpoint-"+digest+".json")
}

// claim is exclusive across processes. A missing terminal receipt after an
// interruption means the outcome is unknown and must never be replayed.
func (s *ContextStore) claim(e ContextEnvelope, entry string) error {
	if err := os.Mkdir(entry, 0700); err != nil {
		return err
	}
	runDigest, _ := ContextDigest([]string{e.TenantID, e.RunID})
	if err := publishContextFile(filepath.Join(s.root, "run-"+runDigest+".json"), e.EnvelopeDigest); err != nil {
		if errors.Is(err, os.ErrExist) {
			_ = os.Remove(entry)
			return errRunClaimed
		}
		return err
	}
	return publishContextFile(filepath.Join(entry, "claim.json"), contextClaim{
		RunID: e.RunID, EnvelopeDigest: e.EnvelopeDigest, AgentID: e.Subject.AgentID,
		Audience: e.Subject.Audience, ProjectID: e.Project.PrimaryID,
		AuthorizationVersion: e.KnowledgeManifest.AuthorizationVersion,
	})
}

const contextFileLimit = 1 << 20

func publishContextFile(path string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil || len(raw) > contextFileLimit {
		return errors.New("context journal record exceeds encoding limits")
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".record-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, writeErr := file.Write(raw)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return err
	}
	if err := os.Link(file.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func readContextFile(path string, dest any) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > contextFileLimit {
		return errors.New("invalid context journal file")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	actual, err := file.Stat()
	if err != nil || !os.SameFile(info, actual) {
		return errors.New("context journal file changed")
	}
	raw, err := io.ReadAll(io.LimitReader(file, contextFileLimit+1))
	if err != nil || len(raw) > contextFileLimit {
		return errors.New("cannot read context journal file")
	}
	return json.Unmarshal(raw, dest)
}

func validIDs(ids ...string) bool {
	for _, id := range ids {
		if !runIDPattern.MatchString(id) {
			return false
		}
	}
	return true
}
