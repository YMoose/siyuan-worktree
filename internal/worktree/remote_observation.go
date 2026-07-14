package worktree

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"siyuan-worktree/internal/siyuan"
)

const remoteObservationAttempts = 4

var ErrRemoteUnstable = errors.New("remote unstable: SiYuan changed while the remote state was being read")

type remoteDocumentObservation struct {
	Document AnnotatedDocument
	Metadata DocumentMetadata
	Version  string
}

type remoteWorkspaceObservation struct {
	Inventory  remoteInventory
	LocalPaths map[string]string
	Documents  map[string]remoteDocumentObservation
	Version    string
}

type kernelTransactionFlusher interface {
	FlushTransactions(context.Context) error
}

// flushKernelTransactions establishes a read barrier for transactions that
// were already queued in the Kernel. It is deliberately not treated as a
// lock: another client may enqueue a new transaction immediately afterwards.
func (s *Syncer) flushKernelTransactions(ctx context.Context) error {
	flusher, ok := s.api.(kernelTransactionFlusher)
	if !ok {
		return nil
	}
	if err := flusher.FlushTransactions(ctx); err != nil {
		return fmt.Errorf("flush pending SiYuan transactions: %w", err)
	}
	return nil
}

func (s *Syncer) observeStableRemoteDocument(
	ctx context.Context,
	documentID string,
	notebookID string,
	notebookName string,
	title string,
	remotePath string,
	localPath string,
) (remoteDocumentObservation, error) {
	if err := s.flushKernelTransactions(ctx); err != nil {
		return remoteDocumentObservation{}, err
	}

	var previous remoteDocumentObservation
	for attempt := 0; attempt < remoteObservationAttempts; attempt++ {
		current, err := s.collectRemoteDocumentOnce(
			ctx,
			documentID,
			notebookID,
			notebookName,
			title,
			remotePath,
			localPath,
		)
		if err != nil {
			if errors.Is(err, ErrRemoteUnstable) {
				previous = remoteDocumentObservation{}
				continue
			}
			return remoteDocumentObservation{}, err
		}
		if previous.Version != "" && previous.Version == current.Version {
			current.Metadata.RefreshedAt = time.Now().UTC()
			return current, nil
		}
		previous = current
	}

	return remoteDocumentObservation{}, fmt.Errorf("%w: document %s did not produce two consecutive identical observations", ErrRemoteUnstable, documentID)
}

func (s *Syncer) observeStableRemoteWorkspace(ctx context.Context) (remoteWorkspaceObservation, error) {
	if err := s.flushKernelTransactions(ctx); err != nil {
		return remoteWorkspaceObservation{}, err
	}

	var previous remoteWorkspaceObservation
	for attempt := 0; attempt < remoteObservationAttempts; attempt++ {
		current, err := s.collectRemoteWorkspaceOnce(ctx)
		if err != nil {
			if errors.Is(err, ErrRemoteUnstable) {
				previous = remoteWorkspaceObservation{}
				continue
			}
			return remoteWorkspaceObservation{}, err
		}
		if previous.Version != "" && previous.Version == current.Version {
			refreshedAt := time.Now().UTC()
			for documentID, observation := range current.Documents {
				observation.Metadata.RefreshedAt = refreshedAt
				current.Documents[documentID] = observation
			}
			return current, nil
		}
		previous = current
	}

	return remoteWorkspaceObservation{}, fmt.Errorf("%w: mapped workspace did not produce two consecutive identical observations", ErrRemoteUnstable)
}

func (s *Syncer) collectRemoteWorkspaceOnce(ctx context.Context) (remoteWorkspaceObservation, error) {
	inventory, err := s.loadRemoteInventoryOnce(ctx)
	if err != nil {
		return remoteWorkspaceObservation{}, err
	}
	localPaths := BuildDocumentPaths(inventory.Notebooks, inventory.DocumentsByNotebook)
	documents := make(map[string]remoteDocumentObservation, len(inventory.DocumentsByID))
	for _, document := range sortedRemoteDocuments(inventory.DocumentsByID) {
		observation, err := s.collectRemoteDocumentOnce(
			ctx,
			document.ID,
			document.NotebookID,
			document.NotebookName,
			document.Name,
			document.Path,
			localPaths[document.ID],
		)
		if err != nil {
			return remoteWorkspaceObservation{}, err
		}
		documents[document.ID] = observation
	}

	finalInventory, err := s.loadRemoteInventoryOnce(ctx)
	if err != nil {
		return remoteWorkspaceObservation{}, err
	}
	unchanged, err := sameRemoteInventory(inventory, finalInventory)
	if err != nil {
		return remoteWorkspaceObservation{}, err
	}
	if !unchanged {
		return remoteWorkspaceObservation{}, fmt.Errorf("%w: notebook or document inventory changed during workspace observation", ErrRemoteUnstable)
	}

	version, err := remoteWorkspaceVersion(inventory, documents)
	if err != nil {
		return remoteWorkspaceObservation{}, err
	}
	return remoteWorkspaceObservation{
		Inventory: inventory, LocalPaths: localPaths, Documents: documents, Version: version,
	}, nil
}

func remoteWorkspaceVersion(inventory remoteInventory, documents map[string]remoteDocumentObservation) (string, error) {
	if len(documents) != len(inventory.DocumentsByID) {
		return "", errors.New("remote workspace observation does not match its inventory")
	}
	inventoryVersion, err := remoteInventoryVersion(inventory)
	if err != nil {
		return "", err
	}
	type documentVersion struct {
		DocumentID string `json:"documentId"`
		Version    string `json:"version"`
	}
	documentVersions := make([]documentVersion, 0, len(documents))
	for documentID, observation := range documents {
		if _, ok := inventory.DocumentsByID[documentID]; !ok {
			return "", fmt.Errorf("remote workspace observation contains document %s outside its inventory", documentID)
		}
		documentVersions = append(documentVersions, documentVersion{DocumentID: documentID, Version: observation.Version})
	}
	sort.Slice(documentVersions, func(i, j int) bool { return documentVersions[i].DocumentID < documentVersions[j].DocumentID })
	encoded, err := json.Marshal(struct {
		Inventory string            `json:"inventory"`
		Documents []documentVersion `json:"documents"`
	}{Inventory: inventoryVersion, Documents: documentVersions})
	if err != nil {
		return "", fmt.Errorf("encode remote workspace observation: %w", err)
	}
	return HashContent(string(encoded)), nil
}

func (s *Syncer) collectRemoteDocumentOnce(
	ctx context.Context,
	documentID string,
	notebookID string,
	notebookName string,
	title string,
	remotePath string,
	localPath string,
) (remoteDocumentObservation, error) {
	relationsBefore := make([]blockRelation, 0)
	if err := s.collectBlockRelations(ctx, documentID, map[string]bool{}, &relationsBefore); err != nil {
		return remoteDocumentObservation{}, err
	}

	ids := make([]string, 0, len(relationsBefore))
	for _, relation := range relationsBefore {
		ids = append(ids, relation.ID)
	}
	kramdowns, err := s.api.GetBlockKramdowns(ctx, ids)
	if err != nil {
		return remoteDocumentObservation{}, err
	}
	attrIDs := make([]string, 0, len(ids)+1)
	attrIDs = append(attrIDs, documentID)
	attrIDs = append(attrIDs, ids...)
	attrs, err := s.api.BatchGetBlockAttrs(ctx, attrIDs)
	if err != nil {
		return remoteDocumentObservation{}, err
	}

	relationsAfter := make([]blockRelation, 0, len(relationsBefore))
	if err := s.collectBlockRelations(ctx, documentID, map[string]bool{}, &relationsAfter); err != nil {
		return remoteDocumentObservation{}, err
	}
	if !slices.Equal(relationsBefore, relationsAfter) {
		return remoteDocumentObservation{}, fmt.Errorf("%w: block structure of document %s changed during observation", ErrRemoteUnstable, documentID)
	}

	annotated := AnnotatedDocument{Blocks: make([]AnnotatedBlock, 0)}
	blocks := make([]BlockMetadata, 0, len(relationsBefore))
	for _, relation := range relationsBefore {
		kramdown, ok := kramdowns[relation.ID]
		if !ok {
			return remoteDocumentObservation{}, fmt.Errorf("SiYuan did not return Kramdown for block %s", relation.ID)
		}
		content := Canonicalize(kramdown)
		if !containsID(ExtractBlockIDs(content), relation.ID) {
			return remoteDocumentObservation{}, fmt.Errorf("SiYuan block %s Kramdown does not contain its IAL", relation.ID)
		}
		if relation.ParentID == documentID {
			annotated.Blocks = append(annotated.Blocks, AnnotatedBlock{
				ID:      relation.ID,
				Type:    relation.Type,
				Content: content,
			})
		}
		blocks = append(blocks, BlockMetadata{
			ID:         relation.ID,
			Type:       relation.Type,
			SubType:    relation.SubType,
			ParentID:   relation.ParentID,
			PreviousID: relation.PreviousID,
			Hash:       HashContent(content),
			Attrs:      cloneAttrs(attrs[relation.ID]),
		})
	}

	metadata := DocumentMetadata{
		Version:       1,
		DocumentID:    documentID,
		NotebookID:    notebookID,
		NotebookName:  notebookName,
		Title:         title,
		RemotePath:    remotePath,
		LocalPath:     localPath,
		RemoteHash:    HashContent(RenderAnnotated(annotated)),
		DocumentAttrs: cloneAttrs(attrs[documentID]),
		Blocks:        blocks,
	}
	version, err := remoteDocumentVersion(annotated, metadata)
	if err != nil {
		return remoteDocumentObservation{}, err
	}
	return remoteDocumentObservation{Document: annotated, Metadata: metadata, Version: version}, nil
}

func remoteDocumentVersion(document AnnotatedDocument, metadata DocumentMetadata) (string, error) {
	metadata.RefreshedAt = time.Time{}
	encoded, err := json.Marshal(struct {
		Document AnnotatedDocument `json:"document"`
		Metadata DocumentMetadata  `json:"metadata"`
	}{Document: document, Metadata: metadata})
	if err != nil {
		return "", fmt.Errorf("encode remote document observation: %w", err)
	}
	return HashContent(string(encoded)), nil
}

type inventoryNotebookVersion struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Sort   int    `json:"sort"`
	Closed bool   `json:"closed"`
}

type inventoryDocumentVersion struct {
	ID           string `json:"id"`
	NotebookID   string `json:"notebookId"`
	NotebookName string `json:"notebookName"`
	Name         string `json:"name"`
	Path         string `json:"path"`
	SubFileCount int    `json:"subFileCount"`
	Sort         int    `json:"sort"`
}

func remoteInventoryVersion(inventory remoteInventory) (string, error) {
	notebooks := make([]inventoryNotebookVersion, 0, len(inventory.Notebooks))
	for _, notebook := range inventory.Notebooks {
		notebooks = append(notebooks, inventoryNotebookVersion{
			ID: notebook.ID, Name: notebook.Name, Sort: notebook.Sort, Closed: notebook.Closed,
		})
	}
	sort.Slice(notebooks, func(i, j int) bool { return notebooks[i].ID < notebooks[j].ID })

	documents := make([]inventoryDocumentVersion, 0, len(inventory.DocumentsByID))
	for _, document := range inventory.DocumentsByID {
		documents = append(documents, inventoryDocumentVersion{
			ID: document.ID, NotebookID: document.NotebookID, NotebookName: document.NotebookName,
			Name: document.Name, Path: document.Path, SubFileCount: document.SubFileCount, Sort: document.Sort,
		})
	}
	sort.Slice(documents, func(i, j int) bool { return documents[i].ID < documents[j].ID })

	encoded, err := json.Marshal(struct {
		Notebooks []inventoryNotebookVersion `json:"notebooks"`
		Documents []inventoryDocumentVersion `json:"documents"`
	}{Notebooks: notebooks, Documents: documents})
	if err != nil {
		return "", fmt.Errorf("encode remote inventory observation: %w", err)
	}
	return HashContent(string(encoded)), nil
}

func sameRemoteInventory(left, right remoteInventory) (bool, error) {
	leftVersion, err := remoteInventoryVersion(left)
	if err != nil {
		return false, err
	}
	rightVersion, err := remoteInventoryVersion(right)
	if err != nil {
		return false, err
	}
	return leftVersion == rightVersion, nil
}

// Ensure the production client remains the only package dependency of the
// observation layer; test APIs may opt into the flush barrier by implementing
// the same small interface.
var _ kernelTransactionFlusher = (*siyuan.Client)(nil)
