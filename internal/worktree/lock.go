package worktree

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"siyuan-worktree/internal/config"
)

type WorkspaceLock struct {
	path string
	file *os.File
}

func AcquireWorkspaceLock(root string) (*WorkspaceLock, error) {
	path := filepath.Join(root, config.MetadataDir, "lock")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			owner, _ := os.ReadFile(path)
			return nil, fmt.Errorf("worktree is locked by another command (%s); if no command is running, remove %s", string(owner), path)
		}
		return nil, fmt.Errorf("acquire worktree lock: %w", err)
	}
	if _, err := fmt.Fprintf(file, "pid=%d started=%s", os.Getpid(), time.Now().UTC().Format(time.RFC3339)); err != nil {
		file.Close()
		os.Remove(path)
		return nil, err
	}
	if err := recoverRepositoryRefsLock(root); err != nil {
		file.Close()
		os.Remove(path)
		return nil, err
	}
	return &WorkspaceLock{path: path, file: file}, nil
}

func (l *WorkspaceLock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	closeErr := l.file.Close()
	l.file = nil
	removeErr := os.Remove(l.path)
	if closeErr != nil {
		return closeErr
	}
	if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return removeErr
	}
	return nil
}
