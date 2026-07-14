package worktree

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"siyuan-worktree/internal/siyuan"
)

type OperationType string

const (
	OperationUpdate OperationType = "update"
	OperationInsert OperationType = "insert"
	OperationDelete OperationType = "delete"
)

type DocumentPatchStatus string

const (
	DocumentPatchStaged    DocumentPatchStatus = "staged"
	DocumentPatchCommitted DocumentPatchStatus = "committed"
	DocumentPatchApplying  DocumentPatchStatus = "applying"
	DocumentPatchApplied   DocumentPatchStatus = "applied"
	DocumentPatchConflict  DocumentPatchStatus = "conflict"
	DocumentPatchFailed    DocumentPatchStatus = "failed"
)

type HistoryCheckpointStatus string

const (
	HistoryCheckpointRequested          HistoryCheckpointStatus = "requested"
	HistoryCheckpointAcceptedUnverified HistoryCheckpointStatus = "accepted-unverified"
	HistoryCheckpointVerified           HistoryCheckpointStatus = "verified"
	HistoryCheckpointUnverified         HistoryCheckpointStatus = "unverified"
)

type HistoryCheckpoint struct {
	Status            HistoryCheckpointStatus `json:"status"`
	RequestedAt       time.Time               `json:"requestedAt"`
	AcceptedAt        *time.Time              `json:"acceptedAt,omitempty"`
	VerifiedCreated   string                  `json:"verifiedCreated,omitempty"`
	VerificationError string                  `json:"verificationError,omitempty"`
}

type InsertPrecondition struct {
	ChildBlockIDs []string `json:"childBlockIds"`
}

type Operation struct {
	OperationID        string                       `json:"operationId"`
	Type               OperationType                `json:"type"`
	BlockID            string                       `json:"blockId,omitempty"`
	BlockType          string                       `json:"blockType,omitempty"`
	ParentID           string                       `json:"parentId,omitempty"`
	PreviousID         string                       `json:"previousId,omitempty"`
	NextID             string                       `json:"nextId,omitempty"`
	ExpectedHash       string                       `json:"expectedHash,omitempty"`
	ContentHash        string                       `json:"contentHash,omitempty"`
	Content            string                       `json:"content,omitempty"`
	PreservedAttrs     map[string]map[string]string `json:"preservedAttrs,omitempty"`
	InsertPrecondition *InsertPrecondition          `json:"insertPrecondition,omitempty"`
	KernelReceipt      *siyuan.MutationReceipt      `json:"kernelReceipt,omitempty"`
	ReceiptBlockIDs    []string                     `json:"receiptBlockIds,omitempty"`
	ResultBlockIDs     []string                     `json:"resultBlockIds,omitempty"`
}

type DocumentPatch struct {
	Version           int                 `json:"version"`
	DocumentID        string              `json:"documentId"`
	LocalPath         string              `json:"localPath"`
	BaseHash          string              `json:"baseHash"`
	LocalHash         string              `json:"localHash"`
	LocalContent      string              `json:"localContent"`
	Status            DocumentPatchStatus `json:"status"`
	AppliedOperations int                 `json:"appliedOperations"`
	InFlightOperation *int                `json:"inFlightOperation,omitempty"`
	HistoryCheckpoint *HistoryCheckpoint  `json:"historyCheckpoint,omitempty"`
	Operations        []Operation         `json:"operations"`
	Error             string              `json:"error,omitempty"`
}

type DocumentPatchConflictError struct {
	Operation OperationType
	BlockID   string
	Message   string
}

func (e *DocumentPatchConflictError) Error() string {
	target := e.BlockID
	if target == "" {
		target = "document position"
	}
	return fmt.Sprintf("%s %s: %s", e.Operation, target, e.Message)
}

func BuildDocumentPatch(documentID, localPath, base, local string) (DocumentPatch, error) {
	baseDocument, err := ParseAnnotated(base)
	if err != nil {
		return DocumentPatch{}, fmt.Errorf("parse base document: %w", err)
	}
	localDocument, err := ParseEditable(local)
	if err != nil {
		return DocumentPatch{}, fmt.Errorf("parse local document: %w", err)
	}
	baseBlocks := baseDocument.BlockMap()
	localBlocks := map[string]AnnotatedBlock{}
	var localOrder []string
	for _, entry := range localDocument.Entries {
		if entry.Block == nil {
			continue
		}
		baseBlock, ok := baseBlocks[entry.Block.ID]
		if !ok {
			return DocumentPatch{}, fmt.Errorf("local marker references unknown block %s", entry.Block.ID)
		}
		if baseBlock.Type != entry.Block.Type {
			return DocumentPatch{}, errors.New("top-level block type markers must not be changed")
		}
		localBlocks[entry.Block.ID] = *entry.Block
		localOrder = append(localOrder, entry.Block.ID)
	}
	if err := validateExistingOrder(baseDocument, localOrder, localBlocks); err != nil {
		return DocumentPatch{}, err
	}
	operations := make([]Operation, 0)
	for _, baseBlock := range baseDocument.Blocks {
		localBlock, exists := localBlocks[baseBlock.ID]
		if !exists {
			continue
		}
		if HashContent(baseBlock.Content) == HashContent(localBlock.Content) {
			continue
		}
		operations = append(operations, Operation{
			Type:         OperationUpdate,
			BlockID:      baseBlock.ID,
			BlockType:    baseBlock.Type,
			ExpectedHash: HashContent(baseBlock.Content),
			ContentHash:  HashContent(localBlock.Content),
			Content:      Canonicalize(localBlock.Content),
		})
	}
	for _, baseBlock := range baseDocument.Blocks {
		if _, exists := localBlocks[baseBlock.ID]; exists {
			continue
		}
		operations = append(operations, Operation{
			Type:         OperationDelete,
			BlockID:      baseBlock.ID,
			ExpectedHash: HashContent(baseBlock.Content),
		})
	}
	for index, entry := range localDocument.Entries {
		if entry.Block != nil {
			continue
		}
		previousID, nextID := surroundingBlockIDs(localDocument.Entries, index)
		operations = append(operations, Operation{
			Type:        OperationInsert,
			ParentID:    documentID,
			PreviousID:  previousID,
			NextID:      nextID,
			ContentHash: HashContent(entry.NewContent),
			Content:     Canonicalize(entry.NewContent),
		})
	}
	for index := range operations {
		operationID, err := stableOperationID(documentID, index, operations[index])
		if err != nil {
			return DocumentPatch{}, err
		}
		operations[index].OperationID = operationID
	}
	return DocumentPatch{
		Version:      3,
		DocumentID:   documentID,
		LocalPath:    localPath,
		BaseHash:     HashContent(base),
		LocalHash:    HashContent(local),
		LocalContent: Canonicalize(local),
		Operations:   operations,
	}, nil
}

func ValidateDocumentPatchSafety(patch DocumentPatch, base string) error {
	baseDocument, err := ParseAnnotated(base)
	if err != nil {
		return fmt.Errorf("parse patch base: %w", err)
	}
	baseBlocks := baseDocument.BlockMap()
	operationIDs := make(map[string]bool, len(patch.Operations))
	for index, operation := range patch.Operations {
		if operation.OperationID == "" {
			return fmt.Errorf("operation %d has no operationId", index)
		}
		if operationIDs[operation.OperationID] {
			return fmt.Errorf("duplicate operationId %s", operation.OperationID)
		}
		operationIDs[operation.OperationID] = true
		expectedOperationID, err := stableOperationID(patch.DocumentID, index, operation)
		if err != nil {
			return err
		}
		if operation.OperationID != expectedOperationID {
			return fmt.Errorf("operation %d operationId does not match its immutable plan", index)
		}
		switch operation.Type {
		case OperationUpdate:
			if operation.BlockType != "" && !safeEditableBlockTypes[operation.BlockType] {
				return fmt.Errorf("block %s has read-only type %s", operation.BlockID, operation.BlockType)
			}
			baseBlock, ok := baseBlocks[operation.BlockID]
			if !ok {
				return fmt.Errorf("patch base block %s is missing", operation.BlockID)
			}
			if err := ValidateUniqueBlockIDs(operation.Content); err != nil {
				return fmt.Errorf("block %s: %w", operation.BlockID, err)
			}
			if err := ValidateReadOnlyBlockAttrs(baseBlock.Content, operation.Content); err != nil {
				return fmt.Errorf("block %s: %w", operation.BlockID, err)
			}
		case OperationDelete:
			if _, ok := baseBlocks[operation.BlockID]; !ok {
				return fmt.Errorf("delete base block %s is missing", operation.BlockID)
			}
		case OperationInsert:
			if strings.TrimSpace(operation.Content) == "" {
				return errors.New("insert operation content is empty")
			}
			if ids := ExtractBlockIDs(operation.Content); len(ids) > 0 {
				return fmt.Errorf("new Markdown must not contain existing SiYuan block IDs: %s", strings.Join(ids, ", "))
			}
			if operation.ParentID == "" && operation.PreviousID == "" && operation.NextID == "" {
				return errors.New("insert operation has no anchor")
			}
		default:
			return fmt.Errorf("unsupported patch operation %s", operation.Type)
		}
	}
	return nil
}

func stableOperationID(documentID string, index int, operation Operation) (string, error) {
	identity := struct {
		DocumentID   string        `json:"documentId"`
		Index        int           `json:"index"`
		Type         OperationType `json:"type"`
		BlockID      string        `json:"blockId,omitempty"`
		BlockType    string        `json:"blockType,omitempty"`
		ParentID     string        `json:"parentId,omitempty"`
		PreviousID   string        `json:"previousId,omitempty"`
		NextID       string        `json:"nextId,omitempty"`
		ExpectedHash string        `json:"expectedHash,omitempty"`
		ContentHash  string        `json:"contentHash,omitempty"`
		Content      string        `json:"content,omitempty"`
	}{
		DocumentID:   documentID,
		Index:        index,
		Type:         operation.Type,
		BlockID:      operation.BlockID,
		BlockType:    operation.BlockType,
		ParentID:     operation.ParentID,
		PreviousID:   operation.PreviousID,
		NextID:       operation.NextID,
		ExpectedHash: operation.ExpectedHash,
		ContentHash:  operation.ContentHash,
		Content:      operation.Content,
	}
	encoded, err := json.Marshal(identity)
	if err != nil {
		return "", fmt.Errorf("encode operation identity: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func MergeDocumentPatch(patch DocumentPatch, remote AnnotatedDocument) (AnnotatedDocument, error) {
	if err := ValidateDocumentPatchAgainstRemote(patch, remote); err != nil {
		return AnnotatedDocument{}, err
	}
	merged := AnnotatedDocument{Blocks: append([]AnnotatedBlock(nil), remote.Blocks...)}
	remoteBlocks := remote.BlockMap()
	for _, operation := range patch.Operations {
		switch operation.Type {
		case OperationUpdate:
			remoteBlock := remoteBlocks[operation.BlockID]
			if HashContent(remoteBlock.Content) == operation.ExpectedHash {
				if err := replaceAnnotatedBlock(&merged, operation.BlockID, operation.Content); err != nil {
					return AnnotatedDocument{}, err
				}
			}
		case OperationDelete:
			merged.Blocks = removeAnnotatedBlock(merged.Blocks, operation.BlockID)
		case OperationInsert:
			return AnnotatedDocument{}, errors.New("local insert requires push before it can be merged with new remote changes")
		default:
			return AnnotatedDocument{}, fmt.Errorf("unsupported patch operation %s", operation.Type)
		}
	}
	return merged, nil
}

func ValidateDocumentPatchAgainstRemote(patch DocumentPatch, remote AnnotatedDocument) error {
	remoteBlocks := remote.BlockMap()
	deleted := map[string]bool{}
	for _, operation := range patch.Operations {
		if operation.Type == OperationDelete {
			deleted[operation.BlockID] = true
		}
	}
	remainingOrder := make([]string, 0, len(remote.Blocks))
	for _, block := range remote.Blocks {
		if !deleted[block.ID] {
			remainingOrder = append(remainingOrder, block.ID)
		}
	}
	for _, operation := range patch.Operations {
		switch operation.Type {
		case OperationUpdate:
			remoteBlock, ok := remoteBlocks[operation.BlockID]
			if !ok {
				return fmt.Errorf("remote block %s no longer exists", operation.BlockID)
			}
			remoteHash := HashContent(remoteBlock.Content)
			if remoteHash != operation.ExpectedHash && remoteHash != operation.ContentHash && !EquivalentBlockContent(operation.Content, remoteBlock.Content) {
				return fmt.Errorf("block %s changed in both local and SiYuan", operation.BlockID)
			}
		case OperationDelete:
			if remoteBlock, ok := remoteBlocks[operation.BlockID]; ok && HashContent(remoteBlock.Content) != operation.ExpectedHash {
				return fmt.Errorf("block %s changed in SiYuan before deletion", operation.BlockID)
			}
		case OperationInsert:
			if operation.ParentID != patch.DocumentID {
				return fmt.Errorf("insert parent %s is not document %s", operation.ParentID, patch.DocumentID)
			}
			if err := validateInsertionPoint(remainingOrder, operation); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported patch operation %s", operation.Type)
		}
	}
	return nil
}

func validateInsertionPoint(order []string, operation Operation) error {
	previousIndex := -1
	if operation.PreviousID != "" {
		previousIndex = indexOfID(order, operation.PreviousID)
		if previousIndex < 0 {
			return fmt.Errorf("insert previous anchor %s no longer exists", operation.PreviousID)
		}
	}
	nextIndex := len(order)
	if operation.NextID != "" {
		nextIndex = indexOfID(order, operation.NextID)
		if nextIndex < 0 {
			return fmt.Errorf("insert next anchor %s no longer exists", operation.NextID)
		}
	}
	switch {
	case operation.PreviousID == "" && operation.NextID == "":
		if len(order) != 0 {
			return errors.New("unanchored insert is only valid for an empty document")
		}
	case operation.PreviousID == "":
		if nextIndex != 0 {
			return fmt.Errorf("insert next anchor %s is no longer the first block", operation.NextID)
		}
	case operation.NextID == "":
		if previousIndex != len(order)-1 {
			return fmt.Errorf("insert previous anchor %s is no longer the last block", operation.PreviousID)
		}
	case previousIndex+1 != nextIndex:
		return fmt.Errorf("insert anchors %s and %s are no longer adjacent", operation.PreviousID, operation.NextID)
	}
	return nil
}

func indexOfID(ids []string, expected string) int {
	for index, id := range ids {
		if id == expected {
			return index
		}
	}
	return -1
}

func validateExistingOrder(base AnnotatedDocument, localOrder []string, localBlocks map[string]AnnotatedBlock) error {
	expectedOrder := make([]string, 0, len(localOrder))
	for _, block := range base.Blocks {
		if _, exists := localBlocks[block.ID]; exists {
			expectedOrder = append(expectedOrder, block.ID)
		}
	}
	for index := range expectedOrder {
		if expectedOrder[index] != localOrder[index] {
			return errors.New("top-level block reordering is not supported by the safe patch engine yet")
		}
	}
	return nil
}

func surroundingBlockIDs(entries []EditableEntry, index int) (string, string) {
	previousID := ""
	for current := index - 1; current >= 0; current-- {
		if entries[current].Block != nil {
			previousID = entries[current].Block.ID
			break
		}
	}
	nextID := ""
	for current := index + 1; current < len(entries); current++ {
		if entries[current].Block != nil {
			nextID = entries[current].Block.ID
			break
		}
	}
	return previousID, nextID
}

func removeAnnotatedBlock(blocks []AnnotatedBlock, id string) []AnnotatedBlock {
	result := make([]AnnotatedBlock, 0, len(blocks))
	for _, block := range blocks {
		if block.ID != id {
			result = append(result, block)
		}
	}
	return result
}

var safeEditableBlockTypes = map[string]bool{
	"p":       true,
	"h":       true,
	"l":       true,
	"c":       true,
	"m":       true,
	"t":       true,
	"b":       true,
	"s":       true,
	"html":    true,
	"tb":      true,
	"callout": true,
}
