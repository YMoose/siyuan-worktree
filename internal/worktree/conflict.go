package worktree

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"siyuan-worktree/internal/config"
)

type Conflict struct {
	DocumentID string    `json:"documentId"`
	LocalPath  string    `json:"localPath"`
	BaseHash   string    `json:"baseHash"`
	LocalHash  string    `json:"localHash"`
	RemoteHash string    `json:"remoteHash"`
	CreatedAt  time.Time `json:"createdAt"`
}

func conflictRoot(root string) string {
	return filepath.Join(root, config.MetadataDir, "conflicts")
}

func conflictDirectory(root, documentID string) string {
	return filepath.Join(conflictRoot(root), documentID)
}

func writeConflict(root string, document DocumentState, base, local, remote string) (Conflict, error) {
	conflict := Conflict{
		DocumentID: document.ID,
		LocalPath:  document.LocalPath,
		BaseHash:   HashContent(base),
		LocalHash:  HashContent(local),
		RemoteHash: HashContent(remote),
		CreatedAt:  time.Now().UTC(),
	}
	directory := conflictDirectory(root, document.ID)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return Conflict{}, err
	}
	files := map[string]string{
		"base.md":   Canonicalize(base),
		"local.md":  Canonicalize(local),
		"remote.md": Canonicalize(remote),
	}
	for name, content := range files {
		if err := WriteFileAtomic(filepath.Join(directory, name), []byte(content), 0o644); err != nil {
			return Conflict{}, err
		}
	}
	if err := writeJSONAtomic(filepath.Join(directory, "conflict.json"), conflict); err != nil {
		return Conflict{}, err
	}
	return conflict, nil
}

func ListConflicts(root string) ([]Conflict, error) {
	entries, err := os.ReadDir(conflictRoot(root))
	if errors.Is(err, os.ErrNotExist) {
		return []Conflict{}, nil
	}
	if err != nil {
		return nil, err
	}
	conflicts := make([]Conflict, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		var conflict Conflict
		if err := readJSONFile(filepath.Join(conflictRoot(root), entry.Name(), "conflict.json"), &conflict); err != nil {
			return nil, fmt.Errorf("read conflict %s: %w", entry.Name(), err)
		}
		conflicts = append(conflicts, conflict)
	}
	sort.Slice(conflicts, func(i, j int) bool { return conflicts[i].LocalPath < conflicts[j].LocalPath })
	return conflicts, nil
}

func readConflictRemote(root, documentID string) (Conflict, string, error) {
	var conflict Conflict
	if err := readJSONFile(filepath.Join(conflictDirectory(root, documentID), "conflict.json"), &conflict); err != nil {
		return Conflict{}, "", err
	}
	remote, err := os.ReadFile(filepath.Join(conflictDirectory(root, documentID), "remote.md"))
	if err != nil {
		return Conflict{}, "", err
	}
	return conflict, string(remote), nil
}

func clearConflict(root, documentID string) error {
	return os.RemoveAll(conflictDirectory(root, documentID))
}
