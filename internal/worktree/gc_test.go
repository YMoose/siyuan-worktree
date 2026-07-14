package worktree

import (
	"os"
	"path/filepath"
	"testing"

	"siyuan-worktree/internal/config"
	"siyuan-worktree/internal/siyuan"
)

func TestPruneRepositoryObjectsRemovesUnreachableObjects(t *testing.T) {
	_, _, _, root := newSingleDocumentFixture(t, map[string]string{
		testBlockID: "base\n{: id=\"" + testBlockID + "\"}\n",
	}, []siyuan.ChildBlock{{ID: testBlockID, Type: "p"}}, nil)
	store := NewObjectStore(root)
	orphan, err := store.Put(blockSnapshotObjectType, snapshotObjectVersion, BlockSnapshot{
		BlockID:   "20260714120000-orphan1",
		BlockType: "p",
		Kramdown:  "orphan\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	staleTemporary := filepath.Join(root, config.MetadataDir, "objects", "sha256", "ff", ".tmp-stale")
	if err := os.MkdirAll(filepath.Dir(staleTemporary), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staleTemporary, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := pruneRepositoryObjects(root); err != nil {
		t.Fatal(err)
	}
	if err := store.Get(orphan, blockSnapshotObjectType, snapshotObjectVersion, nil); err == nil {
		t.Fatal("unreachable object was not pruned")
	}
	if _, err := os.Stat(staleTemporary); !os.IsNotExist(err) {
		t.Fatalf("stale temporary object was not pruned: %v", err)
	}
	refs, err := LoadRepositoryRefs(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCommitObject(store, refs.Head); err != nil {
		t.Fatalf("reachable HEAD was pruned: %v", err)
	}
}
