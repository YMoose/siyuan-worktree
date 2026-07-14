package worktree

import (
	"testing"

	"siyuan-worktree/internal/siyuan"
)

func TestBuildDocumentPathsMapsParentsAndDuplicateTitles(t *testing.T) {
	notebooks := []siyuan.Notebook{{ID: "20260714100000-nbook01", Name: "Work"}}
	parent := &DocumentNode{
		Document: siyuan.Document{ID: "20260714110000-parent1", Name: "Project"},
		Children: []*DocumentNode{
			{Document: siyuan.Document{ID: "20260714110100-child01", Name: "Design"}},
			{Document: siyuan.Document{ID: "20260714110200-child02", Name: "Design"}},
		},
	}
	paths := BuildDocumentPaths(notebooks, map[string][]*DocumentNode{notebooks[0].ID: {parent}})
	if got := paths[parent.ID]; got != "Work/Project/_index.md" {
		t.Fatalf("parent path = %q", got)
	}
	if got := paths[parent.Children[0].ID]; got != "Work/Project/Design--child01.md" {
		t.Fatalf("first duplicate path = %q", got)
	}
	if got := paths[parent.Children[1].ID]; got != "Work/Project/Design--child02.md" {
		t.Fatalf("second duplicate path = %q", got)
	}
}
