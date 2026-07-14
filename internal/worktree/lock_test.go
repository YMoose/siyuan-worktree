package worktree

import (
	"os"
	"path/filepath"
	"testing"

	"siyuan-worktree/internal/config"
)

func TestWorkspaceLockIsExclusiveAndReleased(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, config.MetadataDir), 0o755); err != nil {
		t.Fatal(err)
	}
	lock, err := AcquireWorkspaceLock(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireWorkspaceLock(root); err == nil {
		t.Fatal("expected a second lock acquisition to fail")
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	lock, err = AcquireWorkspaceLock(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceLockRemovesAbandonedRefsLock(t *testing.T) {
	root := t.TempDir()
	refsLock := repositoryRefsPath(root) + ".lock"
	if err := os.MkdirAll(filepath.Dir(refsLock), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(refsLock, []byte("incomplete"), 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := AcquireWorkspaceLock(root)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	if _, err := os.Stat(refsLock); !os.IsNotExist(err) {
		t.Fatalf("abandoned refs lock was not removed: %v", err)
	}
}

func TestWorkspaceLockPromotesValidPreparedRefs(t *testing.T) {
	root := t.TempDir()
	refs, err := EnsureRepositorySnapshots(root, config.Default(), State{Version: 3, Documents: map[string]DocumentState{}})
	if err != nil {
		t.Fatal(err)
	}
	candidate := refs
	candidate.Generation++
	if err := writeJSONAtomic(repositoryRefsPath(root)+".lock", candidate); err != nil {
		t.Fatal(err)
	}
	lock, err := AcquireWorkspaceLock(root)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	recovered, err := LoadRepositoryRefs(root)
	if err != nil {
		t.Fatal(err)
	}
	if recovered != candidate {
		t.Fatalf("prepared refs were not promoted: got=%+v want=%+v", recovered, candidate)
	}
}
