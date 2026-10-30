package transcript

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"github.com/zhoushoujianwork/offload/internal/history"
)

// Retention bounds. They are variables only so tests can shorten them.
var (
	RetainFor   = 14 * 24 * time.Hour // transcripts older than this are removed when a new capture starts
	RetainFiles = 200                 // newest transcripts kept
	RetainBytes = int64(512 << 20)    // total size kept, oldest removed first
)

var (
	runIDPattern  = regexp.MustCompile(`^[0-9a-f]{32}$`)
	fileNameParts = regexp.MustCompile(`^([0-9a-f]{32})\.ndjson$`)
	safeIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:@/+=-]{1,128}$`)
)

// ValidRunID reports whether s has the shape of an offload run ID. Only such values are ever turned
// into a file name.
func ValidRunID(s string) bool { return runIDPattern.MatchString(s) }

// Dir is the transcript store, next to the run log.
func Dir() string { return filepath.Join(filepath.Dir(history.Path()), "transcripts") }

// Path is the transcript file of one run.
func Path(runID string) string { return filepath.Join(Dir(), runID+".ndjson") }

// checkDir rejects a store directory that is a symlink, not a directory, owned by someone else or
// open to other users.
func checkDir(dir string) error {
	st, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if st.Mode()&os.ModeSymlink != 0 || !st.IsDir() {
		return errors.New("transcript directory is not a plain directory")
	}
	if !ownedByUs(st) {
		return errors.New("transcript directory is not owned by the current user")
	}
	if otherAccess(st) {
		return errors.New("transcript directory is accessible to other users")
	}
	return nil
}

// prepareDir creates the store with private permissions and validates it.
func prepareDir(dir string) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return checkDir(dir)
}

// openRegular opens a store file for reading after checking, by Lstat and again on the opened
// descriptor, that it is a private regular file owned by the current user.
func openRegular(path string) (*os.File, os.FileInfo, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if err := checkFile(st); err != nil {
		return nil, nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	fst, err := f.Stat()
	if err != nil || !os.SameFile(st, fst) || checkFile(fst) != nil {
		f.Close()
		return nil, nil, errors.New("transcript file changed while opening")
	}
	return f, fst, nil
}

func checkFile(st os.FileInfo) error {
	switch {
	case st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular():
		return errors.New("transcript is not a regular file")
	case !ownedByUs(st):
		return errors.New("transcript is not owned by the current user")
	case otherAccess(st):
		return errors.New("transcript is accessible to other users")
	}
	return nil
}

type storeFile struct {
	runID string
	size  int64
	mod   time.Time
	path  string
}

// storeFiles lists regular transcript files, newest first. Anything else in the directory is left
// alone.
func storeFiles(dir string) []storeFile {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []storeFile
	for _, e := range entries {
		m := fileNameParts.FindStringSubmatch(e.Name())
		if m == nil || !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, storeFile{runID: m[1], size: info.Size(), mod: info.ModTime(), path: filepath.Join(dir, e.Name())})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].mod.After(out[j].mod) })
	return out
}

// Prune applies the retention bounds. keep is never removed. It reports how many files it removed.
// A file whose writer is still alive is never removed, however old it is.
func Prune(dir, keep string, now time.Time) int {
	removed, total := 0, int64(0)
	files := storeFiles(dir)
	// Count the protected capture first, including when filesystem timestamp
	// precision ties its new file with a completed capture.
	sort.SliceStable(files, func(i, j int) bool {
		return files[i].runID == keep && files[j].runID != keep
	})
	for i, f := range files {
		total += f.size
		drop := f.runID != keep && (now.Sub(f.mod) > RetainFor || i >= RetainFiles || total > RetainBytes)
		if !drop || writerAlive(f.path) {
			continue
		}
		if os.Remove(f.path) == nil {
			removed++
		}
	}
	return removed
}

// writerAlive reports whether a transcript is still being written: its first event names a process
// that is running, and no execution_end has been written yet.
func writerAlive(path string) bool {
	f, st, err := openRegular(path)
	if err != nil {
		return false
	}
	defer f.Close()
	head, err := headOf(f)
	if err != nil || !processAlive(head.PID, head.PIDStart) {
		return false
	}
	_, ended := tailEnd(f, st.Size())
	return !ended
}

// headBytes bounds how much of a transcript is read to find its first event. capture_start is a
// few hundred bytes.
const headBytes = 16 << 10

var errNoHead = errors.New("first event is not complete yet")

// readHead returns the capture_start event of a transcript.
func readHead(path string) (Event, error) {
	f, _, err := openRegular(path)
	if err != nil {
		return Event{}, err
	}
	defer f.Close()
	return headOf(f)
}

func headOf(f *os.File) (Event, error) {
	buf := make([]byte, headBytes)
	n, _ := f.ReadAt(buf, 0)
	i := bytes.IndexByte(buf[:n], '\n')
	if i < 0 {
		return Event{}, errNoHead
	}
	var e Event
	if err := unmarshalEvent(buf[:i], &e); err != nil {
		return Event{}, err
	}
	if e.Type != TypeCaptureStart {
		return Event{}, errors.New("first event is not capture_start")
	}
	return e, nil
}
