package worker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// FileStore keeps the change-feed checkpoint in one small JSON file.
//
// Losing it is survivable rather than fatal: the service replays the library from the beginning,
// finds every page's text already current, and settles. That costs a scan and no model runs,
// which is why nothing here works hard to protect it.
type FileStore struct {
	path string
	mu   sync.Mutex
}

func NewFileStore(path string) *FileStore { return &FileStore{path: path} }

type state struct {
	// Seq is a position in one server's change feed. Pointing the service at a different
	// CouchDB with the same state file would skip everything before that server's own position,
	// which is why the file belongs beside the deployment rather than in a shared location.
	Seq string `json:"seq"`
}

func (s *FileStore) Checkpoint() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			// A first run reads the library from the start, which is exactly the backfill.
			return "0", nil
		}
		return "", err
	}
	var current state
	if err := json.Unmarshal(data, &current); err != nil {
		// A truncated or hand-edited file replays rather than stops. See the type comment: a
		// replay is cheap, and refusing to start is not.
		return "0", nil
	}
	if current.Seq == "" {
		return "0", nil
	}
	return current.Seq, nil
}

func (s *FileStore) SetCheckpoint(seq string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(state{Seq: seq})
	if err != nil {
		return err
	}

	// Written to a neighbour and renamed: a half-written checkpoint read on the next start is
	// the one failure here that is not self-correcting.
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
