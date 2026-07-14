package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"siyuan-worktree/internal/config"
	"siyuan-worktree/internal/siyuan"
)

type PullResult struct {
	Added              int      `json:"added"`
	Updated            int      `json:"updated"`
	Unchanged          int      `json:"unchanged"`
	PreservedLocal     int      `json:"preservedLocal"`
	PushReconciliation string   `json:"pushReconciliation,omitempty"`
	Conflicts          []string `json:"conflicts"`
	RemoteMissing      []string `json:"remoteMissing"`
}

type pushReconciliation string

const (
	pushReconciliationReady      pushReconciliation = "ready"
	pushReconciliationApplied    pushReconciliation = "applied"
	pushReconciliationNotApplied pushReconciliation = "not-applied"
	pushReconciliationConflict   pushReconciliation = "conflict"
)

type StatusKind string

const (
	StatusClean          StatusKind = "clean"
	StatusLocalModified  StatusKind = "local-modified"
	StatusRemoteModified StatusKind = "remote-modified"
	StatusMergeable      StatusKind = "mergeable"
	StatusConverged      StatusKind = "converged"
	StatusConflict       StatusKind = "conflict"
	StatusLocalMissing   StatusKind = "local-missing"
	StatusRemoteMissing  StatusKind = "remote-missing"
)

type DocumentStatus struct {
	Document   DocumentState `json:"document"`
	Kind       StatusKind    `json:"kind"`
	LocalHash  string        `json:"localHash,omitempty"`
	RemoteHash string        `json:"remoteHash,omitempty"`
}

type PushResult struct {
	PushedCommits   int      `json:"pushedCommits"`
	PushedDocuments int      `json:"pushedDocuments"`
	Changes         []string `json:"changes"`
	CommitIDs       []string `json:"commitIds"`
	Conflicts       []string `json:"conflicts"`
}

type RestoreResult struct {
	DocumentID string     `json:"documentId"`
	LocalPath  string     `json:"localPath"`
	Strategy   string     `json:"strategy"`
	Status     StatusKind `json:"status"`
}

type remoteInventory struct {
	Notebooks           []siyuan.Notebook
	DocumentsByNotebook map[string][]*DocumentNode
	DocumentsByID       map[string]*DocumentNode
}

type Syncer struct {
	root       string
	config     config.Config
	api        siyuan.API
	outputRoot string
}

func NewSyncer(root string, cfg config.Config, api siyuan.API) *Syncer {
	return &Syncer{
		root:       root,
		config:     cfg,
		api:        api,
		outputRoot: config.OutputPath(root, cfg),
	}
}

func (s *Syncer) Pull(ctx context.Context) (PullResult, error) {
	result := PullResult{Conflicts: []string{}, RemoteMissing: []string{}}
	state, err := LoadState(s.root)
	if err != nil {
		return result, err
	}
	refs, err := EnsureRepositorySnapshots(s.root, s.config, state)
	if err != nil {
		return result, err
	}
	if refs.Operation != "" {
		kind, err := LoadRepositoryOperationKind(NewObjectStore(s.root), refs.Operation)
		if err != nil {
			return result, err
		}
		switch kind {
		case "push":
			return s.reconcileActivePushDuringPull(ctx, result, state, refs)
		case "pull":
			return s.resumePullOperation(ctx, refs)
		default:
			return result, fmt.Errorf("unsupported active operation kind %s", kind)
		}
	}
	staged, err := ListStagedDocumentPatches(s.root)
	if err != nil {
		return result, err
	}
	pending, err := HasPendingCommit(s.root)
	if err != nil {
		return result, err
	}
	if len(staged) > 0 || pending {
		return result, errors.New("pull requires an empty index and no pending commits; commit and push, or reset them first")
	}
	if _, err := markRepositoryObjects(s.root, refs); err != nil {
		return result, fmt.Errorf("validate repository object graph before pull: %w", err)
	}
	working, err := s.scanWorkingTree(state)
	if err != nil {
		return result, err
	}
	if err := os.MkdirAll(s.outputRoot, 0o755); err != nil {
		return result, fmt.Errorf("create output directory: %w", err)
	}
	execution, err := s.beginPullExecution(NewObjectStore(s.root), refs, state, working)
	if err != nil {
		return result, err
	}
	activeRefs, err := LoadRepositoryRefs(s.root)
	if err != nil {
		return result, err
	}
	if activeRefs.Operation != execution.operationID {
		return result, errors.New("pull operation ref was not persisted")
	}
	return s.resumePullOperation(ctx, activeRefs)
}

func (s *Syncer) reconcileActivePushDuringPull(
	ctx context.Context,
	result PullResult,
	state State,
	refs RepositoryRefs,
) (PullResult, error) {
	store := NewObjectStore(s.root)
	operationState, err := LoadPushOperation(store, refs.Operation)
	if err != nil {
		return result, err
	}
	if operationState.Phase != PushOperationPrepared && operationState.Phase != PushOperationApplying {
		return result, fmt.Errorf("pull cannot reconcile push phase %s; resume push instead", operationState.Phase)
	}
	commit := operationState.Commit
	if commit.AppliedDocuments < 0 || commit.AppliedDocuments > len(commit.DocumentPatches) {
		return result, errors.New("active push has an invalid completed document count")
	}
	if commit.AppliedDocuments == len(commit.DocumentPatches) {
		return result, errors.New("push has already applied every document; resume push to finish local materialization")
	}
	baseWorkspace, err := LoadWorkspaceTree(store, operationState.BaseTree)
	if err != nil {
		return result, err
	}
	execution := &pushExecution{syncer: s, store: store, operation: operationState}
	persist := func() error {
		return execution.persist(&commit, execution.operation.Phase, commit.Error)
	}
	outcome := pushReconciliationReady
	scanAllRemaining := !pushOperationHasRemoteEffects(operationState)

	for documentIndex := commit.AppliedDocuments; documentIndex < len(commit.DocumentPatches); documentIndex++ {
		patch := &commit.DocumentPatches[documentIndex]
		document, ok := state.Documents[patch.DocumentID]
		if !ok {
			return result, fmt.Errorf("active push references untracked document %s", patch.DocumentID)
		}
		baseDocument, ok := WorkspaceDocumentByID(baseWorkspace, patch.DocumentID)
		if !ok {
			return result, fmt.Errorf("push base snapshot does not contain document %s", patch.DocumentID)
		}
		base, err := RenderDocumentTree(store, baseDocument.DocumentTreeID)
		if err != nil {
			return result, err
		}

		if documentIndex == commit.AppliedDocuments && patch.InFlightOperation != nil {
			reconciled, err := s.reconcileInFlightOperation(ctx, patch, true, persist)
			if err != nil {
				var conflictErr *DocumentPatchConflictError
				if !errors.As(err, &conflictErr) {
					return result, err
				}
				remoteDocument, fetchErr := s.fetchAnnotatedDocument(ctx, patch.DocumentID)
				if fetchErr != nil {
					return result, fetchErr
				}
				return s.recordPushReconciliationConflict(result, execution, &commit, patch, document, base, RenderAnnotated(remoteDocument), conflictErr.Error())
			}
			outcome = reconciled
		}

		remoteDocument, err := s.fetchAnnotatedDocument(ctx, patch.DocumentID)
		if err != nil {
			return result, err
		}
		remote := RenderAnnotated(remoteDocument)
		if documentIndex == commit.AppliedDocuments {
			if err := s.validateAppliedOperations(ctx, *patch); err != nil {
				return s.recordPushReconciliationConflict(result, execution, &commit, patch, document, base, remote, err.Error())
			}
		}
		remainingPatch := *patch
		remainingPatch.Operations = patch.Operations[patch.AppliedOperations:]
		if err := ValidateDocumentPatchAgainstRemote(remainingPatch, remoteDocument); err != nil {
			return s.recordPushReconciliationConflict(result, execution, &commit, patch, document, base, remote, err.Error())
		}
		if err := clearConflict(s.root, patch.DocumentID); err != nil {
			return result, err
		}

		if documentIndex == commit.AppliedDocuments && patch.AppliedOperations == len(patch.Operations) {
			documentTreeID, err := StoreDocumentTree(store, patch.DocumentID, remoteDocument)
			if err != nil {
				return result, err
			}
			patch.Status = DocumentPatchApplied
			patch.Error = ""
			commit.AppliedDocuments = documentIndex + 1
			if err := execution.recordCanonicalDocument(&commit, patch.DocumentID, documentTreeID); err != nil {
				return result, err
			}
			continue
		}

		patch.Error = ""
		if patch.AppliedOperations > 0 || patch.HistoryCheckpoint != nil || patch.InFlightOperation != nil {
			patch.Status = DocumentPatchApplying
		} else {
			patch.Status = DocumentPatchCommitted
		}
		if !scanAllRemaining {
			break
		}
	}

	commit.Error = ""
	if execution.operation.Phase == PushOperationPrepared {
		commit.Status = CommitPending
	} else {
		commit.Status = CommitPushing
	}
	if err := persist(); err != nil {
		return result, err
	}
	result.PushReconciliation = string(outcome)
	return result, nil
}

func (s *Syncer) recordPushReconciliationConflict(
	result PullResult,
	execution *pushExecution,
	commit *Commit,
	patch *DocumentPatch,
	document DocumentState,
	base, remote, message string,
) (PullResult, error) {
	commit.Status = CommitConflict
	commit.Error = message
	patch.Status = DocumentPatchConflict
	patch.Error = message
	if err := execution.persist(commit, execution.operation.Phase, message); err != nil {
		return result, err
	}
	if _, err := writeConflict(s.root, document, base, patch.LocalContent, remote); err != nil {
		return result, err
	}
	result.PushReconciliation = string(pushReconciliationConflict)
	result.Conflicts = append(result.Conflicts, fmt.Sprintf("%s: %s", patch.LocalPath, message))
	return result, nil
}

func (s *Syncer) status(ctx context.Context) ([]DocumentStatus, error) {
	state, err := LoadState(s.root)
	if err != nil {
		return nil, err
	}
	refs, err := EnsureRepositorySnapshots(s.root, s.config, state)
	if err != nil {
		return nil, err
	}
	store := NewObjectStore(s.root)
	if err := rejectActivePull(store, refs, "status"); err != nil {
		return nil, err
	}
	working, err := s.scanWorkingTree(state)
	if err != nil {
		return nil, err
	}
	remote, err := s.observeStableRemoteWorkspace(ctx)
	if err != nil {
		return nil, err
	}
	inventory := remote.Inventory
	baseWorkspace, err := LoadWorkspaceTree(store, refs.Remote)
	if err != nil {
		return nil, err
	}
	statuses := make([]DocumentStatus, 0, len(state.Documents))
	for _, document := range sortedStateDocuments(state) {
		record, ok := working.record(document.ID)
		if !ok {
			return nil, fmt.Errorf("working snapshot does not contain document %s", document.ID)
		}
		if record.Missing {
			statuses = append(statuses, DocumentStatus{Document: document, Kind: StatusLocalMissing})
			continue
		}
		local, ok := working.content(document.ID)
		if !ok {
			return nil, fmt.Errorf("working snapshot has no content for %s", document.LocalPath)
		}
		localHash := record.ContentHash
		if _, found := inventory.DocumentsByID[document.ID]; !found {
			statuses = append(statuses, DocumentStatus{Document: document, Kind: StatusRemoteMissing, LocalHash: localHash})
			continue
		}
		observation, ok := remote.Documents[document.ID]
		if !ok {
			return nil, fmt.Errorf("stable remote workspace is missing tracked document %s", document.ID)
		}
		remoteDocument := observation.Document
		remoteHash := HashContent(RenderAnnotated(remoteDocument))
		baseDocument, exists := WorkspaceDocumentByID(baseWorkspace, document.ID)
		if !exists {
			return nil, fmt.Errorf("tracked document %s is absent from the Remote baseline; remote-missing recovery is not supported yet, create a fresh clone", document.ID)
		}
		base, err := RenderDocumentTree(store, baseDocument.DocumentTreeID)
		if err != nil {
			return nil, err
		}
		baseHash := HashContent(base)
		localChanged := localHash != baseHash
		remoteChanged := remoteHash != baseHash
		kind := StatusClean
		switch {
		case localChanged && remoteChanged && localHash != remoteHash:
			if patch, patchErr := BuildDocumentPatch(document.ID, document.LocalPath, base, local); patchErr == nil {
				if safetyErr := ValidateDocumentPatchSafety(patch, base); safetyErr == nil {
					if _, mergeErr := MergeDocumentPatch(patch, remoteDocument); mergeErr == nil {
						kind = StatusMergeable
						break
					}
				}
			}
			kind = StatusConflict
		case localChanged && remoteChanged && localHash == remoteHash:
			kind = StatusConverged
		case localChanged && !remoteChanged:
			kind = StatusLocalModified
		case remoteChanged && !localChanged:
			kind = StatusRemoteModified
		}
		statuses = append(statuses, DocumentStatus{
			Document:   document,
			Kind:       kind,
			LocalHash:  localHash,
			RemoteHash: remoteHash,
		})
	}
	return statuses, nil
}

func (s *Syncer) RepositoryStatus(ctx context.Context) (RepositoryStatus, error) {
	state, err := LoadState(s.root)
	if err != nil {
		return RepositoryStatus{}, err
	}
	refs, err := EnsureRepositorySnapshots(s.root, s.config, state)
	if err != nil {
		return RepositoryStatus{}, err
	}
	store := NewObjectStore(s.root)
	activeOperation, err := loadActiveOperationStatus(store, refs)
	if err != nil {
		return RepositoryStatus{}, err
	}
	documents := []DocumentStatus{}
	// During an active operation, status must remain available even if SiYuan is
	// offline or changing. Report the durable sequencer state instead of starting
	// a new remote observation. A pull may also have materialized only a prefix of
	// its plan, so a normal document comparison would be misleading.
	if activeOperation == nil {
		documents, err = s.status(ctx)
		if err != nil {
			return RepositoryStatus{}, err
		}
	}
	stagedDocumentPatches, err := ListStagedDocumentPatches(s.root)
	if err != nil {
		return RepositoryStatus{}, err
	}
	pendingCommits, err := ListPendingCommits(s.root)
	if err != nil {
		return RepositoryStatus{}, err
	}
	conflicts, err := ListConflicts(s.root)
	if err != nil {
		return RepositoryStatus{}, err
	}
	result := RepositoryStatus{
		Documents:                  documents,
		DocumentComparisonDeferred: activeOperation != nil,
		Staged:                     []string{},
		PendingCommits:             []CommitSummary{},
		Conflicts:                  conflicts,
		ActiveOperation:            activeOperation,
	}
	for _, patch := range stagedDocumentPatches {
		result.Staged = append(result.Staged, patch.LocalPath)
	}
	for _, commit := range pendingCommits {
		result.PendingCommits = append(result.PendingCommits, SummarizeCommit(commit))
	}
	return result, nil
}

func (s *Syncer) Diffs() ([]Diff, error) {
	state, err := LoadState(s.root)
	if err != nil {
		return nil, err
	}
	refs, err := EnsureRepositorySnapshots(s.root, s.config, state)
	if err != nil {
		return nil, err
	}
	store := NewObjectStore(s.root)
	if err := rejectActivePull(store, refs, "diff"); err != nil {
		return nil, err
	}
	working, err := s.scanWorkingTree(state)
	if err != nil {
		return nil, err
	}
	indexWorkspace, err := LoadWorkspaceTree(store, refs.Index)
	if err != nil {
		return nil, err
	}
	var diffs []Diff
	for _, document := range sortedStateDocuments(state) {
		record, ok := working.record(document.ID)
		if !ok || record.Missing {
			return nil, fmt.Errorf("read %s: working file is missing", document.LocalPath)
		}
		local, ok := working.content(document.ID)
		if !ok {
			return nil, fmt.Errorf("working snapshot has no content for %s", document.LocalPath)
		}
		indexDocument, exists := WorkspaceDocumentByID(indexWorkspace, document.ID)
		if !exists {
			return nil, fmt.Errorf("Index snapshot does not contain document %s", document.ID)
		}
		indexContent, err := RenderDocumentTree(store, indexDocument.DocumentTreeID)
		if err != nil {
			return nil, err
		}
		if record.ContentHash == HashContent(indexContent) {
			continue
		}
		diffs = append(diffs, Diff{Path: document.LocalPath, Content: FormatSimpleDiff(indexContent, local, document.LocalPath)})
	}
	return diffs, nil
}

func (s *Syncer) StagedDiffs() ([]Diff, error) {
	state, err := LoadState(s.root)
	if err != nil {
		return nil, err
	}
	refs, err := EnsureRepositorySnapshots(s.root, s.config, state)
	if err != nil {
		return nil, err
	}
	store := NewObjectStore(s.root)
	headCommit, err := LoadHeadCommit(store, refs)
	if err != nil {
		return nil, err
	}
	headWorkspace, err := LoadWorkspaceTree(store, headCommit.Tree)
	if err != nil {
		return nil, err
	}
	patches, err := ListStagedDocumentPatches(s.root)
	if err != nil {
		return nil, err
	}
	diffs := make([]Diff, 0, len(patches))
	for _, patch := range patches {
		document, exists := WorkspaceDocumentByID(headWorkspace, patch.DocumentID)
		if !exists {
			return nil, fmt.Errorf("HEAD snapshot does not contain document %s", patch.DocumentID)
		}
		base, err := RenderDocumentTree(store, document.DocumentTreeID)
		if err != nil {
			return nil, err
		}
		diffs = append(diffs, Diff{Path: patch.LocalPath, Content: FormatSimpleDiff(base, patch.LocalContent, patch.LocalPath)})
	}
	return diffs, nil
}

func (s *Syncer) Log() ([]CommitSummary, error) {
	return ListCurrentCommitSummaries(s.root)
}

func (s *Syncer) Add(ctx context.Context, options AddOptions) (AddResult, error) {
	result := AddResult{Staged: []string{}, Unchanged: []string{}}
	if pending, err := HasPendingCommit(s.root); err != nil {
		return result, err
	} else if pending {
		return result, errors.New("cannot add while a commit is waiting to be pushed")
	}
	state, err := LoadState(s.root)
	if err != nil {
		return result, err
	}
	refs, err := EnsureRepositorySnapshots(s.root, s.config, state)
	if err != nil {
		return result, err
	}
	store := NewObjectStore(s.root)
	if err := rejectActivePull(store, refs, "add"); err != nil {
		return result, err
	}
	documents, err := s.selectDocumentsForAdd(state, options)
	if err != nil {
		return result, err
	}
	conflicts, err := ListConflicts(s.root)
	if err != nil {
		return result, err
	}
	conflicted := make(map[string]bool, len(conflicts))
	for _, conflict := range conflicts {
		conflicted[conflict.DocumentID] = true
	}
	for _, document := range documents {
		if !conflicted[document.ID] {
			continue
		}
		if _, err := s.restoreConflict(ctx, &state, document, "manual"); err != nil {
			return result, fmt.Errorf("prepare conflicted file %s before add: %w", document.LocalPath, err)
		}
	}
	working, err := s.scanWorkingTree(state)
	if err != nil {
		return result, err
	}
	refs, err = EnsureRepositorySnapshots(s.root, s.config, state)
	if err != nil {
		return result, err
	}
	headCommit, err := LoadHeadCommit(store, refs)
	if err != nil {
		return result, err
	}
	if headCommit.Kind != baselineCommitObjectKind || headCommit.Tree != refs.Remote {
		return result, errors.New("cannot add while HEAD and the latest SiYuan snapshot diverge; resolve the pull divergence first")
	}
	baseWorkspace, err := LoadWorkspaceTree(store, headCommit.Tree)
	if err != nil {
		return result, err
	}
	indexWorkspace, err := LoadWorkspaceTree(store, refs.Index)
	if err != nil {
		return result, err
	}
	existingDocumentPatches, err := ListStagedDocumentPatches(s.root)
	if err != nil {
		return result, err
	}
	patchesByDocument := make(map[string]DocumentPatch, len(existingDocumentPatches))
	for _, patch := range existingDocumentPatches {
		patchesByDocument[patch.DocumentID] = immutableDocumentPatch(patch)
		if refs.IndexPatch == "" {
			documentTreeID, err := StoreEditableDocumentTree(store, patch.DocumentID, patch.LocalContent, patch)
			if err != nil {
				return result, err
			}
			if err := ReplaceWorkspaceDocumentTree(&indexWorkspace, patch.DocumentID, documentTreeID); err != nil {
				return result, err
			}
		}
	}
	for _, document := range documents {
		record, ok := working.record(document.ID)
		if !ok || record.Missing {
			return result, fmt.Errorf("read %s: working file is missing", document.LocalPath)
		}
		local, ok := working.content(document.ID)
		if !ok {
			return result, fmt.Errorf("working snapshot has no content for %s", document.LocalPath)
		}
		baseDocument, exists := WorkspaceDocumentByID(baseWorkspace, document.ID)
		if !exists {
			return result, fmt.Errorf("HEAD snapshot does not contain document %s", document.ID)
		}
		base, err := RenderDocumentTree(store, baseDocument.DocumentTreeID)
		if err != nil {
			return result, err
		}
		if record.ContentHash == HashContent(base) {
			if err := ReplaceWorkspaceDocumentTree(&indexWorkspace, document.ID, baseDocument.DocumentTreeID); err != nil {
				return result, err
			}
			delete(patchesByDocument, document.ID)
			result.Unchanged = append(result.Unchanged, document.LocalPath)
			continue
		}
		patch, err := BuildDocumentPatch(document.ID, document.LocalPath, base, local)
		if err != nil {
			return result, fmt.Errorf("%s: %w", document.LocalPath, err)
		}
		if err := ValidateDocumentPatchSafety(patch, base); err != nil {
			return result, fmt.Errorf("%s: %w", document.LocalPath, err)
		}
		if len(patch.Operations) == 0 {
			if err := ReplaceWorkspaceDocumentTree(&indexWorkspace, document.ID, baseDocument.DocumentTreeID); err != nil {
				return result, err
			}
			delete(patchesByDocument, document.ID)
			result.Unchanged = append(result.Unchanged, document.LocalPath)
			continue
		}
		documentTreeID, err := StoreEditableDocumentTree(store, document.ID, local, patch)
		if err != nil {
			return result, err
		}
		if err := ReplaceWorkspaceDocumentTree(&indexWorkspace, document.ID, documentTreeID); err != nil {
			return result, err
		}
		patchesByDocument[document.ID] = immutableDocumentPatch(patch)
		result.Staged = append(result.Staged, document.LocalPath)
		result.Operations += len(patch.Operations)
	}
	patches := make([]DocumentPatch, 0, len(patchesByDocument))
	for _, patch := range patchesByDocument {
		patches = append(patches, immutableDocumentPatch(patch))
	}
	sort.Slice(patches, func(i, j int) bool { return patches[i].LocalPath < patches[j].LocalPath })
	indexTreeID := headCommit.Tree
	patchID := ObjectID("")
	if len(patches) > 0 {
		indexTreeID, err = StoreWorkspaceTreeObject(store, indexWorkspace)
		if err != nil {
			return result, err
		}
		var patch PatchObject
		patchID, patch, err = StorePatch(store, headCommit.Tree, indexTreeID, patches)
		if err != nil {
			return result, err
		}
		if err := ValidatePatch(store, patch); err != nil {
			return result, err
		}
	}
	nextRefs := refs
	nextRefs.Index = indexTreeID
	nextRefs.IndexPatch = patchID
	if err := SaveRepositoryRefs(s.root, refs.Generation, nextRefs); err != nil {
		return result, err
	}
	pruneRepositoryObjectsBestEffort(s.root)
	return result, nil
}

func (s *Syncer) Commit(commitMessage string) (Commit, error) {
	message := strings.TrimSpace(commitMessage)
	if message == "" {
		return Commit{}, errors.New("commit message must not be empty")
	}
	if pending, err := HasPendingCommit(s.root); err != nil {
		return Commit{}, err
	} else if pending {
		return Commit{}, errors.New("push the pending commit before creating another commit")
	}
	patches, err := ListStagedDocumentPatches(s.root)
	if err != nil {
		return Commit{}, err
	}
	if len(patches) == 0 {
		return Commit{}, errors.New("nothing staged; run add first")
	}
	state, err := LoadState(s.root)
	if err != nil {
		return Commit{}, err
	}
	refs, err := EnsureRepositorySnapshots(s.root, s.config, state)
	if err != nil {
		return Commit{}, err
	}
	if refs.IndexPatch == "" {
		return Commit{}, errors.New("nothing staged; run add first")
	}
	store := NewObjectStore(s.root)
	headCommit, err := LoadHeadCommit(store, refs)
	if err != nil {
		return Commit{}, err
	}
	if headCommit.Kind != baselineCommitObjectKind || headCommit.Tree != refs.Remote {
		return Commit{}, errors.New("cannot commit while HEAD and the latest SiYuan snapshot diverge; resolve the pull divergence first")
	}
	commitPatch, err := LoadPatch(store, refs.IndexPatch)
	if err != nil {
		return Commit{}, err
	}
	if commitPatch.BaseTree != headCommit.Tree || commitPatch.TargetTree != refs.Index {
		return Commit{}, errors.New("staged Patch does not match HEAD and Index snapshots")
	}
	if err := ValidatePatch(store, commitPatch); err != nil {
		return Commit{}, err
	}
	patches = immutableDocumentPatches(commitPatch.DocumentPatches)
	for _, patch := range patches {
		_, ok := state.Documents[patch.DocumentID]
		if !ok {
			return Commit{}, fmt.Errorf("staged document %s is no longer tracked", patch.DocumentID)
		}
		baseWorkspace, err := LoadWorkspaceTree(store, commitPatch.BaseTree)
		if err != nil {
			return Commit{}, err
		}
		baseDocument, exists := WorkspaceDocumentByID(baseWorkspace, patch.DocumentID)
		if !exists {
			return Commit{}, fmt.Errorf("HEAD snapshot does not contain document %s", patch.DocumentID)
		}
		base, err := RenderDocumentTree(store, baseDocument.DocumentTreeID)
		if err != nil {
			return Commit{}, err
		}
		if HashContent(base) != patch.BaseHash {
			return Commit{}, fmt.Errorf("%s: Base changed after add; run add again", patch.LocalPath)
		}
		if err := ValidateDocumentPatchSafety(patch, base); err != nil {
			return Commit{}, fmt.Errorf("%s: %w", patch.LocalPath, err)
		}
	}
	commit := NewCommit(message, patches)
	commitObjectID, err := store.Put(commitObjectType, snapshotObjectVersion, CommitObject{
		Kind:       userCommitObjectKind,
		DisplayID:  commit.ID,
		Tree:       refs.Index,
		BaseHead:   refs.Head,
		RemoteBase: headCommit.Tree,
		Patch:      refs.IndexPatch,
		Message:    message,
		CreatedAt:  commit.CreatedAt,
	})
	if err != nil {
		return Commit{}, err
	}
	commit.ObjectID = commitObjectID
	commit.Tree = refs.Index
	commit.BaseHead = refs.Head
	commit.RemoteBase = headCommit.Tree
	commit.Patch = refs.IndexPatch
	nextRefs := refs
	nextRefs.Head = commitObjectID
	nextRefs.IndexPatch = ""
	if err := SaveRepositoryRefs(s.root, refs.Generation, nextRefs); err != nil {
		return Commit{}, err
	}
	pruneRepositoryObjectsBestEffort(s.root)
	return commit, nil
}

func (s *Syncer) Reset(force bool) (ResetResult, error) {
	result := ResetResult{DiscardedCommits: []string{}}
	state, err := LoadState(s.root)
	if err != nil {
		return result, err
	}
	var refs RepositoryRefs
	if force {
		refs, err = LoadRepositoryRefs(s.root)
		if err == nil && (refs.Head == "" || refs.Index == "" || refs.Remote == "") {
			refs, err = EnsureRepositorySnapshots(s.root, s.config, state)
		}
	} else {
		refs, err = EnsureRepositorySnapshots(s.root, s.config, state)
	}
	if err != nil {
		return result, err
	}
	store := NewObjectStore(s.root)
	staged, err := ListStagedDocumentPatches(s.root)
	if err != nil && !force {
		return result, err
	}
	if err == nil {
		result.ClearedStaged = len(staged)
	}
	var commits []Commit
	if force {
		head, err := LoadHeadCommit(store, refs)
		if err != nil {
			return result, err
		}
		if head.Kind == userCommitObjectKind {
			commits = []Commit{{ID: head.DisplayID, ObjectID: refs.Head}}
		} else if head.Kind != baselineCommitObjectKind {
			return result, fmt.Errorf("cannot force reset unsupported HEAD kind %s", head.Kind)
		}
	} else {
		commits, err = ListPendingCommits(s.root)
		if err != nil {
			return result, err
		}
	}
	if refs.Operation != "" && !force {
		kind, err := LoadRepositoryOperationKind(store, refs.Operation)
		if err != nil {
			return result, err
		}
		switch kind {
		case "push":
			operation, err := LoadPushOperation(store, refs.Operation)
			if err != nil {
				return result, err
			}
			if pushOperationHasRemoteEffects(operation) {
				return result, errors.New("cannot reset after push may have modified SiYuan; resume push or use reset --force after verifying the remote state")
			}
		case "pull":
			operation, err := LoadPullOperation(store, refs.Operation)
			if err != nil {
				return result, err
			}
			if operation.Phase == PullOperationMaterializing || len(operation.MaterializedDocuments) > 0 {
				return result, errors.New("cannot reset after pull modified the working tree; resume pull or use reset --force after inspecting local files")
			}
		default:
			return result, fmt.Errorf("cannot reset unsupported operation kind %s", kind)
		}
	}
	nextRefs := refs
	if len(commits) > 0 {
		commit := commits[len(commits)-1]
		if commit.ObjectID == "" || refs.Head != commit.ObjectID {
			return result, fmt.Errorf("pending commit %s is not repository HEAD", commit.ID)
		}
		commitObject, err := LoadCommitObject(store, commit.ObjectID)
		if err != nil {
			return result, err
		}
		if commitObject.BaseHead == "" {
			return result, errors.New("pending CommitObject has no stable baseHead")
		}
		baseCommit, err := LoadCommitObject(store, commitObject.BaseHead)
		if err != nil {
			return result, err
		}
		if baseCommit.Kind != baselineCommitObjectKind || baseCommit.Tree != commitObject.RemoteBase {
			return result, errors.New("pending Commit baseHead does not match its stable RemoteBase")
		}
		if refs.Remote != baseCommit.Tree {
			return result, errors.New("pending Commit stable base does not match the Remote ref")
		}
		if _, err := LoadWorkspaceTree(store, baseCommit.Tree); err != nil {
			return result, err
		}
		nextRefs.Head = commitObject.BaseHead
		nextRefs.Index = baseCommit.Tree
	} else {
		head, err := LoadHeadCommit(store, refs)
		if err != nil {
			return result, err
		}
		if _, err := LoadWorkspaceTree(store, head.Tree); err != nil {
			return result, err
		}
		if _, err := LoadWorkspaceTree(store, refs.Remote); err != nil {
			return result, err
		}
		nextRefs.Index = head.Tree
	}
	nextRefs.IndexPatch = ""
	nextRefs.Operation = ""
	if nextRefs != refs {
		if _, err := markRepositoryObjects(s.root, nextRefs); err != nil {
			return result, fmt.Errorf("validate reset baseline: %w", err)
		}
		if err := SaveRepositoryRefs(s.root, refs.Generation, nextRefs); err != nil {
			return result, err
		}
	}
	for _, commit := range commits {
		result.DiscardedCommits = append(result.DiscardedCommits, commit.ID)
	}
	pruneRepositoryObjectsBestEffort(s.root)
	return result, nil
}

func (s *Syncer) selectDocumentsForAdd(state State, options AddOptions) ([]DocumentState, error) {
	if !options.All && len(options.Paths) == 0 {
		return nil, errors.New("add requires one or more paths, or -A")
	}
	if options.All {
		return sortedStateDocuments(state), nil
	}
	requested := make(map[string]bool, len(options.Paths))
	for _, value := range options.Paths {
		cleaned := filepath.ToSlash(filepath.Clean(value))
		if cleaned == "." {
			return nil, fmt.Errorf("invalid add path %q", value)
		}
		requested[strings.TrimPrefix(cleaned, "./")] = false
	}
	documents := make([]DocumentState, 0)
	for _, document := range sortedStateDocuments(state) {
		localAbsolute, err := s.localAbsolutePath(document.LocalPath)
		if err != nil {
			return nil, err
		}
		candidates := []string{
			filepath.ToSlash(document.LocalPath),
			filepath.ToSlash(filepath.Join(s.config.OutputDir, filepath.FromSlash(document.LocalPath))),
			filepath.ToSlash(localAbsolute),
		}
		matched := false
		for path := range requested {
			for _, candidate := range candidates {
				if candidate == path || strings.HasPrefix(candidate, strings.TrimSuffix(path, "/")+"/") {
					requested[path] = true
					matched = true
				}
			}
		}
		if matched {
			documents = append(documents, document)
		}
	}
	for path, matched := range requested {
		if !matched {
			return nil, fmt.Errorf("path is not a tracked SiYuan document: %s", path)
		}
	}
	return documents, nil
}

func (s *Syncer) Push(ctx context.Context) (PushResult, error) {
	result := PushResult{Changes: []string{}, CommitIDs: []string{}, Conflicts: []string{}}
	state, err := LoadState(s.root)
	if err != nil {
		return result, err
	}
	refs, err := EnsureRepositorySnapshots(s.root, s.config, state)
	if err != nil {
		return result, err
	}
	store := NewObjectStore(s.root)
	if err := rejectActivePull(store, refs, "push"); err != nil {
		return result, err
	}
	if _, err := markRepositoryObjects(s.root, refs); err != nil {
		return result, fmt.Errorf("validate repository object graph before push: %w", err)
	}
	commits, err := ListPendingCommits(s.root)
	if err != nil {
		return result, err
	}
	if len(commits) == 0 {
		return result, nil
	}
	if len(commits) > 1 {
		return result, errors.New("snapshot push currently supports exactly one pending commit")
	}
	for commitIndex := range commits {
		commit := &commits[commitIndex]
		result.CommitIDs = append(result.CommitIDs, commit.ID)
		refs, err = LoadRepositoryRefs(s.root)
		if err != nil {
			return result, err
		}
		if refs.Head != commit.ObjectID {
			return result, fmt.Errorf("pending commit %s is not repository HEAD", commit.ID)
		}
		execution, err := s.beginPushExecution(store, refs, commit)
		if err != nil {
			return result, err
		}
		commitObject, err := LoadCommitObject(store, commit.ObjectID)
		if err != nil {
			return result, err
		}
		patch, err := LoadPatch(store, commitObject.Patch)
		if err != nil {
			return result, err
		}
		baseWorkspace, err := LoadWorkspaceTree(store, patch.BaseTree)
		if err != nil {
			return result, err
		}
		targetWorkspace, err := LoadWorkspaceTree(store, patch.TargetTree)
		if err != nil {
			return result, err
		}
		persistApplying := func() error {
			return execution.persist(commit, PushOperationApplying, "")
		}
		persistConflict := func() error {
			return execution.persist(commit, execution.operation.Phase, commit.Error)
		}
		persistFailed := func() error {
			return execution.persist(commit, execution.operation.Phase, commit.Error)
		}
		for documentIndex := commit.AppliedDocuments; documentIndex < len(commit.DocumentPatches); documentIndex++ {
			patch := commit.DocumentPatches[documentIndex]
			document, ok := state.Documents[patch.DocumentID]
			if !ok {
				return result, fmt.Errorf("commit %s references untracked document %s", commit.ID, patch.DocumentID)
			}
			localAbsolute, err := s.localAbsolutePath(document.LocalPath)
			if err != nil {
				return result, err
			}
			local, err := os.ReadFile(localAbsolute)
			if err != nil {
				return result, fmt.Errorf("read %s: %w", document.LocalPath, err)
			}
			allowedHash := patch.LocalHash
			canonicalHash := ""
			if canonicalDocumentTreeID := execution.operation.CanonicalDocuments[patch.DocumentID]; canonicalDocumentTreeID != "" {
				canonical, err := RenderDocumentTree(store, canonicalDocumentTreeID)
				if err != nil {
					return result, err
				}
				canonicalHash = HashContent(canonical)
			}
			localHash := HashContent(string(local))
			if localHash != allowedHash && localHash != canonicalHash {
				return result, fmt.Errorf("%s: working tree changed after commit; push refused", patch.LocalPath)
			}
			baseDocument, exists := WorkspaceDocumentByID(baseWorkspace, patch.DocumentID)
			if !exists {
				return result, fmt.Errorf("push base snapshot does not contain document %s", patch.DocumentID)
			}
			base, err := RenderDocumentTree(store, baseDocument.DocumentTreeID)
			if err != nil {
				return result, err
			}
			if HashContent(base) != patch.BaseHash {
				return result, fmt.Errorf("%s: Base changed after commit; push refused", patch.LocalPath)
			}
		}
		if (execution.operation.Phase == PushOperationPrepared || execution.operation.Phase == PushOperationApplying) &&
			!pushOperationHasRemoteEffects(execution.operation) {
			resetPreparedPreflightConflicts(commit)
			preflightDocuments := make(map[string]ObjectID, len(commit.DocumentPatches))
			for documentIndex := range commit.DocumentPatches {
				documentPatch := &commit.DocumentPatches[documentIndex]
				document := state.Documents[documentPatch.DocumentID]
				baseDocument, exists := WorkspaceDocumentByID(baseWorkspace, documentPatch.DocumentID)
				if !exists {
					return result, fmt.Errorf("push base snapshot does not contain document %s", documentPatch.DocumentID)
				}
				base, err := RenderDocumentTree(store, baseDocument.DocumentTreeID)
				if err != nil {
					return result, err
				}
				remoteDocument, err := s.fetchAnnotatedDocument(ctx, documentPatch.DocumentID)
				if err != nil {
					var apiError *siyuan.APIError
					if errors.As(err, &apiError) {
						return s.recordCommitConflict(result, commit, documentPatch, document, base, documentPatch.LocalContent, "push preflight could not read the document: "+err.Error(), "", persistConflict)
					}
					return result, err
				}
				if err := ValidateDocumentPatchAgainstRemote(*documentPatch, remoteDocument); err != nil {
					return s.recordCommitConflict(result, commit, documentPatch, document, base, documentPatch.LocalContent, "push preflight failed: "+err.Error(), RenderAnnotated(remoteDocument), persistConflict)
				}
				documentTreeID, err := StoreDocumentTree(store, documentPatch.DocumentID, remoteDocument)
				if err != nil {
					return result, err
				}
				preflightDocuments[documentPatch.DocumentID] = documentTreeID
			}
			execution.operation.PreflightDocuments = preflightDocuments
			if err := execution.persist(commit, execution.operation.Phase, ""); err != nil {
				return result, err
			}
		}

		commit.Status = CommitPushing
		commit.Error = ""
		resumePhase := execution.operation.Phase
		if resumePhase == PushOperationPrepared {
			resumePhase = PushOperationApplying
		}
		if err := execution.persist(commit, resumePhase, ""); err != nil {
			return result, err
		}

		for documentIndex := commit.AppliedDocuments; documentIndex < len(commit.DocumentPatches); documentIndex++ {
			patch := &commit.DocumentPatches[documentIndex]
			document := state.Documents[patch.DocumentID]
			baseDocument, exists := WorkspaceDocumentByID(baseWorkspace, patch.DocumentID)
			if !exists {
				return result, fmt.Errorf("push base snapshot does not contain document %s", patch.DocumentID)
			}
			base, err := RenderDocumentTree(store, baseDocument.DocumentTreeID)
			if err != nil {
				return result, err
			}
			remoteDocument, err := s.fetchAnnotatedDocument(ctx, document.ID)
			if err != nil {
				var apiError *siyuan.APIError
				if errors.As(err, &apiError) {
					return s.recordCommitConflict(result, commit, patch, document, base, patch.LocalContent, "document unavailable in SiYuan: "+err.Error(), "", persistConflict)
				}
				return result, err
			}
			remote := RenderAnnotated(remoteDocument)
			if patch.InFlightOperation != nil {
				operationIndex := *patch.InFlightOperation
				if operationIndex < 0 || operationIndex >= len(patch.Operations) || operationIndex != patch.AppliedOperations {
					message := fmt.Sprintf("invalid in-flight operation index %d for applied operation count %d", operationIndex, patch.AppliedOperations)
					return s.recordCommitConflict(result, commit, patch, document, base, patch.LocalContent, message, remote, persistConflict)
				}
				if _, err := s.reconcileInFlightOperation(ctx, patch, false, persistApplying); err != nil {
					var conflictErr *DocumentPatchConflictError
					if errors.As(err, &conflictErr) {
						return s.recordCommitConflict(result, commit, patch, document, base, patch.LocalContent, conflictErr.Error(), remote, persistConflict)
					}
					return result, err
				}
			}
			if err := s.validateAppliedOperations(ctx, *patch); err != nil {
				return s.recordCommitConflict(result, commit, patch, document, base, patch.LocalContent, err.Error(), remote, persistConflict)
			}
			remainingPatch := *patch
			remainingPatch.Operations = patch.Operations[patch.AppliedOperations:]
			if err := ValidateDocumentPatchAgainstRemote(remainingPatch, remoteDocument); err != nil {
				return s.recordCommitConflict(result, commit, patch, document, base, patch.LocalContent, err.Error(), remote, persistConflict)
			}
			if patch.AppliedOperations == 0 && patch.InFlightOperation == nil {
				if err := s.ensureDocumentHistory(ctx, patch, persistApplying); err != nil {
					commit.Status = CommitFailed
					commit.Error = err.Error()
					patch.Status = DocumentPatchFailed
					patch.Error = err.Error()
					_ = persistFailed()
					return result, err
				}
			}

			patch.Status = DocumentPatchApplying
			if err := persistApplying(); err != nil {
				return result, err
			}
			for operationIndex := patch.AppliedOperations; operationIndex < len(patch.Operations); operationIndex++ {
				operation := &patch.Operations[operationIndex]
				if err := s.prepareOperation(ctx, operation); err != nil {
					var conflictErr *DocumentPatchConflictError
					if errors.As(err, &conflictErr) {
						latest, fetchErr := s.fetchAnnotatedDocument(ctx, document.ID)
						if fetchErr != nil {
							return result, fetchErr
						}
						return s.recordCommitConflict(result, commit, patch, document, base, patch.LocalContent, conflictErr.Error(), RenderAnnotated(latest), persistConflict)
					}
					commit.Status = CommitFailed
					commit.Error = err.Error()
					patch.Status = DocumentPatchFailed
					patch.Error = err.Error()
					_ = persistFailed()
					return result, err
				}
				if err := persistApplying(); err != nil {
					return result, err
				}
				inFlightOperation := operationIndex
				patch.InFlightOperation = &inFlightOperation
				if err := persistApplying(); err != nil {
					return result, err
				}
				if err := s.applyOperation(ctx, operation, persistApplying); err != nil {
					var conflictErr *DocumentPatchConflictError
					if errors.As(err, &conflictErr) {
						latest, fetchErr := s.fetchAnnotatedDocument(ctx, document.ID)
						if fetchErr != nil {
							return result, fetchErr
						}
						return s.recordCommitConflict(result, commit, patch, document, base, patch.LocalContent, conflictErr.Error(), RenderAnnotated(latest), persistConflict)
					}
					commit.Status = CommitFailed
					commit.Error = err.Error()
					patch.Status = DocumentPatchFailed
					patch.Error = err.Error()
					_ = persistFailed()
					return result, err
				}
				patch.AppliedOperations = operationIndex + 1
				patch.InFlightOperation = nil
				if err := persistApplying(); err != nil {
					return result, err
				}
			}

			freshRemoteDocument, err := s.fetchAnnotatedDocument(ctx, document.ID)
			if err != nil {
				return result, err
			}
			canonicalDocumentTreeID, err := StoreDocumentTree(store, document.ID, freshRemoteDocument)
			if err != nil {
				return result, err
			}
			patch.Status = DocumentPatchApplied
			patch.Error = ""
			commit.AppliedDocuments = documentIndex + 1
			if err := execution.recordCanonicalDocument(commit, document.ID, canonicalDocumentTreeID); err != nil {
				return result, err
			}
		}

		canonicalWorkspace := targetWorkspace
		for documentIndex := range commit.DocumentPatches {
			patch := &commit.DocumentPatches[documentIndex]
			documentTreeID := execution.operation.CanonicalDocuments[patch.DocumentID]
			if documentTreeID == "" {
				return result, fmt.Errorf("push phase %s is missing the canonical snapshot for %s", execution.operation.Phase, patch.LocalPath)
			}
			if err := ReplaceWorkspaceDocumentTree(&canonicalWorkspace, patch.DocumentID, documentTreeID); err != nil {
				return result, err
			}
		}
		if execution.operation.Phase == PushOperationApplying {
			if err := execution.persist(commit, PushOperationRemoteVerified, ""); err != nil {
				return result, err
			}
		}
		expectedCanonicalTreeID, err := StoreWorkspaceTreeObject(store, canonicalWorkspace)
		if err != nil {
			return result, err
		}
		canonicalTreeID := execution.operation.CanonicalTree
		if execution.operation.Phase == PushOperationRemoteVerified {
			canonicalTreeID = expectedCanonicalTreeID
			execution.operation.CanonicalTree = canonicalTreeID
			if err := execution.persist(commit, PushOperationCanonicalSnapshot, ""); err != nil {
				return result, err
			}
		} else if canonicalTreeID != expectedCanonicalTreeID {
			return result, errors.New("saved canonical WorkspaceTree does not match the verified document snapshots")
		}
		if execution.operation.Phase == PushOperationCanonicalSnapshot {
			if err := execution.persist(commit, PushOperationMaterializing, ""); err != nil {
				return result, err
			}
		}

		for documentIndex := range commit.DocumentPatches {
			patch := &commit.DocumentPatches[documentIndex]
			if containsString(execution.operation.MaterializedDocuments, patch.DocumentID) {
				continue
			}
			document := state.Documents[patch.DocumentID]
			localAbsolute, err := s.localAbsolutePath(document.LocalPath)
			if err != nil {
				return result, err
			}
			canonicalDocumentTreeID := execution.operation.CanonicalDocuments[patch.DocumentID]
			canonical, err := RenderDocumentTree(store, canonicalDocumentTreeID)
			if err != nil {
				return result, err
			}
			local, err := os.ReadFile(localAbsolute)
			if err != nil {
				return result, err
			}
			localHash := HashContent(string(local))
			if localHash != patch.LocalHash && localHash != HashContent(canonical) {
				return result, fmt.Errorf("%s: working tree changed before canonical materialization", patch.LocalPath)
			}
			freshObservation, err := s.observeStableRemoteDocument(
				ctx,
				document.ID,
				document.NotebookID,
				document.NotebookName,
				document.Title,
				document.RemotePath,
				document.LocalPath,
			)
			if err != nil {
				return result, err
			}
			freshRemoteDocument := freshObservation.Document
			freshRemote := RenderAnnotated(freshRemoteDocument)
			if HashContent(freshRemote) != HashContent(canonical) {
				return s.recordCommitConflict(result, commit, patch, document, "", patch.LocalContent, "SiYuan changed after the pushed result was verified", freshRemote, persistConflict)
			}
			if err := writeDocumentMetadata(s.root, freshObservation.Metadata); err != nil {
				return result, err
			}
			latestLocal, err := os.ReadFile(localAbsolute)
			if err != nil {
				return result, err
			}
			latestLocalHash := HashContent(string(latestLocal))
			canonicalHash := HashContent(canonical)
			if latestLocalHash != patch.LocalHash && latestLocalHash != canonicalHash {
				return result, fmt.Errorf("%s: working tree changed during canonical materialization", patch.LocalPath)
			}
			if latestLocalHash != canonicalHash {
				if err := WriteFileAtomic(localAbsolute, []byte(canonical), 0o644); err != nil {
					return result, err
				}
			}
			state.Documents[document.ID] = document
			now := time.Now().UTC()
			state.LastSyncAt = &now
			if err := SaveState(s.root, state); err != nil {
				return result, err
			}
			if err := clearConflict(s.root, document.ID); err != nil {
				return result, err
			}
			if err := execution.recordMaterializedDocument(commit, document.ID); err != nil {
				return result, err
			}
			result.PushedDocuments++
			result.Changes = append(result.Changes, patch.LocalPath)
		}
		if err := execution.finish(commit.ObjectID, canonicalTreeID); err != nil {
			return result, err
		}
		result.PushedCommits++
	}
	return result, nil
}

func (s *Syncer) ContinuePush(ctx context.Context) (PushResult, error) {
	refs, err := LoadRepositoryRefs(s.root)
	if err != nil {
		return PushResult{}, err
	}
	if refs.Operation == "" {
		return PushResult{}, errors.New("no push operation is in progress")
	}
	kind, err := LoadRepositoryOperationKind(NewObjectStore(s.root), refs.Operation)
	if err != nil {
		return PushResult{}, err
	}
	if kind != "push" {
		return PushResult{}, fmt.Errorf("active operation is %s, not push", kind)
	}
	return s.Push(ctx)
}

func (s *Syncer) ensureDocumentHistory(ctx context.Context, patch *DocumentPatch, persist func() error) error {
	if patch.HistoryCheckpoint != nil && patch.HistoryCheckpoint.Status == HistoryCheckpointVerified {
		return nil
	}
	if patch.HistoryCheckpoint == nil {
		now := time.Now().UTC()
		patch.HistoryCheckpoint = &HistoryCheckpoint{Status: HistoryCheckpointRequested, RequestedAt: now}
		if err := persist(); err != nil {
			return err
		}
	}
	if patch.HistoryCheckpoint.Status == HistoryCheckpointRequested {
		if err := s.api.CreateDocHistory(ctx, patch.DocumentID); err != nil {
			return fmt.Errorf("create SiYuan history for %s: %w", patch.LocalPath, err)
		}
		now := time.Now().UTC()
		patch.HistoryCheckpoint.Status = HistoryCheckpointAcceptedUnverified
		patch.HistoryCheckpoint.AcceptedAt = &now
		patch.HistoryCheckpoint.VerificationError = ""
		if err := persist(); err != nil {
			return err
		}
	}
	created, err := s.verifyDocumentHistory(ctx, patch.DocumentID, patch.HistoryCheckpoint.RequestedAt)
	if err != nil {
		patch.HistoryCheckpoint.Status = HistoryCheckpointUnverified
		patch.HistoryCheckpoint.VerificationError = err.Error()
	} else if created == "" {
		patch.HistoryCheckpoint.Status = HistoryCheckpointUnverified
		patch.HistoryCheckpoint.VerificationError = "searchHistory did not return a matching document history checkpoint"
	} else {
		patch.HistoryCheckpoint.Status = HistoryCheckpointVerified
		patch.HistoryCheckpoint.VerifiedCreated = created
		patch.HistoryCheckpoint.VerificationError = ""
	}
	if err := persist(); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return nil
}

func (s *Syncer) verifyDocumentHistory(ctx context.Context, documentID string, requestedAt time.Time) (string, error) {
	const attempts = 5
	const retryDelay = 100 * time.Millisecond
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		result, err := s.api.SearchHistory(ctx, documentID)
		if err == nil {
			lastErr = nil
			if created := matchingHistoryCreated(result.Histories, requestedAt); created != "" {
				return created, nil
			}
		} else {
			lastErr = err
		}
		if attempt == attempts-1 {
			break
		}
		timer := time.NewTimer(retryDelay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return "", ctx.Err()
		case <-timer.C:
		}
	}
	return "", lastErr
}

func matchingHistoryCreated(histories []string, requestedAt time.Time) string {
	threshold := requestedAt.Unix() - 1
	for _, created := range histories {
		createdUnix, err := strconv.ParseInt(created, 10, 64)
		if err == nil && createdUnix >= threshold {
			return created
		}
	}
	return ""
}

func (s *Syncer) recordCommitConflict(
	result PushResult,
	commit *Commit,
	patch *DocumentPatch,
	document DocumentState,
	base, local, message, remote string,
	persist func() error,
) (PushResult, error) {
	commit.Status = CommitConflict
	commit.Error = message
	patch.Status = DocumentPatchConflict
	patch.Error = message
	if err := persist(); err != nil {
		return result, err
	}
	if remote != "" {
		if _, err := writeConflict(s.root, document, base, local, remote); err != nil {
			return result, err
		}
	}
	result.Conflicts = append(result.Conflicts, fmt.Sprintf("%s: %s", patch.LocalPath, message))
	return result, nil
}

func (s *Syncer) applyOperation(ctx context.Context, operation *Operation, persistReceipt func() error) error {
	switch operation.Type {
	case OperationUpdate:
		return s.applyUpdateOperation(ctx, operation, persistReceipt)
	case OperationDelete:
		return s.applyDeleteOperation(ctx, operation, persistReceipt)
	case OperationInsert:
		return s.applyInsertOperation(ctx, operation, persistReceipt)
	default:
		return fmt.Errorf("unsupported patch operation %s", operation.Type)
	}
}

func (s *Syncer) prepareOperation(ctx context.Context, operation *Operation) error {
	if operation.Type == OperationInsert && operation.InsertPrecondition == nil {
		children, err := s.readStableChildBlocks(ctx, operation.ParentID)
		if err != nil {
			return fmt.Errorf("read insert parent %s: %w", operation.ParentID, err)
		}
		childIDs := childBlockIDs(children)
		if err := validateInsertionPoint(childIDs, *operation); err != nil {
			return &DocumentPatchConflictError{Operation: operation.Type, Message: err.Error()}
		}
		operation.InsertPrecondition = &InsertPrecondition{ChildBlockIDs: childIDs}
		return nil
	}
	if operation.Type != OperationUpdate || operation.PreservedAttrs != nil {
		return nil
	}
	current, exists, err := s.readBlockKramdown(ctx, operation.BlockID)
	if err != nil {
		return err
	}
	if !exists {
		return &DocumentPatchConflictError{Operation: operation.Type, BlockID: operation.BlockID, Message: "target block no longer exists"}
	}
	blockIDs := uniqueIDs(ExtractBlockIDs(current))
	attrsByID, err := s.api.BatchGetBlockAttrs(ctx, blockIDs)
	if err != nil {
		return fmt.Errorf("read attributes for block %s: %w", operation.BlockID, err)
	}
	operation.PreservedAttrs = make(map[string]map[string]string, len(blockIDs))
	for _, blockID := range blockIDs {
		operation.PreservedAttrs[blockID] = preservableAttrs(attrsByID[blockID])
	}
	return nil
}

func (s *Syncer) applyUpdateOperation(ctx context.Context, operation *Operation, persistReceipt func() error) error {
	current, exists, err := s.readBlockKramdown(ctx, operation.BlockID)
	if err != nil {
		return err
	}
	if !exists {
		return &DocumentPatchConflictError{Operation: operation.Type, BlockID: operation.BlockID, Message: "target block no longer exists"}
	}
	currentHash := HashContent(current)
	if currentHash == operation.ContentHash || EquivalentBlockContent(operation.Content, current) {
		return s.verifyOperationAttrsPreserved(ctx, *operation, current)
	}
	if currentHash != operation.ExpectedHash {
		return &DocumentPatchConflictError{Operation: operation.Type, BlockID: operation.BlockID, Message: "target block changed after the patch was planned"}
	}
	if err := ValidateUniqueBlockIDs(operation.Content); err != nil {
		return &DocumentPatchConflictError{Operation: operation.Type, BlockID: operation.BlockID, Message: err.Error()}
	}
	existingIDs := make(map[string]bool)
	for _, blockID := range ExtractBlockIDs(current) {
		existingIDs[blockID] = true
	}
	if err := s.verifyPreservedAttrs(ctx, operation.PreservedAttrs, existingIDs); err != nil {
		return err
	}
	receipt, err := s.api.UpdateBlock(ctx, operation.BlockID, operation.Content)
	if err != nil {
		return fmt.Errorf("update block %s: %w", operation.BlockID, err)
	}
	if err := persistMutationReceipt(operation, receipt, persistReceipt); err != nil {
		return err
	}
	updated, exists, err := s.readBlockKramdown(ctx, operation.BlockID)
	if err != nil {
		return fmt.Errorf("re-read block %s after update: %w", operation.BlockID, err)
	}
	if !exists {
		return &DocumentPatchConflictError{Operation: operation.Type, BlockID: operation.BlockID, Message: "SiYuan removed the block while applying its update"}
	}
	if err := assertIDsPreserved(operation.Content, updated, operation.BlockID); err != nil {
		return &DocumentPatchConflictError{Operation: operation.Type, BlockID: operation.BlockID, Message: err.Error()}
	}
	return s.verifyOperationAttrsPreserved(ctx, *operation, updated)
}

func (s *Syncer) verifyOperationAttrsPreserved(ctx context.Context, operation Operation, current string) error {
	existingIDs := make(map[string]bool)
	for _, blockID := range ExtractBlockIDs(current) {
		existingIDs[blockID] = true
	}
	if !EquivalentBlockContent(operation.Content, current) {
		return &DocumentPatchConflictError{Operation: operation.Type, BlockID: operation.BlockID, Message: "SiYuan canonical content does not match the submitted block update"}
	}
	if err := s.verifyPreservedAttrs(ctx, operation.PreservedAttrs, existingIDs); err != nil {
		return err
	}
	return nil
}

func (s *Syncer) applyDeleteOperation(ctx context.Context, operation *Operation, persistReceipt func() error) error {
	current, exists, err := s.readBlockKramdown(ctx, operation.BlockID)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if HashContent(current) != operation.ExpectedHash {
		return &DocumentPatchConflictError{Operation: operation.Type, BlockID: operation.BlockID, Message: "target block changed after the patch was planned"}
	}
	receipt, err := s.api.DeleteBlock(ctx, operation.BlockID)
	if err != nil {
		return fmt.Errorf("delete block %s: %w", operation.BlockID, err)
	}
	if err := persistMutationReceipt(operation, receipt, persistReceipt); err != nil {
		return err
	}
	if _, exists, err := s.readBlockKramdown(ctx, operation.BlockID); err != nil {
		return fmt.Errorf("re-read block %s after deletion: %w", operation.BlockID, err)
	} else if exists {
		return &DocumentPatchConflictError{Operation: operation.Type, BlockID: operation.BlockID, Message: "SiYuan still returns the block after deletion"}
	}
	return nil
}

func (s *Syncer) applyInsertOperation(ctx context.Context, operation *Operation, persistReceipt func() error) error {
	if operation.InsertPrecondition == nil {
		return errors.New("insert operation has no persisted child-block precondition")
	}
	beforeIDs := append([]string(nil), operation.InsertPrecondition.ChildBlockIDs...)
	current, err := s.readStableChildBlocks(ctx, operation.ParentID)
	if err != nil {
		return fmt.Errorf("read insert parent %s: %w", operation.ParentID, err)
	}
	if !equalStringSlices(beforeIDs, childBlockIDs(current)) {
		return &DocumentPatchConflictError{Operation: operation.Type, Message: "insert parent changed after its precondition was saved"}
	}
	receipt, err := s.api.InsertBlock(ctx, operation.Content, operation.ParentID, operation.PreviousID, operation.NextID)
	if err != nil {
		return fmt.Errorf("insert block into document %s: %w", operation.ParentID, err)
	}
	if err := persistMutationReceipt(operation, receipt, persistReceipt); err != nil {
		return err
	}
	after, err := s.readStableChildBlocks(ctx, operation.ParentID)
	if err != nil {
		return fmt.Errorf("re-read insert parent %s: %w", operation.ParentID, err)
	}
	insertedIDs, err := verifyInsertedChildren(beforeIDs, childBlockIDs(after), *operation)
	if err != nil {
		return &DocumentPatchConflictError{Operation: operation.Type, Message: err.Error()}
	}
	if err := verifyReceiptBlockIDs(operation.ReceiptBlockIDs, insertedIDs); err != nil {
		return &DocumentPatchConflictError{Operation: operation.Type, Message: err.Error()}
	}
	operation.ResultBlockIDs = insertedIDs
	if err := persistReceipt(); err != nil {
		return fmt.Errorf("persist insert readback for operation %s: %w", operation.OperationID, err)
	}
	return nil
}

func persistMutationReceipt(operation *Operation, receipt siyuan.MutationReceipt, persist func() error) error {
	operation.KernelReceipt = &receipt
	operation.ReceiptBlockIDs = receipt.BlockIDs()
	if err := persist(); err != nil {
		return fmt.Errorf("persist Kernel receipt for operation %s: %w", operation.OperationID, err)
	}
	return nil
}

func (s *Syncer) reconcileInFlightOperation(
	ctx context.Context,
	patch *DocumentPatch,
	allowInsertContentMatch bool,
	persist func() error,
) (pushReconciliation, error) {
	if patch.InFlightOperation == nil {
		return "", errors.New("document patch has no in-flight operation")
	}
	operationIndex := *patch.InFlightOperation
	if operationIndex < 0 || operationIndex >= len(patch.Operations) || operationIndex != patch.AppliedOperations {
		return "", fmt.Errorf("invalid in-flight operation index %d", operationIndex)
	}
	switch patch.Operations[operationIndex].Type {
	case OperationUpdate:
		return s.reconcileInFlightUpdate(ctx, patch, persist)
	case OperationDelete:
		return s.reconcileInFlightDelete(ctx, patch, persist)
	case OperationInsert:
		return s.reconcileInFlightInsert(ctx, patch, allowInsertContentMatch, persist)
	default:
		return "", &DocumentPatchConflictError{
			Operation: patch.Operations[operationIndex].Type,
			BlockID:   patch.Operations[operationIndex].BlockID,
			Message:   "unsupported in-flight operation",
		}
	}
}

func (s *Syncer) reconcileInFlightUpdate(
	ctx context.Context,
	patch *DocumentPatch,
	persist func() error,
) (pushReconciliation, error) {
	operationIndex := *patch.InFlightOperation
	operation := &patch.Operations[operationIndex]
	current, exists, err := s.readBlockKramdown(ctx, operation.BlockID)
	if err != nil {
		return "", err
	}
	if !exists {
		return "", &DocumentPatchConflictError{Operation: operation.Type, BlockID: operation.BlockID, Message: "target block disappeared while update result was uncertain"}
	}
	if EquivalentBlockContent(operation.Content, current) {
		if err := s.verifyOperationAttrsPreserved(ctx, *operation, current); err != nil {
			return "", err
		}
		patch.AppliedOperations = operationIndex + 1
		patch.InFlightOperation = nil
		if err := persist(); err != nil {
			return "", err
		}
		return pushReconciliationApplied, nil
	}
	if HashContent(current) != operation.ExpectedHash {
		return "", &DocumentPatchConflictError{Operation: operation.Type, BlockID: operation.BlockID, Message: "remote content matches neither the update precondition nor its target"}
	}
	if operation.KernelReceipt != nil {
		return "", &DocumentPatchConflictError{Operation: operation.Type, BlockID: operation.BlockID, Message: "Kernel returned an update receipt, but the target content is absent during recovery"}
	}
	existingIDs := make(map[string]bool)
	for _, blockID := range ExtractBlockIDs(current) {
		existingIDs[blockID] = true
	}
	if err := s.verifyPreservedAttrs(ctx, operation.PreservedAttrs, existingIDs); err != nil {
		return "", err
	}
	patch.InFlightOperation = nil
	if err := persist(); err != nil {
		return "", err
	}
	return pushReconciliationNotApplied, nil
}

func (s *Syncer) reconcileInFlightDelete(
	ctx context.Context,
	patch *DocumentPatch,
	persist func() error,
) (pushReconciliation, error) {
	operationIndex := *patch.InFlightOperation
	operation := &patch.Operations[operationIndex]
	current, exists, err := s.readBlockKramdown(ctx, operation.BlockID)
	if err != nil {
		return "", err
	}
	if !exists {
		patch.AppliedOperations = operationIndex + 1
		patch.InFlightOperation = nil
		if err := persist(); err != nil {
			return "", err
		}
		return pushReconciliationApplied, nil
	}
	if HashContent(current) != operation.ExpectedHash {
		return "", &DocumentPatchConflictError{Operation: operation.Type, BlockID: operation.BlockID, Message: "remote content matches neither the delete precondition nor the deleted state"}
	}
	if operation.KernelReceipt != nil {
		return "", &DocumentPatchConflictError{Operation: operation.Type, BlockID: operation.BlockID, Message: "Kernel returned a delete receipt, but the block still exists during recovery"}
	}
	patch.InFlightOperation = nil
	if err := persist(); err != nil {
		return "", err
	}
	return pushReconciliationNotApplied, nil
}

func (s *Syncer) reconcileInFlightInsert(
	ctx context.Context,
	patch *DocumentPatch,
	allowContentMatch bool,
	persist func() error,
) (pushReconciliation, error) {
	operationIndex := *patch.InFlightOperation
	operation := &patch.Operations[operationIndex]
	if operation.InsertPrecondition == nil {
		return "", &DocumentPatchConflictError{
			Operation: operation.Type,
			Message:   "in-flight insert has no persisted child-block precondition",
		}
	}
	children, err := s.readStableChildBlocks(ctx, operation.ParentID)
	if err != nil {
		return "", fmt.Errorf("recover insert parent %s: %w", operation.ParentID, err)
	}
	beforeIDs := operation.InsertPrecondition.ChildBlockIDs
	afterIDs := childBlockIDs(children)
	if equalStringSlices(beforeIDs, afterIDs) {
		if operation.KernelReceipt != nil {
			return "", &DocumentPatchConflictError{
				Operation: operation.Type,
				Message:   "Kernel returned an insert receipt, but the inserted blocks are absent during recovery",
			}
		}
		patch.InFlightOperation = nil
		if err := persist(); err != nil {
			return "", err
		}
		return pushReconciliationNotApplied, nil
	}
	if operation.KernelReceipt == nil || len(operation.ReceiptBlockIDs) == 0 {
		if !allowContentMatch {
			return "", &DocumentPatchConflictError{
				Operation: operation.Type,
				Message:   "insert parent changed while the Kernel receipt was unavailable; run pull to reconcile the remote result",
			}
		}
		insertedIDs, err := verifyInsertedChildren(beforeIDs, afterIDs, *operation)
		if err != nil {
			return "", &DocumentPatchConflictError{Operation: operation.Type, Message: err.Error()}
		}
		matches, err := s.insertedContentMatches(ctx, insertedIDs, operation.Content)
		if err != nil {
			return "", err
		}
		if !matches {
			return "", &DocumentPatchConflictError{
				Operation: operation.Type,
				Message:   "the blocks inserted between the saved anchors do not uniquely match the pending insert",
			}
		}
		operation.ResultBlockIDs = insertedIDs
		patch.AppliedOperations = operationIndex + 1
		patch.InFlightOperation = nil
		if err := persist(); err != nil {
			return "", err
		}
		return pushReconciliationApplied, nil
	}
	insertedIDs, err := verifyInsertedChildren(beforeIDs, afterIDs, *operation)
	if err != nil {
		return "", &DocumentPatchConflictError{Operation: operation.Type, Message: err.Error()}
	}
	if err := verifyReceiptBlockIDs(operation.ReceiptBlockIDs, insertedIDs); err != nil {
		return "", &DocumentPatchConflictError{Operation: operation.Type, Message: err.Error()}
	}
	operation.ResultBlockIDs = insertedIDs
	patch.AppliedOperations = operationIndex + 1
	patch.InFlightOperation = nil
	if err := persist(); err != nil {
		return "", err
	}
	return pushReconciliationApplied, nil
}

func (s *Syncer) insertedContentMatches(ctx context.Context, blockIDs []string, expected string) (bool, error) {
	kramdowns, err := s.api.GetBlockKramdowns(ctx, blockIDs)
	if err != nil {
		return false, fmt.Errorf("read candidate inserted blocks: %w", err)
	}
	parts := make([]string, 0, len(blockIDs))
	for _, blockID := range blockIDs {
		content, ok := kramdowns[blockID]
		if !ok {
			return false, fmt.Errorf("SiYuan did not return Kramdown for candidate inserted block %s", blockID)
		}
		parts = append(parts, stripGeneratedBlockIdentity(content))
	}
	expected = stripGeneratedBlockIdentity(expected)
	for _, actual := range []string{
		strings.Join(parts, ""),
		strings.Join(parts, "\n"),
	} {
		if HashContent(actual) == HashContent(expected) {
			return true, nil
		}
	}
	return false, nil
}

func verifyReceiptBlockIDs(receiptIDs, insertedIDs []string) error {
	if len(receiptIDs) == 0 {
		return errors.New("Kernel insert receipt did not contain a generated Block ID")
	}
	inserted := make(map[string]bool, len(insertedIDs))
	for _, id := range insertedIDs {
		inserted[id] = true
	}
	for _, id := range receiptIDs {
		if !inserted[id] {
			return fmt.Errorf("Kernel receipt Block ID %s is not present in the insert readback", id)
		}
	}
	return nil
}

func equalStringSlices(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func (s *Syncer) validateAppliedOperations(ctx context.Context, patch DocumentPatch) error {
	for index := 0; index < patch.AppliedOperations; index++ {
		operation := patch.Operations[index]
		switch operation.Type {
		case OperationUpdate:
			current, exists, err := s.readBlockKramdown(ctx, operation.BlockID)
			if err != nil {
				return err
			}
			if !exists || !EquivalentBlockContent(operation.Content, current) {
				return &DocumentPatchConflictError{Operation: operation.Type, BlockID: operation.BlockID, Message: "an operation recorded as applied is no longer present in SiYuan"}
			}
			existingIDs := make(map[string]bool)
			for _, blockID := range ExtractBlockIDs(current) {
				existingIDs[blockID] = true
			}
			if err := s.verifyPreservedAttrs(ctx, operation.PreservedAttrs, existingIDs); err != nil {
				return err
			}
		case OperationDelete:
			if _, exists, err := s.readBlockKramdown(ctx, operation.BlockID); err != nil {
				return err
			} else if exists {
				return &DocumentPatchConflictError{Operation: operation.Type, BlockID: operation.BlockID, Message: "an operation recorded as applied was reverted in SiYuan"}
			}
		case OperationInsert:
			children, err := s.readStableChildBlocks(ctx, operation.ParentID)
			if err != nil {
				return err
			}
			if err := validateAppliedInsert(childBlockIDs(children), operation); err != nil {
				return &DocumentPatchConflictError{Operation: operation.Type, Message: err.Error()}
			}
		default:
			return &DocumentPatchConflictError{Operation: operation.Type, BlockID: operation.BlockID, Message: "unsupported applied operation"}
		}
	}
	return nil
}

func (s *Syncer) readBlockKramdown(ctx context.Context, blockID string) (string, bool, error) {
	if err := s.flushKernelTransactions(ctx); err != nil {
		return "", false, err
	}
	previousContent := ""
	previousExists := false
	havePrevious := false
	for attempt := 0; attempt < remoteObservationAttempts; attempt++ {
		kramdown, err := s.api.GetBlockKramdown(ctx, blockID)
		if err != nil {
			return "", false, err
		}
		content := Canonicalize(kramdown)
		exists := content != ""
		if havePrevious && exists == previousExists && content == previousContent {
			return content, exists, nil
		}
		previousContent = content
		previousExists = exists
		havePrevious = true
	}
	return "", false, fmt.Errorf("%w: block %s did not produce two consecutive identical observations", ErrRemoteUnstable, blockID)
}

func (s *Syncer) readStableChildBlocks(ctx context.Context, parentID string) ([]siyuan.ChildBlock, error) {
	if err := s.flushKernelTransactions(ctx); err != nil {
		return nil, err
	}
	var previous []siyuan.ChildBlock
	havePrevious := false
	for attempt := 0; attempt < remoteObservationAttempts; attempt++ {
		current, err := s.api.GetChildBlocks(ctx, parentID)
		if err != nil {
			return nil, err
		}
		if havePrevious && slices.Equal(previous, current) {
			return current, nil
		}
		previous = append([]siyuan.ChildBlock(nil), current...)
		havePrevious = true
	}
	return nil, fmt.Errorf("%w: child list of %s did not produce two consecutive identical observations", ErrRemoteUnstable, parentID)
}

func (s *Syncer) verifyPreservedAttrs(ctx context.Context, expected map[string]map[string]string, existing map[string]bool) error {
	ids := make([]string, 0, len(expected))
	for blockID := range expected {
		if existing[blockID] {
			ids = append(ids, blockID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	attrsByID, err := s.api.BatchGetBlockAttrs(ctx, ids)
	if err != nil {
		return fmt.Errorf("verify preserved block attributes: %w", err)
	}
	for _, blockID := range ids {
		actual := attrsByID[blockID]
		for name, value := range expected[blockID] {
			if actual[name] != value {
				return &DocumentPatchConflictError{Operation: OperationUpdate, BlockID: blockID, Message: fmt.Sprintf("attribute %s was not preserved", name)}
			}
		}
	}
	return nil
}

func uniqueIDs(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	result := make([]string, 0, len(ids))
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		result = append(result, id)
	}
	return result
}

func childBlockIDs(blocks []siyuan.ChildBlock) []string {
	ids := make([]string, 0, len(blocks))
	for _, block := range blocks {
		ids = append(ids, block.ID)
	}
	return ids
}

func verifyInsertedChildren(before, after []string, operation Operation) ([]string, error) {
	beforeSet := make(map[string]bool, len(before))
	for _, id := range before {
		beforeSet[id] = true
	}
	retained := make([]string, 0, len(before))
	for _, id := range after {
		if beforeSet[id] {
			retained = append(retained, id)
		}
	}
	if len(retained) != len(before) {
		return nil, errors.New("an existing top-level block disappeared during insertion")
	}
	for index := range before {
		if retained[index] != before[index] {
			return nil, errors.New("top-level block order changed during insertion")
		}
	}
	previousIndex := -1
	if operation.PreviousID != "" {
		previousIndex = indexOfID(after, operation.PreviousID)
	}
	nextIndex := len(after)
	if operation.NextID != "" {
		nextIndex = indexOfID(after, operation.NextID)
	}
	if nextIndex < 0 || previousIndex >= nextIndex {
		return nil, errors.New("insert anchors became invalid after insertion")
	}
	inserted := after[previousIndex+1 : nextIndex]
	if len(inserted) == 0 {
		return nil, errors.New("SiYuan did not create a top-level block for the insert operation")
	}
	for _, id := range inserted {
		if beforeSet[id] {
			return nil, errors.New("SiYuan placed the inserted content outside the requested anchors")
		}
	}
	return append([]string(nil), inserted...), nil
}

func validateAppliedInsert(children []string, operation Operation) error {
	if len(operation.ResultBlockIDs) == 0 {
		return errors.New("applied insert has no recorded Kernel block IDs")
	}
	start := 0
	if operation.PreviousID != "" {
		previousIndex := indexOfID(children, operation.PreviousID)
		if previousIndex < 0 {
			return fmt.Errorf("insert previous anchor %s no longer exists", operation.PreviousID)
		}
		start = previousIndex + 1
	}
	end := start + len(operation.ResultBlockIDs)
	if end > len(children) {
		return errors.New("inserted Kernel blocks no longer exist at the recorded position")
	}
	for index, blockID := range operation.ResultBlockIDs {
		if children[start+index] != blockID {
			return errors.New("inserted Kernel blocks no longer exist at the recorded position")
		}
	}
	if operation.NextID != "" {
		if end >= len(children) || children[end] != operation.NextID {
			return fmt.Errorf("insert next anchor %s is no longer adjacent", operation.NextID)
		}
	} else if end != len(children) {
		return errors.New("recorded append is no longer at the end of the document")
	}
	return nil
}

func (s *Syncer) Restore(ctx context.Context, localPath, requestedStrategy string) (RestoreResult, error) {
	if pending, err := HasPendingCommit(s.root); err != nil {
		return RestoreResult{}, err
	} else if pending {
		return RestoreResult{}, errors.New("reset the pending commit before restoring a conflicted file")
	}
	state, err := LoadState(s.root)
	if err != nil {
		return RestoreResult{}, err
	}
	refs, err := EnsureRepositorySnapshots(s.root, s.config, state)
	if err != nil {
		return RestoreResult{}, err
	}
	if err := rejectActivePull(NewObjectStore(s.root), refs, "restore"); err != nil {
		return RestoreResult{}, err
	}
	document, err := s.findDocumentForRestore(state, localPath)
	if err != nil {
		return RestoreResult{}, err
	}
	strategy := strings.ToLower(requestedStrategy)
	if strategy != "ours" && strategy != "theirs" {
		return RestoreResult{}, errors.New("restore requires exactly one of --ours or --theirs")
	}
	staged, err := ListStagedDocumentPatches(s.root)
	if err != nil {
		return RestoreResult{}, err
	}
	for _, patch := range staged {
		if patch.DocumentID == document.ID {
			return RestoreResult{}, fmt.Errorf("%s is staged; reset the index before restoring it", document.LocalPath)
		}
	}
	return s.restoreConflict(ctx, &state, document, strategy)
}

func (s *Syncer) restoreConflict(ctx context.Context, state *State, document DocumentState, strategy string) (RestoreResult, error) {
	conflict, savedRemote, err := readConflictRemote(s.root, document.ID)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("read conflict for %s: %w", document.LocalPath, err)
	}
	currentRemoteDocument, err := s.fetchAnnotatedDocument(ctx, document.ID)
	if err != nil {
		return RestoreResult{}, err
	}
	currentRemote := RenderAnnotated(currentRemoteDocument)
	if HashContent(currentRemote) != conflict.RemoteHash || HashContent(savedRemote) != conflict.RemoteHash {
		return RestoreResult{}, errors.New("SiYuan changed after the conflict snapshot; run pull again before restoring")
	}
	localAbsolute, err := s.localAbsolutePath(document.LocalPath)
	if err != nil {
		return RestoreResult{}, err
	}
	status := StatusClean
	switch strategy {
	case "theirs":
		if err := WriteFileAtomic(localAbsolute, []byte(currentRemote), 0o644); err != nil {
			return RestoreResult{}, err
		}
	case "ours", "manual":
		var local []byte
		if strategy == "ours" {
			local, err = os.ReadFile(filepath.Join(conflictDirectory(s.root, document.ID), "local.md"))
		} else {
			local, err = os.ReadFile(localAbsolute)
		}
		if err != nil {
			return RestoreResult{}, fmt.Errorf("read local resolution: %w", err)
		}
		resolvedPatch, err := BuildDocumentPatch(document.ID, document.LocalPath, currentRemote, string(local))
		if err != nil {
			return RestoreResult{}, fmt.Errorf("resolved content is unsafe: %w", err)
		}
		if err := ValidateDocumentPatchSafety(resolvedPatch, currentRemote); err != nil {
			return RestoreResult{}, fmt.Errorf("resolved content is unsafe: %w", err)
		}
		if strategy == "ours" {
			if err := WriteFileAtomic(localAbsolute, local, 0o644); err != nil {
				return RestoreResult{}, err
			}
		}
		status = StatusLocalModified
	}
	if err := s.writeRemoteMetadata(
		ctx,
		document.ID,
		document.NotebookID,
		document.NotebookName,
		document.Title,
		document.RemotePath,
		document.LocalPath,
		currentRemoteDocument,
	); err != nil {
		return RestoreResult{}, err
	}
	state.Documents[document.ID] = document
	now := time.Now().UTC()
	state.LastSyncAt = &now
	if err := SaveState(s.root, *state); err != nil {
		return RestoreResult{}, err
	}
	refs, err := EnsureRepositorySnapshots(s.root, s.config, *state)
	if err != nil {
		return RestoreResult{}, err
	}
	store := NewObjectStore(s.root)
	remoteWorkspace, err := LoadWorkspaceTree(store, refs.Remote)
	if err != nil {
		return RestoreResult{}, err
	}
	documentTreeID, err := StoreDocumentTree(store, document.ID, currentRemoteDocument)
	if err != nil {
		return RestoreResult{}, err
	}
	if err := ReplaceWorkspaceDocumentTree(&remoteWorkspace, document.ID, documentTreeID); err != nil {
		return RestoreResult{}, err
	}
	remoteTreeID, err := StoreWorkspaceTreeObject(store, remoteWorkspace)
	if err != nil {
		return RestoreResult{}, err
	}
	if err := AdvanceRemoteSnapshot(s.root, store, remoteTreeID, true); err != nil {
		return RestoreResult{}, err
	}
	if err := clearConflict(s.root, document.ID); err != nil {
		return RestoreResult{}, err
	}
	return RestoreResult{DocumentID: document.ID, LocalPath: document.LocalPath, Strategy: strategy, Status: status}, nil
}

func (s *Syncer) loadRemoteInventory(ctx context.Context) (remoteInventory, error) {
	if err := s.flushKernelTransactions(ctx); err != nil {
		return remoteInventory{}, err
	}
	var previous remoteInventory
	for attempt := 0; attempt < remoteObservationAttempts; attempt++ {
		current, err := s.loadRemoteInventoryOnce(ctx)
		if err != nil {
			return remoteInventory{}, err
		}
		if previous.DocumentsByID != nil {
			equal, err := sameRemoteInventory(previous, current)
			if err != nil {
				return remoteInventory{}, err
			}
			if equal {
				return current, nil
			}
		}
		previous = current
	}
	return remoteInventory{}, fmt.Errorf("%w: notebook and document inventory did not produce two consecutive identical observations", ErrRemoteUnstable)
}

func (s *Syncer) loadRemoteInventoryOnce(ctx context.Context) (remoteInventory, error) {
	allNotebooks, err := s.api.ListNotebooks(ctx)
	if err != nil {
		return remoteInventory{}, err
	}
	selected := make(map[string]bool, len(s.config.NotebookIDs))
	for _, id := range s.config.NotebookIDs {
		selected[id] = true
	}
	var notebooks []siyuan.Notebook
	found := map[string]bool{}
	for _, notebook := range allNotebooks {
		if len(selected) == 0 {
			if notebook.Closed {
				continue
			}
		} else if !selected[notebook.ID] {
			continue
		}
		notebooks = append(notebooks, notebook)
		found[notebook.ID] = true
	}
	for id := range selected {
		if !found[id] {
			return remoteInventory{}, fmt.Errorf("configured notebook ID not found: %s", id)
		}
	}
	sort.Slice(notebooks, func(i, j int) bool {
		if notebooks[i].Sort != notebooks[j].Sort {
			return notebooks[i].Sort < notebooks[j].Sort
		}
		return notebooks[i].Name < notebooks[j].Name
	})

	inventory := remoteInventory{
		Notebooks:           notebooks,
		DocumentsByNotebook: map[string][]*DocumentNode{},
		DocumentsByID:       map[string]*DocumentNode{},
	}
	for _, notebook := range notebooks {
		documents, err := s.loadDocumentChildren(ctx, notebook, "/", inventory.DocumentsByID)
		if err != nil {
			return remoteInventory{}, err
		}
		inventory.DocumentsByNotebook[notebook.ID] = documents
	}
	return inventory, nil
}

func (s *Syncer) loadDocumentChildren(ctx context.Context, notebook siyuan.Notebook, remotePath string, byID map[string]*DocumentNode) ([]*DocumentNode, error) {
	documents, err := s.api.ListDocuments(ctx, notebook.ID, remotePath)
	if err != nil {
		return nil, err
	}
	nodes := make([]*DocumentNode, 0, len(documents))
	for _, document := range documents {
		node := &DocumentNode{
			Document:     document,
			NotebookID:   notebook.ID,
			NotebookName: notebook.Name,
		}
		byID[node.ID] = node
		if document.SubFileCount > 0 {
			node.Children, err = s.loadDocumentChildren(ctx, notebook, document.Path, byID)
			if err != nil {
				return nil, err
			}
		}
		nodes = append(nodes, node)
	}
	SortNodes(nodes)
	return nodes, nil
}

func (s *Syncer) fetchAnnotatedDocument(ctx context.Context, documentID string) (AnnotatedDocument, error) {
	observation, err := s.observeStableRemoteDocument(ctx, documentID, "", "", "", "", "")
	if err != nil {
		return AnnotatedDocument{}, err
	}
	return observation.Document, nil
}

func (s *Syncer) localAbsolutePath(localPath string) (string, error) {
	absolute := filepath.Join(s.outputRoot, filepath.FromSlash(localPath))
	relative, err := filepath.Rel(s.outputRoot, absolute)
	if err != nil {
		return "", err
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", fmt.Errorf("unsafe mapped path: %s", localPath)
	}
	return absolute, nil
}

func newDocumentState(document *DocumentNode, localPath string) DocumentState {
	return DocumentState{
		ID:           document.ID,
		NotebookID:   document.NotebookID,
		NotebookName: document.NotebookName,
		Title:        document.Name,
		RemotePath:   document.Path,
		LocalPath:    localPath,
	}
}

func updateDocumentMetadata(previous DocumentState, document *DocumentNode, localPath string) DocumentState {
	previous.NotebookID = document.NotebookID
	previous.NotebookName = document.NotebookName
	previous.Title = document.Name
	previous.RemotePath = document.Path
	previous.LocalPath = localPath
	return previous
}

func sortedRemoteDocuments(documents map[string]*DocumentNode) []*DocumentNode {
	result := make([]*DocumentNode, 0, len(documents))
	for _, document := range documents {
		result = append(result, document)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].NotebookName != result[j].NotebookName {
			return result[i].NotebookName < result[j].NotebookName
		}
		if result[i].Path != result[j].Path {
			return result[i].Path < result[j].Path
		}
		return result[i].ID < result[j].ID
	})
	return result
}

func sortedStateDocuments(state State) []DocumentState {
	result := make([]DocumentState, 0, len(state.Documents))
	for _, document := range state.Documents {
		result = append(result, document)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].LocalPath < result[j].LocalPath })
	return result
}

func (s *Syncer) findDocumentForRestore(state State, path string) (DocumentState, error) {
	cleaned := filepath.ToSlash(filepath.Clean(path))
	if cleaned == "." {
		return DocumentState{}, errors.New("restore requires a tracked document path")
	}
	cleaned = strings.TrimPrefix(cleaned, "./")
	for _, document := range state.Documents {
		localAbsolute, err := s.localAbsolutePath(document.LocalPath)
		if err != nil {
			return DocumentState{}, err
		}
		candidates := []string{
			filepath.ToSlash(document.LocalPath),
			filepath.ToSlash(filepath.Join(s.config.OutputDir, filepath.FromSlash(document.LocalPath))),
			filepath.ToSlash(localAbsolute),
		}
		for _, candidate := range candidates {
			if candidate == cleaned {
				return document, nil
			}
		}
	}
	return DocumentState{}, fmt.Errorf("path is not a tracked SiYuan document: %s", path)
}

func assertIDsPreserved(local, remote, localPath string) error {
	remoteIDs := map[string]bool{}
	for _, id := range ExtractBlockIDs(remote) {
		remoteIDs[id] = true
	}
	var missing []string
	for _, id := range ExtractBlockIDs(local) {
		if !remoteIDs[id] {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		if len(missing) > 8 {
			missing = missing[:8]
		}
		return fmt.Errorf("%s: SiYuan did not preserve submitted block IDs: %s", localPath, strings.Join(missing, ", "))
	}
	return nil
}

func removeEmptyParents(directory, stopAt string) {
	for directory != stopAt {
		relative, err := filepath.Rel(stopAt, directory)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return
		}
		if err := os.Remove(directory); err != nil {
			return
		}
		directory = filepath.Dir(directory)
	}
}
