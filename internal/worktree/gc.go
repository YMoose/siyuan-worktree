package worktree

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"siyuan-worktree/internal/config"
)

// pruneRepositoryObjects removes immutable objects that are no longer reachable
// from the latest confirmed Remote snapshot or the current transient Index,
// Commit, or Operation state. Repository mutations must be serialized while it
// runs so newly written objects cannot be collected before their ref is moved.
func pruneRepositoryObjects(root string) error {
	refs, err := LoadRepositoryRefs(root)
	if err != nil {
		return err
	}
	if !repositoryRefsInitialized(refs) {
		return nil
	}
	reachable, err := markRepositoryObjects(root, refs)
	if err != nil {
		return err
	}
	return removeUnreachableObjects(root, reachable)
}

func markRepositoryObjects(root string, refs RepositoryRefs) (map[ObjectID]bool, error) {
	marker := objectReachability{
		store:     NewObjectStore(root),
		reachable: map[ObjectID]bool{},
	}
	if err := marker.markCommit(refs.Head); err != nil {
		return nil, fmt.Errorf("mark HEAD: %w", err)
	}
	if err := marker.markWorkspace(refs.Index); err != nil {
		return nil, fmt.Errorf("mark Index: %w", err)
	}
	if err := marker.markPatch(refs.IndexPatch); err != nil {
		return nil, fmt.Errorf("mark Index Patch: %w", err)
	}
	if err := marker.markWorkspace(refs.Remote); err != nil {
		return nil, fmt.Errorf("mark Remote: %w", err)
	}
	if err := marker.markOperation(refs.Operation); err != nil {
		return nil, fmt.Errorf("mark Operation: %w", err)
	}
	return marker.reachable, nil
}

func pruneRepositoryObjectsBestEffort(root string) {
	// Ref updates are already committed when cleanup runs. A cleanup failure must
	// not turn a successful pull or push into an ambiguous synchronization error;
	// later successful mutations will retry the reachability sweep.
	_ = pruneRepositoryObjects(root)
}

type objectReachability struct {
	store     *ObjectStore
	reachable map[ObjectID]bool
}

func (m *objectReachability) markCommit(id ObjectID) error {
	if id == "" || m.reachable[id] {
		return nil
	}
	m.reachable[id] = true
	commit, err := LoadCommitObject(m.store, id)
	if err != nil {
		return err
	}
	if err := m.markWorkspace(commit.Tree); err != nil {
		return err
	}
	if err := m.markWorkspace(commit.RemoteBase); err != nil {
		return err
	}
	if err := m.markPatch(commit.Patch); err != nil {
		return err
	}
	// BaseHead is a transient reset target for the single pending user Commit.
	// Stable baseline commits deliberately do not retain history.
	if commit.Kind == userCommitObjectKind {
		return m.markCommit(commit.BaseHead)
	}
	return nil
}

func (m *objectReachability) markPatch(id ObjectID) error {
	if id == "" || m.reachable[id] {
		return nil
	}
	m.reachable[id] = true
	patch, err := LoadPatch(m.store, id)
	if err != nil {
		return err
	}
	if err := m.markWorkspace(patch.BaseTree); err != nil {
		return err
	}
	return m.markWorkspace(patch.TargetTree)
}

func (m *objectReachability) markWorkspace(id ObjectID) error {
	if id == "" || m.reachable[id] {
		return nil
	}
	m.reachable[id] = true
	tree, err := LoadWorkspaceTree(m.store, id)
	if err != nil {
		return err
	}
	for _, document := range tree.Documents {
		if err := m.markDocument(document.DocumentTreeID, document.ID); err != nil {
			return err
		}
	}
	return nil
}

func (m *objectReachability) markDocument(id ObjectID, expectedDocumentID string) error {
	if id == "" {
		return nil
	}
	if m.reachable[id] {
		tree, err := LoadDocumentTree(m.store, id)
		if err != nil {
			return err
		}
		if expectedDocumentID != "" && tree.DocumentID != expectedDocumentID {
			return fmt.Errorf("DocumentTree %s belongs to %s, expected %s", id, tree.DocumentID, expectedDocumentID)
		}
		return nil
	}
	m.reachable[id] = true
	tree, err := LoadDocumentTree(m.store, id)
	if err != nil {
		return err
	}
	if expectedDocumentID != "" && tree.DocumentID != expectedDocumentID {
		return fmt.Errorf("DocumentTree %s belongs to %s, expected %s", id, tree.DocumentID, expectedDocumentID)
	}
	for _, block := range tree.Blocks {
		if block.ObjectID != "" {
			m.reachable[block.ObjectID] = true
			var snapshot BlockSnapshot
			if err := m.store.Get(block.ObjectID, blockSnapshotObjectType, snapshotObjectVersion, &snapshot); err != nil {
				return err
			}
			if snapshot.BlockID != block.BlockID {
				return fmt.Errorf("BlockSnapshot %s belongs to %s, expected %s", block.ObjectID, snapshot.BlockID, block.BlockID)
			}
		}
	}
	return nil
}

func (m *objectReachability) markOperation(id ObjectID) error {
	if id == "" || m.reachable[id] {
		return nil
	}
	m.reachable[id] = true
	kind, err := LoadRepositoryOperationKind(m.store, id)
	if err != nil {
		return err
	}
	if kind == "pull" {
		return m.markPullOperation(id)
	}
	if kind != "push" {
		return fmt.Errorf("unsupported operation kind %s", kind)
	}
	operation, err := LoadPushOperation(m.store, id)
	if err != nil {
		return err
	}
	if err := m.markCommit(operation.CommitObjectID); err != nil {
		return err
	}
	for _, treeID := range []ObjectID{operation.BaseTree, operation.TargetTree, operation.CanonicalTree} {
		if err := m.markWorkspace(treeID); err != nil {
			return err
		}
	}
	for documentID, documentTreeID := range operation.PreflightDocuments {
		if err := m.markDocument(documentTreeID, documentID); err != nil {
			return err
		}
	}
	for documentID, documentTreeID := range operation.CanonicalDocuments {
		if err := m.markDocument(documentTreeID, documentID); err != nil {
			return err
		}
	}
	return nil
}

func (m *objectReachability) markPullOperation(id ObjectID) error {
	operation, err := LoadPullOperation(m.store, id)
	if err != nil {
		return err
	}
	if err := m.markCommit(operation.Head); err != nil {
		return err
	}
	for _, treeID := range []ObjectID{operation.Index, operation.BaseTree, operation.RemoteTree} {
		if err := m.markWorkspace(treeID); err != nil {
			return err
		}
	}
	if err := m.markWorkingTree(operation.WorkingTree); err != nil {
		return err
	}
	for _, plan := range operation.Plans {
		if plan.TargetContent != "" {
			m.reachable[plan.TargetContent] = true
			if _, err := LoadWorkingFileContent(m.store, plan.TargetContent); err != nil {
				return err
			}
		}
		if plan.Conflict != nil {
			for _, contentID := range []ObjectID{plan.Conflict.Base, plan.Conflict.Local, plan.Conflict.Remote} {
				m.reachable[contentID] = true
				if _, err := LoadWorkingFileContent(m.store, contentID); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (m *objectReachability) markWorkingTree(id ObjectID) error {
	if id == "" || m.reachable[id] {
		return nil
	}
	m.reachable[id] = true
	snapshot, err := LoadWorkingTreeSnapshot(m.store, id)
	if err != nil {
		return err
	}
	for _, record := range snapshot.Files {
		if record.ContentObject != "" {
			m.reachable[record.ContentObject] = true
		}
	}
	return nil
}

func removeUnreachableObjects(root string, reachable map[ObjectID]bool) error {
	objectsRoot := filepath.Join(root, config.MetadataDir, "objects", "sha256")
	var directories []string
	err := filepath.WalkDir(objectsRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != objectsRoot {
				directories = append(directories, path)
			}
			return nil
		}
		if strings.HasPrefix(entry.Name(), ".tmp-") {
			return os.Remove(path)
		}
		relative, err := filepath.Rel(objectsRoot, path)
		if err != nil {
			return err
		}
		parts := strings.Split(filepath.ToSlash(relative), "/")
		if len(parts) != 2 {
			return nil
		}
		id := ObjectID(objectIDPrefix + parts[0] + parts[1])
		if _, err := parseObjectID(id); err != nil || reachable[id] {
			return nil
		}
		return os.Remove(path)
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	sort.Slice(directories, func(i, j int) bool { return len(directories[i]) > len(directories[j]) })
	for _, directory := range directories {
		entries, err := os.ReadDir(directory)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if len(entries) != 0 {
			continue
		}
		if err := os.Remove(directory); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}
