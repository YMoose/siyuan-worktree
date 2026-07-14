package worktree

import (
	"strings"
	"testing"
)

func TestStorePatchOrdersDocumentPatchesDeterministically(t *testing.T) {
	store := NewObjectStore(t.TempDir())
	baseTree := ObjectID("sha256:" + strings.Repeat("b", 64))
	targetTree := ObjectID("sha256:" + strings.Repeat("c", 64))

	id, stored, err := StorePatch(store, baseTree, targetTree, []DocumentPatch{
		{DocumentID: "second", LocalPath: "second.md"},
		{DocumentID: "first", LocalPath: "first.md"},
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadPatch(store, id)
	if err != nil {
		t.Fatal(err)
	}
	if stored.BaseTree != baseTree || stored.TargetTree != targetTree || len(stored.DocumentPatches) != 2 {
		t.Fatalf("stored Patch = %+v", stored)
	}
	if loaded.DocumentPatches[0].DocumentID != "first" || loaded.DocumentPatches[1].DocumentID != "second" {
		t.Fatalf("loaded Patch document order = %+v", loaded.DocumentPatches)
	}
}

func TestStorePushOperationRejectsInvalidSequencerState(t *testing.T) {
	commitID := ObjectID("sha256:" + strings.Repeat("a", 64))
	baseTree := ObjectID("sha256:" + strings.Repeat("b", 64))
	targetTree := ObjectID("sha256:" + strings.Repeat("c", 64))
	tests := []struct {
		name      string
		operation PushOperationState
		want      string
	}{
		{
			name: "negative completed document count",
			operation: PushOperationState{
				Phase: PushOperationApplying, CommitObjectID: commitID, BaseTree: baseTree, TargetTree: targetTree,
				Commit: Commit{Version: 2, ObjectID: commitID, Status: CommitPushing, AppliedDocuments: -1},
			},
			want: "invalid applied document count",
		},
		{
			name: "document progress is not a prefix",
			operation: PushOperationState{
				Phase: PushOperationApplying, CommitObjectID: commitID, BaseTree: baseTree, TargetTree: targetTree,
				PreflightDocuments: map[string]ObjectID{
					"first":  ObjectID("sha256:" + strings.Repeat("d", 64)),
					"second": ObjectID("sha256:" + strings.Repeat("e", 64)),
				},
				Commit: Commit{
					Version: 2, ObjectID: commitID, Status: CommitPushing,
					DocumentPatches: []DocumentPatch{
						{DocumentID: "first", LocalPath: "first.md", Status: DocumentPatchCommitted},
						{DocumentID: "second", LocalPath: "second.md", Status: DocumentPatchApplying},
					},
				},
			},
			want: "out-of-order progress",
		},
		{
			name: "mutation evidence has no in-flight marker",
			operation: PushOperationState{
				Phase: PushOperationApplying, CommitObjectID: commitID, BaseTree: baseTree, TargetTree: targetTree,
				Commit: Commit{
					Version: 2, ObjectID: commitID, Status: CommitPushing,
					DocumentPatches: []DocumentPatch{{
						DocumentID: "document", LocalPath: "document.md", Status: DocumentPatchApplying,
						Operations: []Operation{{Type: OperationInsert, ReceiptBlockIDs: []string{"generated"}}},
					}},
				},
			},
			want: "mutation evidence without in-flight state",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := StorePushOperation(NewObjectStore(t.TempDir()), test.operation); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("expected %q rejection, got %v", test.want, err)
			}
		})
	}
}

func TestRepositoryRefsV2RequiresFreshClone(t *testing.T) {
	root := t.TempDir()
	if err := writeJSONAtomic(repositoryRefsPath(root), RepositoryRefs{Version: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRepositoryRefs(root); err == nil || !strings.Contains(err.Error(), "unsupported repository refs version 2") {
		t.Fatalf("expected v2 compatibility boundary, got %v", err)
	}
}
