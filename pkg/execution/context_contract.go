package execution

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
)

const ContextSchemaVersion = "1.0"

type RuntimeMode string

const (
	RuntimeClean   RuntimeMode = "clean"
	RuntimeAmbient RuntimeMode = "ambient"
)

// ContextEnvelope freezes AGH's authorization and assembly input. The caller
// resolves project, root and Skill references to trusted local paths before
// sealing. Instruction and task bytes travel separately in Request.
type ContextEnvelope struct {
	SchemaVersion        string                     `json:"schema_version"`
	EnvelopeID           string                     `json:"envelope_id"`
	CreatedAt            time.Time                  `json:"created_at"`
	ExpiresAt            time.Time                  `json:"expires_at"`
	TenantID             string                     `json:"tenant_id"`
	RunID                string                     `json:"run_id"`
	TaskID               string                     `json:"task_id"`
	Caller               string                     `json:"caller"`
	Subject              ContextSubject             `json:"subject"`
	Scope                []string                   `json:"scope"`
	Project              ContextProject             `json:"project"`
	Runtime              ContextRuntime             `json:"runtime"`
	ResourceRoots        []ContextRoot              `json:"resource_roots,omitempty"`
	KnowledgeManifest    KnowledgeManifest          `json:"knowledge_manifest"`
	TaskDigest           string                     `json:"task_digest"`
	InstructionsDigest   string                     `json:"instructions_digest"`
	PresetDocuments      []ContextDocument          `json:"preset_documents,omitempty"`
	Skills               []ContextSkill             `json:"skills,omitempty"`
	Tools                []ContextTool              `json:"tools,omitempty"`
	CredentialBindings   []CredentialBinding        `json:"credential_bindings,omitempty"`
	CheckpointRef        *CheckpointRef             `json:"checkpoint_ref,omitempty"`
	RequiredCapabilities []string                   `json:"required_capabilities,omitempty"`
	IdempotencyKey       string                     `json:"idempotency_key"`
	EnvelopeDigest       string                     `json:"envelope_digest,omitempty"`
	Extensions           map[string]json.RawMessage `json:"-"`
}

type ContextSubject struct {
	UserID   string   `json:"user_id"`
	AgentID  string   `json:"agent_id"`
	RoleIDs  []string `json:"role_ids,omitempty"`
	Audience string   `json:"audience"`
}

type ContextProject struct {
	PrimaryID    string `json:"primary_id"`
	WorkspaceRef string `json:"workspace_ref"`
	Revision     string `json:"revision,omitempty"`
}

type ContextRuntime struct {
	Mode             RuntimeMode       `json:"mode,omitempty"`
	Adapter          string            `json:"adapter"`
	Profile          string            `json:"profile"`
	Model            string            `json:"model,omitempty"`
	Executable       string            `json:"executable"`
	ExecutableDigest string            `json:"executable_digest"`
	AuthVersion      string            `json:"auth_version"`
	PermissionMode   string            `json:"permission_mode"`
	ToolsDisabled    bool              `json:"tools_disabled,omitempty"`
	NativeSessionRef *NativeSessionRef `json:"native_session_ref,omitempty"`
}

type NativeSessionRef struct {
	ID                 string `json:"id"`
	SourceRunID        string `json:"source_run_id"`
	IdempotencyKey     string `json:"idempotency_key"`
	RuntimeFingerprint string `json:"runtime_fingerprint"`
}

type ContextRoot struct {
	ID      string `json:"id"`
	PathRef string `json:"path_ref"`
	// "workspace" is a native working root. It does not assert OS containment.
	Access string `json:"access"`
	Digest string `json:"digest,omitempty"`
}

type KnowledgeManifest struct {
	GenerationID         string `json:"generation_id"`
	NoteSnapshotVersion  string `json:"note_snapshot_version"`
	AuthorizationVersion string `json:"authorization_version"`
	Digest               string `json:"digest"`
}

type ContextDocument struct {
	Name           string `json:"name"`
	ContentRef     string `json:"content_ref"`
	Digest         string `json:"digest"`
	Bytes          int    `json:"bytes"`
	Classification string `json:"classification"`
}

type ContextSkill struct {
	StableID string `json:"stable_id"`
	Revision string `json:"revision"`
	Digest   string `json:"digest"`
	MountRef string `json:"mount_ref"`
}

type ContextTool struct {
	ID               string `json:"id"`
	Version          string `json:"version"`
	CapabilityDigest string `json:"capability_digest"`
	PolicyRef        string `json:"policy_ref"`
}

// CredentialBinding carries only opaque metadata. The AGH broker owns tool
// authorization, redemption and lease recovery after crashes.
type CredentialBinding struct {
	BindingID         string    `json:"binding_id"`
	BindingVersion    string    `json:"binding_version"`
	PolicyFingerprint string    `json:"policy_fingerprint"`
	ToolID            string    `json:"tool_id"`
	TargetEnvironment string    `json:"target_environment"`
	Purpose           string    `json:"purpose"`
	RunID             string    `json:"run_id"`
	Delivery          string    `json:"delivery"`
	AllowedOperations []string  `json:"allowed_operations"`
	IssuedAt          time.Time `json:"issued_at"`
	NotAfter          time.Time `json:"not_after"`
	MaxRedemptions    int       `json:"max_redemptions"`
	CleanupRequired   bool      `json:"cleanup_required"`
	MetadataDigest    string    `json:"metadata_digest,omitempty"`
}

type CredentialReceipt struct {
	BindingID        string     `json:"binding_id"`
	BindingVersion   string     `json:"binding_version"`
	ToolID           string     `json:"tool_id"`
	LeaseState       string     `json:"lease_state"`
	RedeemedAt       *time.Time `json:"redeemed_at,omitempty"`
	ExpiredAt        *time.Time `json:"expired_at,omitempty"`
	CleanupState     string     `json:"cleanup_state"`
	CleanupErrorCode string     `json:"cleanup_error_code,omitempty"`
}

// ContextAuthority evaluates current grants, including explicit ambient
// permission and every checkpoint item. It must honor cancellation and must
// not mutate the input. Offload calls it again at the native launch boundary.
type ContextAuthority func(context.Context, ContextEnvelope, *TaskCheckpoint) error

// CredentialBroker installs a dedicated MCP endpoint for this Run and tool.
// Activate returns only an executable and non-secret arguments. The server
// authenticates at its tool boundary, redacts tool results before returning
// them to the model, and enforces TTL, revocation and redemption limits.
// Release is idempotent, including after partial activation. Implementations
// honor cancellation and recover leases after Offload process death.
type CredentialBroker interface {
	Activate(context.Context, CredentialBinding) (MCPServer, error)
	Release(context.Context, CredentialBinding) (LeaseOutcome, error)
}

type LeaseOutcome struct {
	State      string
	RedeemedAt *time.Time
}

// ContextExecution carries caller-owned services and resolved MCP configs.
// CodexAPIKey is a clean-mode provider credential passed only to the native
// child's environment. Tool credentials use the broker.
type ContextExecution struct {
	Envelope    ContextEnvelope
	Store       *ContextStore    `json:"-"`
	Authorize   ContextAuthority `json:"-"`
	Broker      CredentialBroker `json:"-"`
	MCPServers  []MCPServer      `json:"-"`
	CodexAPIKey string           `json:"-"`
}

type ContextReceipt struct {
	SchemaVersion             string              `json:"schema_version"`
	EnvelopeID                string              `json:"envelope_id"`
	RunID                     string              `json:"run_id"`
	AcceptedEnvelopeDigest    string              `json:"accepted_envelope_digest,omitempty"`
	RuntimeFingerprint        string              `json:"runtime_fingerprint,omitempty"`
	AdapterCapabilitiesDigest string              `json:"adapter_capabilities_digest"`
	Mode                      RuntimeMode         `json:"mode"`
	Workspace                 string              `json:"workspace,omitempty"`
	Ambient                   *AmbientSources     `json:"ambient,omitempty"`
	KnowledgeManifest         *KnowledgeManifest  `json:"knowledge_manifest,omitempty"`
	MountedRoots              []ContextRoot       `json:"mounted_roots,omitempty"`
	InjectedDocuments         []InjectedDocument  `json:"injected_documents,omitempty"`
	InstructionsDigest        string              `json:"instructions_digest,omitempty"`
	InstructionsBytes         int                 `json:"instructions_bytes"`
	MountedSkills             []MountedSkill      `json:"mounted_skills,omitempty"`
	ToolBindings              []ToolBinding       `json:"tool_bindings,omitempty"`
	CredentialReceipts        []CredentialReceipt `json:"credential_receipts,omitempty"`
	NativeSessionRef          *NativeSessionRef   `json:"native_session_ref,omitempty"`
	CheckpointRef             *CheckpointRef      `json:"checkpoint_ref,omitempty"`
	EvidenceRefs              []string            `json:"evidence_refs,omitempty"`
	StartedAt                 time.Time           `json:"started_at"`
	CompletedAt               time.Time           `json:"completed_at"`
	Status                    string              `json:"status"`
	ErrorCode                 string              `json:"error_code,omitempty"`
}

type InjectedDocument struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
	Bytes  int    `json:"bytes"`
}

type MountedSkill struct {
	StableID string `json:"stable_id"`
	Revision string `json:"revision"`
	Digest   string `json:"digest"`
	Path     string `json:"path"`
}

type ToolBinding struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

// AmbientSources describes inherited paths and environment classes without
// logging environment values.
type AmbientSources struct {
	Home               string   `json:"home"`
	NativeConfig       string   `json:"native_config"`
	SkillRoots         []string `json:"skill_roots"`
	EnvironmentClasses []string `json:"environment_classes"`
}

type AdapterCapabilities struct {
	Clean, Ambient, PresetDocuments, ResourceRoots, NativeResume, ExactSkills, MCP, CredentialBroker, Checkpoint bool
}

func ContextCapabilities(adapter string) AdapterCapabilities {
	switch adapter {
	case "claude", "codex":
		return AdapterCapabilities{
			Clean: true, Ambient: adapter == "claude", PresetDocuments: true, ResourceRoots: true,
			NativeResume: true, ExactSkills: adapter == "codex", MCP: true, CredentialBroker: true, Checkpoint: true,
		}
	default:
		return AdapterCapabilities{}
	}
}

type envelopeFields ContextEnvelope

// MarshalJSON preserves optional extension fields when forwarding an envelope.
func (e ContextEnvelope) MarshalJSON() ([]byte, error) {
	raw, err := json.Marshal(envelopeFields(e))
	if err != nil || len(e.Extensions) == 0 {
		return raw, err
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	known := knownEnvelopeFields()
	for key, value := range e.Extensions {
		if known[strings.ToLower(key)] {
			return nil, errors.New("context extension conflicts with a known field")
		}
		fields[key] = value
	}
	return json.Marshal(fields)
}

func (e *ContextEnvelope) UnmarshalJSON(raw []byte) error {
	var decoded envelopeFields
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	for key := range knownEnvelopeFields() {
		delete(fields, key)
	}
	*e = ContextEnvelope(decoded)
	if len(fields) != 0 {
		e.Extensions = fields
	}
	return nil
}

func knownEnvelopeFields() map[string]bool {
	const names = "schema_version envelope_id created_at expires_at tenant_id run_id task_id caller subject scope project runtime resource_roots knowledge_manifest task_digest instructions_digest preset_documents skills tools credential_bindings checkpoint_ref required_capabilities idempotency_key envelope_digest"
	fields := make(map[string]bool)
	for _, name := range strings.Fields(names) {
		fields[name] = true
	}
	return fields
}

func ContextContentDigest(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// ContextDigest hashes compact Go JSON with ordered object keys, preserving
// number tokens. Envelope digests omit the envelope_digest field.
func ContextDigest(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var canonical any
	if err := decoder.Decode(&canonical); err != nil {
		return "", err
	}
	raw, err = json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	return ContextContentDigest(raw), nil
}

func (e ContextEnvelope) Digest() (string, error) {
	e.EnvelopeDigest = ""
	return ContextDigest(e)
}

func (b CredentialBinding) Digest() (string, error) {
	b.MetadataDigest = ""
	return ContextDigest(b)
}

func readerDigest(r io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func validDigest(value string) bool {
	_, err := hex.DecodeString(value)
	return len(value) == 64 && err == nil && strings.ToLower(value) == value
}

func validContextVersion(version string) bool {
	major, minor, ok := strings.Cut(version, ".")
	return ok && major == "1" && minor != "" && strings.Trim(minor, "0123456789") == ""
}
