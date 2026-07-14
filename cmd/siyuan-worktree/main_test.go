package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"siyuan-worktree/internal/config"
	"siyuan-worktree/internal/siyuan"
	"siyuan-worktree/internal/worktree"
)

func TestRunCloneInitializesAndPullsFromURL(t *testing.T) {
	previousFactory := newSiYuanAPI
	defer func() { newSiYuanAPI = previousFactory }()
	var endpoint string
	newSiYuanAPI = func(value, _ string) siyuan.API {
		endpoint = value
		return emptyAPI{}
	}

	destination := filepath.Join(t.TempDir(), "clone")
	remote := "http://127.0.0.1:6806"
	if err := runClone([]string{remote, destination}); err != nil {
		t.Fatal(err)
	}
	if endpoint != remote {
		t.Fatalf("client endpoint = %q", endpoint)
	}
	cfg, err := config.Load(destination)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Endpoint != remote {
		t.Fatalf("endpoint = %q", cfg.Endpoint)
	}
	state, err := worktree.LoadState(destination)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Documents) != 0 || state.LastSyncAt == nil {
		t.Fatalf("state = %+v", state)
	}
	if _, err := os.Stat(filepath.Join(destination, config.MetadataDir, "lock")); !os.IsNotExist(err) {
		t.Fatalf("clone lock was not released: %v", err)
	}
}

type emptyAPI struct{}

func (emptyAPI) ListNotebooks(context.Context) ([]siyuan.Notebook, error) {
	return []siyuan.Notebook{}, nil
}

func (emptyAPI) ListDocuments(context.Context, string, string) ([]siyuan.Document, error) {
	return []siyuan.Document{}, nil
}

func (emptyAPI) GetChildBlocks(context.Context, string) ([]siyuan.ChildBlock, error) {
	return []siyuan.ChildBlock{}, nil
}

func (emptyAPI) GetBlockKramdown(context.Context, string) (string, error) {
	return "", nil
}

func (emptyAPI) GetBlockKramdowns(context.Context, []string) (map[string]string, error) {
	return map[string]string{}, nil
}

func (emptyAPI) BatchGetBlockAttrs(context.Context, []string) (map[string]map[string]string, error) {
	return map[string]map[string]string{}, nil
}

func (emptyAPI) CreateDocHistory(context.Context, string) error {
	return nil
}

func (emptyAPI) SearchHistory(context.Context, string) (siyuan.HistorySearchResult, error) {
	return siyuan.HistorySearchResult{}, nil
}

func (emptyAPI) InsertBlock(context.Context, string, string, string, string) (siyuan.MutationReceipt, error) {
	return siyuan.MutationReceipt{}, nil
}

func (emptyAPI) DeleteBlock(context.Context, string) (siyuan.MutationReceipt, error) {
	return siyuan.MutationReceipt{}, nil
}

func (emptyAPI) UpdateBlock(context.Context, string, string) (siyuan.MutationReceipt, error) {
	return siyuan.MutationReceipt{}, nil
}

func TestDefaultCloneDirectory(t *testing.T) {
	tests := map[string]string{
		"http://127.0.0.1:6806":        "127.0.0.1-6806",
		"https://notes.example.com/":   "notes.example.com",
		"https://notes.example.com/si": "si",
	}
	for rawURL, expected := range tests {
		actual, err := defaultCloneDirectory(rawURL)
		if err != nil {
			t.Fatalf("%s: %v", rawURL, err)
		}
		if actual != expected {
			t.Fatalf("%s: directory = %q, want %q", rawURL, actual, expected)
		}
	}
	if _, err := defaultCloneDirectory("127.0.0.1:6806"); err == nil {
		t.Fatal("expected a relative URL to be rejected")
	}
}

func TestCloneRejectsNonEmptyDestination(t *testing.T) {
	destination := t.TempDir()
	if err := os.WriteFile(filepath.Join(destination, "keep.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runClone([]string{"http://127.0.0.1:6806", destination}); err == nil {
		t.Fatal("expected a non-empty clone destination to be rejected")
	}
	if data, err := os.ReadFile(filepath.Join(destination, "keep.txt")); err != nil || string(data) != "keep" {
		t.Fatalf("existing destination content changed: %q, %v", data, err)
	}
}
