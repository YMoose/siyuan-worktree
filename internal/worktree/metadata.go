package worktree

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"siyuan-worktree/internal/config"
)

type BlockMetadata struct {
	ID         string            `json:"id"`
	Type       string            `json:"type"`
	SubType    string            `json:"subType,omitempty"`
	ParentID   string            `json:"parentId"`
	PreviousID string            `json:"previousId,omitempty"`
	Hash       string            `json:"hash"`
	Attrs      map[string]string `json:"attrs"`
}

type DocumentMetadata struct {
	Version       int               `json:"version"`
	DocumentID    string            `json:"documentId"`
	NotebookID    string            `json:"notebookId"`
	NotebookName  string            `json:"notebookName"`
	Title         string            `json:"title"`
	RemotePath    string            `json:"remotePath"`
	LocalPath     string            `json:"localPath"`
	RemoteHash    string            `json:"remoteHash"`
	DocumentAttrs map[string]string `json:"documentAttrs"`
	Blocks        []BlockMetadata   `json:"blocks"`
	RefreshedAt   time.Time         `json:"refreshedAt"`
}

type blockRelation struct {
	ID         string
	Type       string
	SubType    string
	ParentID   string
	PreviousID string
}

func (s *Syncer) writeRemoteMetadata(
	ctx context.Context,
	documentID string,
	notebookID string,
	notebookName string,
	title string,
	remotePath string,
	localPath string,
	annotated AnnotatedDocument,
) error {
	observation, err := s.observeStableRemoteDocument(ctx, documentID, notebookID, notebookName, title, remotePath, localPath)
	if err != nil {
		return err
	}
	if HashContent(RenderAnnotated(observation.Document)) != HashContent(RenderAnnotated(annotated)) {
		return fmt.Errorf("%w: document %s changed before metadata was materialized", ErrRemoteUnstable, documentID)
	}
	return writeDocumentMetadata(s.root, observation.Metadata)
}

func writeDocumentMetadata(root string, metadata DocumentMetadata) error {
	return writeJSONAtomic(documentMetadataPath(root, metadata.DocumentID), metadata)
}

func (s *Syncer) collectBlockRelations(ctx context.Context, parentID string, seen map[string]bool, output *[]blockRelation) error {
	children, err := s.api.GetChildBlocks(ctx, parentID)
	if err != nil {
		return err
	}
	previousID := ""
	for _, child := range children {
		if seen[child.ID] {
			previousID = child.ID
			continue
		}
		seen[child.ID] = true
		*output = append(*output, blockRelation{
			ID:         child.ID,
			Type:       child.Type,
			SubType:    child.SubType,
			ParentID:   parentID,
			PreviousID: previousID,
		})
		if metadataContainerTypes[child.Type] {
			if err := s.collectBlockRelations(ctx, child.ID, seen, output); err != nil {
				return err
			}
		}
		previousID = child.ID
	}
	return nil
}

func documentMetadataPath(root, documentID string) string {
	return filepath.Join(root, config.MetadataDir, "meta", "documents", documentID+".json")
}

func cloneAttrs(attrs map[string]string) map[string]string {
	cloned := make(map[string]string, len(attrs))
	for name, value := range attrs {
		cloned[name] = value
	}
	return cloned
}

var metadataContainerTypes = map[string]bool{
	"l":       true,
	"i":       true,
	"b":       true,
	"s":       true,
	"callout": true,
}

func preservableAttrs(attrs map[string]string) map[string]string {
	preserved := map[string]string{}
	for name, value := range attrs {
		switch name {
		case "id", "updated", "type":
			continue
		default:
			preserved[name] = value
		}
	}
	return preserved
}
