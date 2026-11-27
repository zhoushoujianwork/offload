package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

type activeContextRun struct {
	mode                    RuntimeMode
	executable, codexAPIKey string
	authorize               func(context.Context) error
	dispatched, injected    atomic.Bool
}

type contextPlan struct {
	mode         RuntimeMode
	roots        []ContextRoot
	instructions []byte
	native       *NativeControls
	ambient      *AmbientSources
	fingerprint  string
}

func (c *Client) runContext(ctx context.Context, req Request) (result Result, resultErr error) {
	services := *req.Context
	e, err := snapshotEnvelope(services.Envelope)
	mode := e.Runtime.Mode
	if mode == "" {
		mode = RuntimeClean
	}
	capabilities := ContextCapabilities(c.profile.BackendName())
	capabilityDigest, _ := ContextDigest(capabilities)
	receipt := &ContextReceipt{
		SchemaVersion: ContextSchemaVersion, EnvelopeID: e.EnvelopeID, RunID: e.RunID,
		AdapterCapabilitiesDigest: capabilityDigest, Mode: mode, StartedAt: time.Now().UTC(),
	}
	fail := func(kind, message string) (Result, error) {
		return Result{RunID: e.RunID, Profile: c.cfg.ProfileName, Backend: c.profile.BackendName(),
				Status: "error", ErrorKind: kind, Error: message, ExitCode: 2},
			&RunError{Kind: kind, Status: "error", ExitCode: 2, Cause: errors.New(message)}
	}
	defer func() {
		if result.ContextReceipt == nil {
			finishContextReceipt(receipt, result, "rejected")
			result.ContextReceipt = receipt
		}
	}()
	if err != nil || services.Store == nil || services.Store.root == "" || services.Authorize == nil {
		return fail("context_invalid", "context requires a valid envelope, journal and authorization callback")
	}
	plan, kind, err := c.planContext(req, e, mode, capabilities, services)
	if err != nil {
		return fail(kind, err.Error())
	}
	store := services.Store
	var checkpoint *TaskCheckpoint
	if e.CheckpointRef != nil {
		checkpoint, err = store.loadCheckpoint(e.TenantID, e.TaskID, *e.CheckpointRef)
		if err != nil {
			return fail("checkpoint_invalid", "checkpoint reference is missing or has changed")
		}
	}
	deadline := e.ExpiresAt
	if checkpoint != nil && checkpoint.ExpiresAt.Before(deadline) {
		deadline = checkpoint.ExpiresAt
	}
	for _, binding := range e.CredentialBindings {
		if binding.NotAfter.Before(deadline) {
			deadline = binding.NotAfter
		}
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	authorize := func(ctx context.Context) error {
		if ctx.Err() != nil {
			return errors.New("managed context is canceled or expired")
		}
		if err := services.Authorize(ctx, e, checkpoint); err != nil || ctx.Err() != nil {
			return errors.New("current context authorization was denied")
		}
		return nil
	}
	if err := authorize(ctx); err != nil {
		return fail("context_denied", err.Error())
	}
	entry := store.entry(e.TenantID, e.TaskID, e.IdempotencyKey)
	if _, err := os.Lstat(entry); err == nil {
		return readContextRetry(entry, e, receipt)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fail("context_journal", "cannot inspect the dispatch journal")
	}
	environment, sourceEntry, err := store.contextEnvironment(e, plan, entry)
	if err != nil {
		return fail("session_incompatible", err.Error())
	}
	if err := store.claim(e, entry); err != nil {
		if errors.Is(err, errRunClaimed) {
			return fail("idempotency_conflict", err.Error())
		}
		if _, statErr := os.Lstat(entry); statErr != nil {
			return fail("context_journal", "cannot create the dispatch journal claim")
		}
		return readContextRetry(entry, e, receipt)
	}
	active := &activeContextRun{mode: mode, executable: e.Runtime.Executable, codexAPIKey: services.CodexAPIKey}
	nativeCompleted := false
	receipt.AcceptedEnvelopeDigest = e.EnvelopeDigest
	receipt.CheckpointRef = e.CheckpointRef
	receipt.RuntimeFingerprint, _ = ContextDigest([]string{plan.fingerprint, environment})
	defer func() {
		if active.dispatched.Load() {
			receipt.EvidenceRefs = []string{e.RunID}
		}
		if active.injected.Load() {
			manifest := e.KnowledgeManifest
			receipt.KnowledgeManifest = &manifest
			receipt.Workspace, receipt.Ambient = req.Directory, plan.ambient
			receipt.MountedRoots = plan.roots
			for _, doc := range e.PresetDocuments {
				receipt.InjectedDocuments = append(receipt.InjectedDocuments, InjectedDocument{Name: doc.Name, Digest: doc.Digest, Bytes: doc.Bytes})
			}
			for index, skill := range e.Skills {
				receipt.MountedSkills = append(receipt.MountedSkills, MountedSkill{
					StableID: skill.StableID, Revision: skill.Revision, Digest: skill.Digest, Path: plan.native.SkillRoots[index],
				})
			}
			for _, tool := range e.Tools {
				receipt.ToolBindings = append(receipt.ToolBindings, ToolBinding{ID: tool.ID, Version: tool.Version})
			}
			receipt.InstructionsDigest, receipt.InstructionsBytes = ContextContentDigest(plan.instructions), len(plan.instructions)
		}
		status := "rejected"
		if active.dispatched.Load() {
			status = "unknown"
		}
		if nativeCompleted && active.injected.Load() {
			status = "completed"
			if mode == RuntimeClean && len(e.CredentialBindings) == 0 && validIDs(result.NativeSessionID) {
				receipt.NativeSessionRef = &NativeSessionRef{result.NativeSessionID, e.RunID, e.IdempotencyKey, receipt.RuntimeFingerprint}
			}
		}
		finishContextReceipt(receipt, result, status)
		if err := publishContextFile(filepath.Join(entry, "receipt.json"), contextCompletion{Receipt: *receipt, Environment: environment}); err != nil {
			receipt.NativeSessionRef = nil
			resultErr = errors.Join(resultErr, &RunError{Kind: "context_journal", Status: status, ExitCode: result.ExitCode})
		}
		result.ContextReceipt = receipt
	}()
	if sourceEntry != "" {
		if err := publishContextFile(filepath.Join(sourceEntry, "successor.json"), e.RunID); err != nil {
			return fail("session_incompatible", "native session source already has a continuation")
		}
	}
	if mode == RuntimeClean {
		if sourceEntry == "" {
			if _, err := NewEnvironmentAt(environment); err != nil {
				return fail("context_environment", "could not create the clean runtime")
			}
		}
		req.EnvironmentDirectory = environment
		req.Directory = filepath.Join(environment, "workspace")
	} else {
		req.Directory = e.Project.WorkspaceRef
	}
	if e.Runtime.NativeSessionRef != nil {
		req.NativeSessionID = e.Runtime.NativeSessionRef.ID
	}
	plan.native.SkillRoots, err = stageContextSkills(e.Skills, environment, sourceEntry != "")
	if err != nil {
		return fail("context_material", "could not verify or stage exact Skill snapshots")
	}
	req.AdditionalDirectories = make([]string, 0, len(plan.roots))
	for _, root := range plan.roots {
		req.AdditionalDirectories = append(req.AdditionalDirectories, root.PathRef)
	}
	if mode == RuntimeAmbient {
		plan.native.ProfileDirectory = plan.ambient.NativeConfig
	}
	active.authorize = func(ctx context.Context) error {
		if err := authorize(ctx); err != nil {
			return err
		}
		if err := verifyContextExecutable(e.Runtime); err != nil {
			return err
		}
		for _, root := range plan.roots {
			if !contextDirectory(root.PathRef) {
				return errors.New("authorized resource root changed before dispatch")
			}
		}
		if mode == RuntimeAmbient && !contextDirectory(e.Project.WorkspaceRef) {
			return errors.New("authorized working directory changed before dispatch")
		}
		return nil
	}
	// Register cleanup before activation. Release is safe after partial activation
	// and runs before the terminal receipt is published, even after cancellation.
	activeBindings := 0
	defer func() {
		for index := activeBindings - 1; index >= 0; index-- {
			binding := e.CredentialBindings[index]
			state := &receipt.CredentialReceipts[index]
			releaseCtx, releaseCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			outcome, err := services.Broker.Release(releaseCtx, binding)
			releaseCancel()
			expired := !time.Now().Before(binding.NotAfter)
			if expired {
				state.ExpiredAt = &binding.NotAfter
			}
			if err != nil || !validLeaseOutcome(outcome, binding) {
				state.CleanupState, state.CleanupErrorCode = "failed", "broker_release_failed"
				resultErr = errors.Join(resultErr, &RunError{Kind: "credential_cleanup", Status: result.Status, ExitCode: result.ExitCode})
				if result.ErrorKind == "" {
					result.ErrorKind = "credential_cleanup"
				}
				continue
			}
			state.LeaseState, state.RedeemedAt, state.CleanupState = outcome.State, outcome.RedeemedAt, "released"
		}
	}()
	for _, binding := range e.CredentialBindings {
		if ctx.Err() != nil {
			return fail("context_denied", "managed context expired before credential activation")
		}
		receipt.CredentialReceipts = append(receipt.CredentialReceipts, CredentialReceipt{
			BindingID: binding.BindingID, BindingVersion: binding.BindingVersion, ToolID: binding.ToolID,
			LeaseState: "activation_unknown", CleanupState: "pending",
		})
		activeBindings++
		server, err := services.Broker.Activate(ctx, binding)
		if err != nil || server.Name != binding.ToolID {
			return fail("credential_binding", "broker could not activate the authorized MCP tool")
		}
		receipt.CredentialReceipts[activeBindings-1].LeaseState = "active"
		plan.native.MCPServers = append(plan.native.MCPServers, cloneContextServer(server))
	}
	req.Context = nil
	req.RunID, req.Caller, req.Model = e.RunID, e.Caller, e.Runtime.Model
	req.Native, req.Instructions, req.PresetDocuments = plan.native, plan.instructions, nil
	req.ToolsDisabled = e.Runtime.ToolsDisabled
	result, resultErr = c.run(ctx, req, active)
	nativeCompleted = resultErr == nil && result.Status == "ok"
	return result, resultErr
}

func snapshotEnvelope(e ContextEnvelope) (ContextEnvelope, error) {
	raw, err := json.Marshal(e)
	if err != nil || len(raw) > 256<<10 {
		return ContextEnvelope{}, errors.New("invalid or oversized envelope")
	}
	var snapshot ContextEnvelope
	err = json.Unmarshal(raw, &snapshot)
	return snapshot, err
}

func finishContextReceipt(receipt *ContextReceipt, result Result, status string) {
	receipt.Status, receipt.ErrorCode = status, result.ErrorKind
	receipt.CompletedAt = time.Now().UTC()
}

func readContextRetry(entry string, e ContextEnvelope, receipt *ContextReceipt) (Result, error) {
	result := Result{RunID: e.RunID, Status: "error", ExitCode: 2, ContextReceipt: receipt}
	var claim contextClaim
	var done contextCompletion
	switch {
	case readContextFile(filepath.Join(entry, "claim.json"), &claim) != nil:
		result.ErrorKind = "dispatch_unknown"
	case claim.EnvelopeDigest != e.EnvelopeDigest || claim.RunID != e.RunID:
		result.ErrorKind = "idempotency_conflict"
	case readContextFile(filepath.Join(entry, "receipt.json"), &done) != nil:
		result.ErrorKind = "dispatch_unknown"
	default:
		result.ErrorKind, result.ContextReceipt = "dispatch_already_recorded", &done.Receipt
	}
	if result.ContextReceipt == receipt {
		finishContextReceipt(receipt, result, "unknown")
	}
	return result, &RunError{Kind: result.ErrorKind, Status: result.Status, ExitCode: result.ExitCode}
}

func (s *ContextStore) contextEnvironment(e ContextEnvelope, plan contextPlan, entry string) (environment, sourceEntry string, err error) {
	if plan.mode == RuntimeAmbient {
		return "", "", nil
	}
	session := e.Runtime.NativeSessionRef
	if session == nil {
		return filepath.Join(entry, "runtime"), "", nil
	}
	sourceEntry = s.entry(e.TenantID, e.TaskID, session.IdempotencyKey)
	var source contextCompletion
	if err := readContextFile(filepath.Join(sourceEntry, "receipt.json"), &source); err != nil || source.Receipt.NativeSessionRef == nil {
		return "", "", errors.New("native session source receipt is unavailable")
	}
	expected, _ := ContextDigest([]string{plan.fingerprint, source.Environment})
	if *source.Receipt.NativeSessionRef != *session || expected != session.RuntimeFingerprint || source.Environment == "" {
		return "", "", errors.New("native session was issued for a different runtime or authorization")
	}
	return source.Environment, sourceEntry, nil
}

func validLeaseOutcome(outcome LeaseOutcome, binding CredentialBinding) bool {
	if outcome.RedeemedAt != nil && (outcome.RedeemedAt.Before(binding.IssuedAt) || !outcome.RedeemedAt.Before(binding.NotAfter)) {
		return false
	}
	switch outcome.State {
	case "unused", "expired", "revoked":
		return true
	case "redeemed":
		return outcome.RedeemedAt != nil
	default:
		return false
	}
}

func (c *Client) planContext(req Request, e ContextEnvelope, mode RuntimeMode, capabilities AdapterCapabilities, services ContextExecution) (contextPlan, string, error) {
	plan := contextPlan{mode: mode}
	invalid := func(message string) (contextPlan, string, error) { return plan, "context_invalid", errors.New(message) }
	unsupported := func(message string) (contextPlan, string, error) {
		return plan, "unsupported_controls", errors.New(message)
	}
	now := time.Now()
	if !validContextVersion(e.SchemaVersion) || !validIDs(e.EnvelopeID, e.TenantID, e.TaskID, e.RunID, e.IdempotencyKey,
		e.Subject.UserID, e.Subject.AgentID, e.Subject.Audience, e.Project.PrimaryID, e.Runtime.AuthVersion,
		e.KnowledgeManifest.GenerationID, e.KnowledgeManifest.NoteSnapshotVersion, e.KnowledgeManifest.AuthorizationVersion) ||
		len(e.Scope) == 0 || !validIDs(e.Scope...) || !validIDs(e.Subject.RoleIDs...) ||
		e.Caller != "agh" || e.CreatedAt.IsZero() || e.CreatedAt.After(now) || !e.ExpiresAt.After(now) ||
		!validDigest(e.KnowledgeManifest.Digest) {
		return invalid("invalid context identity, version, authorization scope or lifetime")
	}
	digest, err := e.Digest()
	if err != nil || digest != e.EnvelopeDigest {
		return invalid("envelope digest does not match")
	}
	if req.RunID != "" && req.RunID != e.RunID || req.Caller != "" && req.Caller != e.Caller ||
		req.Directory != "" && req.Directory != e.Project.WorkspaceRef || req.Model != "" && req.Model != e.Runtime.Model ||
		req.Native != nil || req.EnvironmentDirectory != "" || req.NativeSessionID != "" || req.CodexHome != "" ||
		len(req.AdditionalDirectories) != 0 || req.ProcessEnvironment != nil || req.InstructionsFile != "" || req.AllowSalvage ||
		req.ExecutionPolicy != "" || req.CodexSandbox != "" || req.AllowedTools != "" || req.DisallowedTools != "" || req.OnUnsupportedTools != "" ||
		req.ToolsDisabled && !e.Runtime.ToolsDisabled {
		return invalid("run overrides conflict with managed context")
	}
	if e.Runtime.Adapter != c.profile.BackendName() || e.Runtime.Profile != c.cfg.ProfileName ||
		mode != RuntimeClean && mode != RuntimeAmbient {
		return invalid("runtime adapter, profile or mode does not match the selected client")
	}
	if !capabilities.Clean || mode == RuntimeAmbient && !capabilities.Ambient ||
		len(e.Skills) > 0 && !capabilities.ExactSkills ||
		e.Runtime.NativeSessionRef != nil && (mode != RuntimeClean || len(e.CredentialBindings) > 0) {
		return unsupported("selected adapter or runtime mode cannot honor the requested context")
	}
	if err := requireContextCapabilities(e.RequiredCapabilities, capabilities, mode); err != nil {
		return unsupported(err.Error())
	}
	if mode == RuntimeClean && (c.profile.UsesNativeAuth() || c.profile.TokenCmd != "" || c.profile.CodexAccount != "") {
		return unsupported("clean context requires explicit provider credentials without native account or token-command inheritance")
	}
	if e.Runtime.Adapter == "codex" && (strings.TrimSpace(services.CodexAPIKey) == "" || strings.ContainsAny(services.CodexAPIKey, "\x00\r\n")) ||
		e.Runtime.Adapter != "codex" && services.CodexAPIKey != "" {
		return invalid("clean Codex requires a caller-supplied provider API key")
	}
	if err := verifyContextExecutable(e.Runtime); err != nil {
		return invalid(err.Error())
	}
	if !contextDirectory(e.Project.WorkspaceRef) {
		return invalid("primary project must resolve to a canonical absolute directory")
	}
	if mode == RuntimeClean {
		plan.roots = append(plan.roots, ContextRoot{ID: e.Project.PrimaryID, PathRef: e.Project.WorkspaceRef, Access: "workspace"})
	} else {
		plan.ambient, err = ambientClaudeSources()
		if err != nil {
			return invalid("cannot identify the ambient Claude home")
		}
	}
	knownIDs := map[string]bool{e.Project.PrimaryID: true}
	knownPaths := map[string]bool{e.Project.WorkspaceRef: true}
	for _, root := range e.ResourceRoots {
		if root.Access != "workspace" {
			return unsupported("adapters cannot enforce the requested root access mode")
		}
		if !validIDs(root.ID) || !contextDirectory(root.PathRef) || knownIDs[root.ID] || knownPaths[root.PathRef] ||
			root.Digest != "" && !validDigest(root.Digest) {
			return invalid("invalid or duplicate resource root")
		}
		knownIDs[root.ID], knownPaths[root.PathRef] = true, true
		plan.roots = append(plan.roots, root)
	}
	if e.CheckpointRef != nil && (!validIDs(e.CheckpointRef.ID) || !validDigest(e.CheckpointRef.Digest)) {
		return invalid("invalid checkpoint reference")
	}
	if session := e.Runtime.NativeSessionRef; session != nil &&
		(!validIDs(session.ID, session.SourceRunID, session.IdempotencyKey) || !validDigest(session.RuntimeFingerprint)) {
		return invalid("invalid native session reference")
	}
	plan.instructions, err = contextInstructions(req, e)
	if err != nil {
		return invalid(err.Error())
	}
	if err := validateContextSkills(e.Skills); err != nil {
		return invalid(err.Error())
	}
	for _, binding := range e.CredentialBindings {
		if binding.Delivery != "mcp" && binding.Delivery != "broker" {
			return unsupported("credential delivery requires a tool-bound MCP broker")
		}
	}
	servers, err := contextTools(e, services, now)
	if err != nil {
		return invalid(err.Error())
	}
	plan.native = &NativeControls{CallerContext: true, CodexAppServer: e.Runtime.Adapter == "codex",
		PermissionMode: e.Runtime.PermissionMode, MCPServers: servers}
	if err := nativeSnapshot(plan.native).Validate(e.Runtime.Adapter, "", e.Runtime.ToolsDisabled); err != nil ||
		e.Runtime.ToolsDisabled && (len(e.Tools) > 0 || len(e.Skills) > 0) {
		return unsupported("selected adapter cannot enforce the requested native controls")
	}
	// Session compatibility excludes run identity and task bytes, but covers
	// every versioned input that could change the context of a native thread.
	runtime := e.Runtime
	runtime.Mode, runtime.NativeSessionRef = mode, nil
	plan.fingerprint, err = ContextDigest([]any{
		e.TenantID, e.TaskID, e.Subject, e.Scope, e.Project, runtime, plan.roots,
		e.KnowledgeManifest, e.PresetDocuments, ContextContentDigest(plan.instructions),
		e.Skills, e.Tools, e.CredentialBindings, e.CheckpointRef, e.RequiredCapabilities,
		e.Extensions, capabilities, c.cfg.Profile, plan.ambient,
	})
	if err != nil {
		return invalid("cannot fingerprint managed runtime")
	}
	return plan, "", nil
}

func requireContextCapabilities(required []string, c AdapterCapabilities, mode RuntimeMode) error {
	known := map[string]bool{"clean": c.Clean && mode == RuntimeClean, "ambient": c.Ambient && mode == RuntimeAmbient,
		"preset_documents": c.PresetDocuments, "resource_roots": c.ResourceRoots,
		"native_resume": c.NativeResume && mode == RuntimeClean, "exact_skills": c.ExactSkills,
		"mcp": c.MCP, "credential_broker": c.CredentialBroker, "checkpoint": c.Checkpoint}
	for _, name := range required {
		if !known[name] {
			return errors.New("required context capability is unsupported")
		}
	}
	return nil
}

func verifyContextExecutable(runtime ContextRuntime) error {
	if !filepath.IsAbs(runtime.Executable) || filepath.Clean(runtime.Executable) != runtime.Executable || !validDigest(runtime.ExecutableDigest) {
		return errors.New("native executable requires an absolute path and pinned digest")
	}
	f, err := os.Open(runtime.Executable)
	if err != nil {
		return errors.New("cannot read the pinned native executable")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return errors.New("pinned native executable must be executable and regular")
	}
	digest, err := readerDigest(f)
	if err != nil || digest != runtime.ExecutableDigest {
		return errors.New("pinned native executable changed")
	}
	return nil
}

func contextDirectory(dir string) bool {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return false
	}
	canonical, err := filepath.EvalSymlinks(dir)
	if err != nil || canonical != dir {
		return false
	}
	info, err := os.Stat(dir)
	return err == nil && info.IsDir()
}

func ambientClaudeSources() (*AmbientSources, error) {
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) {
		return nil, errors.New("ambient user home is unavailable")
	}
	configDir := os.Getenv("CLAUDE_CONFIG_DIR")
	if configDir == "" {
		configDir = filepath.Join(home, ".claude")
	}
	if !filepath.IsAbs(configDir) || filepath.Clean(configDir) != configDir {
		return nil, errors.New("ambient Claude configuration path is invalid")
	}
	return &AmbientSources{Home: home, NativeConfig: configDir,
		SkillRoots:         []string{filepath.Join(configDir, "skills"), filepath.Join(configDir, "plugins")},
		EnvironmentClasses: []string{"caller-process-environment", "native-profile-state"}}, nil
}

func contextInstructions(req Request, e ContextEnvelope) ([]byte, error) {
	if strings.TrimSpace(req.Task) == "" || !utf8.ValidString(req.Task) || strings.ContainsRune(req.Task, 0) ||
		len(req.Instructions) > 256<<10 || !utf8.Valid(req.Instructions) ||
		bytes.IndexByte(req.Instructions, 0) >= 0 ||
		ContextContentDigest([]byte(req.Task)) != e.TaskDigest || ContextContentDigest(req.Instructions) != e.InstructionsDigest ||
		len(req.PresetDocuments) != len(e.PresetDocuments) {
		return nil, errors.New("task, instructions or preset payload does not match the envelope")
	}
	for _, document := range e.PresetDocuments {
		index := slices.IndexFunc(req.PresetDocuments, func(payload PresetDocument) bool {
			return strings.ToUpper(payload.Name) == document.Name
		})
		if index < 0 || !validIDs(document.ContentRef, document.Classification) {
			return nil, errors.New("invalid preset reference")
		}
		payload := req.PresetDocuments[index].Content
		if len(payload) != document.Bytes || ContextContentDigest(payload) != document.Digest {
			return nil, errors.New("preset digest or byte count changed")
		}
	}
	if len(e.PresetDocuments) == 0 {
		return bytes.Clone(req.Instructions), nil
	}
	seen := make(map[string]bool)
	for _, document := range e.PresetDocuments {
		if seen[document.Name] {
			return nil, errors.New("duplicate preset reference")
		}
		seen[document.Name] = true
	}
	return compilePresetDocuments(req.Instructions, req.PresetDocuments)
}

func contextTools(e ContextEnvelope, services ContextExecution, now time.Time) ([]MCPServer, error) {
	tools := make(map[string]ContextTool, len(e.Tools))
	for _, tool := range e.Tools {
		if !validIDs(tool.ID, tool.Version, tool.PolicyRef) || !validDigest(tool.CapabilityDigest) || tools[tool.ID].ID != "" {
			return nil, errors.New("invalid or duplicate tool")
		}
		tools[tool.ID] = tool
	}
	provided, bindings := make(map[string]bool), make(map[string]bool)
	for _, b := range e.CredentialBindings {
		digest, _ := b.Digest()
		if services.Broker == nil || !validIDs(b.BindingID, b.BindingVersion, b.ToolID, b.TargetEnvironment, b.Purpose) ||
			!validDigest(b.PolicyFingerprint) || b.MetadataDigest != digest || b.RunID != e.RunID ||
			tools[b.ToolID].ID == "" || bindings[b.BindingID] || provided[b.ToolID] ||
			len(b.AllowedOperations) == 0 || !validIDs(b.AllowedOperations...) || b.MaxRedemptions < 1 ||
			!b.CleanupRequired || b.IssuedAt.IsZero() || b.IssuedAt.After(now) || !b.NotAfter.After(now) {
			return nil, errors.New("invalid, expired or mismatched credential binding")
		}
		provided[b.ToolID], bindings[b.BindingID] = true, true
	}
	servers := make([]MCPServer, 0, len(services.MCPServers))
	for _, server := range services.MCPServers {
		digest, _ := ContextDigest(server)
		if provided[server.Name] || tools[server.Name].CapabilityDigest != digest {
			return nil, errors.New("MCP configuration does not match an authorized tool")
		}
		servers = append(servers, cloneContextServer(server))
		provided[server.Name] = true
	}
	if len(provided) != len(tools) {
		return nil, errors.New("authorized tools lack resolved MCP endpoints")
	}
	return servers, nil
}

func cloneContextServer(server MCPServer) MCPServer {
	server.Args = slices.Clone(server.Args)
	return server
}
