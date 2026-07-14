package worktree

import (
	"strings"
	"testing"
)

func TestCanonicalizePreservesMarkdownLineBreakSpaces(t *testing.T) {
	input := "line with break  \r\nnext\r\n\r\n"
	got := Canonicalize(input)
	want := "line with break  \nnext\n"
	if got != want {
		t.Fatalf("Canonicalize() = %q, want %q", got, want)
	}
}

func TestValidateUniqueBlockIDsRejectsDuplicates(t *testing.T) {
	duplicate := strings.Repeat("x\n{: id=\"20260714120000-abcdefg\"}\n", 2)
	if err := ValidateUniqueBlockIDs(duplicate); err == nil {
		t.Fatal("expected duplicate block ID rejection")
	}
}

func TestValidateReadOnlyBlockAttrs(t *testing.T) {
	base := "text\n{: id=\"20260714120000-block01\" custom-owner=\"agent\" updated=\"20260714120000\"}\n"
	if err := ValidateReadOnlyBlockAttrs(base, "changed\n{: updated=\"20260714120000\" custom-owner=\"agent\" id=\"20260714120000-block01\"}\n"); err != nil {
		t.Fatalf("attribute reordering should be allowed: %v", err)
	}
	if err := ValidateReadOnlyBlockAttrs(base, "changed\n{: id=\"20260714120000-block01\" custom-owner=\"local\" updated=\"20260714120000\"}\n"); err == nil || !strings.Contains(err.Error(), "custom-owner") {
		t.Fatalf("expected read-only attribute rejection, got %v", err)
	}
	if err := ValidateReadOnlyBlockAttrs(base, "changed\n{: id=\"20260714120000-unknown\"}\n"); err == nil || !strings.Contains(err.Error(), "unknown SiYuan block ID") {
		t.Fatalf("expected unknown block ID rejection, got %v", err)
	}
}
