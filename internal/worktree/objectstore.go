package worktree

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"siyuan-worktree/internal/atomicfile"
	"siyuan-worktree/internal/config"
)

const objectIDPrefix = "sha256:"

type ObjectID string

type storedObject struct {
	Type    string          `json:"type"`
	Version int             `json:"version"`
	Data    json.RawMessage `json:"data"`
}

type ObjectStore struct {
	root string
}

func NewObjectStore(root string) *ObjectStore {
	return &ObjectStore{root: root}
}

func (s *ObjectStore) Put(objectType string, version int, value any) (ObjectID, error) {
	if strings.TrimSpace(objectType) == "" || version < 1 {
		return "", errors.New("object type and positive version are required")
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode %s object data: %w", objectType, err)
	}
	record, err := json.Marshal(storedObject{Type: objectType, Version: version, Data: payload})
	if err != nil {
		return "", fmt.Errorf("encode %s object: %w", objectType, err)
	}
	id := hashObject(record)
	path, err := s.path(id)
	if err != nil {
		return "", err
	}
	if existing, readErr := os.ReadFile(path); readErr == nil {
		if !bytes.Equal(existing, record) {
			return "", fmt.Errorf("object %s exists with different content", id)
		}
		return id, nil
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return "", fmt.Errorf("read existing object %s: %w", id, readErr)
	}
	if err := atomicfile.Write(path, record, 0o600); err != nil {
		return "", fmt.Errorf("write object %s: %w", id, err)
	}
	return id, nil
}

func (s *ObjectStore) Get(id ObjectID, expectedType string, expectedVersion int, destination any) error {
	path, err := s.path(id)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read object %s: %w", id, err)
	}
	if actual := hashObject(data); actual != id {
		return fmt.Errorf("object %s failed checksum verification; got %s", id, actual)
	}
	var record storedObject
	if err := json.Unmarshal(data, &record); err != nil {
		return fmt.Errorf("decode object %s: %w", id, err)
	}
	if expectedType != "" && record.Type != expectedType {
		return fmt.Errorf("object %s has type %s, expected %s", id, record.Type, expectedType)
	}
	if expectedVersion > 0 && record.Version != expectedVersion {
		return fmt.Errorf("object %s has version %d, expected %d", id, record.Version, expectedVersion)
	}
	if destination == nil {
		return nil
	}
	if err := json.Unmarshal(record.Data, destination); err != nil {
		return fmt.Errorf("decode object %s data: %w", id, err)
	}
	return nil
}

func (s *ObjectStore) removeObjectBestEffort(id ObjectID) {
	path, err := s.path(id)
	if err != nil {
		return
	}
	_ = os.Remove(path)
}

func (s *ObjectStore) path(id ObjectID) (string, error) {
	hexID, err := parseObjectID(id)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.root, config.MetadataDir, "objects", "sha256", hexID[:2], hexID[2:]), nil
}

func parseObjectID(id ObjectID) (string, error) {
	value := string(id)
	if !strings.HasPrefix(value, objectIDPrefix) {
		return "", fmt.Errorf("invalid object ID %q", value)
	}
	hexID := strings.TrimPrefix(value, objectIDPrefix)
	if len(hexID) != sha256.Size*2 {
		return "", fmt.Errorf("invalid object ID %q", value)
	}
	if _, err := hex.DecodeString(hexID); err != nil {
		return "", fmt.Errorf("invalid object ID %q: %w", value, err)
	}
	return hexID, nil
}

func hashObject(data []byte) ObjectID {
	sum := sha256.Sum256(data)
	return ObjectID(objectIDPrefix + hex.EncodeToString(sum[:]))
}
