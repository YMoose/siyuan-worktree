package worktree

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"siyuan-worktree/internal/atomicfile"
	"siyuan-worktree/internal/config"
)

type DocumentState struct {
	ID           string `json:"id"`
	NotebookID   string `json:"notebookId"`
	NotebookName string `json:"notebookName"`
	Title        string `json:"title"`
	RemotePath   string `json:"remotePath"`
	LocalPath    string `json:"localPath"`
}

type State struct {
	Version    int                      `json:"version"`
	LastSyncAt *time.Time               `json:"lastSyncAt"`
	Documents  map[string]DocumentState `json:"documents"`
}

func LoadState(root string) (State, error) {
	data, err := os.ReadFile(config.StatePath(root))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return State{Version: 3, Documents: map[string]DocumentState{}}, nil
		}
		return State{}, fmt.Errorf("read state: %w", err)
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return State{}, fmt.Errorf("decode state: %w", err)
	}
	if state.Version != 3 || state.Documents == nil {
		return State{}, errors.New("unsupported or invalid sync state; create a fresh v3 worktree with clone")
	}
	return state, nil
}

func SaveState(root string, state State) error {
	return writeJSONAtomic(config.StatePath(root), state)
}

func WriteFileAtomic(target string, data []byte, mode os.FileMode) error {
	return atomicfile.Write(target, data, mode)
}

func writeJSONAtomic(target string, value any) error {
	return atomicfile.WriteJSON(target, value, 0o644)
}

func readJSONFile(path string, destination any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, destination)
}
