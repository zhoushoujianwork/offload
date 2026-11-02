package execution

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"

	"github.com/zhoushoujianwork/offload/internal/history"
	"github.com/zhoushoujianwork/offload/internal/runcontrol"
	"github.com/zhoushoujianwork/offload/internal/transcript"
)

// MonitorOptions opts into owner-private per-run controls and bounded output
// capture. ResumeCommand returns control to an embedding Host; it is never a
// shell string. Omit it when the caller cannot reconstruct a compatible run.
type MonitorOptions struct {
	CaptureTranscript bool
	ResumeCommand     []string
	ResumeDirectory   string
	ResumedFrom       string
}

type monitoring struct {
	registration *runcontrol.Registration
	capture      *transcript.Recorder
}

// ProfileFingerprint compares configuration without persisting credentials.
func ProfileFingerprint(c Config) string {
	raw, _ := json.Marshal(struct {
		ProfileName     string
		Profile         Profile
		Token, TokenCmd string
	}{c.ProfileName, c.Profile, c.Profile.Token, c.Profile.TokenCmd})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (c *Client) startMonitoring(req Request, id, model string, cancel context.CancelFunc) (*monitoring, error) {
	o := req.Monitor
	info := runcontrol.Info{RunID: id, Caller: history.CallerOrUnspecified(req.Caller), CallerRunID: req.CallerRunID,
		Profile: c.cfg.ProfileName, Backend: c.profile.BackendName(), Model: model, Directory: req.Directory,
		ResumeCommand: append([]string(nil), o.ResumeCommand...), ResumeDirectory: o.ResumeDirectory,
		ProfileFingerprint: ProfileFingerprint(c.cfg), ResumedFrom: o.ResumedFrom}
	if err := runcontrol.Validate(info); err != nil {
		return nil, err
	}
	m := &monitoring{}
	if o.CaptureTranscript && req.Capture == nil {
		capture, err := transcript.Open(transcript.Identity{RunID: id, CallerRunID: req.CallerRunID, Caller: info.Caller, Backend: info.Backend, Model: model, Session: req.NativeSessionID}, transcript.NewRedactor(transcript.EnvSecrets(os.Environ())...))
		if err != nil {
			return nil, err
		}
		m.capture = capture
	}
	r, err := runcontrol.Register(info, cancel)
	if err != nil {
		if m.capture != nil {
			m.capture.End(transcript.EndInfo{Status: "error", ErrorKind: "monitor"})
		}
		return nil, err
	}
	m.registration = r
	return m, nil
}

func (m *monitoring) finish(rec history.Record) error {
	if m.capture != nil {
		m.capture.End(transcript.EndInfo{Status: rec.Status, ExitCode: rec.ExitCode, ErrorKind: rec.ErrorKind, StopReason: rec.StopReason, TerminationClass: rec.TerminationClass(), Session: rec.Session})
	}
	return m.registration.Finish(rec)
}

func (m *monitoring) finishResult(result *Result) {
	var rec history.Record
	if err := json.Unmarshal(result.RawRecord, &rec); err != nil {
		result.MonitorError = errors.New("cannot decode monitor terminal record")
		rec = history.Record{Status: "error", ErrorKind: "monitor"}
	}
	result.MonitorError = errors.Join(result.MonitorError, m.finish(rec))
}

// MonitorResumeSource identifies only the Monitor-launched business invocation.
func MonitorResumeSource() string { return runcontrol.ResumeSource() }
