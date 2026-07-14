package worktree

import (
	"errors"
	"fmt"
	"os"
	"sort"
)

const stableFileReadAttempts = 3

type WorkingFileContent struct {
	Markdown string `json:"markdown"`
}

type WorkingFileRecord struct {
	DocumentID         string   `json:"documentId"`
	LocalPath          string   `json:"localPath"`
	Missing            bool     `json:"missing,omitempty"`
	Size               int64    `json:"size,omitempty"`
	ModifiedAtUnixNano int64    `json:"modifiedAtUnixNano,omitempty"`
	ContentHash        string   `json:"contentHash,omitempty"`
	ContentObject      ObjectID `json:"contentObject,omitempty"`
}

type WorkingTreeSnapshot struct {
	Files []WorkingFileRecord `json:"files"`
}

type workingTreeScan struct {
	Snapshot WorkingTreeSnapshot
	contents map[string]string
}

func (s *Syncer) scanWorkingTree(state State) (workingTreeScan, error) {
	scan := workingTreeScan{
		Snapshot: WorkingTreeSnapshot{Files: make([]WorkingFileRecord, 0, len(state.Documents))},
		contents: make(map[string]string, len(state.Documents)),
	}
	for _, document := range sortedStateDocuments(state) {
		absolute, err := s.localAbsolutePath(document.LocalPath)
		if err != nil {
			return workingTreeScan{}, err
		}
		content, info, missing, err := readStableWorkingFile(absolute)
		if err != nil {
			return workingTreeScan{}, fmt.Errorf("read stable working file %s: %w", document.LocalPath, err)
		}
		record := WorkingFileRecord{DocumentID: document.ID, LocalPath: document.LocalPath, Missing: missing}
		if !missing {
			record.Size = info.Size()
			record.ModifiedAtUnixNano = info.ModTime().UnixNano()
			record.ContentHash = HashContent(content)
			scan.contents[document.ID] = Canonicalize(content)
		}
		scan.Snapshot.Files = append(scan.Snapshot.Files, record)
	}
	sort.Slice(scan.Snapshot.Files, func(i, j int) bool {
		return scan.Snapshot.Files[i].DocumentID < scan.Snapshot.Files[j].DocumentID
	})
	return scan, nil
}

func readStableWorkingFile(path string) (content string, info os.FileInfo, missing bool, err error) {
	for attempt := 0; attempt < stableFileReadAttempts; attempt++ {
		before, statErr := os.Stat(path)
		if errors.Is(statErr, os.ErrNotExist) {
			return "", nil, true, nil
		}
		if statErr != nil {
			return "", nil, false, statErr
		}
		if before.IsDir() {
			return "", nil, false, errors.New("mapped Markdown path is a directory")
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			if errors.Is(readErr, os.ErrNotExist) {
				continue
			}
			return "", nil, false, readErr
		}
		after, statErr := os.Stat(path)
		if errors.Is(statErr, os.ErrNotExist) {
			continue
		}
		if statErr != nil {
			return "", nil, false, statErr
		}
		if os.SameFile(before, after) && before.Size() == after.Size() && before.ModTime().Equal(after.ModTime()) && int64(len(data)) == after.Size() {
			return string(data), after, false, nil
		}
	}
	return "", nil, false, errors.New("file changed while it was being read")
}

func (scan workingTreeScan) record(documentID string) (WorkingFileRecord, bool) {
	for _, record := range scan.Snapshot.Files {
		if record.DocumentID == documentID {
			return record, true
		}
	}
	return WorkingFileRecord{}, false
}

func (scan workingTreeScan) content(documentID string) (string, bool) {
	content, ok := scan.contents[documentID]
	return content, ok
}

func persistWorkingTreeSnapshot(store *ObjectStore, scan workingTreeScan) (ObjectID, WorkingTreeSnapshot, error) {
	snapshot := WorkingTreeSnapshot{Files: append([]WorkingFileRecord(nil), scan.Snapshot.Files...)}
	for index := range snapshot.Files {
		record := &snapshot.Files[index]
		if record.Missing {
			continue
		}
		content, ok := scan.contents[record.DocumentID]
		if !ok {
			return "", WorkingTreeSnapshot{}, fmt.Errorf("working file %s has no scanned content", record.LocalPath)
		}
		contentID, err := store.Put(workingFileObjectType, snapshotObjectVersion, WorkingFileContent{Markdown: Canonicalize(content)})
		if err != nil {
			return "", WorkingTreeSnapshot{}, err
		}
		record.ContentObject = contentID
	}
	id, err := store.Put(workingTreeObjectType, snapshotObjectVersion, snapshot)
	return id, snapshot, err
}

func LoadWorkingTreeSnapshot(store *ObjectStore, id ObjectID) (WorkingTreeSnapshot, error) {
	var snapshot WorkingTreeSnapshot
	if err := store.Get(id, workingTreeObjectType, snapshotObjectVersion, &snapshot); err != nil {
		return WorkingTreeSnapshot{}, err
	}
	seen := make(map[string]bool, len(snapshot.Files))
	for _, record := range snapshot.Files {
		if record.DocumentID == "" || record.LocalPath == "" || seen[record.DocumentID] {
			return WorkingTreeSnapshot{}, errors.New("working tree snapshot contains an invalid or duplicate document")
		}
		seen[record.DocumentID] = true
		if record.Missing {
			if record.ContentObject != "" || record.ContentHash != "" {
				return WorkingTreeSnapshot{}, fmt.Errorf("missing working file %s contains content", record.LocalPath)
			}
			continue
		}
		content, err := LoadWorkingFileContent(store, record.ContentObject)
		if err != nil {
			return WorkingTreeSnapshot{}, err
		}
		if HashContent(content) != record.ContentHash {
			return WorkingTreeSnapshot{}, fmt.Errorf("working file %s content hash does not match its object", record.LocalPath)
		}
	}
	return snapshot, nil
}

func LoadWorkingFileContent(store *ObjectStore, id ObjectID) (string, error) {
	if id == "" {
		return "", errors.New("working file content object is empty")
	}
	var content WorkingFileContent
	if err := store.Get(id, workingFileObjectType, snapshotObjectVersion, &content); err != nil {
		return "", err
	}
	return Canonicalize(content.Markdown), nil
}

func WorkingFileByDocumentID(snapshot WorkingTreeSnapshot, documentID string) (WorkingFileRecord, bool) {
	for _, record := range snapshot.Files {
		if record.DocumentID == documentID {
			return record, true
		}
	}
	return WorkingFileRecord{}, false
}
