package worktree

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

type pushExecution struct {
	syncer    *Syncer
	store     *ObjectStore
	operation PushOperationState
}

func PendingCommitFromHead(store *ObjectStore, refs RepositoryRefs) (Commit, bool, error) {
	if refs.Head == "" {
		return Commit{}, false, nil
	}
	object, err := LoadCommitObject(store, refs.Head)
	if err != nil {
		return Commit{}, false, err
	}
	if object.Kind != userCommitObjectKind {
		return Commit{}, false, nil
	}
	patch, err := LoadPatch(store, object.Patch)
	if err != nil {
		return Commit{}, false, err
	}
	if err := ValidatePatch(store, patch); err != nil {
		return Commit{}, false, err
	}
	commit := NewCommit(object.Message, patch.DocumentPatches)
	commit.ID = object.DisplayID
	commit.ObjectID = refs.Head
	commit.Tree = object.Tree
	commit.BaseHead = object.BaseHead
	commit.RemoteBase = object.RemoteBase
	commit.Patch = object.Patch
	commit.CreatedAt = object.CreatedAt
	commit.UpdatedAt = object.CreatedAt
	return commit, true, nil
}

func (s *Syncer) beginPushExecution(store *ObjectStore, refs RepositoryRefs, commit *Commit) (*pushExecution, error) {
	commitObject, err := LoadCommitObject(store, commit.ObjectID)
	if err != nil {
		return nil, err
	}
	if commitObject.Kind != userCommitObjectKind {
		return nil, fmt.Errorf("commit %s has unsupported kind %s", commit.ObjectID, commitObject.Kind)
	}
	if commitObject.BaseHead != "" {
		baseHead, err := LoadCommitObject(store, commitObject.BaseHead)
		if err != nil {
			return nil, fmt.Errorf("load CommitObject baseHead: %w", err)
		}
		if baseHead.Kind != baselineCommitObjectKind || baseHead.Tree != commitObject.RemoteBase {
			return nil, errors.New("CommitObject baseHead does not match its stable RemoteBase")
		}
	} else {
		return nil, errors.New("pending CommitObject has no stable baseHead")
	}
	patch, err := LoadPatch(store, commitObject.Patch)
	if err != nil {
		return nil, err
	}
	if patch.BaseTree != commitObject.RemoteBase || patch.TargetTree != commitObject.Tree {
		return nil, errors.New("CommitObject Patch does not match its RemoteBase and target snapshots")
	}
	if err := ValidatePatch(store, patch); err != nil {
		return nil, err
	}
	if len(commit.DocumentPatches) == 0 {
		commit.DocumentPatches = immutableDocumentPatches(patch.DocumentPatches)
		for index := range commit.DocumentPatches {
			commit.DocumentPatches[index].Status = DocumentPatchCommitted
		}
	} else if !sameDocumentPatchIntents(commit.DocumentPatches, patch.DocumentPatches) {
		return nil, errors.New("pending Commit runtime DocumentPatch state does not match its immutable Patch")
	}
	execution := &pushExecution{syncer: s, store: store}
	if refs.Operation != "" {
		operation, err := LoadPushOperation(store, refs.Operation)
		if err != nil {
			return nil, err
		}
		if operation.CommitObjectID != commit.ObjectID || operation.Commit.ObjectID != commit.ObjectID {
			return nil, fmt.Errorf("active push operation belongs to commit %s", operation.CommitObjectID)
		}
		if operation.BaseTree != patch.BaseTree || operation.TargetTree != patch.TargetTree {
			return nil, errors.New("active push operation does not match the Commit Patch")
		}
		execution.operation = operation
		*commit = operation.Commit
		return execution, nil
	}
	execution.operation = PushOperationState{
		Kind:               "push",
		Phase:              PushOperationPrepared,
		CommitObjectID:     commit.ObjectID,
		BaseTree:           patch.BaseTree,
		TargetTree:         patch.TargetTree,
		PreflightDocuments: map[string]ObjectID{},
		CanonicalDocuments: map[string]ObjectID{},
		Commit:             *commit,
	}
	if err := execution.persist(commit, PushOperationPrepared, ""); err != nil {
		return nil, err
	}
	return execution, nil
}

func (e *pushExecution) persist(commit *Commit, phase PushOperationPhase, operationError string) error {
	currentRank, currentOK := pushOperationPhaseRank(e.operation.Phase)
	nextRank, nextOK := pushOperationPhaseRank(phase)
	if !currentOK || !nextOK {
		return fmt.Errorf("unsupported push phase transition %s -> %s", e.operation.Phase, phase)
	}
	if nextRank < currentRank || nextRank > currentRank+1 {
		return fmt.Errorf("invalid push phase transition %s -> %s", e.operation.Phase, phase)
	}
	e.operation.Phase = phase
	e.operation.Commit = *commit
	e.operation.Error = operationError
	operationID, err := StorePushOperation(e.store, e.operation)
	if err != nil {
		return err
	}
	refs, err := LoadRepositoryRefs(e.syncer.root)
	if err != nil {
		return err
	}
	if refs.Head != commit.ObjectID {
		return fmt.Errorf("repository HEAD %s no longer matches pushing commit %s", refs.Head, commit.ObjectID)
	}
	next := refs
	next.Operation = operationID
	if err := SaveRepositoryRefs(e.syncer.root, refs.Generation, next); err != nil {
		return err
	}
	if refs.Operation != "" && refs.Operation != operationID {
		e.store.removeObjectBestEffort(refs.Operation)
	}
	e.operation.UpdatedAt = time.Now().UTC()
	return nil
}

func (e *pushExecution) recordCanonicalDocument(commit *Commit, documentID string, documentTreeID ObjectID) error {
	documentTree, err := LoadDocumentTree(e.store, documentTreeID)
	if err != nil {
		return err
	}
	if documentTree.DocumentID != documentID {
		return fmt.Errorf("canonical snapshot for %s belongs to document %s", documentID, documentTree.DocumentID)
	}
	if e.operation.CanonicalDocuments == nil {
		e.operation.CanonicalDocuments = map[string]ObjectID{}
	}
	e.operation.CanonicalDocuments[documentID] = documentTreeID
	return e.persist(commit, PushOperationApplying, "")
}

func (e *pushExecution) recordMaterializedDocument(commit *Commit, documentID string) error {
	if !containsString(e.operation.MaterializedDocuments, documentID) {
		e.operation.MaterializedDocuments = append(e.operation.MaterializedDocuments, documentID)
		sort.Strings(e.operation.MaterializedDocuments)
	}
	return e.persist(commit, PushOperationMaterializing, "")
}

func (e *pushExecution) finish(commitObjectID, canonicalTree ObjectID) error {
	resultCommitID, err := StoreBaselineCommit(e.store, canonicalTree)
	if err != nil {
		return err
	}
	refs, err := LoadRepositoryRefs(e.syncer.root)
	if err != nil {
		return err
	}
	if refs.Head != commitObjectID {
		return fmt.Errorf("repository HEAD %s no longer matches pushed commit %s", refs.Head, commitObjectID)
	}
	next := refs
	next.Head = resultCommitID
	next.Index = canonicalTree
	next.IndexPatch = ""
	next.Remote = canonicalTree
	next.Operation = ""
	if _, err := markRepositoryObjects(e.syncer.root, next); err != nil {
		return fmt.Errorf("validate final baseline: %w", err)
	}
	if err := SaveRepositoryRefs(e.syncer.root, refs.Generation, next); err != nil {
		return err
	}
	pruneRepositoryObjectsBestEffort(e.syncer.root)
	return nil
}

func sameDocumentPatchIntents(left, right []DocumentPatch) bool {
	leftJSON, err := patchJSON(PatchObject{DocumentPatches: immutableDocumentPatches(left)})
	if err != nil {
		return false
	}
	rightJSON, err := patchJSON(PatchObject{DocumentPatches: immutableDocumentPatches(right)})
	if err != nil {
		return false
	}
	return string(leftJSON) == string(rightJSON)
}

func containsString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func pushOperationHasRemoteEffects(operation PushOperationState) bool {
	if operation.CanonicalTree != "" || len(operation.CanonicalDocuments) > 0 || len(operation.MaterializedDocuments) > 0 || operation.Commit.AppliedDocuments > 0 {
		return true
	}
	for _, patch := range operation.Commit.DocumentPatches {
		if patch.AppliedOperations > 0 || patch.InFlightOperation != nil {
			return true
		}
		for _, blockOperation := range patch.Operations {
			if operationHasMutationEvidence(blockOperation) {
				return true
			}
		}
	}
	return false
}

func validatePushOperationProgress(operation PushOperationState) error {
	rank, ok := pushOperationPhaseRank(operation.Phase)
	if !ok {
		return fmt.Errorf("unsupported push operation phase %s", operation.Phase)
	}
	if operation.Commit.Version != 2 || operation.CommitObjectID == "" || operation.Commit.ObjectID != operation.CommitObjectID || operation.BaseTree == "" || operation.TargetTree == "" {
		return errors.New("push operation has incomplete snapshot references")
	}
	switch operation.Commit.Status {
	case CommitPending, CommitPushing, CommitConflict, CommitFailed:
	default:
		return fmt.Errorf("push operation has unsupported Commit status %s", operation.Commit.Status)
	}
	if operation.Commit.AppliedDocuments < 0 || operation.Commit.AppliedDocuments > len(operation.Commit.DocumentPatches) {
		return errors.New("push operation has an invalid applied document count")
	}
	patches := make(map[string]bool, len(operation.Commit.DocumentPatches))
	for _, patch := range operation.Commit.DocumentPatches {
		if patch.AppliedOperations < 0 || patch.AppliedOperations > len(patch.Operations) {
			return fmt.Errorf("push operation has an invalid applied operation count for %s", patch.LocalPath)
		}
		if patch.InFlightOperation != nil && (*patch.InFlightOperation < 0 || *patch.InFlightOperation >= len(patch.Operations) || *patch.InFlightOperation != patch.AppliedOperations) {
			return fmt.Errorf("push operation has an invalid in-flight operation for %s", patch.LocalPath)
		}
		for operationIndex, blockOperation := range patch.Operations {
			switch {
			case operationIndex < patch.AppliedOperations:
			case operationIndex == patch.AppliedOperations:
				if operationHasMutationEvidence(blockOperation) && patch.InFlightOperation == nil {
					return fmt.Errorf("push operation has mutation evidence without in-flight state for %s", patch.LocalPath)
				}
			case operationHasRuntimeEvidence(blockOperation):
				return fmt.Errorf("push operation has out-of-order operation evidence for %s", patch.LocalPath)
			}
		}
		if patches[patch.DocumentID] {
			return fmt.Errorf("push operation contains duplicate document %s", patch.DocumentID)
		}
		patches[patch.DocumentID] = true
	}
	if len(operation.PreflightDocuments) != 0 && len(operation.PreflightDocuments) != len(operation.Commit.DocumentPatches) {
		return errors.New("push operation preflight document count does not match the Commit")
	}
	for documentID, documentTreeID := range operation.PreflightDocuments {
		if !patches[documentID] || documentTreeID == "" {
			return fmt.Errorf("push operation has an invalid preflight snapshot for document %s", documentID)
		}
	}
	if rank >= 1 && len(operation.PreflightDocuments) != len(operation.Commit.DocumentPatches) {
		return fmt.Errorf("push phase %s requires a complete preflight snapshot", operation.Phase)
	}
	if len(operation.CanonicalDocuments) != operation.Commit.AppliedDocuments {
		return errors.New("push operation canonical document count does not match applied documents")
	}
	for documentID, documentTreeID := range operation.CanonicalDocuments {
		if !patches[documentID] {
			return fmt.Errorf("push operation has a canonical snapshot for unknown document %s", documentID)
		}
		if documentTreeID == "" {
			return fmt.Errorf("push operation has an empty canonical snapshot for document %s", documentID)
		}
	}
	for index, patch := range operation.Commit.DocumentPatches {
		_, canonical := operation.CanonicalDocuments[patch.DocumentID]
		switch {
		case index < operation.Commit.AppliedDocuments:
			if !canonical || patch.AppliedOperations != len(patch.Operations) || patch.InFlightOperation != nil {
				return fmt.Errorf("push operation completed document %s has inconsistent progress", patch.LocalPath)
			}
		case canonical:
			return fmt.Errorf("push operation has a canonical snapshot for unapplied document %s", patch.LocalPath)
		case index > operation.Commit.AppliedDocuments && documentPatchHasRuntimeProgress(patch) && !isPreflightConflict(operation, patch):
			return fmt.Errorf("push operation has out-of-order progress for %s", patch.LocalPath)
		}
	}
	materialized := make(map[string]bool, len(operation.MaterializedDocuments))
	for _, documentID := range operation.MaterializedDocuments {
		if !patches[documentID] {
			return fmt.Errorf("push operation materialized unknown document %s", documentID)
		}
		if materialized[documentID] {
			return fmt.Errorf("push operation materialized document %s more than once", documentID)
		}
		materialized[documentID] = true
	}
	for index, patch := range operation.Commit.DocumentPatches {
		shouldBeMaterialized := index < len(operation.MaterializedDocuments)
		if materialized[patch.DocumentID] != shouldBeMaterialized {
			return errors.New("push operation materialized documents are not a completed prefix")
		}
	}
	if rank >= 2 {
		if operation.Commit.AppliedDocuments != len(operation.Commit.DocumentPatches) || len(operation.CanonicalDocuments) != len(operation.Commit.DocumentPatches) {
			return fmt.Errorf("push phase %s requires all document patches to be applied and verified", operation.Phase)
		}
		for _, patch := range operation.Commit.DocumentPatches {
			if patch.AppliedOperations != len(patch.Operations) || patch.InFlightOperation != nil {
				return fmt.Errorf("push phase %s requires all operations for %s to be applied", operation.Phase, patch.LocalPath)
			}
		}
	}
	if rank >= 3 && operation.CanonicalTree == "" {
		return fmt.Errorf("push phase %s requires a canonical WorkspaceTree", operation.Phase)
	}
	if rank < 3 && operation.CanonicalTree != "" {
		return fmt.Errorf("push phase %s cannot reference a canonical WorkspaceTree", operation.Phase)
	}
	if rank < 4 && len(operation.MaterializedDocuments) != 0 {
		return fmt.Errorf("push phase %s cannot contain materialized documents", operation.Phase)
	}
	return nil
}

func isPreflightConflict(operation PushOperationState, patch DocumentPatch) bool {
	if (operation.Phase != PushOperationPrepared && operation.Phase != PushOperationApplying) || pushOperationHasRemoteEffects(operation) ||
		operation.Commit.Status != CommitConflict || operation.Commit.AppliedDocuments != 0 ||
		patch.Status != DocumentPatchConflict || patch.Error == "" || patch.AppliedOperations != 0 || patch.InFlightOperation != nil || patch.HistoryCheckpoint != nil {
		return false
	}
	for _, blockOperation := range patch.Operations {
		if operationHasRuntimeEvidence(blockOperation) {
			return false
		}
	}
	return true
}

func resetPreparedPreflightConflicts(commit *Commit) {
	if commit.Status != CommitConflict || commit.AppliedDocuments != 0 {
		return
	}
	commit.Status = CommitPending
	commit.Error = ""
	for index := range commit.DocumentPatches {
		patch := &commit.DocumentPatches[index]
		if patch.Status != DocumentPatchConflict || patch.AppliedOperations != 0 || patch.InFlightOperation != nil || patch.HistoryCheckpoint != nil {
			continue
		}
		hasRuntimeEvidence := false
		for _, blockOperation := range patch.Operations {
			if operationHasRuntimeEvidence(blockOperation) {
				hasRuntimeEvidence = true
				break
			}
		}
		if hasRuntimeEvidence {
			continue
		}
		patch.Status = DocumentPatchCommitted
		patch.Error = ""
	}
}

func documentPatchHasRuntimeProgress(patch DocumentPatch) bool {
	if patch.Status != DocumentPatchCommitted || patch.AppliedOperations != 0 || patch.InFlightOperation != nil || patch.HistoryCheckpoint != nil || patch.Error != "" {
		return true
	}
	for _, operation := range patch.Operations {
		if operationHasRuntimeEvidence(operation) {
			return true
		}
	}
	return false
}

func operationHasRuntimeEvidence(operation Operation) bool {
	return operation.PreservedAttrs != nil || operation.InsertPrecondition != nil || operationHasMutationEvidence(operation)
}

func operationHasMutationEvidence(operation Operation) bool {
	return operation.KernelReceipt != nil || len(operation.ReceiptBlockIDs) > 0 || len(operation.ResultBlockIDs) > 0
}
