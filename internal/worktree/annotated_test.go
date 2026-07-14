package worktree

import "testing"

func TestParseAnnotatedRejectsUnmarkedContent(t *testing.T) {
	if _, err := ParseAnnotated("ordinary markdown\n"); err == nil {
		t.Fatal("expected unmarked content to be rejected")
	}
}
