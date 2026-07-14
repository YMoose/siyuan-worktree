package worktree

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"siyuan-worktree/internal/siyuan"
)

func TestWorkingTreeSnapshotIsContentAddressed(t *testing.T) {
	syncer, _, localPath, root := newSingleDocumentFixture(t, map[string]string{
		testBlockID: "base\n{: id=\"" + testBlockID + "\"}\n",
	}, []siyuan.ChildBlock{{ID: testBlockID, Type: "p"}}, nil)
	state, err := LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	store := NewObjectStore(root)
	firstScan, err := syncer.scanWorkingTree(state)
	if err != nil {
		t.Fatal(err)
	}
	firstID, first, err := persistWorkingTreeSnapshot(store, firstScan)
	if err != nil {
		t.Fatal(err)
	}
	secondScan, err := syncer.scanWorkingTree(state)
	if err != nil {
		t.Fatal(err)
	}
	secondID, second, err := persistWorkingTreeSnapshot(store, secondScan)
	if err != nil {
		t.Fatal(err)
	}
	if firstID != secondID || first.Files[0].ContentObject != second.Files[0].ContentObject {
		t.Fatalf("unchanged working snapshot was not reused: first=%s second=%s", firstID, secondID)
	}
	writeSingleBlock(t, localPath, testBlockID, "changed")
	changedScan, err := syncer.scanWorkingTree(state)
	if err != nil {
		t.Fatal(err)
	}
	changedID, changed, err := persistWorkingTreeSnapshot(store, changedScan)
	if err != nil {
		t.Fatal(err)
	}
	if changedID == firstID || changed.Files[0].ContentObject == first.Files[0].ContentObject {
		t.Fatal("changed working file reused the old snapshot object")
	}
}

func TestPullOperationResumesMaterialization(t *testing.T) {
	syncer, api, localPath, root := newSingleDocumentFixture(t, map[string]string{
		testBlockID: "base\n{: id=\"" + testBlockID + "\"}\n",
	}, []siyuan.ChildBlock{{ID: testBlockID, Type: "p"}}, nil)
	const addedDocumentID = "20260714110101-child02"
	const addedBlockID = "20260714120003-block03"
	api.contents[testBlockID] = "remote changed\n{: id=\"" + testBlockID + "\"}\n"
	api.documents["/"] = append(api.documents["/"], siyuan.Document{ID: addedDocumentID, Name: "Added", Path: "/" + addedDocumentID + ".sy"})
	api.children[addedDocumentID] = []siyuan.ChildBlock{{ID: addedBlockID, Type: "p"}}
	api.contents[addedBlockID] = "added remotely\n{: id=\"" + addedBlockID + "\"}\n"

	state, err := LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := LoadRepositoryRefs(root)
	if err != nil {
		t.Fatal(err)
	}
	working, err := syncer.scanWorkingTree(state)
	if err != nil {
		t.Fatal(err)
	}
	execution, err := syncer.beginPullExecution(NewObjectStore(root), refs, state, working)
	if err != nil {
		t.Fatal(err)
	}
	if err := execution.prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := execution.persist(PullOperationMaterializing, ""); err != nil {
		t.Fatal(err)
	}
	workingTreeID := execution.operation.WorkingTree
	if len(execution.operation.Plans) != 2 {
		t.Fatalf("pull plans = %+v", execution.operation.Plans)
	}
	first := execution.operation.Plans[0]
	if err := syncer.materializePullDocument(execution.store, first); err != nil {
		t.Fatal(err)
	}
	execution.operation.MaterializedDocuments = append(execution.operation.MaterializedDocuments, first.Document.ID)
	if err := execution.persist(PullOperationMaterializing, ""); err != nil {
		t.Fatal(err)
	}
	status, err := syncer.RepositoryStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.ActiveOperation == nil || status.ActiveOperation.Kind != "pull" ||
		status.ActiveOperation.Phase != string(PullOperationMaterializing) ||
		status.ActiveOperation.MaterializedDocuments != 1 || status.ActiveOperation.TotalDocuments != 2 ||
		status.ActiveOperation.CurrentDocument != execution.operation.Plans[1].LocalPath ||
		status.ActiveOperation.NextAction != "siyuan-worktree pull" {
		t.Fatalf("interrupted pull status = %+v", status.ActiveOperation)
	}
	if len(status.Documents) != 0 {
		t.Fatalf("interrupted pull reported a partially materialized document comparison: %+v", status.Documents)
	}
	if !status.DocumentComparisonDeferred {
		t.Fatal("interrupted pull status did not mark document comparison as deferred")
	}
	if _, err := syncer.Reset(false); err == nil || !strings.Contains(err.Error(), "pull modified the working tree") {
		t.Fatalf("reset should preserve an interrupted pull, got %v", err)
	}

	result, err := syncer.Pull(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Updated != 1 || result.Added != 1 {
		t.Fatalf("resumed pull = %+v", result)
	}
	local, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(local), "remote changed") {
		t.Fatalf("first materialized file = %q", local)
	}
	added, err := os.ReadFile(filepath.Join(root, "notes", "Work", "Added.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(added), "added remotely") {
		t.Fatalf("resumed file = %q", added)
	}
	completedRefs, err := LoadRepositoryRefs(root)
	if err != nil {
		t.Fatal(err)
	}
	if completedRefs.Operation != "" {
		t.Fatalf("completed pull retained OperationState: %+v", completedRefs)
	}
	if err := ValidateRepositorySnapshots(root, completedRefs); err != nil {
		t.Fatal(err)
	}
	if err := NewObjectStore(root).Get(workingTreeID, workingTreeObjectType, snapshotObjectVersion, nil); err == nil {
		t.Fatal("completed pull retained its transient WorkingTreeSnapshot")
	}
}

func TestPullOperationRejectsWorkingTreeChangeAfterPrepare(t *testing.T) {
	syncer, api, localPath, root := newSingleDocumentFixture(t, map[string]string{
		testBlockID: "base\n{: id=\"" + testBlockID + "\"}\n",
	}, []siyuan.ChildBlock{{ID: testBlockID, Type: "p"}}, nil)
	api.contents[testBlockID] = "remote changed\n{: id=\"" + testBlockID + "\"}\n"
	state, err := LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := LoadRepositoryRefs(root)
	if err != nil {
		t.Fatal(err)
	}
	working, err := syncer.scanWorkingTree(state)
	if err != nil {
		t.Fatal(err)
	}
	execution, err := syncer.beginPullExecution(NewObjectStore(root), refs, state, working)
	if err != nil {
		t.Fatal(err)
	}
	if err := execution.prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := execution.persist(PullOperationMaterializing, ""); err != nil {
		t.Fatal(err)
	}
	writeSingleBlock(t, localPath, testBlockID, "edited after pull started")
	if err := execution.materialize(); err == nil || !strings.Contains(err.Error(), "changed after pull started") {
		t.Fatalf("expected working tree precondition failure, got %v", err)
	}
	if _, err := syncer.Reset(false); err == nil || !strings.Contains(err.Error(), "pull modified the working tree") {
		t.Fatalf("normal reset should preserve materialization evidence, got %v", err)
	}
	if _, err := syncer.Reset(true); err != nil {
		t.Fatal(err)
	}
}

func TestPullOperationRecognizesTargetWrittenBeforeProgressWasSaved(t *testing.T) {
	syncer, api, localPath, root := newSingleDocumentFixture(t, map[string]string{
		testBlockID: "base\n{: id=\"" + testBlockID + "\"}\n",
	}, []siyuan.ChildBlock{{ID: testBlockID, Type: "p"}}, nil)
	api.contents[testBlockID] = "remote changed\n{: id=\"" + testBlockID + "\"}\n"
	state, err := LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := LoadRepositoryRefs(root)
	if err != nil {
		t.Fatal(err)
	}
	working, err := syncer.scanWorkingTree(state)
	if err != nil {
		t.Fatal(err)
	}
	execution, err := syncer.beginPullExecution(NewObjectStore(root), refs, state, working)
	if err != nil {
		t.Fatal(err)
	}
	if err := execution.prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := execution.persist(PullOperationMaterializing, ""); err != nil {
		t.Fatal(err)
	}
	if len(execution.operation.Plans) != 1 || execution.operation.Plans[0].TargetContent == "" {
		t.Fatalf("pull plan = %+v", execution.operation.Plans)
	}
	target, err := LoadWorkingFileContent(execution.store, execution.operation.Plans[0].TargetContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(localPath, []byte(target), 0o644); err != nil {
		t.Fatal(err)
	}
	if len(execution.operation.MaterializedDocuments) != 0 {
		t.Fatal("test must simulate a crash before materialization progress was saved")
	}

	result, err := syncer.Pull(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Updated != 1 {
		t.Fatalf("resumed pull = %+v", result)
	}
	completedRefs, err := LoadRepositoryRefs(root)
	if err != nil {
		t.Fatal(err)
	}
	if completedRefs.Operation != "" {
		t.Fatalf("completed pull retained OperationState: %+v", completedRefs)
	}
}
