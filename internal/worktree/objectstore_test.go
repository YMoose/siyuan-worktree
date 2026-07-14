package worktree

import (
	"os"
	"testing"
)

func TestObjectStoreIsContentAddressedAndVerifiesIntegrity(t *testing.T) {
	root := t.TempDir()
	store := NewObjectStore(root)
	value := BlockSnapshot{BlockID: "20260714120000-block01", BlockType: "p", Kramdown: "text\n"}
	first, err := store.Put(blockSnapshotObjectType, snapshotObjectVersion, value)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Put(blockSnapshotObjectType, snapshotObjectVersion, value)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("same object produced different IDs: %s != %s", first, second)
	}
	var decoded BlockSnapshot
	if err := store.Get(first, blockSnapshotObjectType, snapshotObjectVersion, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.BlockID != value.BlockID || decoded.Kramdown != value.Kramdown {
		t.Fatalf("decoded object = %+v", decoded)
	}
	path, err := store.path(first)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("object permissions = %o", info.Mode().Perm())
	}
	if err := os.WriteFile(path, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Get(first, blockSnapshotObjectType, snapshotObjectVersion, &decoded); err == nil {
		t.Fatal("expected checksum verification failure")
	}
}

func TestStoreDocumentTreeReusesUnchangedBlockSnapshots(t *testing.T) {
	store := NewObjectStore(t.TempDir())
	document := AnnotatedDocument{Blocks: []AnnotatedBlock{
		{ID: "20260714120000-block01", Type: "p", Content: "unchanged\n{: id=\"20260714120000-block01\"}\n"},
		{ID: "20260714120001-block02", Type: "p", Content: "before\n{: id=\"20260714120001-block02\"}\n"},
	}}
	first, err := StoreDocumentTree(store, "20260714110000-doc0001", document)
	if err != nil {
		t.Fatal(err)
	}
	document.Blocks = append([]AnnotatedBlock(nil), document.Blocks...)
	document.Blocks[1].Content = "after\n{: id=\"20260714120001-block02\"}\n"
	second, err := StoreDocumentTree(store, "20260714110000-doc0001", document)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("changed document reused the previous DocumentTree")
	}
	firstTree, err := LoadDocumentTree(store, first)
	if err != nil {
		t.Fatal(err)
	}
	secondTree, err := LoadDocumentTree(store, second)
	if err != nil {
		t.Fatal(err)
	}
	if firstTree.Blocks[0].ObjectID != secondTree.Blocks[0].ObjectID {
		t.Fatal("unchanged block did not reuse its BlockSnapshot")
	}
	if firstTree.Blocks[1].ObjectID == secondTree.Blocks[1].ObjectID {
		t.Fatal("changed block reused its previous BlockSnapshot")
	}
}

func TestRenderDocumentTreeVerifiesMarkdownAgainstBlockSnapshots(t *testing.T) {
	store := NewObjectStore(t.TempDir())
	blockID, err := store.Put(blockSnapshotObjectType, snapshotObjectVersion, BlockSnapshot{
		BlockID:   "20260714120000-block01",
		BlockType: "p",
		Kramdown:  "snapshot\n{: id=\"20260714120000-block01\"}\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	documentID, err := store.Put(documentTreeObjectType, snapshotObjectVersion, DocumentTree{
		DocumentID: "20260714110000-doc0001",
		Blocks: []BlockSnapshotRef{{
			BlockID:  "20260714120000-block01",
			ObjectID: blockID,
		}},
		Markdown: "different\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RenderDocumentTree(store, documentID); err == nil {
		t.Fatal("expected inconsistent document tree to be rejected")
	}
}

func TestValidatePatchRejectsWorkspaceMetadataChanges(t *testing.T) {
	store := NewObjectStore(t.TempDir())
	base, err := StoreWorkspaceTreeObject(store, WorkspaceTree{AllOpenNotebooks: true})
	if err != nil {
		t.Fatal(err)
	}
	target, err := StoreWorkspaceTreeObject(store, WorkspaceTree{AllOpenNotebooks: false, SelectedNotebooks: []string{"notebook"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidatePatch(store, PatchObject{BaseTree: base, TargetTree: target}); err == nil {
		t.Fatal("expected workspace metadata changes to be rejected")
	}
}

func TestRepositoryRefsUseGenerationCompareAndSwap(t *testing.T) {
	root := t.TempDir()
	refs := RepositoryRefs{Version: repositoryRefsVersion, Remote: ObjectID("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")}
	if err := SaveRepositoryRefs(root, 0, refs); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadRepositoryRefs(root)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Generation != 1 || loaded.Remote != refs.Remote {
		t.Fatalf("refs = %+v", loaded)
	}
	if info, err := os.Stat(repositoryRefsPath(root)); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("refs permissions = %o", info.Mode().Perm())
	}
	if err := SaveRepositoryRefs(root, 0, loaded); err == nil {
		t.Fatal("expected stale generation update to fail")
	}
	lockPath := repositoryRefsPath(root) + ".lock"
	if err := os.WriteFile(lockPath, []byte("locked"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SaveRepositoryRefs(root, loaded.Generation, loaded); err == nil {
		t.Fatal("expected existing refs lock to block update")
	}
}
