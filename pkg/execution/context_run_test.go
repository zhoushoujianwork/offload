package execution

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

const contextFixtureSession = "11111111-2222-4333-8444-555555555555"
const fixtureAPIKey = "synthetic-provider-key-8624"

func TestContextCancellationReleasesActivatedBinding(t *testing.T) {
	f := newContextFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	broker := &fixtureBroker{failAfterActivation: cancel}
	req := withFixtureBinding(t, f.request(t, "claude", "cancel"), broker)
	result, err := f.clients["claude"].Run(ctx, req)
	if !errors.Is(err, context.Canceled) || result.ContextReceipt.Status != "rejected" ||
		len(result.ContextReceipt.CredentialReceipts) != 1 ||
		result.ContextReceipt.CredentialReceipts[0].CleanupState != "released" ||
		!reflect.DeepEqual(broker.activated, broker.released) {
		t.Fatalf("cancellation did not release the lease: %v %+v", err, result.ContextReceipt)
	}
}

func TestContextConcurrentRetryCannotClaimInFlightRun(t *testing.T) {
	f := newContextFixture(t)
	activated, finish := make(chan struct{}), make(chan struct{})
	broker := &fixtureBroker{failAfterActivation: func() { close(activated); <-finish }}
	req := withFixtureBinding(t, f.request(t, "claude", "concurrent"), broker)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	type outcome struct {
		result Result
		err    error
	}
	first := make(chan outcome, 1)
	go func() {
		result, err := f.clients["claude"].Run(ctx, req)
		first <- outcome{result, err}
	}()
	select {
	case <-activated:
	case early := <-first:
		t.Fatalf("run ended before activation: %v", early.err)
	case <-ctx.Done():
		close(finish)
		t.Fatal(ctx.Err())
	}
	result, err := f.clients["claude"].Run(ctx, req)
	close(finish)
	done := <-first
	if err == nil || result.ErrorKind != "dispatch_unknown" ||
		done.err != nil || done.result.ContextReceipt.Status != "completed" ||
		len(broker.activated) != 1 || len(broker.released) != 1 {
		t.Fatalf("concurrent retry: %s %v, first: %v", result.ErrorKind, err, done.err)
	}
}

func TestContextResumeRequiresCompatibleCurrentRuntime(t *testing.T) {
	for _, backend := range []string{"claude", "codex"} {
		t.Run(backend, func(t *testing.T) {
			f := newContextFixture(t)
			req := f.request(t, backend, "original")
			if backend == "codex" {
				skillDir := filepath.Join(f.dir, "skill")
				if err := os.Mkdir(skillDir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("exact approved skill"), 0600); err != nil {
					t.Fatal(err)
				}
				digest, err := ContextSkillDigest(skillDir)
				if err != nil {
					t.Fatal(err)
				}
				req.Context.Envelope.Skills = []ContextSkill{{StableID: "exact", Revision: "v1", Digest: digest, MountRef: skillDir}}
				req = sealContextRequest(t, req)
			}
			client := f.clients[backend]
			first, err := client.Run(context.Background(), req)
			if err != nil || first.ContextReceipt.NativeSessionRef == nil {
				t.Fatalf("first dispatch: %v %s", err, first.Error)
			}
			if backend == "codex" {
				mounted := first.ContextReceipt.MountedSkills
				if len(mounted) != 1 || mounted[0].Digest != req.Context.Envelope.Skills[0].Digest ||
					!strings.HasPrefix(mounted[0].Path, first.EnvironmentDirectory+string(filepath.Separator)) {
					t.Fatalf("skill receipt: %+v", mounted)
				}
			}
			resume := func(id string) Request {
				next := f.request(t, backend, id)
				next.Task = "Continue the approved native task"
				next.Context.Envelope.Skills = req.Context.Envelope.Skills
				next.Context.Envelope.Runtime.NativeSessionRef = first.ContextReceipt.NativeSessionRef
				return next
			}
			variants := map[string]func(*ContextEnvelope){
				"audience":      func(e *ContextEnvelope) { e.Subject.Audience = "other" },
				"agent":         func(e *ContextEnvelope) { e.Subject.AgentID = "agent-b" },
				"scope":         func(e *ContextEnvelope) { e.Scope = []string{"project:write"} },
				"authorization": func(e *ContextEnvelope) { e.KnowledgeManifest.AuthorizationVersion = "grants-v2" },
				"credential":    func(e *ContextEnvelope) { e.Runtime.AuthVersion = "login-v2" },
				"model":         func(e *ContextEnvelope) { e.Runtime.Model = "another-model" },
			}
			for change, changeEnvelope := range variants {
				next := resume("changed-" + change)
				changeEnvelope(&next.Context.Envelope)
				r, err := client.Run(context.Background(), sealContextRequest(t, next))
				if err == nil || r.ErrorKind != "session_incompatible" || len(r.ContextReceipt.MountedRoots) != 0 {
					t.Fatalf("%s allowed old session: %v %s", change, err, r.ErrorKind)
				}
			}
			next := sealContextRequest(t, resume("resume"))
			second, err := client.Run(context.Background(), next)
			if err != nil || second.ContextReceipt.NativeSessionRef == nil ||
				first.ContextReceipt.RuntimeFingerprint != second.ContextReceipt.RuntimeFingerprint ||
				first.EnvironmentDirectory != second.EnvironmentDirectory || f.observe(t, backend).Resume != first.NativeSessionID {
				t.Fatalf("compatible session did not resume: %v %s %+v", err, second.Error, second.ContextReceipt)
			}
			stale, err := client.Run(context.Background(), sealContextRequest(t, resume("stale-source")))
			if err == nil || stale.ErrorKind != "session_incompatible" {
				t.Fatalf("old session source issued another continuation: %v %s", err, stale.ErrorKind)
			}
		})
	}
}

func TestContextRejectsUnsupportedOrUnapprovedDispatch(t *testing.T) {
	f := newContextFixture(t)
	for _, tc := range []struct {
		name, backend, kind string
		change              func(*Request)
	}{
		{"unknown-capability", "claude", "unsupported_controls", func(r *Request) { r.Context.Envelope.RequiredCapabilities = []string{"future"} }},
		{"ambient-codex", "codex", "unsupported_controls", func(r *Request) { r.Context.Envelope.Runtime.Mode = RuntimeAmbient }},
		{"tool-free-codex", "codex", "unsupported_controls", func(r *Request) { r.Context.Envelope.Runtime.ToolsDisabled = true }},
		{"exact-skills-claude", "claude", "unsupported_controls", func(r *Request) { r.Context.Envelope.Skills = []ContextSkill{{}} }},
		{"root-readonly", "codex", "unsupported_controls", func(r *Request) { r.Context.Envelope.ResourceRoots[0].Access = "read-only" }},
		{"unknown-major", "claude", "context_invalid", func(r *Request) { r.Context.Envelope.SchemaVersion = "2.0" }},
		{"expired-envelope", "claude", "context_invalid", func(r *Request) { r.Context.Envelope.ExpiresAt = time.Now().Add(-time.Second) }},
		{"altered-doc", "claude", "context_invalid", func(r *Request) { r.PresetDocuments[0].Content = []byte("unapproved") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := f.request(t, tc.backend, tc.name)
			tc.change(&req)
			outcome, err := f.clients[tc.backend].Run(context.Background(), sealContextRequest(t, req))
			if err == nil || outcome.ErrorKind != tc.kind || len(outcome.ContextReceipt.MountedRoots) != 0 {
				t.Fatalf("dispatch should fail before native launch: %s %v", outcome.ErrorKind, err)
			}
			if _, err := os.Stat(filepath.Join(f.dir, tc.backend+"-observed.json")); !os.IsNotExist(err) {
				t.Fatal("unsupported request launched the provider")
			}
		})
	}
	ambient := f.request(t, "claude", "explicit-ambient")
	ambient.Context.Envelope.Runtime.Mode = RuntimeAmbient
	ambient.Context.Envelope.RequiredCapabilities = []string{"ambient"}
	outcome, err := f.clients["claude"].Run(context.Background(), sealContextRequest(t, ambient))
	if err != nil {
		t.Fatalf("explicit ambient: %v %s", err, outcome.Error)
	}
	seen := f.observe(t, "claude")
	receipt := outcome.ContextReceipt
	if seen.Cwd != f.project || !seen.Ambient || receipt.Ambient == nil || receipt.Ambient.Home != seen.Home ||
		receipt.Ambient.NativeConfig != seen.Config || receipt.Mode != RuntimeAmbient || receipt.NativeSessionRef != nil {
		t.Fatalf("wrong ambient selection or audit: %+v, %+v", receipt, seen)
	}
}

func TestContextLeaseFailsClosedAndCleansUp(t *testing.T) {
	f := newContextFixture(t)
	for _, tc := range []struct {
		name, kind        string
		modify            func(Request, *fixtureBroker)
		activated, failed bool
	}{
		{"expired", "context_invalid", func(req Request, _ *fixtureBroker) {
			req.Context.Envelope.CredentialBindings[0].NotAfter = time.Now().Add(-time.Second)
		}, false, false},
		{"wrong-run", "context_invalid", func(req Request, _ *fixtureBroker) {
			req.Context.Envelope.CredentialBindings[0].RunID = "other-run"
		}, false, false},
		{"unsafe-delivery", "unsupported_controls", func(req Request, _ *fixtureBroker) {
			req.Context.Envelope.CredentialBindings[0].Delivery = "process_env"
		}, false, false},
		{"partial-activation", "credential_binding", func(_ Request, b *fixtureBroker) {
			b.activateErr = errors.New("broker detail must not enter receipt")
		}, true, false},
		{"revoked-before-start", "context_denied", func(req Request, b *fixtureBroker) {
			allowed := true
			req.Context.Authorize = func(context.Context, ContextEnvelope, *TaskCheckpoint) error {
				if !allowed {
					return errors.New("revoked")
				}
				return nil
			}
			b.failAfterActivation = func() { allowed = false }
		}, true, false},
		{"release-failed", "credential_binding", func(_ Request, b *fixtureBroker) {
			b.activateErr, b.releaseErr = errors.New("partial"), errors.New("broker detail must not enter receipt")
		}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &fixtureBroker{}
			req := withFixtureBinding(t, f.request(t, "codex", tc.name), b)
			tc.modify(req, b)
			result, err := f.clients["codex"].Run(context.Background(), sealContextRequest(t, req))
			if err == nil || result.ErrorKind != tc.kind || len(result.ContextReceipt.MountedRoots) != 0 ||
				(len(b.activated) != 0) != tc.activated || !reflect.DeepEqual(b.activated, b.released) {
				t.Fatalf("failed binding: %s %v %+v", result.ErrorKind, err, result.ContextReceipt)
			}
			if tc.activated {
				receipt := result.ContextReceipt.CredentialReceipts[0]
				if (receipt.CleanupState == "failed") != tc.failed {
					t.Fatalf("unexpected cleanup state: %+v", receipt)
				}
			}
			if _, err := os.Stat(filepath.Join(f.dir, "codex-observed.json")); !os.IsNotExist(err) {
				t.Fatal("failed authorization or lease launched the provider")
			}
		})
	}
}

func TestContextUnknownAndConflictingDispatchDoNotReplay(t *testing.T) {
	f := newContextFixture(t)
	req := f.request(t, "codex", "unknown")
	req.Task = "simulate unknown"
	req = sealContextRequest(t, req)
	first, err := f.clients["codex"].Run(context.Background(), req)
	if err == nil || first.ContextReceipt.Status != "unknown" || first.ContextReceipt.NativeSessionRef != nil {
		t.Fatalf("failed provider was treated as completed: %v %+v", err, first.ContextReceipt)
	}
	seenPath := filepath.Join(f.dir, "codex-observed.json")
	if err := os.Remove(seenPath); err != nil {
		t.Fatal(err)
	}
	retry, err := f.clients["codex"].Run(context.Background(), req)
	if err == nil || retry.ErrorKind != "dispatch_already_recorded" || !reflect.DeepEqual(first.ContextReceipt, retry.ContextReceipt) {
		t.Fatalf("unknown outcome was lost or replayed: %v %+v", err, retry.ContextReceipt)
	}
	req.Task = "unapproved substitution"
	conflict, err := f.clients["codex"].Run(context.Background(), sealContextRequest(t, req))
	if err == nil || conflict.ErrorKind != "idempotency_conflict" {
		t.Fatalf("modified envelope bypassed idempotency: %v %+v", err, conflict.ContextReceipt)
	}
	req.Context.Envelope.IdempotencyKey = "second-key"
	conflict, err = f.clients["codex"].Run(context.Background(), sealContextRequest(t, req))
	if err == nil || conflict.ErrorKind != "idempotency_conflict" {
		t.Fatalf("second key reused an existing Run: %v %+v", err, conflict.ContextReceipt)
	}
	req = f.request(t, "codex", "crash")
	e := req.Context.Envelope
	if err := f.store.claim(e, f.store.entry(e.TenantID, e.TaskID, e.IdempotencyKey)); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenContextStore(f.store.root)
	if err != nil {
		t.Fatal(err)
	}
	req.Context.Store = reopened
	crash, err := f.clients["codex"].Run(context.Background(), req)
	if err == nil || crash.ErrorKind != "dispatch_unknown" || crash.ContextReceipt.Status != "unknown" {
		t.Fatalf("interrupted dispatch was retried: %v %+v", err, crash.ContextReceipt)
	}
	if _, err := os.Stat(seenPath); !os.IsNotExist(err) {
		t.Fatal("unexpected repeated native dispatch")
	}
}

func TestContextEnvelopeMinorExtensionsArePreserved(t *testing.T) {
	f := newContextFixture(t)
	req := f.request(t, "claude", "minor-version")
	req.Context.Envelope.SchemaVersion = "1.2"
	req.Context.Envelope.Extensions = map[string]json.RawMessage{"optional": json.RawMessage(`{"sequence":9007199254740993}`)}
	req = sealContextRequest(t, req)
	e := req.Context.Envelope
	snapshot, err := snapshotEnvelope(e)
	if err != nil || !reflect.DeepEqual(e.Extensions, snapshot.Extensions) {
		t.Fatalf("lost extension: %v", err)
	}
	digest, err := snapshot.Digest()
	if err != nil || digest != e.EnvelopeDigest {
		t.Fatalf("extension changed digest: %v", err)
	}
}

type contextFixture struct {
	store                  *ContextStore
	project, resource, dir string
	clients                map[string]*Client
	bins                   map[string]string
}

func newContextFixture(t *testing.T) contextFixture {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := contextFixture{dir: dir, project: filepath.Join(dir, "project"), resource: filepath.Join(dir, "resource"),
		clients: make(map[string]*Client), bins: make(map[string]string)}
	for _, path := range []string{f.project, f.resource, filepath.Join(dir, "journal"), filepath.Join(dir, "ambient"), filepath.Join(dir, "config")} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	f.store, err = OpenContextStore(filepath.Join(dir, "journal"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", filepath.Join(dir, "ambient"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(dir, "ambient"))
	t.Setenv("CODEX_HOME", filepath.Join(dir, "ambient"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("OFFLOAD_MAX_WORKERS", "-1")
	t.Setenv("TEST_AMBIENT", "ambient-marker")
	if err := os.WriteFile(filepath.Join(dir, "ambient", "auth.json"), []byte("ambient login must not be copied"), 0600); err != nil {
		t.Fatal(err)
	}
	gateway := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(gateway.Close)
	for _, backend := range []string{"claude", "codex"} {
		profile := Profile{Backend: backend, Model: "fixture"}
		if backend == "claude" {
			profile.Token, profile.BaseURL = "synthetic-gateway-token", gateway.URL
		}
		client, err := New(Config{ProfileName: backend + "-fixture", Profile: profile})
		if err != nil {
			t.Fatal(err)
		}
		f.clients[backend] = client
		f.bins[backend] = filepath.Join(dir, backend)
		script := fmt.Sprintf("#!/bin/sh\nexec %s -test.run '^TestContextHarnessHelper$' -- %s %s \"$@\"\n", shellFixtureArg(os.Args[0]), backend, shellFixtureArg(dir))
		if err := os.WriteFile(f.bins[backend], []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func shellFixtureArg(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func (f contextFixture) request(t *testing.T, backend, id string) Request {
	t.Helper()
	executable, err := os.ReadFile(f.bins[backend])
	if err != nil {
		t.Fatal(err)
	}
	timeout := 10 * time.Second
	req := Request{Task: "Do the confirmed task", Instructions: []byte("Authorized instructions"),
		PresetDocuments: []PresetDocument{{Name: "MEMORY.md", Content: []byte("Authorized projection")}}, Timeout: &timeout}
	req.Context = &ContextExecution{Store: f.store, Authorize: func(context.Context, ContextEnvelope, *TaskCheckpoint) error { return nil }}
	if backend == "codex" {
		req.Context.CodexAPIKey = fixtureAPIKey
	}
	now := time.Now().UTC()
	req.Context.Envelope = ContextEnvelope{
		SchemaVersion: ContextSchemaVersion, EnvelopeID: "envelope-" + id, TenantID: "tenant", TaskID: "shared", Caller: "agh",
		RunID: "run-" + id, IdempotencyKey: "dispatch-" + id, CreatedAt: now, ExpiresAt: now.Add(time.Minute),
		Subject: ContextSubject{UserID: "owner", AgentID: "agent-a", Audience: "private"}, Scope: []string{"project:read"},
		Project: ContextProject{PrimaryID: "project", WorkspaceRef: f.project, Revision: "revision"},
		Runtime: ContextRuntime{Adapter: backend, Profile: backend + "-fixture", AuthVersion: "provider-v1",
			PermissionMode: "bypassPermissions", Executable: f.bins[backend], ExecutableDigest: ContextContentDigest(executable)},
		ResourceRoots: []ContextRoot{{ID: "resource", PathRef: f.resource, Access: "workspace"}},
		KnowledgeManifest: KnowledgeManifest{GenerationID: "generation", NoteSnapshotVersion: "snapshot",
			AuthorizationVersion: "grants-v1", Digest: ContextContentDigest([]byte("manifest"))},
		PresetDocuments: []ContextDocument{{Name: "MEMORY.MD", ContentRef: "projection-id", Classification: "private",
			Digest: ContextContentDigest(req.PresetDocuments[0].Content), Bytes: len(req.PresetDocuments[0].Content)}},
	}
	return sealContextRequest(t, req)
}

func sealContextRequest(t *testing.T, req Request) Request {
	t.Helper()
	e := &req.Context.Envelope
	e.TaskDigest, e.InstructionsDigest = ContextContentDigest([]byte(req.Task)), ContextContentDigest(req.Instructions)
	for i := range e.CredentialBindings {
		var err error
		e.CredentialBindings[i].MetadataDigest, err = e.CredentialBindings[i].Digest()
		if err != nil {
			t.Fatal(err)
		}
	}
	var err error
	e.EnvelopeDigest, err = e.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return req
}

type fixtureBroker struct {
	activated, released     []CredentialBinding
	activateErr, releaseErr error
	failAfterActivation     func()
}

func (b *fixtureBroker) Activate(_ context.Context, binding CredentialBinding) (MCPServer, error) {
	b.activated = append(b.activated, binding)
	if b.failAfterActivation != nil {
		b.failAfterActivation()
	}
	return MCPServer{Name: binding.ToolID, Command: "/bin/echo", Args: []string{"opaque-binding", binding.BindingID, binding.RunID}}, b.activateErr
}

func (b *fixtureBroker) Release(ctx context.Context, binding CredentialBinding) (LeaseOutcome, error) {
	b.released = append(b.released, binding)
	if ctx.Err() != nil {
		return LeaseOutcome{}, ctx.Err()
	}
	return LeaseOutcome{State: "revoked"}, b.releaseErr
}

func withFixtureBinding(t *testing.T, req Request, broker *fixtureBroker) Request {
	t.Helper()
	req.Context.Broker = broker
	e := &req.Context.Envelope
	e.Tools = []ContextTool{{ID: "fixture_tool", Version: "v1", PolicyRef: "policy", CapabilityDigest: ContextContentDigest([]byte("tool-capabilities"))}}
	e.CredentialBindings = []CredentialBinding{{
		BindingID: "binding-" + e.RunID, BindingVersion: "v1", PolicyFingerprint: ContextContentDigest([]byte("tool-policy")),
		ToolID: "fixture_tool", TargetEnvironment: "fixture", Purpose: "observe", RunID: e.RunID,
		Delivery: "mcp", AllowedOperations: []string{"inspect"}, IssuedAt: e.CreatedAt, NotAfter: e.ExpiresAt,
		MaxRedemptions: 1, CleanupRequired: true,
	}}
	return sealContextRequest(t, req)
}

type contextFixtureObservation struct {
	Cwd, Home, Config, Task, Instructions, Resume                         string
	Ambient, AmbientAuthCopied, AmbientInstructions, ExplicitProviderAuth bool
	Roots, TurnRoots                                                      []string
	Tools                                                                 json.RawMessage
}

func (f contextFixture) observe(t *testing.T, backend string) contextFixtureObservation {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.dir, backend+"-observed.json"))
	if err != nil {
		t.Fatal(err)
	}
	var seen contextFixtureObservation
	if err := json.Unmarshal(raw, &seen); err != nil {
		t.Fatal(err)
	}
	return seen
}

func TestContextDispatchHandoffAndLeaseReceipt(t *testing.T) {
	f := newContextFixture(t)
	claudeBroker := &fixtureBroker{}
	claudeReq := withFixtureBinding(t, f.request(t, "claude", "produce"), claudeBroker)
	first, err := f.clients["claude"].Run(context.Background(), claudeReq)
	if err != nil {
		t.Fatalf("Claude: %v, %s, %s", err, first.Error, first.NativeStderr)
	}
	firstSeen := f.observe(t, "claude")
	checkFixtureDispatch(t, f, claudeReq, first, firstSeen, claudeBroker)
	firstEnv := claudeReq.Context.Envelope
	source := SourceRun{firstEnv.TenantID, firstEnv.TaskID, firstEnv.RunID, firstEnv.IdempotencyKey}
	checkpoint := TaskCheckpoint{
		SchemaVersion: ContextSchemaVersion, CheckpointID: "checkpoint", TenantID: source.TenantID,
		TaskID: source.TaskID, RunID: source.RunID, ProducerAgentID: firstEnv.Subject.AgentID, Audience: firstEnv.Subject.Audience,
		ProjectID: firstEnv.Project.PrimaryID, EnvelopeDigest: firstEnv.EnvelopeDigest,
		AuthorizationVersion: firstEnv.KnowledgeManifest.AuthorizationVersion,
		CompletedSteps:       []ConfirmedStep{{ID: "confirmed", EvidenceRef: "evidence-1"}}, PendingSteps: []string{"unknown"},
		ArtifactRefs: []string{"artifact-1"}, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().Add(time.Minute).UTC(),
	}
	ref, err := f.store.SaveCheckpoint(source, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := f.store.SaveCheckpoint(source, checkpoint); err != nil || again != ref {
		t.Fatalf("checkpoint retry: %v %v", again, err)
	}
	var allowed bool
	checks := 0
	codexBroker := &fixtureBroker{}
	codexReq := withFixtureBinding(t, f.request(t, "codex", "consume"), codexBroker)
	e := &codexReq.Context.Envelope
	e.CheckpointRef, e.Subject.AgentID, e.Subject.Audience = &ref, "agent-b", "shared-review"
	e.KnowledgeManifest.AuthorizationVersion = "grants-v2"
	e.Scope = []string{"project:review"}
	codexReq.Task = "Continue independently using artifact-1"
	codexReq.Context.Authorize = func(_ context.Context, envelope ContextEnvelope, cp *TaskCheckpoint) error {
		checks++
		if cp == nil || !reflect.DeepEqual(cp, &checkpoint) || envelope.Subject.AgentID != "agent-b" ||
			envelope.KnowledgeManifest.AuthorizationVersion != "grants-v2" || !allowed {
			return errors.New("references are not authorized for this recipient")
		}
		return nil
	}
	codexReq = sealContextRequest(t, codexReq)
	rejected, err := f.clients["codex"].Run(context.Background(), codexReq)
	if err == nil || rejected.ErrorKind != "context_denied" || len(codexBroker.activated) != 0 {
		t.Fatalf("unauthorized handoff: %+v %v", rejected, err)
	}
	allowed = true
	second, err := f.clients["codex"].Run(context.Background(), codexReq)
	if err != nil {
		t.Fatalf("Codex: %v, %s, %s", err, second.Error, second.NativeStderr)
	}
	secondSeen := f.observe(t, "codex")
	checkFixtureDispatch(t, f, codexReq, second, secondSeen, codexBroker)
	if !slices.Equal(secondSeen.TurnRoots, append([]string{secondSeen.Cwd}, firstSeen.Roots...)) ||
		firstSeen.Instructions != secondSeen.Instructions || secondSeen.Resume != "" ||
		firstSeen.Home == secondSeen.Home || checks != 3 || second.ContextReceipt.CheckpointRef == nil {
		t.Fatalf("handoff did not start a fresh Codex thread: %+v, checks=%d", secondSeen, checks)
	}
	retry, err := f.clients["codex"].Run(context.Background(), codexReq)
	if err == nil || retry.ErrorKind != "dispatch_already_recorded" || !reflect.DeepEqual(retry.ContextReceipt, second.ContextReceipt) || len(codexBroker.activated) != 1 {
		t.Fatalf("retry re-executed or lost receipt: %s %v", retry.ErrorKind, err)
	}
	allowed = false
	retry, err = f.clients["codex"].Run(context.Background(), codexReq)
	if err == nil || retry.ErrorKind != "context_denied" || retry.ContextReceipt.AcceptedEnvelopeDigest != "" {
		t.Fatal("revoked recipient retrieved cached dispatch")
	}
}

func checkFixtureDispatch(t *testing.T, f contextFixture, req Request, result Result, seen contextFixtureObservation, broker *fixtureBroker) {
	t.Helper()
	receipt := result.ContextReceipt
	e := req.Context.Envelope
	if receipt == nil || receipt.Status != "completed" || receipt.AcceptedEnvelopeDigest != e.EnvelopeDigest ||
		receipt.KnowledgeManifest == nil || *receipt.KnowledgeManifest != e.KnowledgeManifest || receipt.Workspace != seen.Cwd ||
		seen.Cwd != filepath.Join(result.EnvironmentDirectory, "workspace") || seen.Home != filepath.Join(result.EnvironmentDirectory, "home") ||
		!slices.Equal(seen.Roots, []string{f.project, f.resource}) || seen.Ambient || seen.AmbientAuthCopied || seen.AmbientInstructions ||
		!seen.ExplicitProviderAuth || !slices.Equal(receipt.InjectedDocuments, []InjectedDocument{{Name: e.PresetDocuments[0].Name, Digest: e.PresetDocuments[0].Digest, Bytes: e.PresetDocuments[0].Bytes}}) ||
		receipt.InstructionsBytes != len(seen.Instructions) || ContextContentDigest([]byte(seen.Instructions)) != receipt.InstructionsDigest ||
		!strings.Contains(seen.Instructions, "Authorized instructions") || !strings.Contains(seen.Instructions, "Authorized projection") ||
		seen.Task != req.Task || !strings.Contains(string(seen.Tools), e.CredentialBindings[0].BindingID) ||
		!strings.Contains(string(seen.Tools), e.RunID) || len(receipt.MountedRoots) != 2 ||
		len(receipt.CredentialReceipts) != 1 || receipt.CredentialReceipts[0].CleanupState != "released" ||
		receipt.CredentialReceipts[0].LeaseState != "revoked" || len(broker.activated) != 1 ||
		!reflect.DeepEqual(broker.activated, broker.released) || broker.activated[0].RunID != e.RunID {
		t.Fatalf("dispatch evidence does not match: %+v, %+v", receipt, seen)
	}
	raw, err := json.Marshal(result)
	if err != nil || bytes.Contains(raw, []byte(fixtureAPIKey)) || bytes.Contains(raw, []byte("synthetic-gateway-token")) {
		t.Fatal("provider credential entered the result")
	}
}

// Invoked by the fake CLI wrappers in an isolated child process.
func TestContextHarnessHelper(t *testing.T) {
	split := slices.Index(os.Args, "--")
	if split < 0 {
		return
	}
	args := os.Args[split+1:]
	backend, output := args[0], filepath.Join(args[1], args[0]+"-observed.json")
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	seen := contextFixtureObservation{Cwd: cwd, Home: os.Getenv("HOME"), Ambient: os.Getenv("TEST_AMBIENT") != ""}
	if backend == "claude" {
		seen.Config = os.Getenv("CLAUDE_CONFIG_DIR")
		seen.AmbientInstructions = os.Getenv("CLAUDE_CODE_ADDITIONAL_DIRECTORIES_CLAUDE_MD") != ""
		seen.ExplicitProviderAuth = os.Getenv("ANTHROPIC_AUTH_TOKEN") == "synthetic-gateway-token"
		for i, arg := range args[2:] {
			switch arg {
			case "--add-dir":
				seen.Roots = append(seen.Roots, args[i+3])
			case "--append-system-prompt-file":
				raw, err := os.ReadFile(args[i+3])
				if err != nil {
					t.Fatal(err)
				}
				seen.Instructions = string(raw)
			case "--resume":
				seen.Resume = args[i+3]
			case "--mcp-config":
				seen.Tools = json.RawMessage(args[i+3])
			}
		}
		raw, _ := io.ReadAll(os.Stdin)
		seen.Task = string(raw)
		saveFixtureObservation(t, output, seen)
		fmt.Printf("{\"type\":\"result\",\"subtype\":\"success\",\"session_id\":%q,\"result\":\"fixture done\"}\n", contextFixtureSession)
		os.Exit(0)
	}
	seen.Config = os.Getenv("CODEX_HOME")
	_, err = os.Stat(filepath.Join(seen.Config, "auth.json"))
	seen.AmbientAuthCopied = !os.IsNotExist(err)
	seen.ExplicitProviderAuth = os.Getenv("OPENAI_API_KEY") == fixtureAPIKey
	fakeContextCodex(t, output, seen)
	os.Exit(0)
}

func saveFixtureObservation(t *testing.T, output string, seen contextFixtureObservation) {
	t.Helper()
	raw, err := json.Marshal(seen)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(output, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func fakeContextCodex(t *testing.T, output string, seen contextFixtureObservation) {
	t.Helper()
	scanner := bufio.NewScanner(os.Stdin)
	var selected []string
	for scanner.Scan() {
		var frame struct {
			ID     json.RawMessage
			Method string
			Params struct {
				Cwd, DeveloperInstructions, ThreadID string
				RuntimeWorkspaceRoots, ExtraRoots    []string
				Config                               map[string]json.RawMessage
				Input                                []struct{ Text string }
			}
		}
		if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil {
			t.Fatal(err)
		}
		respond := func(value any) {
			_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"id": frame.ID, "result": value})
		}
		p := frame.Params
		switch frame.Method {
		case "initialized":
			continue
		case "skills/extraRoots/set":
			selected = p.ExtraRoots
			respond(struct{}{})
		case "skills/list":
			skills := make([]map[string]any, 0, len(selected))
			for _, root := range selected {
				skills = append(skills, map[string]any{"path": filepath.Join(root, "SKILL.md"), "enabled": true, "pluginId": nil})
			}
			respond(map[string]any{"data": []any{map[string]any{"errors": []any{}, "skills": skills}}})
		case "skills/config/write":
			respond(map[string]bool{"effectiveEnabled": true})
		case "thread/start", "thread/resume":
			seen.Roots, seen.Instructions, seen.Resume = p.RuntimeWorkspaceRoots[1:], p.DeveloperInstructions, p.ThreadID
			seen.AmbientInstructions = string(p.Config["project_doc_max_bytes"]) != "0" || p.Cwd != seen.Cwd
			seen.Tools = p.Config["mcp_servers"]
			respond(map[string]any{"thread": map[string]string{"id": contextFixtureSession}})
		case "turn/start":
			seen.Task, seen.TurnRoots = p.Input[0].Text, p.RuntimeWorkspaceRoots
			saveFixtureObservation(t, output, seen)
			fixtureSessionDir := filepath.Join(seen.Config, "sessions")
			if err := os.MkdirAll(fixtureSessionDir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(fixtureSessionDir, "rollout-"+contextFixtureSession+".jsonl"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			respond(map[string]any{"turn": map[string]string{"id": "turn"}})
			fmt.Printf("{\"method\":\"turn/started\",\"params\":{\"threadId\":%q,\"turn\":{\"id\":\"turn\"}}}\n", contextFixtureSession)
			if seen.Task == "simulate unknown" {
				return
			}
			fmt.Printf("{\"method\":\"turn/completed\",\"params\":{\"threadId\":%q,\"turn\":{\"id\":\"turn\",\"status\":\"completed\",\"items\":[{\"type\":\"agentMessage\",\"text\":%q}]}}}\n",
				contextFixtureSession, "fixture done "+fixtureAPIKey)
			return
		default:
			respond(struct{}{})
		}
	}
}
