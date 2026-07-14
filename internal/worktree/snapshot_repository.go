package worktree

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"siyuan-worktree/internal/config"
)

type PatchObject struct {
	BaseTree        ObjectID        `json:"baseTree"`
	TargetTree      ObjectID        `json:"targetTree"`
	DocumentPatches []DocumentPatch `json:"documentPatches"`
}

type PushOperationPhase string

const (
	PushOperationPrepared          PushOperationPhase = "prepared"
	PushOperationApplying          PushOperationPhase = "applying"
	PushOperationRemoteVerified    PushOperationPhase = "remote-verified"
	PushOperationCanonicalSnapshot PushOperationPhase = "canonical-snapshot-created"
	PushOperationMaterializing     PushOperationPhase = "materializing-working-tree"
)

type PushOperationState struct {
	Kind                  string              `json:"kind"`
	Phase                 PushOperationPhase  `json:"phase"`
	CommitObjectID        ObjectID            `json:"commitObjectId"`
	BaseTree              ObjectID            `json:"baseTree"`
	TargetTree            ObjectID            `json:"targetTree"`
	PreflightDocuments    map[string]ObjectID `json:"preflightDocuments,omitempty"`
	CanonicalTree         ObjectID            `json:"canonicalTree,omitempty"`
	CanonicalDocuments    map[string]ObjectID `json:"canonicalDocuments,omitempty"`
	MaterializedDocuments []string            `json:"materializedDocuments,omitempty"`
	Commit                Commit              `json:"commit"`
	Error                 string              `json:"error,omitempty"`
	UpdatedAt             time.Time           `json:"updatedAt"`
}

func StoreEditableDocumentTree(store *ObjectStore, documentID, markdown string, patch DocumentPatch) (ObjectID, error) {
	editable, err := ParseEditable(markdown)
	if err != nil {
		return "", err
	}
	insertOperations := make([]Operation, 0)
	for _, operation := range patch.Operations {
		if operation.Type == OperationInsert {
			insertOperations = append(insertOperations, operation)
		}
	}
	insertIndex := 0
	refs := make([]BlockSnapshotRef, 0, len(editable.Entries))
	for _, entry := range editable.Entries {
		block := BlockSnapshot{}
		if entry.Block != nil {
			attrs, err := extractBlockIALAttrs(entry.Block.Content)
			if err != nil {
				return "", fmt.Errorf("snapshot block %s attributes: %w", entry.Block.ID, err)
			}
			block = BlockSnapshot{
				BlockID:        entry.Block.ID,
				BlockType:      entry.Block.Type,
				Kramdown:       Canonicalize(entry.Block.Content),
				AttrsByBlockID: attrs,
			}
		} else {
			if insertIndex >= len(insertOperations) {
				return "", errors.New("editable snapshot has more untracked entries than insert operations")
			}
			operation := insertOperations[insertIndex]
			insertIndex++
			block = BlockSnapshot{
				BlockID:        "local:" + strings.TrimPrefix(operation.OperationID, objectIDPrefix),
				BlockType:      "local-insert",
				Kramdown:       Canonicalize(entry.NewContent),
				AttrsByBlockID: map[string]map[string]string{},
				Provisional:    true,
			}
		}
		blockObjectID, err := store.Put(blockSnapshotObjectType, snapshotObjectVersion, block)
		if err != nil {
			return "", err
		}
		refs = append(refs, BlockSnapshotRef{BlockID: block.BlockID, ObjectID: blockObjectID})
	}
	if insertIndex != len(insertOperations) {
		return "", errors.New("insert operations are not represented in the editable snapshot")
	}
	return store.Put(documentTreeObjectType, snapshotObjectVersion, DocumentTree{
		DocumentID: documentID,
		Blocks:     refs,
		Markdown:   Canonicalize(markdown),
	})
}

func LoadWorkspaceTree(store *ObjectStore, id ObjectID) (WorkspaceTree, error) {
	var tree WorkspaceTree
	if err := store.Get(id, workspaceTreeObjectType, snapshotObjectVersion, &tree); err != nil {
		return WorkspaceTree{}, err
	}
	return tree, nil
}

func StoreWorkspaceTreeObject(store *ObjectStore, tree WorkspaceTree) (ObjectID, error) {
	tree.SelectedNotebooks = append([]string(nil), tree.SelectedNotebooks...)
	tree.Notebooks = append([]WorkspaceNotebook(nil), tree.Notebooks...)
	tree.Documents = append([]WorkspaceDocument(nil), tree.Documents...)
	sort.Strings(tree.SelectedNotebooks)
	sort.Slice(tree.Notebooks, func(i, j int) bool { return tree.Notebooks[i].ID < tree.Notebooks[j].ID })
	sort.Slice(tree.Documents, func(i, j int) bool { return tree.Documents[i].ID < tree.Documents[j].ID })
	return store.Put(workspaceTreeObjectType, snapshotObjectVersion, tree)
}

func LoadCommitObject(store *ObjectStore, id ObjectID) (CommitObject, error) {
	var commit CommitObject
	if err := store.Get(id, commitObjectType, snapshotObjectVersion, &commit); err != nil {
		return CommitObject{}, err
	}
	switch commit.Kind {
	case baselineCommitObjectKind:
		if commit.Tree == "" || commit.Tree != commit.RemoteBase || commit.BaseHead != "" || commit.Patch != "" {
			return CommitObject{}, fmt.Errorf("baseline CommitObject %s has invalid references", id)
		}
	case userCommitObjectKind:
		if commit.Tree == "" || commit.BaseHead == "" || commit.RemoteBase == "" || commit.Patch == "" ||
			commit.DisplayID == "" || strings.TrimSpace(commit.Message) == "" || commit.CreatedAt.IsZero() {
			return CommitObject{}, fmt.Errorf("user CommitObject %s has incomplete state", id)
		}
	default:
		return CommitObject{}, fmt.Errorf("CommitObject %s has unsupported kind %s", id, commit.Kind)
	}
	return commit, nil
}

func LoadDocumentTree(store *ObjectStore, id ObjectID) (DocumentTree, error) {
	var tree DocumentTree
	if err := store.Get(id, documentTreeObjectType, snapshotObjectVersion, &tree); err != nil {
		return DocumentTree{}, err
	}
	return tree, nil
}

func RenderDocumentTree(store *ObjectStore, id ObjectID) (string, error) {
	tree, err := LoadDocumentTree(store, id)
	if err != nil {
		return "", err
	}
	blocks, err := loadDocumentTreeBlocks(store, tree)
	if err != nil {
		return "", err
	}
	if tree.Markdown != "" {
		if err := validateDocumentTreeMarkdown(tree.Markdown, blocks); err != nil {
			return "", fmt.Errorf("document tree %s Markdown does not match its BlockSnapshot graph: %w", id, err)
		}
		return Canonicalize(tree.Markdown), nil
	}
	return renderDocumentTreeBlocks(blocks), nil
}

func loadDocumentTreeBlocks(store *ObjectStore, tree DocumentTree) ([]BlockSnapshot, error) {
	blocks := make([]BlockSnapshot, 0, len(tree.Blocks))
	for _, blockRef := range tree.Blocks {
		var block BlockSnapshot
		if err := store.Get(blockRef.ObjectID, blockSnapshotObjectType, snapshotObjectVersion, &block); err != nil {
			return nil, err
		}
		if block.BlockID != blockRef.BlockID {
			return nil, fmt.Errorf("document block ref %s points to block %s", blockRef.BlockID, block.BlockID)
		}
		blocks = append(blocks, block)
	}
	return blocks, nil
}

func validateDocumentTreeMarkdown(markdown string, blocks []BlockSnapshot) error {
	editable, err := ParseEditable(markdown)
	if err != nil {
		return err
	}
	if len(editable.Entries) != len(blocks) {
		return fmt.Errorf("contains %d entries for %d block snapshots", len(editable.Entries), len(blocks))
	}
	for index, block := range blocks {
		entry := editable.Entries[index]
		if block.Provisional {
			if entry.Block != nil || HashContent(entry.NewContent) != HashContent(block.Kramdown) {
				return fmt.Errorf("provisional entry %d does not match block %s", index, block.BlockID)
			}
			continue
		}
		if entry.Block == nil || entry.Block.ID != block.BlockID || entry.Block.Type != block.BlockType ||
			HashContent(entry.Block.Content) != HashContent(block.Kramdown) {
			return fmt.Errorf("entry %d does not match block %s", index, block.BlockID)
		}
	}
	return nil
}

func renderDocumentTreeBlocks(blocks []BlockSnapshot) string {
	var output strings.Builder
	for index, block := range blocks {
		if index > 0 {
			output.WriteByte('\n')
		}
		if block.Provisional {
			output.WriteString(Canonicalize(block.Kramdown))
			continue
		}
		output.WriteString(RenderAnnotated(AnnotatedDocument{Blocks: []AnnotatedBlock{{
			ID:      block.BlockID,
			Type:    block.BlockType,
			Content: block.Kramdown,
		}}}))
	}
	return Canonicalize(output.String())
}

func WorkspaceDocumentByID(tree WorkspaceTree, documentID string) (WorkspaceDocument, bool) {
	for _, document := range tree.Documents {
		if document.ID == documentID {
			return document, true
		}
	}
	return WorkspaceDocument{}, false
}

func ReplaceWorkspaceDocumentTree(tree *WorkspaceTree, documentID string, documentTreeID ObjectID) error {
	for index := range tree.Documents {
		if tree.Documents[index].ID == documentID {
			tree.Documents[index].DocumentTreeID = documentTreeID
			return nil
		}
	}
	return fmt.Errorf("workspace snapshot does not contain document %s", documentID)
}

func StorePatch(store *ObjectStore, baseTree, targetTree ObjectID, patches []DocumentPatch) (ObjectID, PatchObject, error) {
	patch := PatchObject{
		BaseTree:        baseTree,
		TargetTree:      targetTree,
		DocumentPatches: immutableDocumentPatches(patches),
	}
	id, err := store.Put(patchObjectType, snapshotObjectVersion, patch)
	return id, patch, err
}

func LoadPatch(store *ObjectStore, id ObjectID) (PatchObject, error) {
	var patch PatchObject
	if err := store.Get(id, patchObjectType, snapshotObjectVersion, &patch); err != nil {
		return PatchObject{}, err
	}
	return patch, nil
}

func ValidatePatch(store *ObjectStore, patch PatchObject) error {
	baseTree, err := LoadWorkspaceTree(store, patch.BaseTree)
	if err != nil {
		return fmt.Errorf("load patch base tree: %w", err)
	}
	targetTree, err := LoadWorkspaceTree(store, patch.TargetTree)
	if err != nil {
		return fmt.Errorf("load patch target tree: %w", err)
	}
	if baseTree.AllOpenNotebooks != targetTree.AllOpenNotebooks ||
		!reflect.DeepEqual(baseTree.SelectedNotebooks, targetTree.SelectedNotebooks) ||
		!reflect.DeepEqual(baseTree.Notebooks, targetTree.Notebooks) {
		return errors.New("Patch changes workspace mapping metadata, which is not supported")
	}
	if len(baseTree.Documents) != len(targetTree.Documents) {
		return errors.New("Patch changes the tracked document set, which is not supported")
	}
	patches := make(map[string]DocumentPatch, len(patch.DocumentPatches))
	for _, patch := range patch.DocumentPatches {
		if _, exists := patches[patch.DocumentID]; exists {
			return fmt.Errorf("Patch contains duplicate document %s", patch.DocumentID)
		}
		patches[patch.DocumentID] = patch
	}
	changed := 0
	for _, targetDocument := range targetTree.Documents {
		baseDocument, exists := WorkspaceDocumentByID(baseTree, targetDocument.ID)
		if !exists {
			return fmt.Errorf("patch target contains new document %s, which is not supported yet", targetDocument.ID)
		}
		baseMetadata := baseDocument
		targetMetadata := targetDocument
		baseMetadata.DocumentTreeID = ""
		targetMetadata.DocumentTreeID = ""
		if !reflect.DeepEqual(baseMetadata, targetMetadata) {
			return fmt.Errorf("patch target changes metadata for document %s, which is not supported", targetDocument.ID)
		}
		if baseDocument.DocumentTreeID == targetDocument.DocumentTreeID {
			continue
		}
		changed++
		cached, exists := patches[targetDocument.ID]
		if !exists {
			return fmt.Errorf("changed snapshot document %s has no DocumentPatch", targetDocument.ID)
		}
		baseMarkdown, err := RenderDocumentTree(store, baseDocument.DocumentTreeID)
		if err != nil {
			return err
		}
		targetMarkdown, err := RenderDocumentTree(store, targetDocument.DocumentTreeID)
		if err != nil {
			return err
		}
		recomputed, err := BuildDocumentPatch(targetDocument.ID, targetDocument.LocalPath, baseMarkdown, targetMarkdown)
		if err != nil {
			return fmt.Errorf("recompute patch for %s: %w", targetDocument.LocalPath, err)
		}
		if err := ValidateDocumentPatchSafety(recomputed, baseMarkdown); err != nil {
			return fmt.Errorf("validate recomputed patch for %s: %w", targetDocument.LocalPath, err)
		}
		if !reflect.DeepEqual(immutableDocumentPatch(cached), immutableDocumentPatch(recomputed)) {
			return fmt.Errorf("cached DocumentPatch for %s does not match its snapshots", targetDocument.LocalPath)
		}
		delete(patches, targetDocument.ID)
	}
	if changed != len(patch.DocumentPatches) || len(patches) != 0 {
		return errors.New("Patch and snapshot document changes do not match")
	}
	return nil
}

func immutableDocumentPatches(patches []DocumentPatch) []DocumentPatch {
	result := make([]DocumentPatch, len(patches))
	for index, patch := range patches {
		result[index] = immutableDocumentPatch(patch)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].LocalPath < result[j].LocalPath })
	return result
}

func immutableDocumentPatch(patch DocumentPatch) DocumentPatch {
	patch.Status = DocumentPatchStaged
	patch.AppliedOperations = 0
	patch.InFlightOperation = nil
	patch.HistoryCheckpoint = nil
	patch.Error = ""
	patch.Operations = append([]Operation(nil), patch.Operations...)
	for index := range patch.Operations {
		patch.Operations[index].PreservedAttrs = nil
		patch.Operations[index].InsertPrecondition = nil
		patch.Operations[index].KernelReceipt = nil
		patch.Operations[index].ReceiptBlockIDs = nil
		patch.Operations[index].ResultBlockIDs = nil
	}
	return patch
}

func EnsureRepositorySnapshots(root string, cfg config.Config, state State) (RepositoryRefs, error) {
	store := NewObjectStore(root)
	refs, err := LoadRepositoryRefs(root)
	if err != nil {
		return RepositoryRefs{}, err
	}
	if refs.Head != "" && refs.Index != "" && refs.Remote != "" {
		if err := ValidateRepositorySnapshots(root, refs); err != nil {
			return RepositoryRefs{}, err
		}
		return refs, nil
	}
	if repositoryRefsInitialized(refs) {
		return RepositoryRefs{}, errors.New("repository snapshot refs are incomplete")
	}
	if len(state.Documents) != 0 {
		return RepositoryRefs{}, errors.New("existing worktree has no snapshot refs; create a fresh clone")
	}
	workspaceID, err := StoreWorkspaceTreeObject(store, WorkspaceTree{
		AllOpenNotebooks:  len(cfg.NotebookIDs) == 0,
		SelectedNotebooks: append([]string(nil), cfg.NotebookIDs...),
	})
	if err != nil {
		return RepositoryRefs{}, err
	}
	commitID, err := StoreBaselineCommit(store, workspaceID)
	if err != nil {
		return RepositoryRefs{}, err
	}
	next := refs
	next.Head = commitID
	next.Index = workspaceID
	next.IndexPatch = ""
	next.Remote = workspaceID
	next.Operation = ""
	if err := SaveRepositoryRefs(root, refs.Generation, next); err != nil {
		return RepositoryRefs{}, err
	}
	pruneRepositoryObjectsBestEffort(root)
	return LoadRepositoryRefs(root)
}

func ValidateRepositorySnapshots(root string, refs RepositoryRefs) error {
	store := NewObjectStore(root)
	if _, err := LoadWorkspaceTree(store, refs.Index); err != nil {
		return fmt.Errorf("load Index snapshot: %w", err)
	}
	if _, err := LoadWorkspaceTree(store, refs.Remote); err != nil {
		return fmt.Errorf("load Remote snapshot: %w", err)
	}
	head, err := LoadHeadCommit(store, refs)
	if err != nil {
		return err
	}
	switch head.Kind {
	case baselineCommitObjectKind:
		if refs.IndexPatch == "" {
			if refs.Index != head.Tree {
				return errors.New("clean Index does not match the baseline HEAD")
			}
		} else {
			if refs.Remote != head.Tree {
				return errors.New("staged Index cannot be based on a divergent Remote snapshot")
			}
			patch, err := LoadPatch(store, refs.IndexPatch)
			if err != nil {
				return err
			}
			if patch.BaseTree != head.Tree || patch.TargetTree != refs.Index {
				return errors.New("Index Patch does not match HEAD and Index")
			}
			if err := ValidatePatch(store, patch); err != nil {
				return err
			}
		}
	case userCommitObjectKind:
		if refs.IndexPatch != "" || refs.Index != head.Tree || refs.Remote != head.RemoteBase {
			return errors.New("pending CommitObject does not match Index and Remote refs")
		}
		baseHead, err := LoadCommitObject(store, head.BaseHead)
		if err != nil {
			return err
		}
		if baseHead.Kind != baselineCommitObjectKind || baseHead.Tree != head.RemoteBase {
			return errors.New("pending CommitObject baseHead does not match RemoteBase")
		}
		patch, err := LoadPatch(store, head.Patch)
		if err != nil {
			return err
		}
		if patch.BaseTree != head.RemoteBase || patch.TargetTree != head.Tree {
			return errors.New("pending CommitObject Patch does not match its snapshots")
		}
		if err := ValidatePatch(store, patch); err != nil {
			return err
		}
	default:
		return fmt.Errorf("repository HEAD has unsupported CommitObject kind %s", head.Kind)
	}
	if refs.Operation != "" {
		kind, err := LoadRepositoryOperationKind(store, refs.Operation)
		if err != nil {
			return err
		}
		if kind == "pull" {
			return validateActivePullOperation(store, refs)
		}
		if kind != "push" {
			return fmt.Errorf("active operation has unsupported kind %s", kind)
		}
		operation, err := LoadPushOperation(store, refs.Operation)
		if err != nil {
			return err
		}
		if head.Kind != userCommitObjectKind || operation.CommitObjectID != refs.Head || operation.Commit.ObjectID != refs.Head ||
			operation.BaseTree != head.RemoteBase || operation.TargetTree != head.Tree || operation.Commit.Tree != head.Tree ||
			operation.Commit.BaseHead != head.BaseHead || operation.Commit.RemoteBase != head.RemoteBase || operation.Commit.Patch != head.Patch ||
			operation.Commit.ID != head.DisplayID || operation.Commit.Message != head.Message || !operation.Commit.CreatedAt.Equal(head.CreatedAt) {
			return errors.New("active push OperationState does not match repository refs")
		}
		patch, err := LoadPatch(store, head.Patch)
		if err != nil {
			return err
		}
		if !sameDocumentPatchIntents(operation.Commit.DocumentPatches, patch.DocumentPatches) {
			return errors.New("active push OperationState does not match the immutable Patch")
		}
		for documentID, documentTreeID := range operation.PreflightDocuments {
			documentTree, err := LoadDocumentTree(store, documentTreeID)
			if err != nil {
				return fmt.Errorf("load preflight document %s: %w", documentID, err)
			}
			if documentTree.DocumentID != documentID {
				return fmt.Errorf("preflight document %s points to snapshot for %s", documentID, documentTree.DocumentID)
			}
			if _, err := RenderDocumentTree(store, documentTreeID); err != nil {
				return fmt.Errorf("validate preflight document %s: %w", documentID, err)
			}
		}
		for documentID, documentTreeID := range operation.CanonicalDocuments {
			documentTree, err := LoadDocumentTree(store, documentTreeID)
			if err != nil {
				return fmt.Errorf("load canonical document %s: %w", documentID, err)
			}
			if documentTree.DocumentID != documentID {
				return fmt.Errorf("canonical document %s points to snapshot for %s", documentID, documentTree.DocumentID)
			}
			if _, err := RenderDocumentTree(store, documentTreeID); err != nil {
				return fmt.Errorf("validate canonical document %s: %w", documentID, err)
			}
		}
		if operation.CanonicalTree != "" {
			canonicalWorkspace, err := LoadWorkspaceTree(store, operation.CanonicalTree)
			if err != nil {
				return fmt.Errorf("load canonical WorkspaceTree: %w", err)
			}
			targetWorkspace, err := LoadWorkspaceTree(store, head.Tree)
			if err != nil {
				return fmt.Errorf("load target WorkspaceTree: %w", err)
			}
			if err := validateCanonicalWorkspace(targetWorkspace, canonicalWorkspace, operation.CanonicalDocuments); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateCanonicalWorkspace(target, canonical WorkspaceTree, canonicalDocuments map[string]ObjectID) error {
	if target.AllOpenNotebooks != canonical.AllOpenNotebooks ||
		!reflect.DeepEqual(target.SelectedNotebooks, canonical.SelectedNotebooks) ||
		!reflect.DeepEqual(target.Notebooks, canonical.Notebooks) || len(target.Documents) != len(canonical.Documents) {
		return errors.New("canonical WorkspaceTree changes mapping metadata")
	}
	for _, targetDocument := range target.Documents {
		canonicalDocument, ok := WorkspaceDocumentByID(canonical, targetDocument.ID)
		if !ok {
			return fmt.Errorf("canonical WorkspaceTree is missing document %s", targetDocument.ID)
		}
		expectedTreeID := targetDocument.DocumentTreeID
		if documentTreeID := canonicalDocuments[targetDocument.ID]; documentTreeID != "" {
			expectedTreeID = documentTreeID
		}
		targetMetadata := targetDocument
		canonicalMetadata := canonicalDocument
		targetMetadata.DocumentTreeID = ""
		canonicalMetadata.DocumentTreeID = ""
		if !reflect.DeepEqual(targetMetadata, canonicalMetadata) || canonicalDocument.DocumentTreeID != expectedTreeID {
			return fmt.Errorf("canonical WorkspaceTree has invalid state for document %s", targetDocument.ID)
		}
	}
	return nil
}

func StoreBaselineCommit(store *ObjectStore, workspaceID ObjectID) (ObjectID, error) {
	if _, err := LoadWorkspaceTree(store, workspaceID); err != nil {
		return "", fmt.Errorf("load baseline workspace: %w", err)
	}
	return store.Put(commitObjectType, snapshotObjectVersion, CommitObject{
		Kind:       baselineCommitObjectKind,
		Tree:       workspaceID,
		RemoteBase: workspaceID,
	})
}

func LoadHeadCommit(store *ObjectStore, refs RepositoryRefs) (CommitObject, error) {
	if refs.Head == "" {
		return CommitObject{}, errors.New("repository HEAD is not initialized")
	}
	return LoadCommitObject(store, refs.Head)
}

func ListCurrentCommitSummaries(root string) ([]CommitSummary, error) {
	commits, err := ListPendingCommits(root)
	if err != nil {
		return nil, err
	}
	result := make([]CommitSummary, 0, len(commits))
	for _, commit := range commits {
		result = append(result, SummarizeCommit(commit))
	}
	return result, nil
}

func StorePushOperation(store *ObjectStore, operation PushOperationState) (ObjectID, error) {
	operation.Kind = "push"
	operation.UpdatedAt = time.Now().UTC()
	operation.PreflightDocuments = cloneObjectIDMap(operation.PreflightDocuments)
	operation.CanonicalDocuments = cloneObjectIDMap(operation.CanonicalDocuments)
	operation.MaterializedDocuments = append([]string(nil), operation.MaterializedDocuments...)
	sort.Strings(operation.MaterializedDocuments)
	if err := validatePushOperationProgress(operation); err != nil {
		return "", err
	}
	return store.Put(pushOperationObjectType, snapshotObjectVersion, operation)
}

func LoadPushOperation(store *ObjectStore, id ObjectID) (PushOperationState, error) {
	var operation PushOperationState
	if err := store.Get(id, pushOperationObjectType, snapshotObjectVersion, &operation); err != nil {
		return PushOperationState{}, err
	}
	if operation.Kind != "push" {
		return PushOperationState{}, fmt.Errorf("operation %s has kind %s", id, operation.Kind)
	}
	if _, ok := pushOperationPhaseRank(operation.Phase); !ok {
		return PushOperationState{}, fmt.Errorf("operation %s has unsupported phase %s", id, operation.Phase)
	}
	if err := validatePushOperationProgress(operation); err != nil {
		return PushOperationState{}, fmt.Errorf("operation %s is invalid: %w", id, err)
	}
	return operation, nil
}

func pushOperationPhaseRank(phase PushOperationPhase) (int, bool) {
	switch phase {
	case PushOperationPrepared:
		return 0, true
	case PushOperationApplying:
		return 1, true
	case PushOperationRemoteVerified:
		return 2, true
	case PushOperationCanonicalSnapshot:
		return 3, true
	case PushOperationMaterializing:
		return 4, true
	default:
		return 0, false
	}
}

func cloneObjectIDMap(values map[string]ObjectID) map[string]ObjectID {
	if len(values) == 0 {
		return nil
	}
	result := make(map[string]ObjectID, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func patchJSON(patch PatchObject) ([]byte, error) {
	return json.Marshal(patch)
}
