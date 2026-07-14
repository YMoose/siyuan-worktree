package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

type PullOperationPhase string

const (
	PullOperationPrepared       PullOperationPhase = "prepared"
	PullOperationRemoteSnapshot PullOperationPhase = "remote-snapshot-created"
	PullOperationMaterializing  PullOperationPhase = "materializing-working-tree"
)

type PullConflictPlan struct {
	Base   ObjectID `json:"base"`
	Local  ObjectID `json:"local"`
	Remote ObjectID `json:"remote"`
}

type PullDocumentPlan struct {
	Document        DocumentState     `json:"document"`
	SourcePath      string            `json:"sourcePath,omitempty"`
	LocalPath       string            `json:"localPath"`
	ValidateSource  bool              `json:"validateSource,omitempty"`
	ExpectedMissing bool              `json:"expectedMissing,omitempty"`
	ExpectedHash    string            `json:"expectedHash,omitempty"`
	TargetContent   ObjectID          `json:"targetContent,omitempty"`
	RemoveSource    bool              `json:"removeSource,omitempty"`
	Metadata        DocumentMetadata  `json:"metadata"`
	ClearConflict   bool              `json:"clearConflict,omitempty"`
	Conflict        *PullConflictPlan `json:"conflict,omitempty"`
}

type PullOperationState struct {
	Kind                  string             `json:"kind"`
	Phase                 PullOperationPhase `json:"phase"`
	Head                  ObjectID           `json:"head"`
	Index                 ObjectID           `json:"index"`
	BaseTree              ObjectID           `json:"baseTree"`
	WorkingTree           ObjectID           `json:"workingTree"`
	RemoteTree            ObjectID           `json:"remoteTree,omitempty"`
	Plans                 []PullDocumentPlan `json:"plans,omitempty"`
	MaterializedDocuments []string           `json:"materializedDocuments,omitempty"`
	StartState            State              `json:"startState"`
	TargetState           State              `json:"targetState,omitempty"`
	Result                PullResult         `json:"result"`
	AdvanceHead           bool               `json:"advanceHead,omitempty"`
	Error                 string             `json:"error,omitempty"`
	UpdatedAt             time.Time          `json:"updatedAt"`
}

type pullExecution struct {
	syncer      *Syncer
	store       *ObjectStore
	operationID ObjectID
	operation   PullOperationState
}

func LoadRepositoryOperationKind(store *ObjectStore, id ObjectID) (string, error) {
	if id == "" {
		return "", nil
	}
	var header struct {
		Kind string `json:"kind"`
	}
	if err := store.Get(id, "", snapshotObjectVersion, &header); err != nil {
		return "", err
	}
	if header.Kind == "" {
		return "", fmt.Errorf("operation %s has no kind", id)
	}
	return header.Kind, nil
}

func rejectActivePull(store *ObjectStore, refs RepositoryRefs, command string) error {
	if refs.Operation == "" {
		return nil
	}
	kind, err := LoadRepositoryOperationKind(store, refs.Operation)
	if err != nil {
		return err
	}
	if kind == "pull" {
		return fmt.Errorf("%s cannot run while pull is incomplete; run pull again to resume it", command)
	}
	return nil
}

func StorePullOperation(store *ObjectStore, operation PullOperationState) (ObjectID, error) {
	operation.Kind = "pull"
	if operation.UpdatedAt.IsZero() {
		operation.UpdatedAt = time.Now().UTC()
	}
	if err := validatePullOperation(operation); err != nil {
		return "", err
	}
	return store.Put(pullOperationObjectType, snapshotObjectVersion, operation)
}

func LoadPullOperation(store *ObjectStore, id ObjectID) (PullOperationState, error) {
	var operation PullOperationState
	if err := store.Get(id, pullOperationObjectType, snapshotObjectVersion, &operation); err != nil {
		return PullOperationState{}, err
	}
	if operation.Kind != "pull" {
		return PullOperationState{}, fmt.Errorf("operation %s has kind %s", id, operation.Kind)
	}
	if err := validatePullOperation(operation); err != nil {
		return PullOperationState{}, fmt.Errorf("operation %s is invalid: %w", id, err)
	}
	return operation, nil
}

func validatePullOperation(operation PullOperationState) error {
	rank, ok := pullOperationPhaseRank(operation.Phase)
	if !ok {
		return fmt.Errorf("unsupported pull operation phase %s", operation.Phase)
	}
	if operation.Head == "" || operation.Index == "" || operation.BaseTree == "" || operation.WorkingTree == "" || operation.StartState.Version != 3 {
		return errors.New("pull operation has incomplete starting state")
	}
	if rank == 0 {
		if operation.RemoteTree != "" || len(operation.Plans) != 0 || len(operation.MaterializedDocuments) != 0 {
			return errors.New("prepared pull operation contains planned or materialized state")
		}
		return nil
	}
	if operation.RemoteTree == "" || operation.TargetState.Version != 3 || operation.TargetState.Documents == nil {
		return errors.New("planned pull operation has incomplete target state")
	}
	seen := make(map[string]bool, len(operation.Plans))
	for _, plan := range operation.Plans {
		if plan.Document.ID == "" || plan.LocalPath == "" || seen[plan.Document.ID] || plan.Metadata.DocumentID != plan.Document.ID {
			return errors.New("pull operation contains an invalid or duplicate document plan")
		}
		seen[plan.Document.ID] = true
		if plan.ValidateSource && !plan.ExpectedMissing && plan.ExpectedHash == "" {
			return fmt.Errorf("pull plan for %s has no expected working hash", plan.LocalPath)
		}
		if plan.RemoveSource && (plan.SourcePath == "" || plan.SourcePath == plan.LocalPath || plan.TargetContent == "") {
			return fmt.Errorf("pull plan for %s has an invalid relocation", plan.LocalPath)
		}
		if plan.Conflict != nil && (plan.Conflict.Base == "" || plan.Conflict.Local == "" || plan.Conflict.Remote == "") {
			return fmt.Errorf("pull plan for %s has incomplete conflict objects", plan.LocalPath)
		}
	}
	materialized := make(map[string]bool, len(operation.MaterializedDocuments))
	for _, documentID := range operation.MaterializedDocuments {
		if !seen[documentID] || materialized[documentID] {
			return errors.New("pull operation contains invalid materialization progress")
		}
		materialized[documentID] = true
	}
	for index, plan := range operation.Plans {
		if materialized[plan.Document.ID] != (index < len(operation.MaterializedDocuments)) {
			return errors.New("pull operation materialized documents are not a completed prefix")
		}
	}
	if rank < 2 && len(operation.MaterializedDocuments) != 0 {
		return errors.New("pull operation records materialization before the materializing phase")
	}
	return nil
}

func validateActivePullOperation(store *ObjectStore, refs RepositoryRefs) error {
	operation, err := LoadPullOperation(store, refs.Operation)
	if err != nil {
		return err
	}
	if operation.Head != refs.Head || operation.Index != refs.Index || operation.BaseTree != refs.Remote || refs.IndexPatch != "" {
		return errors.New("active pull OperationState does not match repository refs")
	}
	if _, err := LoadWorkingTreeSnapshot(store, operation.WorkingTree); err != nil {
		return fmt.Errorf("load pull WorkingTreeSnapshot: %w", err)
	}
	if operation.RemoteTree != "" {
		if _, err := LoadWorkspaceTree(store, operation.RemoteTree); err != nil {
			return fmt.Errorf("load pull Remote WorkspaceTree: %w", err)
		}
	}
	for _, plan := range operation.Plans {
		for _, contentID := range []ObjectID{plan.TargetContent} {
			if contentID != "" {
				if _, err := LoadWorkingFileContent(store, contentID); err != nil {
					return err
				}
			}
		}
		if plan.Conflict != nil {
			for _, contentID := range []ObjectID{plan.Conflict.Base, plan.Conflict.Local, plan.Conflict.Remote} {
				if _, err := LoadWorkingFileContent(store, contentID); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func pullOperationPhaseRank(phase PullOperationPhase) (int, bool) {
	switch phase {
	case PullOperationPrepared:
		return 0, true
	case PullOperationRemoteSnapshot:
		return 1, true
	case PullOperationMaterializing:
		return 2, true
	default:
		return 0, false
	}
}

func (s *Syncer) beginPullExecution(store *ObjectStore, refs RepositoryRefs, state State, scan workingTreeScan) (*pullExecution, error) {
	workingTreeID, _, err := persistWorkingTreeSnapshot(store, scan)
	if err != nil {
		return nil, err
	}
	execution := &pullExecution{
		syncer: s,
		store:  store,
		operation: PullOperationState{
			Kind:        "pull",
			Phase:       PullOperationPrepared,
			Head:        refs.Head,
			Index:       refs.Index,
			BaseTree:    refs.Remote,
			WorkingTree: workingTreeID,
			StartState:  cloneState(state),
			Result:      PullResult{Conflicts: []string{}, RemoteMissing: []string{}},
		},
	}
	if err := execution.persist(PullOperationPrepared, ""); err != nil {
		return nil, err
	}
	return execution, nil
}

func (e *pullExecution) persist(phase PullOperationPhase, operationError string) error {
	currentRank, currentOK := pullOperationPhaseRank(e.operation.Phase)
	nextRank, nextOK := pullOperationPhaseRank(phase)
	if !currentOK || !nextOK || nextRank < currentRank || nextRank > currentRank+1 {
		return fmt.Errorf("invalid pull phase transition %s -> %s", e.operation.Phase, phase)
	}
	e.operation.Phase = phase
	e.operation.Error = operationError
	e.operation.UpdatedAt = time.Now().UTC()
	operationID, err := StorePullOperation(e.store, e.operation)
	if err != nil {
		return err
	}
	refs, err := LoadRepositoryRefs(e.syncer.root)
	if err != nil {
		return err
	}
	if refs.Head != e.operation.Head || refs.Index != e.operation.Index || refs.Remote != e.operation.BaseTree {
		return errors.New("repository refs changed during pull")
	}
	next := refs
	next.Operation = operationID
	if err := SaveRepositoryRefs(e.syncer.root, refs.Generation, next); err != nil {
		return err
	}
	if refs.Operation != "" && refs.Operation != operationID {
		e.store.removeObjectBestEffort(refs.Operation)
	}
	e.operationID = operationID
	return nil
}

func (s *Syncer) resumePullOperation(ctx context.Context, refs RepositoryRefs) (PullResult, error) {
	store := NewObjectStore(s.root)
	operation, err := LoadPullOperation(store, refs.Operation)
	if err != nil {
		return PullResult{}, err
	}
	execution := &pullExecution{syncer: s, store: store, operationID: refs.Operation, operation: operation}
	if execution.operation.Phase == PullOperationPrepared {
		if err := execution.prepare(ctx); err != nil {
			_ = execution.persist(PullOperationPrepared, err.Error())
			return execution.operation.Result, err
		}
	}
	if execution.operation.Phase == PullOperationRemoteSnapshot {
		if err := execution.persist(PullOperationMaterializing, ""); err != nil {
			return execution.operation.Result, err
		}
	}
	if err := execution.materialize(); err != nil {
		_ = execution.persist(PullOperationMaterializing, err.Error())
		return execution.operation.Result, err
	}
	if err := execution.finish(); err != nil {
		return execution.operation.Result, err
	}
	return execution.operation.Result, nil
}

func (e *pullExecution) prepare(ctx context.Context) error {
	working, err := LoadWorkingTreeSnapshot(e.store, e.operation.WorkingTree)
	if err != nil {
		return err
	}
	baseWorkspace, err := LoadWorkspaceTree(e.store, e.operation.BaseTree)
	if err != nil {
		return err
	}
	remote, err := e.syncer.observeStableRemoteWorkspace(ctx)
	if err != nil {
		return err
	}
	inventory := remote.Inventory
	localPaths := remote.LocalPaths
	snapshotDocuments := make([]WorkspaceDocument, 0, len(inventory.DocumentsByID))
	targetState := cloneState(e.operation.StartState)
	plans := make([]PullDocumentPlan, 0, len(inventory.DocumentsByID))
	result := PullResult{Conflicts: []string{}, RemoteMissing: []string{}}

	for _, document := range sortedRemoteDocuments(inventory.DocumentsByID) {
		desiredPath := localPaths[document.ID]
		observation, ok := remote.Documents[document.ID]
		if !ok {
			return fmt.Errorf("stable remote workspace is missing document %s", document.ID)
		}
		remoteDocument := observation.Document
		documentTreeID, err := StoreDocumentTree(e.store, document.ID, remoteDocument)
		if err != nil {
			return err
		}
		snapshotDocuments = append(snapshotDocuments, WorkspaceDocument{
			ID: document.ID, NotebookID: document.NotebookID, NotebookName: document.NotebookName,
			Title: document.Name, RemotePath: document.Path, LocalPath: desiredPath, DocumentTreeID: documentTreeID,
		})
		remote := RenderAnnotated(remoteDocument)
		remoteHash := HashContent(remote)
		plan := PullDocumentPlan{
			Document:  newDocumentState(document, desiredPath),
			LocalPath: desiredPath,
			Metadata:  observation.Metadata,
		}
		previous, tracked := e.operation.StartState.Documents[document.ID]
		if !tracked {
			local, _, missing, err := e.syncer.readWorkingPath(desiredPath)
			if err != nil {
				return err
			}
			if !missing && HashContent(local) != remoteHash {
				result.Conflicts = append(result.Conflicts, desiredPath+": local file already exists with different content")
				plans = append(plans, plan)
				continue
			}
			targetState.Documents[document.ID] = newDocumentState(document, desiredPath)
			plan.ClearConflict = true
			if missing {
				plan.SourcePath = desiredPath
				plan.ValidateSource = true
				plan.ExpectedMissing = true
				plan.TargetContent, err = storePullContent(e.store, remote)
				if err != nil {
					return err
				}
			} else {
				plan.ExpectedHash = HashContent(local)
			}
			result.Added++
			plans = append(plans, plan)
			continue
		}

		baseDocument, exists := WorkspaceDocumentByID(baseWorkspace, document.ID)
		if !exists {
			return fmt.Errorf("tracked document %s is absent from the Remote baseline; remote-missing recovery is not supported yet, create a fresh clone", document.ID)
		}
		base, err := RenderDocumentTree(e.store, baseDocument.DocumentTreeID)
		if err != nil {
			return err
		}
		baseHash := HashContent(base)
		record, ok := WorkingFileByDocumentID(working, document.ID)
		if !ok {
			return fmt.Errorf("working snapshot does not contain document %s", document.ID)
		}
		plan.SourcePath = previous.LocalPath
		plan.ExpectedMissing = record.Missing
		plan.ExpectedHash = record.ContentHash
		var local string
		if !record.Missing {
			local, err = LoadWorkingFileContent(e.store, record.ContentObject)
			if err != nil {
				return err
			}
		}
		relocating := previous.LocalPath != desiredPath
		if relocating {
			if record.Missing {
				result.Conflicts = append(result.Conflicts, previous.LocalPath+": cannot move a missing tracked file")
				plans = append(plans, plan)
				continue
			}
			if record.ContentHash != baseHash {
				plan.Document = previous
				plan.LocalPath = previous.LocalPath
				plan.ValidateSource = true
				plan.Conflict, err = storePullConflict(e.store, base, local, remote)
				if err != nil {
					return err
				}
				result.Conflicts = append(result.Conflicts, previous.LocalPath+": SiYuan path changed while local file has unsynced edits")
				plans = append(plans, plan)
				continue
			}
			_, _, missing, err := e.syncer.readWorkingPath(desiredPath)
			if err != nil {
				return err
			}
			if !missing {
				plan.Document = previous
				plan.LocalPath = previous.LocalPath
				plan.ValidateSource = true
				plan.Conflict, err = storePullConflict(e.store, base, local, remote)
				if err != nil {
					return err
				}
				result.Conflicts = append(result.Conflicts, desiredPath+": destination already exists")
				plans = append(plans, plan)
				continue
			}
			plan.RemoveSource = true
		}

		targetState.Documents[document.ID] = updateDocumentMetadata(previous, document, desiredPath)
		if record.Missing {
			result.Conflicts = append(result.Conflicts, desiredPath+": tracked local file is missing")
			plans = append(plans, plan)
			continue
		}
		localHash := record.ContentHash
		localChanged := localHash != baseHash
		remoteChanged := remoteHash != baseHash
		plan.ClearConflict = true

		switch {
		case localChanged && remoteChanged && localHash != remoteHash:
			patch, patchErr := BuildDocumentPatch(document.ID, desiredPath, base, local)
			if patchErr == nil {
				patchErr = ValidateDocumentPatchSafety(patch, base)
			}
			if patchErr == nil {
				mergedDocument, mergeErr := MergeDocumentPatch(patch, remoteDocument)
				if mergeErr == nil {
					merged := RenderAnnotated(mergedDocument)
					plan.TargetContent, err = storePullContent(e.store, merged)
					if err != nil {
						return err
					}
					plan.ValidateSource = true
					result.PreservedLocal++
					break
				}
			}
			plan.ValidateSource = true
			plan.ClearConflict = false
			plan.Conflict, err = storePullConflict(e.store, base, local, remote)
			if err != nil {
				return err
			}
			result.Conflicts = append(result.Conflicts, desiredPath+": both local and SiYuan changed")
		case remoteChanged && !localChanged:
			plan.TargetContent, err = storePullContent(e.store, remote)
			if err != nil {
				return err
			}
			plan.ValidateSource = true
			result.Updated++
		case localChanged && !remoteChanged:
			patch, patchErr := BuildDocumentPatch(document.ID, desiredPath, base, local)
			if patchErr != nil {
				result.Conflicts = append(result.Conflicts, fmt.Sprintf("%s: unsafe local change: %v", desiredPath, patchErr))
			} else if safetyErr := ValidateDocumentPatchSafety(patch, base); safetyErr != nil {
				result.Conflicts = append(result.Conflicts, fmt.Sprintf("%s: unsafe local change: %v", desiredPath, safetyErr))
			}
			result.PreservedLocal++
		case localHash == remoteHash && (localChanged || remoteChanged):
			plan.TargetContent, err = storePullContent(e.store, remote)
			if err != nil {
				return err
			}
			plan.ValidateSource = true
			result.Updated++
		default:
			result.Unchanged++
		}
		if relocating && plan.Conflict == nil {
			if plan.TargetContent == "" {
				plan.TargetContent, err = storePullContent(e.store, local)
				if err != nil {
					return err
				}
			}
			plan.ValidateSource = true
		}
		plans = append(plans, plan)
	}

	for _, document := range e.operation.StartState.Documents {
		if _, found := inventory.DocumentsByID[document.ID]; !found {
			result.RemoteMissing = append(result.RemoteMissing, document.LocalPath)
		}
	}
	sort.Strings(result.Conflicts)
	sort.Strings(result.RemoteMissing)
	remoteTreeID, err := StoreWorkspaceTree(e.store, e.syncer.config.NotebookIDs, inventory.Notebooks, snapshotDocuments)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	targetState.LastSyncAt = &now
	e.operation.RemoteTree = remoteTreeID
	e.operation.Plans = plans
	e.operation.TargetState = targetState
	e.operation.Result = result
	e.operation.AdvanceHead = len(result.Conflicts) == 0 && len(result.RemoteMissing) == 0
	return e.persist(PullOperationRemoteSnapshot, "")
}

func (e *pullExecution) materialize() error {
	for planIndex := range e.operation.Plans {
		plan := &e.operation.Plans[planIndex]
		if containsString(e.operation.MaterializedDocuments, plan.Document.ID) {
			continue
		}
		if err := e.syncer.materializePullDocument(e.store, *plan); err != nil {
			return err
		}
		e.operation.MaterializedDocuments = append(e.operation.MaterializedDocuments, plan.Document.ID)
		if err := e.persist(PullOperationMaterializing, ""); err != nil {
			return err
		}
	}
	return SaveState(e.syncer.root, e.operation.TargetState)
}

func (s *Syncer) materializePullDocument(store *ObjectStore, plan PullDocumentPlan) error {
	if plan.ValidateSource {
		if err := s.validatePullWorkingPrecondition(store, plan); err != nil {
			return err
		}
	}
	if plan.TargetContent != "" {
		target, err := LoadWorkingFileContent(store, plan.TargetContent)
		if err != nil {
			return err
		}
		destination, err := s.localAbsolutePath(plan.LocalPath)
		if err != nil {
			return err
		}
		current, _, missing, err := readStableWorkingFile(destination)
		if err != nil {
			return err
		}
		if missing || HashContent(current) != HashContent(target) {
			if err := WriteFileAtomic(destination, []byte(target), 0o644); err != nil {
				return err
			}
		}
	}
	if plan.RemoveSource && plan.SourcePath != plan.LocalPath {
		source, err := s.localAbsolutePath(plan.SourcePath)
		if err != nil {
			return err
		}
		current, _, missing, err := readStableWorkingFile(source)
		if err != nil {
			return err
		}
		if !missing {
			if HashContent(current) != plan.ExpectedHash {
				return fmt.Errorf("%s changed while pull was relocating it", plan.SourcePath)
			}
			if err := os.Remove(source); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			removeEmptyParents(filepath.Dir(source), s.outputRoot)
		}
	}
	if plan.Conflict != nil {
		base, err := LoadWorkingFileContent(store, plan.Conflict.Base)
		if err != nil {
			return err
		}
		local, err := LoadWorkingFileContent(store, plan.Conflict.Local)
		if err != nil {
			return err
		}
		remote, err := LoadWorkingFileContent(store, plan.Conflict.Remote)
		if err != nil {
			return err
		}
		if _, err := writeConflict(s.root, plan.Document, base, local, remote); err != nil {
			return err
		}
	} else if plan.ClearConflict {
		if err := clearConflict(s.root, plan.Document.ID); err != nil {
			return err
		}
	}
	return writeDocumentMetadata(s.root, plan.Metadata)
}

func (s *Syncer) validatePullWorkingPrecondition(store *ObjectStore, plan PullDocumentPlan) error {
	targetHash := ""
	if plan.TargetContent != "" {
		target, err := LoadWorkingFileContent(store, plan.TargetContent)
		if err != nil {
			return err
		}
		targetHash = HashContent(target)
	}
	sourcePath := plan.SourcePath
	if sourcePath == "" {
		sourcePath = plan.LocalPath
	}
	source, err := s.localAbsolutePath(sourcePath)
	if err != nil {
		return err
	}
	content, _, missing, err := readStableWorkingFile(source)
	if err != nil {
		return err
	}
	if sourcePath == plan.LocalPath {
		if plan.ExpectedMissing {
			if !missing && HashContent(content) != targetHash {
				return fmt.Errorf("%s changed after pull started", plan.LocalPath)
			}
			return nil
		}
		if missing {
			return fmt.Errorf("%s disappeared after pull started", plan.LocalPath)
		}
		currentHash := HashContent(content)
		if currentHash != plan.ExpectedHash && currentHash != targetHash {
			return fmt.Errorf("%s changed after pull started", plan.LocalPath)
		}
		return nil
	}

	destination, err := s.localAbsolutePath(plan.LocalPath)
	if err != nil {
		return err
	}
	destinationContent, _, destinationMissing, err := readStableWorkingFile(destination)
	if err != nil {
		return err
	}
	if missing {
		if !destinationMissing && HashContent(destinationContent) == targetHash {
			return nil
		}
		return fmt.Errorf("%s disappeared before relocation completed", sourcePath)
	}
	if HashContent(content) != plan.ExpectedHash {
		return fmt.Errorf("%s changed after pull started", sourcePath)
	}
	if !destinationMissing && HashContent(destinationContent) != targetHash {
		return fmt.Errorf("%s appeared after pull started", plan.LocalPath)
	}
	return nil
}

func (e *pullExecution) finish() error {
	refs, err := LoadRepositoryRefs(e.syncer.root)
	if err != nil {
		return err
	}
	if refs.Operation != e.operationID || refs.Head != e.operation.Head || refs.Index != e.operation.Index || refs.Remote != e.operation.BaseTree {
		return errors.New("repository refs changed before pull completed")
	}
	next := refs
	next.Remote = e.operation.RemoteTree
	if e.operation.AdvanceHead {
		commitID, err := StoreBaselineCommit(e.store, e.operation.RemoteTree)
		if err != nil {
			return err
		}
		next.Head = commitID
		next.Index = e.operation.RemoteTree
		next.IndexPatch = ""
	}
	next.Operation = ""
	if _, err := markRepositoryObjects(e.syncer.root, next); err != nil {
		return fmt.Errorf("validate completed pull: %w", err)
	}
	if err := SaveRepositoryRefs(e.syncer.root, refs.Generation, next); err != nil {
		return err
	}
	pruneRepositoryObjectsBestEffort(e.syncer.root)
	return nil
}

func (s *Syncer) readWorkingPath(localPath string) (string, os.FileInfo, bool, error) {
	absolute, err := s.localAbsolutePath(localPath)
	if err != nil {
		return "", nil, false, err
	}
	content, info, missing, err := readStableWorkingFile(absolute)
	if err != nil {
		return "", nil, false, fmt.Errorf("read stable working file %s: %w", localPath, err)
	}
	return Canonicalize(content), info, missing, nil
}

func storePullContent(store *ObjectStore, content string) (ObjectID, error) {
	return store.Put(workingFileObjectType, snapshotObjectVersion, WorkingFileContent{Markdown: Canonicalize(content)})
}

func storePullConflict(store *ObjectStore, base, local, remote string) (*PullConflictPlan, error) {
	baseID, err := storePullContent(store, base)
	if err != nil {
		return nil, err
	}
	localID, err := storePullContent(store, local)
	if err != nil {
		return nil, err
	}
	remoteID, err := storePullContent(store, remote)
	if err != nil {
		return nil, err
	}
	return &PullConflictPlan{Base: baseID, Local: localID, Remote: remoteID}, nil
}

func cloneState(state State) State {
	cloned := State{Version: state.Version, Documents: make(map[string]DocumentState, len(state.Documents))}
	if state.LastSyncAt != nil {
		lastSync := *state.LastSyncAt
		cloned.LastSyncAt = &lastSync
	}
	for id, document := range state.Documents {
		cloned.Documents[id] = document
	}
	return cloned
}
