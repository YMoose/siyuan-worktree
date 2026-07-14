package worktree

import (
	"strings"
	"testing"
)

func TestBuildAndMergeUpdateDocumentPatch(t *testing.T) {
	baseDocument := AnnotatedDocument{Blocks: []AnnotatedBlock{
		{ID: "20260714120000-block01", Content: "first\n{: id=\"20260714120000-block01\"}\n"},
		{ID: "20260714120001-block02", Content: "second\n{: id=\"20260714120001-block02\"}\n"},
	}}
	localDocument := baseDocument
	localDocument.Blocks = append([]AnnotatedBlock(nil), baseDocument.Blocks...)
	localDocument.Blocks[0].Content = "first local\n{: id=\"20260714120000-block01\"}\n"
	patch, err := BuildDocumentPatch("doc", "doc.md", RenderAnnotated(baseDocument), RenderAnnotated(localDocument))
	if err != nil {
		t.Fatal(err)
	}
	if len(patch.Operations) != 1 || patch.Operations[0].BlockID != "20260714120000-block01" {
		t.Fatalf("patch = %+v", patch)
	}

	remoteDocument := baseDocument
	remoteDocument.Blocks = append([]AnnotatedBlock(nil), baseDocument.Blocks...)
	remoteDocument.Blocks[1].Content = "second remote\n{: id=\"20260714120001-block02\"}\n"
	merged, err := MergeDocumentPatch(patch, remoteDocument)
	if err != nil {
		t.Fatal(err)
	}
	if merged.Blocks[0].Content != localDocument.Blocks[0].Content || merged.Blocks[1].Content != remoteDocument.Blocks[1].Content {
		t.Fatalf("merged = %+v", merged)
	}
}

func TestMergeDocumentPatchRejectsSameBlockConflict(t *testing.T) {
	base := AnnotatedDocument{Blocks: []AnnotatedBlock{{
		ID: "20260714120000-block01", Content: "base\n{: id=\"20260714120000-block01\"}\n",
	}}}
	local := AnnotatedDocument{Blocks: []AnnotatedBlock{{
		ID: "20260714120000-block01", Content: "local\n{: id=\"20260714120000-block01\"}\n",
	}}}
	remote := AnnotatedDocument{Blocks: []AnnotatedBlock{{
		ID: "20260714120000-block01", Content: "remote\n{: id=\"20260714120000-block01\"}\n",
	}}}
	patch, err := BuildDocumentPatch("doc", "doc.md", RenderAnnotated(base), RenderAnnotated(local))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := MergeDocumentPatch(patch, remote); err == nil {
		t.Fatal("expected same-block conflict")
	}
}

func TestBuildDocumentPatchCreatesInsertOperationForUnmarkedMarkdown(t *testing.T) {
	base := AnnotatedDocument{Blocks: []AnnotatedBlock{{
		ID: "20260714120000-block01", Type: "p", Content: "base\n{: id=\"20260714120000-block01\"}\n",
	}}}
	patch, err := BuildDocumentPatch("doc", "doc.md", RenderAnnotated(base), RenderAnnotated(base)+"new paragraph\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(patch.Operations) != 1 {
		t.Fatalf("patch = %+v", patch)
	}
	operation := patch.Operations[0]
	if operation.Type != OperationInsert || operation.ParentID != "doc" || operation.PreviousID != base.Blocks[0].ID || operation.NextID != "" {
		t.Fatalf("insert operation = %+v", operation)
	}
	if operation.Content != "new paragraph\n" {
		t.Fatalf("insert content = %q", operation.Content)
	}
	if operation.OperationID == "" || operation.ContentHash != HashContent(operation.Content) {
		t.Fatalf("insert identity = %+v", operation)
	}
}

func TestBuildDocumentPatchCreatesStableOperationIDs(t *testing.T) {
	base := AnnotatedDocument{Blocks: []AnnotatedBlock{
		{ID: "20260714120000-block01", Type: "p", Content: "first\n{: id=\"20260714120000-block01\"}\n"},
		{ID: "20260714120001-block02", Type: "p", Content: "second\n{: id=\"20260714120001-block02\"}\n"},
	}}
	local := AnnotatedDocument{Blocks: append([]AnnotatedBlock(nil), base.Blocks...)}
	local.Blocks[0].Content = "changed\n{: id=\"20260714120000-block01\"}\n"
	localMarkdown := RenderAnnotated(local) + "inserted\n"
	first, err := BuildDocumentPatch("doc", "doc.md", RenderAnnotated(base), localMarkdown)
	if err != nil {
		t.Fatal(err)
	}
	second, err := BuildDocumentPatch("doc", "doc.md", RenderAnnotated(base), localMarkdown)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Operations) != 2 || len(second.Operations) != 2 {
		t.Fatalf("operations = %+v", first.Operations)
	}
	if first.Operations[0].OperationID != second.Operations[0].OperationID || first.Operations[1].OperationID != second.Operations[1].OperationID {
		t.Fatalf("operation IDs are not stable: %q/%q and %q/%q",
			first.Operations[0].OperationID, second.Operations[0].OperationID,
			first.Operations[1].OperationID, second.Operations[1].OperationID)
	}
	if first.Operations[0].OperationID == first.Operations[1].OperationID {
		t.Fatal("different operations must not share an operationId")
	}
	if err := ValidateDocumentPatchSafety(first, RenderAnnotated(base)); err != nil {
		t.Fatal(err)
	}
	tampered := first
	tampered.Operations = append([]Operation(nil), first.Operations...)
	tampered.Operations[0].Content = "tampered\n"
	if err := ValidateDocumentPatchSafety(tampered, RenderAnnotated(base)); err == nil || !strings.Contains(err.Error(), "operationId") {
		t.Fatalf("expected operationId mismatch, got %v", err)
	}
}

func TestBuildDocumentPatchCreatesSafeDeleteOperation(t *testing.T) {
	base := AnnotatedDocument{Blocks: []AnnotatedBlock{
		{ID: "20260714120000-block01", Type: "p", Content: "first\n{: id=\"20260714120000-block01\"}\n"},
		{ID: "20260714120001-block02", Type: "p", Content: "second\n{: id=\"20260714120001-block02\"}\n"},
	}}
	local := AnnotatedDocument{Blocks: append([]AnnotatedBlock(nil), base.Blocks[:1]...)}
	patch, err := BuildDocumentPatch("doc", "doc.md", RenderAnnotated(base), RenderAnnotated(local))
	if err != nil {
		t.Fatal(err)
	}
	if len(patch.Operations) != 1 || patch.Operations[0].Type != OperationDelete || patch.Operations[0].BlockID != base.Blocks[1].ID {
		t.Fatalf("patch = %+v", patch)
	}
	if err := ValidateDocumentPatchSafety(patch, RenderAnnotated(base)); err != nil {
		t.Fatalf("commit-approved deletion rejected: %v", err)
	}
}

func TestBuildDocumentPatchRejectsTopLevelReorder(t *testing.T) {
	base := AnnotatedDocument{Blocks: []AnnotatedBlock{
		{ID: "20260714120000-block01", Type: "p", Content: "first\n{: id=\"20260714120000-block01\"}\n"},
		{ID: "20260714120001-block02", Type: "p", Content: "second\n{: id=\"20260714120001-block02\"}\n"},
	}}
	local := AnnotatedDocument{Blocks: []AnnotatedBlock{base.Blocks[1], base.Blocks[0]}}
	if _, err := BuildDocumentPatch("doc", "doc.md", RenderAnnotated(base), RenderAnnotated(local)); err == nil || !strings.Contains(err.Error(), "reordering") {
		t.Fatalf("expected reorder rejection, got %v", err)
	}
}

func TestDocumentPatchRejectsReadOnlyBlockType(t *testing.T) {
	base := AnnotatedDocument{Blocks: []AnnotatedBlock{{
		ID: "20260714120000-block01", Type: "av", Content: "{{{row}}}\n{: id=\"20260714120000-block01\"}\n",
	}}}
	local := AnnotatedDocument{Blocks: append([]AnnotatedBlock(nil), base.Blocks...)}
	local.Blocks[0].Content = "changed\n{: id=\"20260714120000-block01\"}\n"
	patch, err := BuildDocumentPatch("doc", "doc.md", RenderAnnotated(base), RenderAnnotated(local))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateDocumentPatchSafety(patch, RenderAnnotated(base)); err == nil {
		t.Fatal("expected attribute-view block to be read-only")
	}
}

func TestDocumentPatchRejectsAttributeChanges(t *testing.T) {
	base := AnnotatedDocument{Blocks: []AnnotatedBlock{{
		ID: "20260714120000-block01", Type: "p", Content: "base\n{: id=\"20260714120000-block01\" custom-owner=\"agent\"}\n",
	}}}
	local := AnnotatedDocument{Blocks: append([]AnnotatedBlock(nil), base.Blocks...)}
	local.Blocks[0].Content = "changed\n{: id=\"20260714120000-block01\" custom-owner=\"local\"}\n"
	patch, err := BuildDocumentPatch("doc", "doc.md", RenderAnnotated(base), RenderAnnotated(local))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateDocumentPatchSafety(patch, RenderAnnotated(base)); err == nil || !strings.Contains(err.Error(), "attributes") {
		t.Fatalf("expected attribute change to be rejected, got %v", err)
	}
}

func TestEquivalentBlockContentIgnoresKernelUpdatedAttribute(t *testing.T) {
	expected := "text\n{: id=\"20260714120000-block01\" updated=\"20260714120000\"}\n"
	actual := "text\n{: id=\"20260714120000-block01\" updated=\"20260714130000\"}\n"
	if !EquivalentBlockContent(expected, actual) {
		t.Fatal("updated timestamp should not make canonical block content unequal")
	}
	if EquivalentBlockContent(expected, "different\n{: id=\"20260714120000-block01\" updated=\"20260714130000\"}\n") {
		t.Fatal("content changes must remain observable")
	}
}

func TestStripGeneratedBlockIdentityPreservesCustomAttributes(t *testing.T) {
	content := "text\n{: id=\"20260714120000-block01\" updated=\"20260714130000\" custom-owner=\"agent\"}\n"
	want := "text\n{: custom-owner=\"agent\"}\n"
	if got := stripGeneratedBlockIdentity(content); got != want {
		t.Fatalf("stripGeneratedBlockIdentity() = %q, want %q", got, want)
	}
	if got := stripGeneratedBlockIdentity("text\n{: id=\"20260714120000-block01\" updated=\"20260714130000\"}\n"); got != "text\n" {
		t.Fatalf("identity-only IAL was not removed: %q", got)
	}
}
