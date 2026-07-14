package worktree

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"siyuan-worktree/internal/config"
	"siyuan-worktree/internal/siyuan"
)

const (
	testNotebookID    = "20260714100000-nbook01"
	testParentID      = "20260714110000-parent1"
	testChildID       = "20260714110100-child01"
	testParentBlockID = "20260714120000-parentb"
	testBlockID       = "20260714120000-block01"
	testBlockID2      = "20260714120001-block02"
	testNestedBlockID = "20260714120002-nested1"
	testChildID2      = "20260714110101-child02"
	testDocBlockID2   = "20260714120003-block03"
)

type fakeAPI struct {
	mu                      sync.Mutex
	notebooks               []siyuan.Notebook
	documents               map[string][]siyuan.Document
	children                map[string][]siyuan.ChildBlock
	contents                map[string]string
	attrs                   map[string]map[string]string
	listDocumentsCalls      int
	getChildBlocksCalls     int
	getBlockKramdownsCalls  int
	beforeListDocuments     func(*fakeAPI, string, int)
	beforeGetChildBlocks    func(*fakeAPI, string, int)
	beforeGetBlockKramdowns func(*fakeAPI, []string, int)
	beforeBatchGetAttrs     func(*fakeAPI, []string, int)
	beforeUpdateBlock       func(*fakeAPI, string, string)
	batchAttrsCalls         int
	failBatchAttrsAt        int
	batchAttrsAfterUpdate   int
	failAttrsAfterUpdateAt  int
	dropAttrsOnUpdate       bool
	updates                 int
	histories               int
	historyCreated          []string
	createHistoryErr        error
	historySearchEmpty      bool
	historySearchErr        error
	mutationsBeforeHistory  int
	newCounter              int
}

func (f *fakeAPI) ListNotebooks(context.Context) ([]siyuan.Notebook, error) {
	return append([]siyuan.Notebook(nil), f.notebooks...), nil
}

func (f *fakeAPI) ListDocuments(_ context.Context, _ string, path string) ([]siyuan.Document, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listDocumentsCalls++
	if f.beforeListDocuments != nil {
		f.beforeListDocuments(f, path, f.listDocumentsCalls)
	}
	return append([]siyuan.Document(nil), f.documents[path]...), nil
}

func (f *fakeAPI) GetBlockKramdown(_ context.Context, id string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.contents[id], nil
}

func (f *fakeAPI) GetBlockKramdowns(_ context.Context, ids []string) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getBlockKramdownsCalls++
	if f.beforeGetBlockKramdowns != nil {
		f.beforeGetBlockKramdowns(f, append([]string(nil), ids...), f.getBlockKramdownsCalls)
	}
	result := make(map[string]string, len(ids))
	for _, id := range ids {
		result[id] = f.contents[id]
	}
	return result, nil
}

func (f *fakeAPI) BatchGetBlockAttrs(_ context.Context, ids []string) (map[string]map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batchAttrsCalls++
	if f.beforeBatchGetAttrs != nil {
		f.beforeBatchGetAttrs(f, append([]string(nil), ids...), f.batchAttrsCalls)
	}
	if f.failBatchAttrsAt > 0 && f.batchAttrsCalls == f.failBatchAttrsAt {
		return nil, errors.New("injected BatchGetBlockAttrs failure")
	}
	if f.updates > 0 {
		f.batchAttrsAfterUpdate++
		if f.failAttrsAfterUpdateAt > 0 && f.batchAttrsAfterUpdate == f.failAttrsAfterUpdateAt {
			return nil, errors.New("injected BatchGetBlockAttrs failure")
		}
	}
	result := make(map[string]map[string]string, len(ids))
	for _, id := range ids {
		attrs := map[string]string{}
		for name, value := range f.attrs[id] {
			attrs[name] = value
		}
		result[id] = attrs
	}
	return result, nil
}

func (f *fakeAPI) CreateDocHistory(_ context.Context, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createHistoryErr != nil {
		return f.createHistoryErr
	}
	f.histories++
	f.historyCreated = append(f.historyCreated, strconv.FormatInt(time.Now().Unix(), 10))
	return nil
}

func (f *fakeAPI) SearchHistory(_ context.Context, _ string) (siyuan.HistorySearchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.historySearchErr != nil {
		return siyuan.HistorySearchResult{}, f.historySearchErr
	}
	histories := append([]string(nil), f.historyCreated...)
	if f.historySearchEmpty {
		histories = []string{}
	}
	return siyuan.HistorySearchResult{Histories: histories, PageCount: 1, TotalCount: len(histories)}, nil
}

func (f *fakeAPI) InsertBlock(_ context.Context, markdown, parentID, previousID, nextID string) (siyuan.MutationReceipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.histories == 0 {
		f.mutationsBeforeHistory++
	}
	f.newCounter++
	id := fmt.Sprintf("20260714130000-new%04d", f.newCounter)
	f.contents[id] = Canonicalize(markdown) + fmt.Sprintf("{: id=\"%s\"}\n", id)
	if f.attrs == nil {
		f.attrs = map[string]map[string]string{}
	}
	f.attrs[id] = map[string]string{"id": id, "type": "p"}
	children := f.children[parentID]
	insertAt := len(children)
	if nextID != "" {
		for index, child := range children {
			if child.ID == nextID {
				insertAt = index
				break
			}
		}
	} else if previousID != "" {
		for index, child := range children {
			if child.ID == previousID {
				insertAt = index + 1
				break
			}
		}
	}
	children = append(children, siyuan.ChildBlock{})
	copy(children[insertAt+1:], children[insertAt:])
	children[insertAt] = siyuan.ChildBlock{ID: id, Type: "p"}
	f.children[parentID] = children
	return fakeMutationReceipt("insert", id, parentID), nil
}

func (f *fakeAPI) DeleteBlock(_ context.Context, id string) (siyuan.MutationReceipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.histories == 0 {
		f.mutationsBeforeHistory++
	}
	delete(f.contents, id)
	delete(f.attrs, id)
	for parentID, children := range f.children {
		filtered := children[:0]
		for _, child := range children {
			if child.ID != id {
				filtered = append(filtered, child)
			}
		}
		f.children[parentID] = filtered
	}
	return fakeMutationReceipt("delete", id, ""), nil
}

func (f *fakeAPI) GetChildBlocks(_ context.Context, id string) ([]siyuan.ChildBlock, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getChildBlocksCalls++
	if f.beforeGetChildBlocks != nil {
		f.beforeGetChildBlocks(f, id, f.getChildBlocksCalls)
	}
	return append([]siyuan.ChildBlock(nil), f.children[id]...), nil
}

func (f *fakeAPI) UpdateBlock(_ context.Context, id, markdown string) (siyuan.MutationReceipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.beforeUpdateBlock != nil {
		f.beforeUpdateBlock(f, id, markdown)
	}
	if f.histories == 0 {
		f.mutationsBeforeHistory++
	}
	f.contents[id] = markdown
	if f.dropAttrsOnUpdate {
		for _, blockID := range uniqueIDs(ExtractBlockIDs(markdown)) {
			if f.attrs[blockID] != nil {
				f.attrs[blockID] = map[string]string{"id": blockID, "type": f.attrs[blockID]["type"]}
			}
		}
	}
	f.updates++
	return fakeMutationReceipt("update", id, ""), nil
}

func fakeMutationReceipt(action, id, parentID string) siyuan.MutationReceipt {
	return siyuan.MutationReceipt{
		ReceivedAt: time.Now().UTC(),
		Transactions: []siyuan.Transaction{{
			Timestamp: time.Now().UnixMilli(),
			DoOperations: []siyuan.TransactionOperation{{
				Action:   action,
				ID:       id,
				ParentID: parentID,
			}},
		}},
	}
}

func TestGitStyleAddCommitPushAndMetadataRefresh(t *testing.T) {
	root := t.TempDir()
	cfg := config.Default()
	api := &fakeAPI{
		notebooks: []siyuan.Notebook{{ID: testNotebookID, Name: "Work"}},
		documents: map[string][]siyuan.Document{
			"/":                        {{ID: testParentID, Name: "Project", Path: "/" + testParentID + ".sy", SubFileCount: 1}},
			"/" + testParentID + ".sy": {{ID: testChildID, Name: "Design", Path: "/" + testParentID + "/" + testChildID + ".sy"}},
		},
		contents: map[string]string{
			testParentBlockID: "parent\n{: id=\"" + testParentBlockID + "\"}\n",
			testBlockID:       "child\n{: id=\"" + testBlockID + "\"}\n",
		},
		children: map[string][]siyuan.ChildBlock{
			testParentID: {{ID: testParentBlockID, Type: "p"}},
			testChildID:  {{ID: testBlockID, Type: "p"}},
		},
		attrs: map[string]map[string]string{
			testBlockID: {"id": testBlockID, "type": "p", "custom-owner": "agent"},
		},
	}
	syncer := NewSyncer(root, cfg, api)
	pull, err := syncer.Pull(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if pull.Added != 2 {
		t.Fatalf("pull = %+v", pull)
	}
	localPath := filepath.Join(root, "notes", "Work", "Project", "Design.md")
	local, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatal(err)
	}
	document, err := ParseAnnotated(string(local))
	if err != nil {
		t.Fatal(err)
	}
	document.Blocks[0].Content = "child changed\n{: id=\"" + testBlockID + "\"}\n"
	if err := os.WriteFile(localPath, []byte(RenderAnnotated(document)), 0o644); err != nil {
		t.Fatal(err)
	}

	add, err := syncer.Add(context.Background(), AddOptions{Paths: []string{"notes/Work/Project/Design.md"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(add.Staged) != 1 || add.Operations != 1 || api.updates != 0 {
		t.Fatalf("add = %+v", add)
	}
	stagedDiffs, err := syncer.StagedDiffs()
	if err != nil {
		t.Fatal(err)
	}
	if len(stagedDiffs) != 1 || !strings.Contains(stagedDiffs[0].Content, "child changed") {
		t.Fatalf("staged diffs = %+v", stagedDiffs)
	}
	status, err := syncer.RepositoryStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Staged) != 1 || len(status.PendingCommits) != 0 {
		t.Fatalf("repository status after add = %+v", status)
	}
	commit, err := syncer.Commit("update design")
	if err != nil {
		t.Fatal(err)
	}
	if commit.Message != "update design" || len(commit.DocumentPatches) != 1 {
		t.Fatalf("commit = %+v", commit)
	}
	status, err = syncer.RepositoryStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Staged) != 0 || len(status.PendingCommits) != 1 || status.PendingCommits[0].ID != commit.ID {
		t.Fatalf("repository status after commit = %+v", status)
	}

	push, err := syncer.Push(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if push.PushedCommits != 1 || push.PushedDocuments != 1 || api.updates != 1 || api.histories != 1 || api.mutationsBeforeHistory != 0 {
		t.Fatalf("push = %+v updates=%d histories=%d mutationsBeforeHistory=%d", push, api.updates, api.histories, api.mutationsBeforeHistory)
	}
	if api.attrs[testBlockID]["custom-owner"] != "agent" {
		t.Fatalf("custom attrs were not preserved: %+v", api.attrs[testBlockID])
	}
	storedCommits, err := syncer.Log()
	if err != nil {
		t.Fatal(err)
	}
	if len(storedCommits) != 0 {
		t.Fatalf("completed commits should not be retained: %+v", storedCommits)
	}
	metadataData, err := os.ReadFile(documentMetadataPath(root, testChildID))
	if err != nil {
		t.Fatal(err)
	}
	var metadata DocumentMetadata
	if err := json.Unmarshal(metadataData, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.RemoteHash != HashContent(apiRenderedDocument(t, api, testChildID)) || metadata.Blocks[0].Attrs["custom-owner"] != "agent" {
		t.Fatalf("metadata = %+v", metadata)
	}
}

func TestPullRecordsContentAddressedRemoteSnapshot(t *testing.T) {
	syncer, api, _, root := newSingleDocumentFixture(t, map[string]string{
		testBlockID: "base\n{: id=\"" + testBlockID + "\" custom-owner=\"agent\"}\n",
	}, []siyuan.ChildBlock{{ID: testBlockID, Type: "p"}}, nil)
	refs, err := LoadRepositoryRefs(root)
	if err != nil {
		t.Fatal(err)
	}
	if refs.Remote == "" || refs.Head == "" || refs.Index != refs.Remote {
		t.Fatalf("refs = %+v", refs)
	}
	store := NewObjectStore(root)
	var commit CommitObject
	if err := store.Get(refs.Head, commitObjectType, snapshotObjectVersion, &commit); err != nil {
		t.Fatal(err)
	}
	if commit.Tree != refs.Remote || commit.Kind != baselineCommitObjectKind {
		t.Fatalf("commit = %+v", commit)
	}
	var workspace WorkspaceTree
	if err := store.Get(refs.Remote, workspaceTreeObjectType, snapshotObjectVersion, &workspace); err != nil {
		t.Fatal(err)
	}
	if len(workspace.Documents) != 1 || workspace.Documents[0].ID != testChildID {
		t.Fatalf("workspace = %+v", workspace)
	}
	var document DocumentTree
	if err := store.Get(workspace.Documents[0].DocumentTreeID, documentTreeObjectType, snapshotObjectVersion, &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Blocks) != 1 || document.Blocks[0].BlockID != testBlockID {
		t.Fatalf("document = %+v", document)
	}
	var block BlockSnapshot
	if err := store.Get(document.Blocks[0].ObjectID, blockSnapshotObjectType, snapshotObjectVersion, &block); err != nil {
		t.Fatal(err)
	}
	if block.AttrsByBlockID[testBlockID]["custom-owner"] != "agent" {
		t.Fatalf("block snapshot = %+v", block)
	}
	if _, err := syncer.Pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	unchanged, err := LoadRepositoryRefs(root)
	if err != nil {
		t.Fatal(err)
	}
	unchanged.Generation = refs.Generation
	if unchanged != refs {
		t.Fatalf("unchanged pull moved refs: before=%+v after=%+v", refs, unchanged)
	}
	api.contents[testBlockID] = "remote changed\n{: id=\"" + testBlockID + "\" custom-owner=\"agent\"}\n"
	if _, err := syncer.Pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	advanced, err := LoadRepositoryRefs(root)
	if err != nil {
		t.Fatal(err)
	}
	if advanced.Generation <= refs.Generation || advanced.Head == refs.Head || advanced.Remote == refs.Remote || advanced.Index != advanced.Remote {
		t.Fatalf("advanced refs = %+v", advanced)
	}
	var nextCommit CommitObject
	if err := store.Get(advanced.Head, commitObjectType, snapshotObjectVersion, &nextCommit); err != nil {
		t.Fatal(err)
	}
	if nextCommit.BaseHead != "" || nextCommit.Tree != advanced.Remote {
		t.Fatalf("next commit = %+v", nextCommit)
	}
	if err := store.Get(refs.Head, commitObjectType, snapshotObjectVersion, nil); err == nil {
		t.Fatal("previous stable HEAD was not pruned after pull")
	}
}

func TestPullRetriesWhenRemoteDocumentContentChangesBetweenReads(t *testing.T) {
	syncer, api, localPath, root := newSingleDocumentFixture(t, map[string]string{
		testBlockID: "base\n{: id=\"" + testBlockID + "\"}\n",
	}, []siyuan.ChildBlock{{ID: testBlockID, Type: "p"}}, nil)

	api.mu.Lock()
	api.getBlockKramdownsCalls = 0
	api.contents[testBlockID] = "remote transient\n{: id=\"" + testBlockID + "\"}\n"
	api.beforeGetBlockKramdowns = func(api *fakeAPI, _ []string, call int) {
		if call == 2 {
			api.contents[testBlockID] = "remote stable\n{: id=\"" + testBlockID + "\"}\n"
		}
	}
	api.mu.Unlock()

	result, err := syncer.Pull(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Updated != 1 {
		t.Fatalf("pull = %+v", result)
	}
	local, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(local), "remote stable") || strings.Contains(string(local), "remote transient") {
		t.Fatalf("pull accepted a transient remote read: %q", local)
	}
	api.mu.Lock()
	readCalls := api.getBlockKramdownsCalls
	api.beforeGetBlockKramdowns = nil
	api.mu.Unlock()
	if readCalls < 3 {
		t.Fatalf("pull did not retry the changing remote document: GetBlockKramdowns calls=%d", readCalls)
	}

	refs, err := LoadRepositoryRefs(root)
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := LoadWorkspaceTree(NewObjectStore(root), refs.Remote)
	if err != nil {
		t.Fatal(err)
	}
	document, ok := WorkspaceDocumentByID(workspace, testChildID)
	if !ok {
		t.Fatal("Remote snapshot is missing the document")
	}
	remote, err := RenderDocumentTree(NewObjectStore(root), document.DocumentTreeID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(remote, "remote stable") || strings.Contains(remote, "remote transient") {
		t.Fatalf("Remote ref points to a transient observation: %q", remote)
	}
}

func TestPullRetriesWhenRemoteAttributesChangeBetweenReads(t *testing.T) {
	syncer, api, _, root := newSingleDocumentFixture(t, map[string]string{
		testBlockID: "base\n{: id=\"" + testBlockID + "\"}\n",
	}, []siyuan.ChildBlock{{ID: testBlockID, Type: "p"}}, map[string]map[string]string{
		testBlockID: {"id": testBlockID, "type": "p", "custom-state": "base"},
	})

	api.mu.Lock()
	api.batchAttrsCalls = 0
	api.attrs[testBlockID]["custom-state"] = "transient"
	api.beforeBatchGetAttrs = func(api *fakeAPI, _ []string, call int) {
		if call == 2 {
			api.attrs[testBlockID]["custom-state"] = "stable"
		}
	}
	api.mu.Unlock()

	if _, err := syncer.Pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(documentMetadataPath(root, testChildID))
	if err != nil {
		t.Fatal(err)
	}
	var metadata DocumentMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		t.Fatal(err)
	}
	if len(metadata.Blocks) != 1 || metadata.Blocks[0].Attrs["custom-state"] != "stable" {
		t.Fatalf("pull accepted transient attributes: %+v", metadata)
	}
	api.mu.Lock()
	attrCalls := api.batchAttrsCalls
	api.beforeBatchGetAttrs = nil
	api.mu.Unlock()
	if attrCalls < 3 {
		t.Fatalf("pull did not retry changing remote attributes: BatchGetBlockAttrs calls=%d", attrCalls)
	}
}

func TestPullRechecksEarlierDocumentsAcrossWorkspaceObservationRounds(t *testing.T) {
	syncer, api, firstPath, _, _ := newTwoDocumentFixture(t)

	api.mu.Lock()
	api.getBlockKramdownsCalls = 0
	changed := false
	api.beforeGetBlockKramdowns = func(api *fakeAPI, ids []string, _ int) {
		if changed || !containsString(ids, testDocBlockID2) {
			return
		}
		changed = true
		api.contents[testBlockID] = "first changed while second was read\n{: id=\"" + testBlockID + "\"}\n"
	}
	api.mu.Unlock()

	result, err := syncer.Pull(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Updated != 1 {
		t.Fatalf("pull = %+v", result)
	}
	first, err := os.ReadFile(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(first), "first changed while second was read") {
		t.Fatalf("pull accepted a mixed-time workspace observation: %q", first)
	}
	api.mu.Lock()
	readCalls := api.getBlockKramdownsCalls
	api.beforeGetBlockKramdowns = nil
	api.mu.Unlock()
	if readCalls < 6 {
		t.Fatalf("pull did not repeat the complete two-document observation: GetBlockKramdowns calls=%d", readCalls)
	}
}

func TestPullRejectsContinuouslyChangingRemoteDocumentStructure(t *testing.T) {
	syncer, api, localPath, root := newSingleDocumentFixture(t, map[string]string{
		testBlockID: "base\n{: id=\"" + testBlockID + "\"}\n",
	}, []siyuan.ChildBlock{{ID: testBlockID, Type: "p"}}, nil)
	before, err := LoadRepositoryRefs(root)
	if err != nil {
		t.Fatal(err)
	}

	const addedBlockID = "20260714120003-block03"
	api.mu.Lock()
	api.getChildBlocksCalls = 0
	api.contents[testBlockID] = "remote changed\n{: id=\"" + testBlockID + "\"}\n"
	api.contents[addedBlockID] = "concurrent block\n{: id=\"" + addedBlockID + "\"}\n"
	api.beforeGetChildBlocks = func(api *fakeAPI, id string, call int) {
		if id != testChildID {
			return
		}
		children := []siyuan.ChildBlock{{ID: testBlockID, Type: "p"}}
		if call%2 == 0 {
			children = append(children, siyuan.ChildBlock{ID: addedBlockID, Type: "p"})
		}
		api.children[testChildID] = children
	}
	api.mu.Unlock()

	if _, err := syncer.Pull(context.Background()); err == nil || !strings.Contains(strings.ToLower(err.Error()), "remote unstable") {
		t.Fatalf("expected remote unstable error, got %v", err)
	}
	after, err := LoadRepositoryRefs(root)
	if err != nil {
		t.Fatal(err)
	}
	assertPullContentRefsUnchanged(t, before, after)
	local, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(local), "base") || strings.Contains(string(local), "remote changed") {
		t.Fatalf("unstable pull modified the working file: %q", local)
	}
}

func TestPullRejectsChangingInventoryWithoutAdvancingContentRefs(t *testing.T) {
	syncer, api, _, root := newSingleDocumentFixture(t, map[string]string{
		testBlockID: "base\n{: id=\"" + testBlockID + "\"}\n",
	}, []siyuan.ChildBlock{{ID: testBlockID, Type: "p"}}, nil)
	before, err := LoadRepositoryRefs(root)
	if err != nil {
		t.Fatal(err)
	}

	const addedDocumentID = "20260714110101-child02"
	const addedBlockID = "20260714120003-block03"
	baseDocuments := []siyuan.Document{{ID: testChildID, Name: "Design", Path: "/" + testChildID + ".sy"}}
	addedDocument := siyuan.Document{ID: addedDocumentID, Name: "Added", Path: "/" + addedDocumentID + ".sy"}
	api.mu.Lock()
	api.listDocumentsCalls = 0
	api.children[addedDocumentID] = []siyuan.ChildBlock{{ID: addedBlockID, Type: "p"}}
	api.contents[addedBlockID] = "added concurrently\n{: id=\"" + addedBlockID + "\"}\n"
	api.beforeListDocuments = func(api *fakeAPI, path string, call int) {
		if path != "/" {
			return
		}
		api.documents[path] = append([]siyuan.Document(nil), baseDocuments...)
		if call%2 == 0 {
			api.documents[path] = append(api.documents[path], addedDocument)
		}
	}
	api.mu.Unlock()

	if _, err := syncer.Pull(context.Background()); err == nil || !strings.Contains(strings.ToLower(err.Error()), "remote unstable") {
		t.Fatalf("expected remote unstable error, got %v", err)
	}
	after, err := LoadRepositoryRefs(root)
	if err != nil {
		t.Fatal(err)
	}
	assertPullContentRefsUnchanged(t, before, after)
	if _, err := os.Stat(filepath.Join(root, "notes", "Work", "Added.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unstable inventory was materialized, stat error=%v", err)
	}
}

func TestAddAndCommitAdvanceIndexAndHeadSnapshots(t *testing.T) {
	syncer, _, localPath, root := newSingleDocumentFixture(t, map[string]string{
		testBlockID: "base\n{: id=\"" + testBlockID + "\"}\n",
	}, []siyuan.ChildBlock{{ID: testBlockID, Type: "p"}}, nil)
	initial, err := LoadRepositoryRefs(root)
	if err != nil {
		t.Fatal(err)
	}
	writeSingleBlock(t, localPath, testBlockID, "staged")
	if _, err := syncer.Add(context.Background(), AddOptions{All: true}); err != nil {
		t.Fatal(err)
	}
	stagedRefs, err := LoadRepositoryRefs(root)
	if err != nil {
		t.Fatal(err)
	}
	if stagedRefs.Head != initial.Head || stagedRefs.Remote != initial.Remote || stagedRefs.Index == initial.Index || stagedRefs.IndexPatch == "" {
		t.Fatalf("staged refs = %+v, initial = %+v", stagedRefs, initial)
	}
	store := NewObjectStore(root)
	patch, err := LoadPatch(store, stagedRefs.IndexPatch)
	if err != nil {
		t.Fatal(err)
	}
	if patch.BaseTree != initial.Index || patch.TargetTree != stagedRefs.Index || len(patch.DocumentPatches) != 1 {
		t.Fatalf("patch = %+v", patch)
	}
	if err := ValidatePatch(store, patch); err != nil {
		t.Fatal(err)
	}
	commit, err := syncer.Commit("snapshot commit")
	if err != nil {
		t.Fatal(err)
	}
	committedRefs, err := LoadRepositoryRefs(root)
	if err != nil {
		t.Fatal(err)
	}
	if committedRefs.Head != commit.ObjectID || committedRefs.Index != stagedRefs.Index || committedRefs.IndexPatch != "" || committedRefs.Remote != initial.Remote {
		t.Fatalf("committed refs = %+v", committedRefs)
	}
	commitObject, err := LoadCommitObject(store, commit.ObjectID)
	if err != nil {
		t.Fatal(err)
	}
	if commitObject.Kind != userCommitObjectKind || commitObject.Tree != stagedRefs.Index || commitObject.BaseHead != initial.Head ||
		commitObject.RemoteBase != initial.Remote || commitObject.Patch != stagedRefs.IndexPatch {
		t.Fatalf("commit object = %+v", commitObject)
	}
}

func TestPushConvergesToCanonicalStableState(t *testing.T) {
	syncer, api, localPath, root := newSingleDocumentFixture(t, map[string]string{
		testBlockID: "base\n{: id=\"" + testBlockID + "\"}\n",
	}, []siyuan.ChildBlock{{ID: testBlockID, Type: "p"}}, nil)
	local, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(localPath, append(local, []byte("new local block\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	stageAndCommitAll(t, syncer, "insert")
	pending := loadPendingCommit(t, root)
	userCommitID := pending.ObjectID
	if userCommitID == "" {
		t.Fatal("pending commit has no immutable object ID")
	}
	refs, err := LoadRepositoryRefs(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := syncer.beginPushExecution(NewObjectStore(root), refs, &pending); err != nil {
		t.Fatal(err)
	}
	refs, err = LoadRepositoryRefs(root)
	if err != nil {
		t.Fatal(err)
	}
	activeOperation := refs.Operation
	if activeOperation == "" {
		t.Fatal("push operation was not created")
	}
	result, err := syncer.Push(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.PushedCommits != 1 || api.newCounter != 1 {
		t.Fatalf("push = %+v inserts=%d", result, api.newCounter)
	}
	canonical, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatal(err)
	}
	canonicalDocument, err := ParseAnnotated(string(canonical))
	if err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	children := append([]siyuan.ChildBlock(nil), api.children[testChildID]...)
	api.mu.Unlock()
	if len(children) != 2 || len(canonicalDocument.Blocks) != 2 || canonicalDocument.Blocks[1].ID != children[1].ID ||
		!strings.Contains(canonicalDocument.Blocks[1].Content, "new local block") {
		t.Fatalf("canonical insert = %+v", canonicalDocument)
	}
	refs, err = LoadRepositoryRefs(root)
	if err != nil {
		t.Fatal(err)
	}
	if refs.Operation != "" || refs.Remote == "" || refs.Index != refs.Remote {
		t.Fatalf("final refs = %+v", refs)
	}
	store := NewObjectStore(root)
	resultCommit, err := LoadCommitObject(store, refs.Head)
	if err != nil {
		t.Fatal(err)
	}
	if resultCommit.Kind != baselineCommitObjectKind || resultCommit.BaseHead != "" || resultCommit.Tree != refs.Remote || resultCommit.RemoteBase != refs.Remote {
		t.Fatalf("result commit = %+v", resultCommit)
	}
	if err := store.Get(userCommitID, commitObjectType, snapshotObjectVersion, nil); err == nil {
		t.Fatal("completed user CommitObject was not pruned")
	}
	if err := store.Get(activeOperation, pushOperationObjectType, snapshotObjectVersion, nil); err == nil {
		t.Fatal("completed PushOperationState was not pruned")
	}
	workspace, err := LoadWorkspaceTree(store, refs.Remote)
	if err != nil {
		t.Fatal(err)
	}
	document, ok := WorkspaceDocumentByID(workspace, testChildID)
	if !ok {
		t.Fatal("canonical workspace is missing the document")
	}
	documentTree, err := LoadDocumentTree(store, document.DocumentTreeID)
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := RenderDocumentTree(store, document.DocumentTreeID)
	if err != nil {
		t.Fatal(err)
	}
	if baseline != string(canonical) {
		t.Fatal("Remote snapshot was not advanced to canonical content")
	}
	for _, blockRef := range documentTree.Blocks {
		var block BlockSnapshot
		if err := store.Get(blockRef.ObjectID, blockSnapshotObjectType, snapshotObjectVersion, &block); err != nil {
			t.Fatal(err)
		}
		if block.Provisional {
			t.Fatalf("canonical snapshot retained provisional block %+v", block)
		}
	}
	if commits, err := ListPendingCommits(root); err != nil {
		t.Fatal(err)
	} else if len(commits) != 0 {
		t.Fatalf("completed pending Commit was retained: %+v", commits)
	}
}

func TestCanonicalDocumentJournalDoesNotAdvanceGlobalPushPhase(t *testing.T) {
	syncer, _, localPath, root := newSingleDocumentFixture(t, map[string]string{
		testBlockID: "base\n{: id=\"" + testBlockID + "\"}\n",
	}, []siyuan.ChildBlock{{ID: testBlockID, Type: "p"}}, nil)
	writeSingleBlock(t, localPath, testBlockID, "target")
	commit := stageAndCommitAll(t, syncer, "update")
	refs, err := LoadRepositoryRefs(root)
	if err != nil {
		t.Fatal(err)
	}
	store := NewObjectStore(root)
	execution, err := syncer.beginPushExecution(store, refs, &commit)
	if err != nil {
		t.Fatal(err)
	}
	persistBasePreflight(t, execution, &commit)
	refs, err = LoadRepositoryRefs(root)
	if err != nil {
		t.Fatal(err)
	}
	preparedOperationID := refs.Operation
	if preparedOperationID == "" {
		t.Fatal("prepared push did not persist an OperationState")
	}
	commitObject, err := LoadCommitObject(store, commit.ObjectID)
	if err != nil {
		t.Fatal(err)
	}
	target, err := LoadWorkspaceTree(store, commitObject.Tree)
	if err != nil {
		t.Fatal(err)
	}
	document, ok := WorkspaceDocumentByID(target, testChildID)
	if !ok {
		t.Fatal("target snapshot is missing the document")
	}
	commit.DocumentPatches[0].AppliedOperations = len(commit.DocumentPatches[0].Operations)
	commit.DocumentPatches[0].InFlightOperation = nil
	commit.DocumentPatches[0].Status = DocumentPatchApplied
	commit.AppliedDocuments = 1
	if err := execution.recordCanonicalDocument(&commit, testChildID, document.DocumentTreeID); err != nil {
		t.Fatal(err)
	}
	refs, err = LoadRepositoryRefs(root)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := LoadPushOperation(store, refs.Operation)
	if err != nil {
		t.Fatal(err)
	}
	if operation.Phase != PushOperationApplying {
		t.Fatalf("canonical document journal advanced the global phase: %s", operation.Phase)
	}
	if refs.Operation == preparedOperationID {
		t.Fatal("canonical document journal did not advance OperationState")
	}
	if err := store.Get(preparedOperationID, pushOperationObjectType, snapshotObjectVersion, nil); err == nil {
		t.Fatal("superseded OperationState was not pruned")
	}
}

func TestPushResumesCanonicalMaterializationFromOperationJournal(t *testing.T) {
	syncer, api, localPath, root := newSingleDocumentFixture(t, map[string]string{
		testBlockID: "base\n{: id=\"" + testBlockID + "\"}\n",
	}, []siyuan.ChildBlock{{ID: testBlockID, Type: "p"}}, nil)
	writeSingleBlock(t, localPath, testBlockID, "canonical target")
	stageAndCommitAll(t, syncer, "update")
	api.mu.Lock()
	api.failAttrsAfterUpdateAt = 4
	api.mu.Unlock()
	if _, err := syncer.Push(context.Background()); err == nil || !strings.Contains(err.Error(), "injected BatchGetBlockAttrs failure") {
		t.Fatalf("expected injected materialization failure, got %v", err)
	}
	if api.updates != 1 {
		t.Fatalf("updates after interrupted push = %d", api.updates)
	}
	refs, err := LoadRepositoryRefs(root)
	if err != nil {
		t.Fatal(err)
	}
	if refs.Operation == "" {
		t.Fatal("interrupted push did not retain OperationState")
	}
	operation, err := LoadPushOperation(NewObjectStore(root), refs.Operation)
	if err != nil {
		t.Fatal(err)
	}
	if operation.Phase != PushOperationMaterializing || operation.CanonicalTree == "" || len(operation.CanonicalDocuments) != 1 || len(operation.MaterializedDocuments) != 0 {
		t.Fatalf("interrupted operation = %+v", operation)
	}
	runtimePatch := operation.Commit.DocumentPatches[0]
	if runtimePatch.HistoryCheckpoint == nil || runtimePatch.HistoryCheckpoint.Status != HistoryCheckpointVerified ||
		runtimePatch.Operations[0].KernelReceipt == nil || len(runtimePatch.Operations[0].ReceiptBlockIDs) == 0 {
		t.Fatalf("interrupted push evidence = %+v", runtimePatch)
	}
	api.mu.Lock()
	api.failAttrsAfterUpdateAt = 0
	api.mu.Unlock()
	result, err := syncer.Push(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.PushedCommits != 1 || api.updates != 1 {
		t.Fatalf("resumed push = %+v updates=%d", result, api.updates)
	}
	finalRefs, err := LoadRepositoryRefs(root)
	if err != nil {
		t.Fatal(err)
	}
	if finalRefs.Operation != "" || finalRefs.Index != finalRefs.Remote {
		t.Fatalf("final refs = %+v", finalRefs)
	}
}

func TestPushDoesNotOverwriteEditingDuringCanonicalMaterialization(t *testing.T) {
	syncer, api, localPath, _ := newSingleDocumentFixture(t, map[string]string{
		testBlockID: "base\n{: id=\"" + testBlockID + "\"}\n",
	}, []siyuan.ChildBlock{{ID: testBlockID, Type: "p"}}, nil)
	writeSingleBlock(t, localPath, testBlockID, "committed target")
	stageAndCommitAll(t, syncer, "update")

	newEditorContent := RenderAnnotated(AnnotatedDocument{Blocks: []AnnotatedBlock{{
		ID: testBlockID, Type: "p", Content: "editor saved during materialization\n{: id=\"" + testBlockID + "\"}\n",
	}}})
	var hookErr error
	api.mu.Lock()
	api.beforeBatchGetAttrs = func(api *fakeAPI, _ []string, _ int) {
		if api.updates == 0 || api.batchAttrsAfterUpdate != 3 || hookErr != nil {
			return
		}
		hookErr = os.WriteFile(localPath, []byte(newEditorContent), 0o644)
	}
	api.mu.Unlock()

	_, err := syncer.Push(context.Background())
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if err == nil || !strings.Contains(err.Error(), "working tree changed during canonical materialization") {
		t.Fatalf("expected materialization race rejection, got %v", err)
	}
	local, readErr := os.ReadFile(localPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(local) != newEditorContent {
		t.Fatalf("push overwrote the editor's newer save: %q", local)
	}
}

func TestConflictedPullAdvancesRemoteRefWithoutMovingHead(t *testing.T) {
	syncer, api, localPath, root := newSingleDocumentFixture(t, map[string]string{
		testBlockID: "base\n{: id=\"" + testBlockID + "\"}\n",
	}, []siyuan.ChildBlock{{ID: testBlockID, Type: "p"}}, nil)
	before, err := LoadRepositoryRefs(root)
	if err != nil {
		t.Fatal(err)
	}
	writeSingleBlock(t, localPath, testBlockID, "local")
	api.contents[testBlockID] = "remote\n{: id=\"" + testBlockID + "\"}\n"
	result, err := syncer.Pull(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Conflicts) != 1 {
		t.Fatalf("pull = %+v", result)
	}
	after, err := LoadRepositoryRefs(root)
	if err != nil {
		t.Fatal(err)
	}
	if after.Remote == before.Remote {
		t.Fatal("remote ref did not advance")
	}
	if after.Head != before.Head || after.Index != before.Index {
		t.Fatalf("conflicted pull moved local refs: before=%+v after=%+v", before, after)
	}
}

func TestCommitUsesIndexAndPushProtectsDirtyWorkingTree(t *testing.T) {
	syncer, api, localPath, root := newSingleDocumentFixture(t, map[string]string{
		testBlockID: "base\n{: id=\"" + testBlockID + "\"}\n",
	}, []siyuan.ChildBlock{{ID: testBlockID, Type: "p"}}, nil)
	writeSingleBlock(t, localPath, testBlockID, "first edit")
	if push, err := syncer.Push(context.Background()); err != nil || push.PushedCommits != 0 || api.updates != 0 {
		t.Fatalf("push without commit = %+v err=%v", push, err)
	}
	if _, err := syncer.Add(context.Background(), AddOptions{All: true}); err != nil {
		t.Fatal(err)
	}
	writeSingleBlock(t, localPath, testBlockID, "second edit")
	commit, err := syncer.Commit("first edit")
	if err != nil {
		t.Fatal(err)
	}
	store := NewObjectStore(root)
	commitObject, err := LoadCommitObject(store, commit.ObjectID)
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := LoadWorkspaceTree(store, commitObject.Tree)
	if err != nil {
		t.Fatal(err)
	}
	document, ok := WorkspaceDocumentByID(workspace, testChildID)
	if !ok {
		t.Fatal("committed snapshot is missing the document")
	}
	committed, err := RenderDocumentTree(store, document.DocumentTreeID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(committed, "first edit") || strings.Contains(committed, "second edit") {
		t.Fatalf("committed snapshot = %q", committed)
	}
	if _, err := syncer.Push(context.Background()); err == nil || !strings.Contains(err.Error(), "working tree changed after commit") {
		t.Fatalf("expected dirty working tree push rejection, got %v", err)
	}
	reset, err := syncer.Reset(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(reset.DiscardedCommits) != 1 {
		t.Fatalf("reset = %+v", reset)
	}
	if err := store.Get(commit.ObjectID, commitObjectType, snapshotObjectVersion, nil); err == nil {
		t.Fatal("reset did not prune the discarded CommitObject")
	}
}

func TestPushPreflightsEveryDocumentBeforeCreatingHistoryOrMutatingKernel(t *testing.T) {
	syncer, api, firstPath, secondPath, _ := newTwoDocumentFixture(t)
	writeSingleBlock(t, firstPath, testBlockID, "first local")
	writeSingleBlock(t, secondPath, testDocBlockID2, "second local")
	commit := stageAndCommitAll(t, syncer, "update two documents")
	if len(commit.DocumentPatches) != 2 || commit.DocumentPatches[0].DocumentID != testChildID || commit.DocumentPatches[1].DocumentID != testChildID2 {
		t.Fatalf("unexpected document patch order: %+v", commit.DocumentPatches)
	}

	api.mu.Lock()
	firstBefore := api.contents[testBlockID]
	secondRemote := "second remote conflict\n{: id=\"" + testDocBlockID2 + "\"}\n"
	api.contents[testDocBlockID2] = secondRemote
	firstChildrenBefore := append([]siyuan.ChildBlock(nil), api.children[testChildID]...)
	secondChildrenBefore := append([]siyuan.ChildBlock(nil), api.children[testChildID2]...)
	api.mu.Unlock()

	result, err := syncer.Push(context.Background())
	api.mu.Lock()
	if api.histories != 0 || api.updates != 0 || api.newCounter != 0 || api.mutationsBeforeHistory != 0 {
		t.Fatalf("push produced side effects before full preflight: histories=%d updates=%d inserts=%d mutationsBeforeHistory=%d", api.histories, api.updates, api.newCounter, api.mutationsBeforeHistory)
	}
	if api.contents[testBlockID] != firstBefore || api.contents[testDocBlockID2] != secondRemote {
		t.Fatalf("push changed remote content before full preflight: first=%q second=%q", api.contents[testBlockID], api.contents[testDocBlockID2])
	}
	if !reflect.DeepEqual(api.children[testChildID], firstChildrenBefore) || !reflect.DeepEqual(api.children[testChildID2], secondChildrenBefore) {
		t.Fatalf("push inserted or deleted blocks before full preflight: first=%+v second=%+v", api.children[testChildID], api.children[testChildID2])
	}
	api.mu.Unlock()
	if err != nil {
		t.Fatalf("push did not persist and report the preflight conflict: %v", err)
	}
	if len(result.Conflicts) != 1 {
		t.Fatalf("push should report the second document preflight conflict: %+v", result)
	}
}

func TestPushPersistsCompletePreflightDocumentsBeforeApplying(t *testing.T) {
	syncer, api, firstPath, secondPath, root := newTwoDocumentFixture(t)
	writeSingleBlock(t, firstPath, testBlockID, "first local")
	writeSingleBlock(t, secondPath, testDocBlockID2, "second local")
	commit := stageAndCommitAll(t, syncer, "update two documents")

	api.mu.Lock()
	api.createHistoryErr = errors.New("injected create history failure")
	api.mu.Unlock()
	if _, err := syncer.Push(context.Background()); err == nil || !strings.Contains(err.Error(), "injected create history failure") {
		t.Fatalf("expected injected history failure after preflight, got %v", err)
	}

	refs, err := LoadRepositoryRefs(root)
	if err != nil {
		t.Fatal(err)
	}
	if refs.Operation == "" {
		t.Fatal("failed push did not retain its OperationState")
	}
	store := NewObjectStore(root)
	operation, err := LoadPushOperation(store, refs.Operation)
	if err != nil {
		t.Fatal(err)
	}
	if operation.Phase != PushOperationApplying {
		t.Fatalf("push reached history creation before entering applying: phase=%s", operation.Phase)
	}
	if len(operation.PreflightDocuments) != len(commit.DocumentPatches) {
		t.Fatalf("preflight journal is incomplete: got=%+v patches=%+v", operation.PreflightDocuments, commit.DocumentPatches)
	}
	for _, documentID := range []string{testChildID, testChildID2} {
		documentTreeID := operation.PreflightDocuments[documentID]
		if documentTreeID == "" {
			t.Fatalf("preflight journal is missing document %s", documentID)
		}
		document, err := LoadDocumentTree(store, documentTreeID)
		if err != nil {
			t.Fatal(err)
		}
		if document.DocumentID != documentID {
			t.Fatalf("preflight document %s points to %s", documentID, document.DocumentID)
		}
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.histories != 0 || api.updates != 0 || api.newCounter != 0 {
		t.Fatalf("history failure produced Kernel effects: histories=%d updates=%d inserts=%d", api.histories, api.updates, api.newCounter)
	}
}

func TestPushRechecksAllDocumentsWhenResumingApplyingWithoutRemoteEffects(t *testing.T) {
	syncer, api, firstPath, secondPath, root := newTwoDocumentFixture(t)
	writeSingleBlock(t, firstPath, testBlockID, "first local")
	writeSingleBlock(t, secondPath, testDocBlockID2, "second local")
	commit := stageAndCommitAll(t, syncer, "update two documents")

	refs, err := LoadRepositoryRefs(root)
	if err != nil {
		t.Fatal(err)
	}
	execution, err := syncer.beginPushExecution(NewObjectStore(root), refs, &commit)
	if err != nil {
		t.Fatal(err)
	}
	persistBasePreflight(t, execution, &commit)
	commit.Status = CommitPushing
	if err := execution.persist(&commit, PushOperationApplying, ""); err != nil {
		t.Fatal(err)
	}

	api.mu.Lock()
	firstBefore := api.contents[testBlockID]
	api.contents[testDocBlockID2] = "second remote conflict\n{: id=\"" + testDocBlockID2 + "\"}\n"
	api.mu.Unlock()

	result, err := syncer.Push(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Conflicts) != 1 {
		t.Fatalf("expected resumed full-preflight conflict, got %+v", result)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.histories != 0 || api.updates != 0 || api.newCounter != 0 || api.contents[testBlockID] != firstBefore {
		t.Fatalf("resumed push produced effects before rechecking every document: histories=%d updates=%d inserts=%d first=%q", api.histories, api.updates, api.newCounter, api.contents[testBlockID])
	}
}

func TestPushResumesApplyingWithoutRemoteEffectsAfterPreflightSucceeds(t *testing.T) {
	syncer, api, localPath, _ := newSingleDocumentFixture(t, map[string]string{
		testBlockID: "base\n{: id=\"" + testBlockID + "\"}\n",
	}, []siyuan.ChildBlock{{ID: testBlockID, Type: "p"}}, nil)
	writeSingleBlock(t, localPath, testBlockID, "local")
	stageAndCommitAll(t, syncer, "update")

	api.mu.Lock()
	api.createHistoryErr = errors.New("injected create history failure")
	api.mu.Unlock()
	if _, err := syncer.Push(context.Background()); err == nil || !strings.Contains(err.Error(), "injected create history failure") {
		t.Fatalf("expected injected history failure, got %v", err)
	}

	api.mu.Lock()
	api.createHistoryErr = nil
	api.mu.Unlock()
	result, err := syncer.Push(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.PushedCommits != 1 || result.PushedDocuments != 1 || api.updates != 1 || api.histories != 1 {
		t.Fatalf("resumed push = %+v updates=%d histories=%d", result, api.updates, api.histories)
	}
}

func TestPushCanRetryAfterPreparedPreflightConflictIsRemoved(t *testing.T) {
	syncer, api, localPath, _ := newSingleDocumentFixture(t, map[string]string{
		testBlockID: "base\n{: id=\"" + testBlockID + "\"}\n",
	}, []siyuan.ChildBlock{{ID: testBlockID, Type: "p"}}, nil)
	writeSingleBlock(t, localPath, testBlockID, "local")
	stageAndCommitAll(t, syncer, "local change")

	api.mu.Lock()
	api.contents[testBlockID] = "remote conflict\n{: id=\"" + testBlockID + "\"}\n"
	api.mu.Unlock()
	first, err := syncer.Push(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Conflicts) != 1 || api.updates != 0 || api.histories != 0 {
		t.Fatalf("first push = %+v updates=%d histories=%d", first, api.updates, api.histories)
	}

	api.mu.Lock()
	api.contents[testBlockID] = "base\n{: id=\"" + testBlockID + "\"}\n"
	api.mu.Unlock()
	second, err := syncer.Push(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if second.PushedCommits != 1 || api.updates != 1 || api.histories != 1 {
		t.Fatalf("retried push = %+v updates=%d histories=%d", second, api.updates, api.histories)
	}
}

func TestUpdateRefusesConcurrentAttributeChangeBeforeMutation(t *testing.T) {
	attrs := map[string]map[string]string{
		testBlockID: {"id": testBlockID, "type": "p", "custom-owner": "agent"},
	}
	syncer, api, localPath, _ := newSingleDocumentFixture(t, map[string]string{
		testBlockID: "base\n{: id=\"" + testBlockID + "\" custom-owner=\"agent\"}\n",
	}, []siyuan.ChildBlock{{ID: testBlockID, Type: "p"}}, attrs)
	target := AnnotatedDocument{Blocks: []AnnotatedBlock{{
		ID: testBlockID, Type: "p", Content: "target\n{: id=\"" + testBlockID + "\" custom-owner=\"agent\"}\n",
	}}}
	if err := os.WriteFile(localPath, []byte(RenderAnnotated(target)), 0o644); err != nil {
		t.Fatal(err)
	}
	add, err := syncer.Add(context.Background(), AddOptions{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(add.Staged) != 1 {
		t.Fatalf("add = %+v", add)
	}
	patches, err := ListStagedDocumentPatches(syncer.root)
	if err != nil {
		t.Fatal(err)
	}
	operation := &patches[0].Operations[0]
	if err := syncer.prepareOperation(context.Background(), operation); err != nil {
		t.Fatal(err)
	}

	api.mu.Lock()
	api.attrs[testBlockID]["custom-owner"] = "other-client"
	api.mu.Unlock()
	err = syncer.applyUpdateOperation(context.Background(), operation, func() error { return nil })
	if err == nil || !strings.Contains(err.Error(), "attribute custom-owner was not preserved") {
		t.Fatalf("expected concurrent attribute conflict, got %v", err)
	}
	if api.updates != 0 {
		t.Fatalf("attribute conflict still updated the block: updates=%d", api.updates)
	}
}

func TestUpdateStopsIfKernelDoesNotPreserveReadOnlyAttributes(t *testing.T) {
	attrs := map[string]map[string]string{
		testBlockID: {"id": testBlockID, "type": "p", "custom-owner": "agent"},
	}
	syncer, api, localPath, _ := newSingleDocumentFixture(t, map[string]string{
		testBlockID: "base\n{: id=\"" + testBlockID + "\" custom-owner=\"agent\"}\n",
	}, []siyuan.ChildBlock{{ID: testBlockID, Type: "p"}}, attrs)
	target := AnnotatedDocument{Blocks: []AnnotatedBlock{{
		ID: testBlockID, Type: "p", Content: "target\n{: id=\"" + testBlockID + "\" custom-owner=\"agent\"}\n",
	}}}
	if err := os.WriteFile(localPath, []byte(RenderAnnotated(target)), 0o644); err != nil {
		t.Fatal(err)
	}
	stageAndCommitAll(t, syncer, "update")
	api.mu.Lock()
	api.dropAttrsOnUpdate = true
	api.mu.Unlock()

	result, err := syncer.Push(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Conflicts) != 1 || !strings.Contains(result.Conflicts[0], "attribute custom-owner was not preserved") {
		t.Fatalf("expected post-update attribute conflict, got %+v", result)
	}
	if api.updates != 1 {
		t.Fatalf("expected one verified Kernel update, got %d", api.updates)
	}
}

func TestResetRequiresForceAfterConfirmedKernelMutation(t *testing.T) {
	syncer, api, localPath, root := newSingleDocumentFixture(t, map[string]string{
		testBlockID: "base\n{: id=\"" + testBlockID + "\"}\n",
	}, []siyuan.ChildBlock{{ID: testBlockID, Type: "p"}}, nil)
	writeSingleBlock(t, localPath, testBlockID, "target")
	commit := stageAndCommitAll(t, syncer, "update")
	refs, err := LoadRepositoryRefs(root)
	if err != nil {
		t.Fatal(err)
	}
	execution, err := syncer.beginPushExecution(NewObjectStore(root), refs, &commit)
	if err != nil {
		t.Fatal(err)
	}
	persistBasePreflight(t, execution, &commit)
	patch := &commit.DocumentPatches[0]
	operation := &patch.Operations[0]
	if err := syncer.prepareOperation(context.Background(), operation); err != nil {
		t.Fatal(err)
	}
	persist := func() error { return execution.persist(&commit, PushOperationApplying, "") }
	if err := persist(); err != nil {
		t.Fatal(err)
	}
	inFlight := 0
	patch.InFlightOperation = &inFlight
	if err := persist(); err != nil {
		t.Fatal(err)
	}
	if err := syncer.applyOperation(context.Background(), operation, persist); err != nil {
		t.Fatal(err)
	}
	patch.AppliedOperations = 1
	patch.InFlightOperation = nil
	if err := persist(); err != nil {
		t.Fatal(err)
	}
	if _, err := syncer.Reset(false); err == nil || !strings.Contains(err.Error(), "modified SiYuan") {
		t.Fatalf("expected reset safety rejection, got %v", err)
	}
	blockedRefs, err := LoadRepositoryRefs(root)
	if err != nil {
		t.Fatal(err)
	}
	if blockedRefs.Operation == "" || !strings.Contains(api.contents[testBlockID], "target") {
		t.Fatalf("reset rejection lost recovery evidence: refs=%+v remote=%q", blockedRefs, api.contents[testBlockID])
	}
	if _, err := syncer.Reset(true); err != nil {
		t.Fatal(err)
	}
	forcedRefs, err := LoadRepositoryRefs(root)
	if err != nil {
		t.Fatal(err)
	}
	if forcedRefs.Operation != "" || forcedRefs.Head == commit.ObjectID {
		t.Fatalf("forced reset did not discard the transaction: %+v", forcedRefs)
	}
}

func TestForceResetCanDiscardCorruptOperationJournal(t *testing.T) {
	syncer, _, localPath, root := newSingleDocumentFixture(t, map[string]string{
		testBlockID: "base\n{: id=\"" + testBlockID + "\"}\n",
	}, []siyuan.ChildBlock{{ID: testBlockID, Type: "p"}}, nil)
	writeSingleBlock(t, localPath, testBlockID, "target")
	commit := stageAndCommitAll(t, syncer, "update")
	store := NewObjectStore(root)
	corruptOperation, err := store.Put(pushOperationObjectType, snapshotObjectVersion, PushOperationState{
		Kind:  "push",
		Phase: "corrupt",
	})
	if err != nil {
		t.Fatal(err)
	}
	refs, err := LoadRepositoryRefs(root)
	if err != nil {
		t.Fatal(err)
	}
	next := refs
	next.Operation = corruptOperation
	if err := SaveRepositoryRefs(root, refs.Generation, next); err != nil {
		t.Fatal(err)
	}
	if _, err := syncer.Reset(false); err == nil || !strings.Contains(err.Error(), "unsupported phase") {
		t.Fatalf("expected normal reset to preserve the corrupt journal, got %v", err)
	}
	if _, err := syncer.Reset(true); err != nil {
		t.Fatal(err)
	}
	refs, err = LoadRepositoryRefs(root)
	if err != nil {
		t.Fatal(err)
	}
	if refs.Operation != "" || refs.Head == commit.ObjectID {
		t.Fatalf("force reset did not recover refs: %+v", refs)
	}
	if err := store.Get(corruptOperation, pushOperationObjectType, snapshotObjectVersion, nil); err == nil {
		t.Fatal("force reset retained the corrupt OperationState")
	}
}

func TestAddRejectsUnresolvedHeadRemoteDivergence(t *testing.T) {
	root := t.TempDir()
	cfg := config.Default()
	api := &fakeAPI{
		notebooks: []siyuan.Notebook{{ID: testNotebookID, Name: "Work"}},
		documents: map[string][]siyuan.Document{"/": {
			{ID: testChildID, Name: "Design", Path: "/" + testChildID + ".sy"},
			{ID: testParentID, Name: "Other", Path: "/" + testParentID + ".sy"},
		}},
		children: map[string][]siyuan.ChildBlock{
			testChildID:  {{ID: testBlockID, Type: "p"}},
			testParentID: {{ID: testParentBlockID, Type: "p"}},
		},
		contents: map[string]string{
			testBlockID:       "design base\n{: id=\"" + testBlockID + "\"}\n",
			testParentBlockID: "other base\n{: id=\"" + testParentBlockID + "\"}\n",
		},
		attrs: map[string]map[string]string{},
	}
	syncer := NewSyncer(root, cfg, api)
	if _, err := syncer.Pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	designPath := filepath.Join(root, "notes", "Work", "Design.md")
	otherPath := filepath.Join(root, "notes", "Work", "Other.md")
	writeSingleBlock(t, designPath, testBlockID, "design local")
	api.contents[testBlockID] = "design remote\n{: id=\"" + testBlockID + "\"}\n"
	pull, err := syncer.Pull(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(pull.Conflicts) != 1 {
		t.Fatalf("pull = %+v", pull)
	}
	writeSingleBlock(t, otherPath, testParentBlockID, "other local")
	if _, err := syncer.Add(context.Background(), AddOptions{Paths: []string{otherPath}}); err == nil || !strings.Contains(err.Error(), "HEAD and the latest SiYuan snapshot diverge") {
		t.Fatalf("expected unresolved divergence rejection, got %v", err)
	}
}

func TestRemoteMissingReappearanceReturnsActionableError(t *testing.T) {
	syncer, api, _, _ := newSingleDocumentFixture(t, map[string]string{
		testBlockID: "base\n{: id=\"" + testBlockID + "\"}\n",
	}, []siyuan.ChildBlock{{ID: testBlockID, Type: "p"}}, nil)
	api.documents["/"] = nil
	pull, err := syncer.Pull(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(pull.RemoteMissing) != 1 {
		t.Fatalf("pull = %+v", pull)
	}
	api.documents["/"] = []siyuan.Document{{ID: testChildID, Name: "Design", Path: "/" + testChildID + ".sy"}}
	if _, err := syncer.Pull(context.Background()); err == nil || !strings.Contains(err.Error(), "remote-missing recovery is not supported yet") {
		t.Fatalf("expected actionable remote-missing error, got %v", err)
	}
}

func TestCommittedConflictCanBeResetResolvedAndRecommitted(t *testing.T) {
	syncer, api, localPath, root := newSingleDocumentFixture(t, map[string]string{
		testBlockID: "base\n{: id=\"" + testBlockID + "\"}\n",
	}, []siyuan.ChildBlock{{ID: testBlockID, Type: "p"}}, nil)
	writeSingleBlock(t, localPath, testBlockID, "local")
	stageAndCommitAll(t, syncer, "local change")
	api.contents[testBlockID] = "remote\n{: id=\"" + testBlockID + "\"}\n"
	result, err := syncer.Push(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Conflicts) != 1 || api.updates != 0 {
		t.Fatalf("push conflict = %+v", result)
	}
	conflictRefs, err := LoadRepositoryRefs(root)
	if err != nil {
		t.Fatal(err)
	}
	if conflictRefs.Operation == "" {
		t.Fatal("push conflict did not preserve OperationState")
	}
	operation, err := LoadPushOperation(NewObjectStore(root), conflictRefs.Operation)
	if err != nil {
		t.Fatal(err)
	}
	if operation.Phase != PushOperationPrepared || operation.Commit.Status != CommitConflict || operation.Error == "" {
		t.Fatalf("conflict operation = %+v", operation)
	}
	if _, err := syncer.Restore(context.Background(), localPath, "ours"); err == nil {
		t.Fatal("restore should require resetting the pending commit")
	}
	if _, err := syncer.Reset(false); err != nil {
		t.Fatal(err)
	}
	resetRefs, err := LoadRepositoryRefs(root)
	if err != nil {
		t.Fatal(err)
	}
	if resetRefs.Operation != "" {
		t.Fatalf("reset left active operation: %+v", resetRefs)
	}
	// Like Git, adding the manually edited file marks the conflict as resolved.
	stageAndCommitAll(t, syncer, "rebase local change")
	result, err = syncer.Push(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.PushedCommits != 1 || api.updates != 1 {
		t.Fatalf("recommitted push = %+v", result)
	}
}

func TestStatusAndRestoreTheirsUseGitStyleConflictFlow(t *testing.T) {
	syncer, api, localPath, _ := newSingleDocumentFixture(t, map[string]string{
		testBlockID: "base\n{: id=\"" + testBlockID + "\"}\n",
	}, []siyuan.ChildBlock{{ID: testBlockID, Type: "p"}}, nil)
	writeSingleBlock(t, localPath, testBlockID, "local")
	api.contents[testBlockID] = "remote\n{: id=\"" + testBlockID + "\"}\n"

	pull, err := syncer.Pull(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(pull.Conflicts) != 1 {
		t.Fatalf("pull = %+v", pull)
	}
	status, err := syncer.RepositoryStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Conflicts) != 1 || status.Conflicts[0].DocumentID != testChildID {
		t.Fatalf("status conflicts = %+v", status.Conflicts)
	}

	restored, err := syncer.Restore(context.Background(), "notes/Work/Design.md", "theirs")
	if err != nil {
		t.Fatal(err)
	}
	if restored.Status != StatusClean {
		t.Fatalf("restore = %+v", restored)
	}
	local, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(local), "remote") || strings.Contains(string(local), "local") {
		t.Fatalf("restored local = %s", local)
	}
	status, err = syncer.RepositoryStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Conflicts) != 0 || len(status.Documents) != 1 || status.Documents[0].Kind != StatusClean {
		t.Fatalf("status after restore = %+v", status)
	}
}

func TestRestoreOursUsesRecordedLocalSide(t *testing.T) {
	syncer, api, localPath, _ := newSingleDocumentFixture(t, map[string]string{
		testBlockID: "base\n{: id=\"" + testBlockID + "\"}\n",
	}, []siyuan.ChildBlock{{ID: testBlockID, Type: "p"}}, nil)
	writeSingleBlock(t, localPath, testBlockID, "recorded local")
	api.contents[testBlockID] = "remote\n{: id=\"" + testBlockID + "\"}\n"
	if pull, err := syncer.Pull(context.Background()); err != nil || len(pull.Conflicts) != 1 {
		t.Fatalf("pull = %+v err=%v", pull, err)
	}
	writeSingleBlock(t, localPath, testBlockID, "later edit")

	if _, err := syncer.Restore(context.Background(), localPath, "ours"); err != nil {
		t.Fatal(err)
	}
	local, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(local), "recorded local") || strings.Contains(string(local), "later edit") {
		t.Fatalf("restored local = %s", local)
	}
}

func TestCommittedUpdateRebasesOntoUnrelatedRemoteBlock(t *testing.T) {
	syncer, api, localPath, _ := newSingleDocumentFixture(t, map[string]string{
		testBlockID:  "first\n{: id=\"" + testBlockID + "\"}\n",
		testBlockID2: "second\n{: id=\"" + testBlockID2 + "\"}\n",
	}, []siyuan.ChildBlock{{ID: testBlockID, Type: "p"}, {ID: testBlockID2, Type: "p"}}, nil)
	local, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatal(err)
	}
	document, err := ParseAnnotated(string(local))
	if err != nil {
		t.Fatal(err)
	}
	document.Blocks[0].Content = "first local\n{: id=\"" + testBlockID + "\"}\n"
	if err := os.WriteFile(localPath, []byte(RenderAnnotated(document)), 0o644); err != nil {
		t.Fatal(err)
	}
	stageAndCommitAll(t, syncer, "update first")
	api.contents[testBlockID2] = "second remote\n{: id=\"" + testBlockID2 + "\"}\n"
	result, err := syncer.Push(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.PushedCommits != 1 || len(result.Conflicts) != 0 {
		t.Fatalf("push = %+v", result)
	}
	if api.contents[testBlockID2] != "second remote\n{: id=\""+testBlockID2+"\"}\n" {
		t.Fatalf("remote block overwritten: %q", api.contents[testBlockID2])
	}
	finalLocal, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatal(err)
	}
	finalDocument, err := ParseAnnotated(string(finalLocal))
	if err != nil {
		t.Fatal(err)
	}
	if finalDocument.Blocks[1].Content != api.contents[testBlockID2] {
		t.Fatalf("remote change not incorporated: %+v", finalDocument)
	}
}

func TestCommitApprovesTopLevelDelete(t *testing.T) {
	syncer, api, localPath, root := newSingleDocumentFixture(t, map[string]string{
		testBlockID:  "first\n{: id=\"" + testBlockID + "\"}\n",
		testBlockID2: "second\n{: id=\"" + testBlockID2 + "\"}\n",
	}, []siyuan.ChildBlock{{ID: testBlockID, Type: "p"}, {ID: testBlockID2, Type: "p"}}, nil)
	local, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatal(err)
	}
	document, err := ParseAnnotated(string(local))
	if err != nil {
		t.Fatal(err)
	}
	document.Blocks = document.Blocks[:1]
	if err := os.WriteFile(localPath, []byte(RenderAnnotated(document)), 0o644); err != nil {
		t.Fatal(err)
	}
	commit := stageAndCommitAll(t, syncer, "delete second block")
	if len(commit.DocumentPatches) != 1 || commit.DocumentPatches[0].Operations[0].Type != OperationDelete {
		t.Fatalf("commit = %+v", commit)
	}
	result, err := syncer.Push(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.PushedCommits != 1 || api.contents[testBlockID2] != "" {
		t.Fatalf("delete push = %+v", result)
	}
	metadataData, err := os.ReadFile(documentMetadataPath(root, testChildID))
	if err != nil {
		t.Fatal(err)
	}
	var metadata DocumentMetadata
	if err := json.Unmarshal(metadataData, &metadata); err != nil {
		t.Fatal(err)
	}
	if len(metadata.Blocks) != 1 || metadata.Blocks[0].ID != testBlockID {
		t.Fatalf("metadata = %+v", metadata)
	}
}

func TestContainerCommitPreservesNestedAttrs(t *testing.T) {
	container := "- item\n  {: id=\"" + testNestedBlockID + "\"}\n{: id=\"" + testBlockID + "\"}\n"
	attrs := map[string]map[string]string{
		testBlockID:       {"id": testBlockID, "type": "l", "custom-owner": "list-owner"},
		testNestedBlockID: {"id": testNestedBlockID, "type": "i", "custom-state": "nested-state"},
	}
	syncer, api, localPath, _ := newSingleDocumentFixture(t, map[string]string{
		testBlockID:       container,
		testNestedBlockID: "item\n{: id=\"" + testNestedBlockID + "\"}\n",
	}, []siyuan.ChildBlock{{ID: testBlockID, Type: "l"}}, attrs)
	api.children[testBlockID] = []siyuan.ChildBlock{{ID: testNestedBlockID, Type: "i"}}
	api.children[testNestedBlockID] = []siyuan.ChildBlock{}
	local, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(localPath, []byte(strings.Replace(string(local), "- item", "- changed item", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	stageAndCommitAll(t, syncer, "update list")
	if _, err := syncer.Push(context.Background()); err != nil {
		t.Fatal(err)
	}
	if api.attrs[testBlockID]["custom-owner"] != "list-owner" || api.attrs[testNestedBlockID]["custom-state"] != "nested-state" {
		t.Fatalf("attrs = %+v", api.attrs)
	}
}

func TestPushOperationRecovery(t *testing.T) {
	t.Run("update", func(t *testing.T) {
		attrs := map[string]map[string]string{testBlockID: {"id": testBlockID, "type": "p", "custom-owner": "agent"}}
		syncer, api, localPath, root := newSingleDocumentFixture(t, map[string]string{
			testBlockID: "base\n{: id=\"" + testBlockID + "\"}\n",
		}, []siyuan.ChildBlock{{ID: testBlockID, Type: "p"}}, attrs)
		writeSingleBlock(t, localPath, testBlockID, "target")
		stageAndCommitAll(t, syncer, "update")
		commit := loadPendingCommit(t, root)
		refs, err := LoadRepositoryRefs(root)
		if err != nil {
			t.Fatal(err)
		}
		execution, err := syncer.beginPushExecution(NewObjectStore(root), refs, &commit)
		if err != nil {
			t.Fatal(err)
		}
		persistBasePreflight(t, execution, &commit)
		patch := &commit.DocumentPatches[0]
		if err := syncer.prepareOperation(context.Background(), &patch.Operations[0]); err != nil {
			t.Fatal(err)
		}
		index := 0
		patch.InFlightOperation = &index
		patch.Status = DocumentPatchApplying
		if err := execution.persist(&commit, PushOperationApplying, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := api.UpdateBlock(context.Background(), testBlockID, patch.Operations[0].Content); err != nil {
			t.Fatal(err)
		}
		result, err := syncer.Push(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if result.PushedCommits != 1 || api.updates != 1 || api.attrs[testBlockID]["custom-owner"] != "agent" {
			t.Fatalf("resume update = %+v attrs=%+v", result, api.attrs[testBlockID])
		}
	})

	t.Run("recorded insert", func(t *testing.T) {
		syncer, api, localPath, root := newSingleDocumentFixture(t, map[string]string{
			testBlockID: "base\n{: id=\"" + testBlockID + "\"}\n",
		}, []siyuan.ChildBlock{{ID: testBlockID, Type: "p"}}, nil)
		local, _ := os.ReadFile(localPath)
		_ = os.WriteFile(localPath, append(local, []byte("inserted\n")...), 0o644)
		stageAndCommitAll(t, syncer, "insert")
		commit := loadPendingCommit(t, root)
		refs, err := LoadRepositoryRefs(root)
		if err != nil {
			t.Fatal(err)
		}
		execution, err := syncer.beginPushExecution(NewObjectStore(root), refs, &commit)
		if err != nil {
			t.Fatal(err)
		}
		persistBasePreflight(t, execution, &commit)
		patch := &commit.DocumentPatches[0]
		operation := &patch.Operations[0]
		if err := syncer.prepareOperation(context.Background(), operation); err != nil {
			t.Fatal(err)
		}
		index := 0
		patch.InFlightOperation = &index
		if err := execution.persist(&commit, PushOperationApplying, ""); err != nil {
			t.Fatal(err)
		}
		receipt, err := api.InsertBlock(context.Background(), operation.Content, operation.ParentID, operation.PreviousID, operation.NextID)
		if err != nil {
			t.Fatal(err)
		}
		operation.KernelReceipt = &receipt
		operation.ReceiptBlockIDs = receipt.BlockIDs()
		if err := execution.persist(&commit, PushOperationApplying, ""); err != nil {
			t.Fatal(err)
		}
		result, err := syncer.Push(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if result.PushedCommits != 1 || api.newCounter != 1 {
			t.Fatalf("resume insert = %+v inserts=%d", result, api.newCounter)
		}
	})

	t.Run("receipt-lost update reconciles through pull", func(t *testing.T) {
		attrs := map[string]map[string]string{testBlockID: {"id": testBlockID, "type": "p", "custom-owner": "agent"}}
		syncer, api, localPath, root := newSingleDocumentFixture(t, map[string]string{
			testBlockID: "base\n{: id=\"" + testBlockID + "\"}\n",
		}, []siyuan.ChildBlock{{ID: testBlockID, Type: "p"}}, attrs)
		writeSingleBlock(t, localPath, testBlockID, "target")
		stageAndCommitAll(t, syncer, "update")
		commit := loadPendingCommit(t, root)
		refs, err := LoadRepositoryRefs(root)
		if err != nil {
			t.Fatal(err)
		}
		execution, err := syncer.beginPushExecution(NewObjectStore(root), refs, &commit)
		if err != nil {
			t.Fatal(err)
		}
		persistBasePreflight(t, execution, &commit)
		patch := &commit.DocumentPatches[0]
		operation := &patch.Operations[0]
		if err := syncer.prepareOperation(context.Background(), operation); err != nil {
			t.Fatal(err)
		}
		index := 0
		patch.InFlightOperation = &index
		patch.Status = DocumentPatchApplying
		if err := execution.persist(&commit, PushOperationApplying, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := api.UpdateBlock(context.Background(), testBlockID, operation.Content); err != nil {
			t.Fatal(err)
		}

		pull, err := syncer.Pull(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if pull.PushReconciliation != string(pushReconciliationApplied) || len(pull.Conflicts) != 0 || api.updates != 1 {
			t.Fatalf("reconcile update = %+v updates=%d", pull, api.updates)
		}
		resumed, err := syncer.ContinuePush(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if resumed.PushedCommits != 1 || api.updates != 1 {
			t.Fatalf("finish update = %+v updates=%d", resumed, api.updates)
		}
	})

	t.Run("receipt-lost delete reconciles through pull", func(t *testing.T) {
		syncer, api, localPath, root := newSingleDocumentFixture(t, map[string]string{
			testBlockID: "base\n{: id=\"" + testBlockID + "\"}\n",
		}, []siyuan.ChildBlock{{ID: testBlockID, Type: "p"}}, nil)
		if err := os.WriteFile(localPath, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		stageAndCommitAll(t, syncer, "delete")
		commit := loadPendingCommit(t, root)
		refs, err := LoadRepositoryRefs(root)
		if err != nil {
			t.Fatal(err)
		}
		execution, err := syncer.beginPushExecution(NewObjectStore(root), refs, &commit)
		if err != nil {
			t.Fatal(err)
		}
		persistBasePreflight(t, execution, &commit)
		patch := &commit.DocumentPatches[0]
		index := 0
		patch.InFlightOperation = &index
		patch.Status = DocumentPatchApplying
		if err := execution.persist(&commit, PushOperationApplying, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := api.DeleteBlock(context.Background(), testBlockID); err != nil {
			t.Fatal(err)
		}

		pull, err := syncer.Pull(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if pull.PushReconciliation != string(pushReconciliationApplied) || len(pull.Conflicts) != 0 {
			t.Fatalf("reconcile delete = %+v", pull)
		}
		resumed, err := syncer.ContinuePush(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if resumed.PushedCommits != 1 {
			t.Fatalf("finish delete = %+v", resumed)
		}
	})

	t.Run("unsent insert retries", func(t *testing.T) {
		syncer, api, localPath, root := newSingleDocumentFixture(t, map[string]string{
			testBlockID: "base\n{: id=\"" + testBlockID + "\"}\n",
		}, []siyuan.ChildBlock{{ID: testBlockID, Type: "p"}}, nil)
		local, _ := os.ReadFile(localPath)
		_ = os.WriteFile(localPath, append(local, []byte("retry\n")...), 0o644)
		stageAndCommitAll(t, syncer, "insert")
		commit := loadPendingCommit(t, root)
		refs, err := LoadRepositoryRefs(root)
		if err != nil {
			t.Fatal(err)
		}
		execution, err := syncer.beginPushExecution(NewObjectStore(root), refs, &commit)
		if err != nil {
			t.Fatal(err)
		}
		persistBasePreflight(t, execution, &commit)
		operation := &commit.DocumentPatches[0].Operations[0]
		if err := syncer.prepareOperation(context.Background(), operation); err != nil {
			t.Fatal(err)
		}
		index := 0
		commit.DocumentPatches[0].InFlightOperation = &index
		if err := execution.persist(&commit, PushOperationApplying, ""); err != nil {
			t.Fatal(err)
		}
		pull, err := syncer.Pull(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if pull.PushReconciliation != string(pushReconciliationNotApplied) || len(pull.Conflicts) != 0 || api.newCounter != 0 {
			t.Fatalf("reconcile unsent insert = %+v inserts=%d", pull, api.newCounter)
		}
		result, err := syncer.Push(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if result.PushedCommits != 1 || api.newCounter != 1 {
			t.Fatalf("retry insert = %+v inserts=%d", result, api.newCounter)
		}
	})

	t.Run("receipt-lost insert reconciles through pull", func(t *testing.T) {
		syncer, api, localPath, root := newSingleDocumentFixture(t, map[string]string{
			testBlockID: "base\n{: id=\"" + testBlockID + "\"}\n",
		}, []siyuan.ChildBlock{{ID: testBlockID, Type: "p"}}, nil)
		local, _ := os.ReadFile(localPath)
		_ = os.WriteFile(localPath, append(local, []byte("uncertain\n")...), 0o644)
		stageAndCommitAll(t, syncer, "insert")
		commit := loadPendingCommit(t, root)
		refs, err := LoadRepositoryRefs(root)
		if err != nil {
			t.Fatal(err)
		}
		execution, err := syncer.beginPushExecution(NewObjectStore(root), refs, &commit)
		if err != nil {
			t.Fatal(err)
		}
		persistBasePreflight(t, execution, &commit)
		operation := &commit.DocumentPatches[0].Operations[0]
		if err := syncer.prepareOperation(context.Background(), operation); err != nil {
			t.Fatal(err)
		}
		index := 0
		commit.DocumentPatches[0].InFlightOperation = &index
		if err := execution.persist(&commit, PushOperationApplying, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := api.InsertBlock(context.Background(), operation.Content, operation.ParentID, operation.PreviousID, operation.NextID); err != nil {
			t.Fatal(err)
		}
		result, err := syncer.Push(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Conflicts) != 1 || api.newCounter != 1 || !strings.Contains(result.Conflicts[0], "receipt was unavailable") {
			t.Fatalf("uncertain insert push = %+v inserts=%d", result, api.newCounter)
		}
		pull, err := syncer.Pull(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if pull.PushReconciliation != string(pushReconciliationApplied) || len(pull.Conflicts) != 0 || api.newCounter != 1 {
			t.Fatalf("reconcile applied insert = %+v inserts=%d", pull, api.newCounter)
		}
		resumed, err := syncer.Push(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if resumed.PushedCommits != 1 || api.newCounter != 1 {
			t.Fatalf("finish reconciled insert = %+v inserts=%d", resumed, api.newCounter)
		}
	})

	t.Run("different insert remains conflict", func(t *testing.T) {
		syncer, api, localPath, root := newSingleDocumentFixture(t, map[string]string{
			testBlockID: "base\n{: id=\"" + testBlockID + "\"}\n",
		}, []siyuan.ChildBlock{{ID: testBlockID, Type: "p"}}, nil)
		local, _ := os.ReadFile(localPath)
		_ = os.WriteFile(localPath, append(local, []byte("expected\n")...), 0o644)
		stageAndCommitAll(t, syncer, "insert")
		commit := loadPendingCommit(t, root)
		refs, err := LoadRepositoryRefs(root)
		if err != nil {
			t.Fatal(err)
		}
		execution, err := syncer.beginPushExecution(NewObjectStore(root), refs, &commit)
		if err != nil {
			t.Fatal(err)
		}
		persistBasePreflight(t, execution, &commit)
		operation := &commit.DocumentPatches[0].Operations[0]
		if err := syncer.prepareOperation(context.Background(), operation); err != nil {
			t.Fatal(err)
		}
		index := 0
		commit.DocumentPatches[0].InFlightOperation = &index
		if err := execution.persist(&commit, PushOperationApplying, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := api.InsertBlock(context.Background(), "different\n", operation.ParentID, operation.PreviousID, operation.NextID); err != nil {
			t.Fatal(err)
		}
		pull, err := syncer.Pull(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if pull.PushReconciliation != string(pushReconciliationConflict) || len(pull.Conflicts) != 1 || !strings.Contains(pull.Conflicts[0], "do not uniquely match") {
			t.Fatalf("different insert reconciliation = %+v", pull)
		}
	})
}

func TestMultiDocumentPartialPushConvergesThroughPullAndContinue(t *testing.T) {
	syncer, api, firstPath, secondPath, _ := newTwoDocumentFixture(t)
	writeSingleBlock(t, firstPath, testBlockID, "first local")
	writeSingleBlock(t, secondPath, testDocBlockID2, "second local")
	stageAndCommitAll(t, syncer, "update two documents")

	triggered := false
	api.mu.Lock()
	api.beforeUpdateBlock = func(api *fakeAPI, id, _ string) {
		if triggered || id != testBlockID {
			return
		}
		triggered = true
		api.contents[testDocBlockID2] = "second concurrent\n{: id=\"" + testDocBlockID2 + "\"}\n"
	}
	api.mu.Unlock()

	first, err := syncer.Push(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Conflicts) != 1 || api.updates != 1 || !strings.Contains(api.contents[testBlockID], "first local") {
		t.Fatalf("partial push = %+v updates=%d first=%q", first, api.updates, api.contents[testBlockID])
	}
	api.mu.Lock()
	listDocumentsBeforeStatus := api.listDocumentsCalls
	api.mu.Unlock()
	status, err := syncer.RepositoryStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.ActiveOperation == nil || status.ActiveOperation.Kind != "push" ||
		status.ActiveOperation.Phase != string(PushOperationApplying) ||
		status.ActiveOperation.Status != operationStatusConflict ||
		status.ActiveOperation.AppliedDocuments != 1 || status.ActiveOperation.TotalDocuments != 2 ||
		status.ActiveOperation.CurrentDocument != "Work/Second.md" ||
		status.ActiveOperation.NextAction != "siyuan-worktree pull" {
		t.Fatalf("partial push status = %+v", status.ActiveOperation)
	}
	if !status.DocumentComparisonDeferred || len(status.Documents) != 0 {
		t.Fatalf("partial push should report sequencer state without a new remote observation: %+v", status)
	}
	api.mu.Lock()
	listDocumentsAfterStatus := api.listDocumentsCalls
	api.mu.Unlock()
	if listDocumentsAfterStatus != listDocumentsBeforeStatus {
		t.Fatalf("active push status read the remote: before=%d after=%d", listDocumentsBeforeStatus, listDocumentsAfterStatus)
	}

	api.mu.Lock()
	api.contents[testDocBlockID2] = "second base\n{: id=\"" + testDocBlockID2 + "\"}\n"
	api.mu.Unlock()
	pull, err := syncer.Pull(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if pull.PushReconciliation != string(pushReconciliationReady) || len(pull.Conflicts) != 0 || api.updates != 1 {
		t.Fatalf("partial push reconciliation = %+v updates=%d", pull, api.updates)
	}
	status, err = syncer.RepositoryStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.ActiveOperation == nil || status.ActiveOperation.Status != operationStatusRunning ||
		status.ActiveOperation.NextAction != "siyuan-worktree push --continue" {
		t.Fatalf("reconciled push status = %+v", status.ActiveOperation)
	}

	resumed, err := syncer.ContinuePush(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if resumed.PushedCommits != 1 || api.updates != 2 {
		t.Fatalf("continued push = %+v updates=%d", resumed, api.updates)
	}
	if !strings.Contains(api.contents[testBlockID], "first local") || !strings.Contains(api.contents[testDocBlockID2], "second local") {
		t.Fatalf("final remote contents: first=%q second=%q", api.contents[testBlockID], api.contents[testDocBlockID2])
	}
}

func TestPullRebasesRemainingPushIntentOverUnrelatedRemoteChange(t *testing.T) {
	syncer, api, localPath, _ := newSingleDocumentFixture(t, map[string]string{
		testBlockID:  "first base\n{: id=\"" + testBlockID + "\"}\n",
		testBlockID2: "second base\n{: id=\"" + testBlockID2 + "\"}\n",
	}, []siyuan.ChildBlock{{ID: testBlockID, Type: "p"}, {ID: testBlockID2, Type: "p"}}, nil)
	local, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(localPath, []byte(strings.Replace(string(local), "first base", "first local", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	stageAndCommitAll(t, syncer, "update first block")

	api.mu.Lock()
	api.contents[testBlockID] = "first conflict\n{: id=\"" + testBlockID + "\"}\n"
	api.mu.Unlock()
	first, err := syncer.Push(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Conflicts) != 1 || api.updates != 0 {
		t.Fatalf("initial conflict = %+v updates=%d", first, api.updates)
	}

	api.mu.Lock()
	api.contents[testBlockID] = "first base\n{: id=\"" + testBlockID + "\"}\n"
	api.contents[testBlockID2] = "second remote\n{: id=\"" + testBlockID2 + "\"}\n"
	api.mu.Unlock()
	pull, err := syncer.Pull(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if pull.PushReconciliation != string(pushReconciliationReady) || len(pull.Conflicts) != 0 {
		t.Fatalf("push rebase = %+v", pull)
	}

	resumed, err := syncer.ContinuePush(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if resumed.PushedCommits != 1 || api.updates != 1 {
		t.Fatalf("continued rebased push = %+v updates=%d", resumed, api.updates)
	}
	if !strings.Contains(api.contents[testBlockID], "first local") || !strings.Contains(api.contents[testBlockID2], "second remote") {
		t.Fatalf("remote merge result: first=%q second=%q", api.contents[testBlockID], api.contents[testBlockID2])
	}
	materialized, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(materialized), "first local") || !strings.Contains(string(materialized), "second remote") {
		t.Fatalf("local merge result: %q", materialized)
	}
}

func TestUnverifiedHistoryDoesNotBlockOrdinaryPush(t *testing.T) {
	syncer, api, localPath, _ := newSingleDocumentFixture(t, map[string]string{
		testBlockID: "base\n{: id=\"" + testBlockID + "\"}\n",
	}, []siyuan.ChildBlock{{ID: testBlockID, Type: "p"}}, nil)
	api.historySearchEmpty = true
	writeSingleBlock(t, localPath, testBlockID, "target")
	stageAndCommitAll(t, syncer, "update")
	result, err := syncer.Push(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.PushedCommits != 1 || api.histories != 1 {
		t.Fatalf("push = %+v histories=%d", result, api.histories)
	}
	commits, err := syncer.Log()
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) != 0 {
		t.Fatalf("completed push should not retain a log entry: %+v", commits)
	}
}

func TestPullRefusesStagedOrCommittedChanges(t *testing.T) {
	syncer, _, localPath, _ := newSingleDocumentFixture(t, map[string]string{
		testBlockID: "base\n{: id=\"" + testBlockID + "\"}\n",
	}, []siyuan.ChildBlock{{ID: testBlockID, Type: "p"}}, nil)
	writeSingleBlock(t, localPath, testBlockID, "local")
	if _, err := syncer.Add(context.Background(), AddOptions{All: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := syncer.Pull(context.Background()); err == nil || !strings.Contains(err.Error(), "empty index") {
		t.Fatalf("expected staged pull rejection, got %v", err)
	}
	if _, err := syncer.Commit("local"); err != nil {
		t.Fatal(err)
	}
	if _, err := syncer.Pull(context.Background()); err == nil || !strings.Contains(err.Error(), "pending commits") {
		t.Fatalf("expected committed pull rejection, got %v", err)
	}
}

func newSingleDocumentFixture(t *testing.T, contents map[string]string, topLevel []siyuan.ChildBlock, attrs map[string]map[string]string) (*Syncer, *fakeAPI, string, string) {
	t.Helper()
	root := t.TempDir()
	cfg := config.Default()
	api := &fakeAPI{
		notebooks: []siyuan.Notebook{{ID: testNotebookID, Name: "Work"}},
		documents: map[string][]siyuan.Document{"/": {{ID: testChildID, Name: "Design", Path: "/" + testChildID + ".sy"}}},
		children:  map[string][]siyuan.ChildBlock{testChildID: append([]siyuan.ChildBlock(nil), topLevel...)},
		contents:  contents,
		attrs:     attrs,
	}
	if api.attrs == nil {
		api.attrs = map[string]map[string]string{}
	}
	syncer := NewSyncer(root, cfg, api)
	if _, err := syncer.Pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	return syncer, api, filepath.Join(root, "notes", "Work", "Design.md"), root
}

func newTwoDocumentFixture(t *testing.T) (*Syncer, *fakeAPI, string, string, string) {
	t.Helper()
	root := t.TempDir()
	cfg := config.Default()
	api := &fakeAPI{
		notebooks: []siyuan.Notebook{{ID: testNotebookID, Name: "Work"}},
		documents: map[string][]siyuan.Document{
			"/": {
				{ID: testChildID, Name: "First", Path: "/" + testChildID + ".sy"},
				{ID: testChildID2, Name: "Second", Path: "/" + testChildID2 + ".sy"},
			},
		},
		children: map[string][]siyuan.ChildBlock{
			testChildID:  {{ID: testBlockID, Type: "p"}},
			testChildID2: {{ID: testDocBlockID2, Type: "p"}},
		},
		contents: map[string]string{
			testBlockID:     "first base\n{: id=\"" + testBlockID + "\"}\n",
			testDocBlockID2: "second base\n{: id=\"" + testDocBlockID2 + "\"}\n",
		},
		attrs: map[string]map[string]string{},
	}
	syncer := NewSyncer(root, cfg, api)
	if _, err := syncer.Pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	return syncer, api,
		filepath.Join(root, "notes", "Work", "First.md"),
		filepath.Join(root, "notes", "Work", "Second.md"),
		root
}

func stageAndCommitAll(t *testing.T, syncer *Syncer, message string) Commit {
	t.Helper()
	add, err := syncer.Add(context.Background(), AddOptions{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(add.Staged) == 0 {
		t.Fatalf("nothing staged: %+v", add)
	}
	commit, err := syncer.Commit(message)
	if err != nil {
		t.Fatal(err)
	}
	return commit
}

func persistBasePreflight(t *testing.T, execution *pushExecution, commit *Commit) {
	t.Helper()
	base, err := LoadWorkspaceTree(execution.store, execution.operation.BaseTree)
	if err != nil {
		t.Fatal(err)
	}
	preflight := make(map[string]ObjectID, len(commit.DocumentPatches))
	for _, patch := range commit.DocumentPatches {
		document, ok := WorkspaceDocumentByID(base, patch.DocumentID)
		if !ok {
			t.Fatalf("base snapshot is missing preflight document %s", patch.DocumentID)
		}
		preflight[patch.DocumentID] = document.DocumentTreeID
	}
	execution.operation.PreflightDocuments = preflight
	if err := execution.persist(commit, PushOperationPrepared, ""); err != nil {
		t.Fatal(err)
	}
}

func loadPendingCommit(t *testing.T, root string) Commit {
	t.Helper()
	commits, err := ListPendingCommits(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) != 1 {
		t.Fatalf("pending commits = %+v", commits)
	}
	return commits[0]
}

func writeSingleBlock(t *testing.T, localPath, blockID, text string) {
	t.Helper()
	document := AnnotatedDocument{Blocks: []AnnotatedBlock{{ID: blockID, Type: "p", Content: text + "\n{: id=\"" + blockID + "\"}\n"}}}
	if err := os.WriteFile(localPath, []byte(RenderAnnotated(document)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func apiRenderedDocument(t *testing.T, api *fakeAPI, documentID string) string {
	t.Helper()
	api.mu.Lock()
	defer api.mu.Unlock()
	document := AnnotatedDocument{Blocks: make([]AnnotatedBlock, 0, len(api.children[documentID]))}
	for _, child := range api.children[documentID] {
		document.Blocks = append(document.Blocks, AnnotatedBlock{ID: child.ID, Type: child.Type, Content: api.contents[child.ID]})
	}
	return RenderAnnotated(document)
}

func assertPullContentRefsUnchanged(t *testing.T, before, after RepositoryRefs) {
	t.Helper()
	if after.Head != before.Head || after.Index != before.Index || after.IndexPatch != before.IndexPatch || after.Remote != before.Remote {
		t.Fatalf("failed pull advanced content refs: before=%+v after=%+v", before, after)
	}
}
