package worktree

import (
	"fmt"
	"sort"
	"time"

	"siyuan-worktree/internal/siyuan"
)

const (
	blockSnapshotObjectType  = "block-snapshot"
	documentTreeObjectType   = "document-tree"
	workspaceTreeObjectType  = "workspace-tree"
	commitObjectType         = "commit"
	patchObjectType          = "patch"
	pushOperationObjectType  = "push-operation"
	pullOperationObjectType  = "pull-operation"
	workingFileObjectType    = "working-file"
	workingTreeObjectType    = "working-tree"
	snapshotObjectVersion    = 3
	repositoryRefsVersion    = 3
	baselineCommitObjectKind = "baseline"
	userCommitObjectKind     = "user"
)

type BlockSnapshot struct {
	BlockID        string                       `json:"blockId"`
	BlockType      string                       `json:"blockType"`
	Kramdown       string                       `json:"kramdown"`
	AttrsByBlockID map[string]map[string]string `json:"attrsByBlockId"`
	Provisional    bool                         `json:"provisional,omitempty"`
}

type BlockSnapshotRef struct {
	BlockID  string   `json:"blockId"`
	ObjectID ObjectID `json:"objectId"`
}

type DocumentTree struct {
	DocumentID string             `json:"documentId"`
	Blocks     []BlockSnapshotRef `json:"blocks"`
	Markdown   string             `json:"markdown,omitempty"`
}

type WorkspaceNotebook struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Sort int    `json:"sort"`
}

type WorkspaceDocument struct {
	ID             string   `json:"id"`
	NotebookID     string   `json:"notebookId"`
	NotebookName   string   `json:"notebookName"`
	Title          string   `json:"title"`
	RemotePath     string   `json:"remotePath"`
	LocalPath      string   `json:"localPath"`
	DocumentTreeID ObjectID `json:"documentTreeId"`
}

type WorkspaceTree struct {
	AllOpenNotebooks  bool                `json:"allOpenNotebooks"`
	SelectedNotebooks []string            `json:"selectedNotebooks"`
	Notebooks         []WorkspaceNotebook `json:"notebooks"`
	Documents         []WorkspaceDocument `json:"documents"`
}

type CommitObject struct {
	Kind       string    `json:"kind"`
	DisplayID  string    `json:"displayId,omitempty"`
	Tree       ObjectID  `json:"tree"`
	BaseHead   ObjectID  `json:"baseHead,omitempty"`
	RemoteBase ObjectID  `json:"remoteBase"`
	Patch      ObjectID  `json:"patch,omitempty"`
	Message    string    `json:"message"`
	CreatedAt  time.Time `json:"createdAt"`
}

func StoreDocumentTree(store *ObjectStore, documentID string, document AnnotatedDocument) (ObjectID, error) {
	refs := make([]BlockSnapshotRef, 0, len(document.Blocks))
	for _, block := range document.Blocks {
		attrs, err := extractBlockIALAttrs(block.Content)
		if err != nil {
			return "", fmt.Errorf("snapshot block %s attributes: %w", block.ID, err)
		}
		blockID, err := store.Put(blockSnapshotObjectType, snapshotObjectVersion, BlockSnapshot{
			BlockID:        block.ID,
			BlockType:      block.Type,
			Kramdown:       Canonicalize(block.Content),
			AttrsByBlockID: attrs,
		})
		if err != nil {
			return "", err
		}
		refs = append(refs, BlockSnapshotRef{BlockID: block.ID, ObjectID: blockID})
	}
	return store.Put(documentTreeObjectType, snapshotObjectVersion, DocumentTree{
		DocumentID: documentID,
		Blocks:     refs,
		Markdown:   RenderAnnotated(document),
	})
}

func StoreWorkspaceTree(
	store *ObjectStore,
	configuredNotebookIDs []string,
	notebooks []siyuan.Notebook,
	documents []WorkspaceDocument,
) (ObjectID, error) {
	selected := append([]string(nil), configuredNotebookIDs...)
	sort.Strings(selected)
	notebookSnapshots := make([]WorkspaceNotebook, 0, len(notebooks))
	for _, notebook := range notebooks {
		notebookSnapshots = append(notebookSnapshots, WorkspaceNotebook{ID: notebook.ID, Name: notebook.Name, Sort: notebook.Sort})
	}
	sort.Slice(notebookSnapshots, func(i, j int) bool { return notebookSnapshots[i].ID < notebookSnapshots[j].ID })
	documents = append([]WorkspaceDocument(nil), documents...)
	sort.Slice(documents, func(i, j int) bool { return documents[i].ID < documents[j].ID })
	return store.Put(workspaceTreeObjectType, snapshotObjectVersion, WorkspaceTree{
		AllOpenNotebooks:  len(configuredNotebookIDs) == 0,
		SelectedNotebooks: selected,
		Notebooks:         notebookSnapshots,
		Documents:         documents,
	})
}
