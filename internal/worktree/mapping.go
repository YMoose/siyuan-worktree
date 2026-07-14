package worktree

import (
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"siyuan-worktree/internal/siyuan"
)

type DocumentNode struct {
	siyuan.Document
	NotebookID   string
	NotebookName string
	Children     []*DocumentNode
}

var (
	invalidPathCharacters = regexp.MustCompile(`[<>:"/\\|?*\x00-\x1f]`)
	whitespace            = regexp.MustCompile(`\s+`)
	windowsReserved       = regexp.MustCompile(`(?i)^(con|prn|aux|nul|com[1-9]|lpt[1-9])(\..*)?$`)
)

func BuildDocumentPaths(notebooks []siyuan.Notebook, documents map[string][]*DocumentNode) map[string]string {
	result := map[string]string{}
	notebookItems := make([]namedID, 0, len(notebooks))
	for _, notebook := range notebooks {
		notebookItems = append(notebookItems, namedID{ID: notebook.ID, Name: SanitizeSegment(notebook.Name, notebook.ID)})
	}
	notebookNames := allocateNames(notebookItems)
	for _, notebook := range notebooks {
		assignDocumentPaths(documents[notebook.ID], notebookNames[notebook.ID], result)
	}
	return result
}

func SanitizeSegment(value, fallback string) string {
	sanitized := invalidPathCharacters.ReplaceAllString(value, "_")
	sanitized = whitespace.ReplaceAllString(sanitized, " ")
	sanitized = strings.TrimSpace(strings.TrimRight(sanitized, ". "))
	if sanitized == "" || sanitized == "." || sanitized == ".." {
		sanitized = fallback
	}
	if windowsReserved.MatchString(sanitized) {
		sanitized = "_" + sanitized
	}
	return truncateRunes(sanitized, 120)
}

func assignDocumentPaths(documents []*DocumentNode, parent string, result map[string]string) {
	items := make([]namedID, 0, len(documents))
	for _, document := range documents {
		name := SanitizeSegment(document.Name, document.ID)
		if strings.EqualFold(name, "_index") {
			name += "--" + shortID(document.ID)
		}
		items = append(items, namedID{ID: document.ID, Name: name})
	}
	names := allocateNames(items)
	for _, document := range documents {
		name := names[document.ID]
		if len(document.Children) > 0 {
			directory := path.Join(parent, name)
			result[document.ID] = path.Join(directory, "_index.md")
			assignDocumentPaths(document.Children, directory, result)
		} else {
			result[document.ID] = path.Join(parent, name+".md")
		}
	}
}

type namedID struct {
	ID   string
	Name string
}

func allocateNames(items []namedID) map[string]string {
	counts := map[string]int{}
	for _, item := range items {
		counts[strings.ToLower(item.Name)]++
	}
	result := map[string]string{}
	for _, item := range items {
		name := item.Name
		if counts[strings.ToLower(name)] > 1 {
			name += "--" + shortID(item.ID)
		}
		result[item.ID] = name
	}
	return result
}

func SortNodes(nodes []*DocumentNode) {
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].Sort != nodes[j].Sort {
			return nodes[i].Sort < nodes[j].Sort
		}
		if nodes[i].Name != nodes[j].Name {
			return nodes[i].Name < nodes[j].Name
		}
		return nodes[i].ID < nodes[j].ID
	})
}

func shortID(id string) string {
	if len(id) <= 7 {
		return id
	}
	return id[len(id)-7:]
}

func truncateRunes(value string, maximum int) string {
	if utf8.RuneCountInString(value) <= maximum {
		return value
	}
	runes := []rune(value)
	return strings.TrimRight(string(runes[:maximum]), ". ")
}
