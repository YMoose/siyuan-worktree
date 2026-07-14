package worktree

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const markerNamespace = "siyuan-worktree:block"

var startMarkerPattern = regexp.MustCompile(`^<!-- siyuan-worktree:block (\d{14}-[a-z0-9]{7})(?: type=([a-z_]+))? -->$`)

type AnnotatedBlock struct {
	ID      string
	Type    string
	Content string
}

type AnnotatedDocument struct {
	Blocks []AnnotatedBlock
}

type EditableEntry struct {
	Block      *AnnotatedBlock
	NewContent string
}

type EditableDocument struct {
	Entries []EditableEntry
}

func RenderAnnotated(document AnnotatedDocument) string {
	if len(document.Blocks) == 0 {
		return ""
	}
	var output strings.Builder
	for index, block := range document.Blocks {
		if index > 0 {
			output.WriteByte('\n')
		}
		fmt.Fprintf(&output, "<!-- %s %s", markerNamespace, block.ID)
		if block.Type != "" {
			fmt.Fprintf(&output, " type=%s", block.Type)
		}
		output.WriteString(" -->\n")
		output.WriteString(Canonicalize(block.Content))
		fmt.Fprintf(&output, "<!-- /%s %s -->\n", markerNamespace, block.ID)
	}
	return output.String()
}

func ParseAnnotated(content string) (AnnotatedDocument, error) {
	editable, err := ParseEditable(content)
	if err != nil {
		return AnnotatedDocument{}, err
	}
	document := AnnotatedDocument{Blocks: []AnnotatedBlock{}}
	for _, entry := range editable.Entries {
		if entry.Block == nil {
			return AnnotatedDocument{}, fmt.Errorf("untracked Markdown content is not valid in a canonical document")
		}
		document.Blocks = append(document.Blocks, *entry.Block)
	}
	return document, nil
}

func ParseEditable(content string) (EditableDocument, error) {
	canonical := Canonicalize(content)
	if canonical == "" {
		return EditableDocument{Entries: []EditableEntry{}}, nil
	}
	lines := strings.Split(canonical, "\n")
	document := EditableDocument{Entries: []EditableEntry{}}
	seen := map[string]bool{}
	var untracked []string
	flushUntracked := func() {
		content := Canonicalize(strings.Join(untracked, "\n"))
		untracked = nil
		if strings.TrimSpace(content) != "" {
			document.Entries = append(document.Entries, EditableEntry{NewContent: content})
		}
	}
	for index := 0; index < len(lines); {
		line := lines[index]
		match := startMarkerPattern.FindStringSubmatch(line)
		if len(match) == 0 {
			if strings.Contains(line, markerNamespace) {
				return EditableDocument{}, fmt.Errorf("malformed %s marker at line %d", markerNamespace, index+1)
			}
			untracked = append(untracked, line)
			index++
			continue
		}
		flushUntracked()
		id := match[1]
		blockType := match[2]
		if seen[id] {
			return EditableDocument{}, fmt.Errorf("duplicate top-level block marker %s", id)
		}
		seen[id] = true
		endMarker := fmt.Sprintf("<!-- /%s %s -->", markerNamespace, id)
		start := index + 1
		end := start
		for end < len(lines) && lines[end] != endMarker {
			if startMarkerPattern.MatchString(lines[end]) {
				return EditableDocument{}, fmt.Errorf("nested block marker before closing %s", id)
			}
			end++
		}
		if end >= len(lines) {
			return EditableDocument{}, fmt.Errorf("missing closing marker for block %s", id)
		}
		blockContent := Canonicalize(strings.Join(lines[start:end], "\n"))
		if blockContent == "" {
			return EditableDocument{}, fmt.Errorf("block %s is empty", id)
		}
		if !containsID(ExtractBlockIDs(blockContent), id) {
			return EditableDocument{}, fmt.Errorf("block %s content no longer contains its SiYuan IAL", id)
		}
		block := AnnotatedBlock{ID: id, Type: blockType, Content: blockContent}
		document.Entries = append(document.Entries, EditableEntry{Block: &block})
		index = end + 1
	}
	flushUntracked()
	return document, nil
}

func (d AnnotatedDocument) BlockMap() map[string]AnnotatedBlock {
	result := make(map[string]AnnotatedBlock, len(d.Blocks))
	for _, block := range d.Blocks {
		result[block.ID] = block
	}
	return result
}

func replaceAnnotatedBlock(document *AnnotatedDocument, id, content string) error {
	for index := range document.Blocks {
		if document.Blocks[index].ID == id {
			document.Blocks[index].Content = Canonicalize(content)
			return nil
		}
	}
	return errors.New("annotated block not found: " + id)
}

func containsID(ids []string, expected string) bool {
	for _, id := range ids {
		if id == expected {
			return true
		}
	}
	return false
}
