// Package runcontrol exposes owner-private control of one execution context.
// A process may host many runs; no control operation signals its PID.
package runcontrol

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/zhoushoujianwork/offload/internal/history"
	"github.com/zhoushoujianwork/offload/internal/proc"
	"github.com/zhoushoujianwork/offload/internal/transcript"
)

type Info struct {
	RunID              string   `json:"run_id"`
	Caller             string   `json:"caller"`
	CallerRunID        string   `json:"caller_run_id,omitempty"`
	Profile            string   `json:"profile"`
	Backend            string   `json:"backend"`
	Model              string   `json:"model"`
	Directory          string   `json:"directory"`
	ResumeCommand      []string `json:"resume_command,omitempty"`
	ResumeDirectory    string   `json:"resume_directory,omitempty"`
	CLIResume          bool     `json:"cli_resume,omitempty"`
	ProfileFingerprint string   `json:"profile_fingerprint,omitempty"`
	ResumedFrom        string   `json:"resumed_from,omitempty"`
}

type Manifest struct {
	ResumePID      int    `json:"resume_pid,omitempty"`
	ResumePIDStart string `json:"resume_pid_start,omitempty"`
	Info
	Version   int       `json:"version"`
	PID       int       `json:"pid"`
	PIDStart  string    `json:"pid_start"`
	Address   string    `json:"address"`
	Token     string    `json:"token"`
	Phase     string    `json:"phase"`
	Status    string    `json:"status,omitempty"`
	Session   string    `json:"session,omitempty"`
	Cleanup   string    `json:"cleanup,omitempty"`
	ResumedBy string    `json:"resumed_by,omitempty"`
	StartedAt time.Time `json:"started_at"`
}

type Registration struct {
	mu       sync.Mutex
	m        Manifest
	server   *http.Server
	listener net.Listener
	cancel   context.CancelFunc
}

func Dir() string           { return filepath.Join(filepath.Dir(history.Path()), "controls") }
func Path(id string) string { return filepath.Join(Dir(), id+".json") }

func private(st os.FileInfo) bool { return owned(st) && st.Mode().Perm()&0077 == 0 }
func prepare() error {
	if err := os.MkdirAll(Dir(), 0700); err != nil {
		return err
	}
	st, err := os.Lstat(Dir())
	if err != nil {
		return err
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 || !private(st) {
		return errors.New("unsafe execution control directory")
	}
	return nil
}

func Validate(info Info) error {
	if !transcript.ValidRunID(info.RunID) {
		return errors.New("monitoring requires a generated 32-character Offload run ID")
	}
	if info.ResumedFrom != "" && (!transcript.ValidRunID(info.ResumedFrom) || info.ResumedFrom == info.RunID) {
		return errors.New("invalid resumed-from run ID")
	}
	if len(info.ResumeCommand) > 256 {
		return errors.New("resume command is too large")
	}
	n := 0
	for _, arg := range info.ResumeCommand {
		n += len(arg)
		if strings.ContainsRune(arg, 0) {
			return errors.New("resume command contains NUL")
		}
	}
	if n > 64<<10 {
		return errors.New("resume command is too large")
	}
	if len(info.ResumeCommand) > 0 {
		if !filepath.IsAbs(info.ResumeCommand[0]) || !filepath.IsAbs(info.ResumeDirectory) {
			return errors.New("caller resume requires absolute executable and directory")
		}
		st, err := os.Stat(info.ResumeCommand[0])
		if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0111 == 0 {
			return errors.New("caller resume executable is unavailable")
		}
		st, err = os.Stat(info.ResumeDirectory)
		if err != nil || !st.IsDir() {
			return errors.New("caller resume directory is unavailable")
		}
	}
	return nil
}

func Register(info Info, cancel context.CancelFunc) (*Registration, error) {
	if err := Validate(info); err != nil {
		return nil, err
	}
	if err := prepare(); err != nil {
		return nil, err
	}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	r := &Registration{m: Manifest{Info: info, Version: 1, PID: os.Getpid(), PIDStart: proc.StartToken(os.Getpid()), Address: listener.Addr().String(), Token: hex.EncodeToString(secret[:]), Phase: "running", StartedAt: time.Now().UTC()}, listener: listener, cancel: cancel}
	// Never overwrite another registration or silently adopt its execution.
	f, err := os.OpenFile(Path(info.RunID), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		listener.Close()
		return nil, err
	}
	err = json.NewEncoder(f).Encode(r.m)
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		listener.Close()
		// This invocation exclusively created the file. A failed initial write
		// must not leave a live-looking registration for work never admitted.
		_ = os.Remove(Path(info.RunID))
		return nil, err
	}
	r.server = &http.Server{Handler: http.HandlerFunc(r.handle), ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second, MaxHeaderBytes: 4096}
	go func() { _ = r.server.Serve(listener) }()
	if info.ResumedFrom != "" {
		if err := SetResumedBy(info.ResumedFrom, info.RunID); err != nil {
			_ = r.Finish(history.Record{Status: "error", ErrorKind: "monitor"})
			return nil, err
		}
	}
	return r, nil
}

func (r *Registration) save() error {
	f, err := os.CreateTemp(Dir(), ".control-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	err = json.NewEncoder(f).Encode(r.m)
	if e := f.Close(); err == nil {
		err = e
	}
	if err == nil {
		err = os.Rename(f.Name(), Path(r.m.RunID))
	}
	return err
}

func (r *Registration) handle(w http.ResponseWriter, q *http.Request) {
	if q.Method != "POST" || q.URL.Path != "/stop" || q.Header.Get("Origin") != "" || q.Host != r.m.Address {
		http.Error(w, "unsupported control request", http.StatusForbidden)
		return
	}
	if subtle.ConstantTimeCompare([]byte(q.Header.Get("Authorization")), []byte("Bearer "+r.m.Token)) != 1 {
		http.Error(w, "unauthorized", http.StatusForbidden)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m.Phase == "done" {
		http.Error(w, "execution has ended", http.StatusConflict)
		return
	}
	r.m.Phase = "stopping"
	if err := r.save(); err != nil {
		http.Error(w, "cannot persist stop request", http.StatusInternalServerError)
		return
	}
	r.cancel()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"run_id": r.m.RunID, "phase": "stopping"})
}

func (r *Registration) Finish(rec history.Record) error {
	r.mu.Lock()
	r.m.Phase = "done"
	r.m.Status = rec.Status
	r.m.Session = rec.Session
	r.m.Cleanup = rec.Cleanup
	if rec.ErrorKind == "canceled" || rec.StopReason == "canceled" {
		r.m.Status = "canceled"
	}
	err := r.save()
	r.mu.Unlock()
	_ = r.server.Close()
	return err
}

func Read(id string) (Manifest, error) {
	var m Manifest
	if !transcript.ValidRunID(id) {
		return m, errors.New("invalid Offload run ID")
	}
	st, err := os.Lstat(Dir())
	if err != nil {
		return m, err
	}
	if !st.IsDir() || !private(st) || st.Mode()&os.ModeSymlink != 0 {
		return m, errors.New("unsafe control directory")
	}
	st, err = os.Lstat(Path(id))
	if err != nil {
		return m, err
	}
	if !st.Mode().IsRegular() || !private(st) || st.Size() > 1<<20 {
		return m, errors.New("unsafe control record")
	}
	f, err := os.Open(Path(id))
	if err != nil {
		return m, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(st, opened) || !private(opened) {
		return m, errors.New("control record changed while opening")
	}
	raw, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return m, errors.New("control record is too large")
	}
	if json.Unmarshal(raw, &m) != nil || m.Version != 1 || m.RunID != id || len(m.Token) != 64 {
		return m, errors.New("invalid control record")
	}
	return m, nil
}

func (m Manifest) Active() bool {
	return m.Phase != "done" && m.PID > 1 && m.PIDStart != "" && proc.StartToken(m.PID) == m.PIDStart
}

func Stop(ctx context.Context, id string) error {
	m, err := Read(id)
	if err != nil {
		return err
	}
	if !m.Active() {
		return errors.New("execution is no longer controllable; refresh its state")
	}
	host, port, err := net.SplitHostPort(m.Address)
	if err != nil || host != "127.0.0.1" || port == "" {
		return errors.New("invalid local control endpoint")
	}
	q, err := http.NewRequestWithContext(ctx, "POST", "http://"+m.Address+"/stop", nil)
	if err != nil {
		return err
	}
	q.Header.Set("Authorization", "Bearer "+m.Token)
	client := http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("control redirects are forbidden") }}
	response, err := client.Do(q)
	if err != nil {
		return errors.New("execution control endpoint is unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("stop request rejected (%d)", response.StatusCode)
	}
	return nil
}

// SetResumedBy records lineage before the new worker is admitted. The resume
// launcher holds the source's lease, so repeated clicks cannot create two runs.
func SetResumedBy(source, id string) error {
	release, err := lease(source, false)
	if err != nil {
		return err
	}
	defer release()
	m, err := Read(source)
	if err != nil {
		return err
	}
	if m.Phase != "done" || m.ResumedBy != "" {
		return errors.New("source is active or already resumed")
	}
	if m.ResumePending() && (m.ResumePID != os.Getpid() || m.ResumePIDStart != proc.StartToken(os.Getpid())) {
		return errors.New("another caller owns this resume")
	}
	m.ResumedBy = id
	r := Registration{m: m}
	return r.save()
}

func (m Manifest) ResumePending() bool {
	return m.ResumedBy == "" && m.ResumePID > 1 && m.ResumePIDStart != "" && proc.StartToken(m.ResumePID) == m.ResumePIDStart
}
func MarkResuming(id string, pid int) error {
	m, err := Read(id)
	if err != nil {
		return err
	}
	m.ResumePID = pid
	m.ResumePIDStart = proc.StartToken(pid)
	if m.ResumePIDStart == "" {
		return errors.New("resume process ended before it could be registered")
	}
	r := Registration{m: m}
	return r.save()
}

// ResumeSource is lineage, never permission to bypass caller session checks.
func ResumeSource() string {
	id := os.Getenv("OFFLOAD_MONITOR_RESUMED_FROM")
	m, err := Read(id)
	if err != nil {
		return ""
	}
	if m.ResumePID == os.Getpid() || os.Getenv("OFFLOAD_MONITOR_RESUME_PARENT_PID") == fmt.Sprint(os.Getppid()) {
		return id
	}
	return ""
}
