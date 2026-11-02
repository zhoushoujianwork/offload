package history

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
)

// FindRun reads one record without retaining unrelated task/report bodies.
func FindRun(id string) (Record, bool, error) {
	var found Record
	seen := false
	f, err := os.Open(Path())
	if errors.Is(err, os.ErrNotExist) {
		return found, false, nil
	}
	if err != nil {
		return found, false, err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 64<<10), 16<<20)
	for s.Scan() {
		var head struct {
			RunID string `json:"run_id"`
		}
		if json.Unmarshal(s.Bytes(), &head) != nil || head.RunID != id {
			continue
		}
		if seen {
			return Record{}, false, errors.New("duplicate run identity in history")
		}
		if err := json.Unmarshal(s.Bytes(), &found); err != nil {
			return found, false, err
		}
		seen = true
	}
	return found, seen, s.Err()
}
