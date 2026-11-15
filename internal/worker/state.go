package worker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// Pending is one coalesced page job. Failures and their next retry survive restarts.
type Pending struct {
	DueAt    int64 `json:"dueAt"`
	Attempts int   `json:"attempts,omitempty"`
}

type State struct {
	Seq             string             `json:"seq"`
	PipelineVersion string             `json:"pipelineVersion,omitempty"`
	Pending         map[string]Pending `json:"pending,omitempty"`
}

// FileStore atomically persists both the feed position and all unfinished work.
type FileStore struct {
	path string
	mu   sync.Mutex
}

func NewFileStore(path string) *FileStore { return &FileStore{path: path} }

func (s *FileStore) Load() (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.path)
	if err != nil && !os.IsNotExist(err) {
		return State{}, err
	}
	var state State
	if err == nil {
		if json.Unmarshal(data, &state) != nil {
			state = State{}
		}
	}
	// Old checkpoint files upgrade in place; corrupt files safely replay the feed.
	if state.Seq == "" {
		state.Seq = "0"
	}
	if state.Pending == nil {
		state.Pending = map[string]Pending{}
	}
	return state, nil
}
func (s *FileStore) Save(state State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(tmp, s.path)
}
