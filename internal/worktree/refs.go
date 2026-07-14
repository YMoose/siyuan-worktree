package worktree

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"siyuan-worktree/internal/atomicfile"
	"siyuan-worktree/internal/config"
)

type RepositoryRefs struct {
	Version    int      `json:"version"`
	Generation uint64   `json:"generation"`
	Head       ObjectID `json:"head,omitempty"`
	Index      ObjectID `json:"index,omitempty"`
	IndexPatch ObjectID `json:"indexPatch,omitempty"`
	Remote     ObjectID `json:"remote,omitempty"`
	Operation  ObjectID `json:"operation,omitempty"`
}

func LoadRepositoryRefs(root string) (RepositoryRefs, error) {
	path := repositoryRefsPath(root)
	var refs RepositoryRefs
	if err := readJSONFile(path, &refs); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return RepositoryRefs{Version: repositoryRefsVersion}, nil
		}
		return RepositoryRefs{}, fmt.Errorf("read repository refs: %w", err)
	}
	if refs.Version != repositoryRefsVersion {
		return RepositoryRefs{}, fmt.Errorf("unsupported repository refs version %d", refs.Version)
	}
	if err := validateRepositoryRefs(refs); err != nil {
		return RepositoryRefs{}, err
	}
	return refs, nil
}

func SaveRepositoryRefs(root string, expectedGeneration uint64, refs RepositoryRefs) error {
	path := repositoryRefsPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create refs directory: %w", err)
	}
	lockPath := path + ".lock"
	lock, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("repository refs are locked by another update: %s", lockPath)
		}
		return fmt.Errorf("lock repository refs: %w", err)
	}
	committed := false
	defer func() {
		lock.Close()
		if !committed {
			os.Remove(lockPath)
		}
	}()

	current, err := LoadRepositoryRefs(root)
	if err != nil {
		return err
	}
	if current.Generation != expectedGeneration {
		return fmt.Errorf("repository refs changed concurrently: expected generation %d, got %d", expectedGeneration, current.Generation)
	}
	refs.Version = repositoryRefsVersion
	refs.Generation = expectedGeneration + 1
	if err := validateRepositoryRefs(refs); err != nil {
		return err
	}
	data, err := json.MarshalIndent(refs, "", "  ")
	if err != nil {
		return fmt.Errorf("encode repository refs: %w", err)
	}
	if _, err := lock.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write repository refs lock: %w", err)
	}
	if err := lock.Sync(); err != nil {
		return fmt.Errorf("sync repository refs lock: %w", err)
	}
	if err := lock.Close(); err != nil {
		return fmt.Errorf("close repository refs lock: %w", err)
	}
	if err := atomicfile.Commit(lockPath, path); err != nil {
		return fmt.Errorf("commit repository refs: %w", err)
	}
	committed = true
	return nil
}

func recoverRepositoryRefsLock(root string) error {
	path := repositoryRefsPath(root)
	lockPath := path + ".lock"
	data, err := os.ReadFile(lockPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read prepared repository refs: %w", err)
	}
	current, err := LoadRepositoryRefs(root)
	if err != nil {
		return err
	}
	var candidate RepositoryRefs
	valid := json.Unmarshal(data, &candidate) == nil && candidate.Version == repositoryRefsVersion &&
		candidate.Generation == current.Generation+1 && validateRepositoryRefs(candidate) == nil &&
		candidate.Head != "" && candidate.Index != "" && candidate.Remote != ""
	if valid {
		valid = ValidateRepositorySnapshots(root, candidate) == nil
	}
	if valid {
		_, err := markRepositoryObjects(root, candidate)
		valid = err == nil
	}
	if valid {
		if err := atomicfile.Commit(lockPath, path); err != nil {
			return fmt.Errorf("recover prepared repository refs: %w", err)
		}
		return nil
	}
	if err := os.Remove(lockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("discard invalid repository refs lock: %w", err)
	}
	return nil
}

func AdvanceRemoteSnapshot(root string, store *ObjectStore, remoteTree ObjectID, advanceHead bool) error {
	if err := store.Get(remoteTree, workspaceTreeObjectType, snapshotObjectVersion, nil); err != nil {
		return fmt.Errorf("validate remote workspace snapshot: %w", err)
	}
	refs, err := LoadRepositoryRefs(root)
	if err != nil {
		return err
	}
	if refs.Remote == remoteTree && (!advanceHead || refs.Index == remoteTree) {
		pruneRepositoryObjectsBestEffort(root)
		return nil
	}
	next := refs
	next.Remote = remoteTree
	if advanceHead {
		if refs.Head != "" {
			if err := store.Get(refs.Head, commitObjectType, snapshotObjectVersion, nil); err != nil {
				return fmt.Errorf("validate current HEAD: %w", err)
			}
		}
		commitID, err := StoreBaselineCommit(store, remoteTree)
		if err != nil {
			return err
		}
		next.Head = commitID
		next.Index = remoteTree
		next.IndexPatch = ""
	}
	if _, err := markRepositoryObjects(root, next); err != nil {
		return fmt.Errorf("validate advanced repository refs: %w", err)
	}
	if err := SaveRepositoryRefs(root, refs.Generation, next); err != nil {
		return err
	}
	pruneRepositoryObjectsBestEffort(root)
	return nil
}

func repositoryRefsPath(root string) string {
	return filepath.Join(root, config.MetadataDir, "refs", "state.json")
}

func repositoryRefsInitialized(refs RepositoryRefs) bool {
	return refs.Generation != 0 || refs.Head != "" || refs.Index != "" || refs.IndexPatch != "" || refs.Remote != "" || refs.Operation != ""
}

func validateRepositoryRefs(refs RepositoryRefs) error {
	values := map[string]ObjectID{
		"head":       refs.Head,
		"index":      refs.Index,
		"indexPatch": refs.IndexPatch,
		"remote":     refs.Remote,
		"operation":  refs.Operation,
	}
	for name, id := range values {
		if id == "" {
			continue
		}
		if _, err := parseObjectID(id); err != nil {
			return fmt.Errorf("invalid repository ref %s: %w", name, err)
		}
	}
	return nil
}
