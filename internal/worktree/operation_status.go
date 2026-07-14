package worktree

import "fmt"

const (
	operationStatusRunning  = "running"
	operationStatusConflict = "conflict"
	operationStatusFailed   = "failed"
)

func loadActiveOperationStatus(store *ObjectStore, refs RepositoryRefs) (*OperationStatus, error) {
	if refs.Operation == "" {
		return nil, nil
	}
	kind, err := LoadRepositoryOperationKind(store, refs.Operation)
	if err != nil {
		return nil, err
	}
	switch kind {
	case "pull":
		operation, err := LoadPullOperation(store, refs.Operation)
		if err != nil {
			return nil, err
		}
		return summarizePullOperation(operation), nil
	case "push":
		operation, err := LoadPushOperation(store, refs.Operation)
		if err != nil {
			return nil, err
		}
		return summarizePushOperation(operation), nil
	default:
		return nil, fmt.Errorf("active operation has unsupported kind %s", kind)
	}
}

func summarizePullOperation(operation PullOperationState) *OperationStatus {
	status := &OperationStatus{
		Kind:                  "pull",
		Phase:                 string(operation.Phase),
		Status:                operationStatusRunning,
		MaterializedDocuments: len(operation.MaterializedDocuments),
		TotalDocuments:        len(operation.Plans),
		Message:               operation.Error,
		NextAction:            "siyuan-worktree pull",
	}
	if operation.Error != "" {
		status.Status = operationStatusFailed
	}
	if next := len(operation.MaterializedDocuments); next < len(operation.Plans) {
		status.CurrentDocument = operation.Plans[next].LocalPath
	}
	return status
}

func summarizePushOperation(operation PushOperationState) *OperationStatus {
	status := &OperationStatus{
		Kind:                  "push",
		Phase:                 string(operation.Phase),
		Status:                operationStatusRunning,
		CommitID:              operation.Commit.ID,
		AppliedDocuments:      operation.Commit.AppliedDocuments,
		MaterializedDocuments: len(operation.MaterializedDocuments),
		TotalDocuments:        len(operation.Commit.DocumentPatches),
		Message:               operation.Error,
		NextAction:            "siyuan-worktree push --continue",
	}
	if status.Message == "" {
		status.Message = operation.Commit.Error
	}
	switch operation.Commit.Status {
	case CommitConflict:
		status.Status = operationStatusConflict
	case CommitFailed:
		status.Status = operationStatusFailed
	}
	if (operation.Phase == PushOperationPrepared || operation.Phase == PushOperationApplying) &&
		(status.Status == operationStatusConflict || status.Status == operationStatusFailed) {
		status.NextAction = "siyuan-worktree pull"
	}
	if operation.Phase == PushOperationPrepared || operation.Phase == PushOperationApplying {
		if next := operation.Commit.AppliedDocuments; next < len(operation.Commit.DocumentPatches) {
			status.CurrentDocument = operation.Commit.DocumentPatches[next].LocalPath
		}
		return status
	}
	for _, patch := range operation.Commit.DocumentPatches {
		if !containsString(operation.MaterializedDocuments, patch.DocumentID) {
			status.CurrentDocument = patch.LocalPath
			break
		}
	}
	return status
}
