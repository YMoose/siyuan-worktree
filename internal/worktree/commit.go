package worktree

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"
)

type CommitStatus string

const (
	CommitPending  CommitStatus = "pending-push"
	CommitPushing  CommitStatus = "pushing"
	CommitConflict CommitStatus = "conflict"
	CommitFailed   CommitStatus = "failed"
)

type Commit struct {
	Version          int             `json:"version"`
	ID               string          `json:"id"`
	ObjectID         ObjectID        `json:"objectId,omitempty"`
	Tree             ObjectID        `json:"tree,omitempty"`
	BaseHead         ObjectID        `json:"baseHead,omitempty"`
	RemoteBase       ObjectID        `json:"remoteBase,omitempty"`
	Patch            ObjectID        `json:"patch,omitempty"`
	Message          string          `json:"message"`
	CreatedAt        time.Time       `json:"createdAt"`
	UpdatedAt        time.Time       `json:"updatedAt"`
	Status           CommitStatus    `json:"status"`
	AppliedDocuments int             `json:"appliedDocuments"`
	DocumentPatches  []DocumentPatch `json:"documentPatches"`
	Error            string          `json:"error,omitempty"`
}

type AddOptions struct {
	All   bool
	Paths []string
}

type AddResult struct {
	Staged     []string `json:"staged"`
	Unchanged  []string `json:"unchanged"`
	Operations int      `json:"operations"`
}

type CommitSummary struct {
	ID        string       `json:"id"`
	Message   string       `json:"message"`
	CreatedAt time.Time    `json:"createdAt"`
	Status    CommitStatus `json:"status"`
	Paths     []string     `json:"paths"`
}

type OperationStatus struct {
	Kind                  string `json:"kind"`
	Phase                 string `json:"phase"`
	Status                string `json:"status"`
	CommitID              string `json:"commitId,omitempty"`
	AppliedDocuments      int    `json:"appliedDocuments,omitempty"`
	MaterializedDocuments int    `json:"materializedDocuments,omitempty"`
	TotalDocuments        int    `json:"totalDocuments"`
	CurrentDocument       string `json:"currentDocument,omitempty"`
	Message               string `json:"message,omitempty"`
	NextAction            string `json:"nextAction"`
}

type RepositoryStatus struct {
	Documents                  []DocumentStatus `json:"documents"`
	DocumentComparisonDeferred bool             `json:"documentComparisonDeferred,omitempty"`
	Staged                     []string         `json:"staged"`
	PendingCommits             []CommitSummary  `json:"pendingCommits"`
	Conflicts                  []Conflict       `json:"conflicts"`
	ActiveOperation            *OperationStatus `json:"activeOperation,omitempty"`
}

type ResetResult struct {
	ClearedStaged    int      `json:"clearedStaged"`
	DiscardedCommits []string `json:"discardedCommits"`
}

func NewCommit(message string, patches []DocumentPatch) Commit {
	now := time.Now().UTC()
	for index := range patches {
		patches[index].Status = DocumentPatchCommitted
		patches[index].AppliedOperations = 0
		patches[index].InFlightOperation = nil
		patches[index].HistoryCheckpoint = nil
		patches[index].Error = ""
	}
	sort.Slice(patches, func(i, j int) bool { return patches[i].LocalPath < patches[j].LocalPath })
	hash := sha256.New()
	hash.Write([]byte(message))
	for _, patch := range patches {
		hash.Write([]byte(patch.DocumentID))
		hash.Write([]byte(patch.BaseHash))
		hash.Write([]byte(patch.LocalHash))
	}
	suffix := hex.EncodeToString(hash.Sum(nil))[:12]
	return Commit{
		Version:         2,
		ID:              now.Format("20060102T150405.000000000Z") + "-" + suffix,
		Message:         message,
		CreatedAt:       now,
		UpdatedAt:       now,
		Status:          CommitPending,
		DocumentPatches: patches,
	}
}

func ListStagedDocumentPatches(root string) ([]DocumentPatch, error) {
	refs, err := LoadRepositoryRefs(root)
	if err != nil {
		return nil, err
	}
	if refs.IndexPatch == "" {
		return []DocumentPatch{}, nil
	}
	store := NewObjectStore(root)
	patch, err := LoadPatch(store, refs.IndexPatch)
	if err != nil {
		return nil, err
	}
	if patch.TargetTree != refs.Index {
		return nil, errors.New("Index Patch target does not match the Index snapshot")
	}
	if err := ValidatePatch(store, patch); err != nil {
		return nil, err
	}
	patches := immutableDocumentPatches(patch.DocumentPatches)
	for index := range patches {
		patches[index].Status = DocumentPatchStaged
	}
	return patches, nil
}

func ListPendingCommits(root string) ([]Commit, error) {
	refs, err := LoadRepositoryRefs(root)
	if err != nil {
		return nil, err
	}
	if !repositoryRefsInitialized(refs) {
		return []Commit{}, nil
	}
	store := NewObjectStore(root)
	if refs.Operation != "" {
		kind, err := LoadRepositoryOperationKind(store, refs.Operation)
		if err != nil {
			return nil, err
		}
		if kind == "pull" {
			return []Commit{}, nil
		}
		if kind != "push" {
			return nil, fmt.Errorf("active operation has unsupported kind %s", kind)
		}
		operation, err := LoadPushOperation(store, refs.Operation)
		if err != nil {
			return nil, err
		}
		if operation.CommitObjectID != refs.Head || operation.Commit.ObjectID != refs.Head {
			return nil, fmt.Errorf("active push operation does not match repository HEAD %s", refs.Head)
		}
		return []Commit{operation.Commit}, nil
	}
	commit, found, err := PendingCommitFromHead(store, refs)
	if err != nil {
		return nil, err
	}
	if !found {
		return []Commit{}, nil
	}
	return []Commit{commit}, nil
}

func HasPendingCommit(root string) (bool, error) {
	commits, err := ListPendingCommits(root)
	return len(commits) > 0, err
}

func SummarizeCommit(commit Commit) CommitSummary {
	paths := make([]string, 0, len(commit.DocumentPatches))
	for _, patch := range commit.DocumentPatches {
		paths = append(paths, patch.LocalPath)
	}
	return CommitSummary{ID: commit.ID, Message: commit.Message, CreatedAt: commit.CreatedAt, Status: commit.Status, Paths: paths}
}
