package worktree

import (
	"fmt"
	"strings"
)

type Diff struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func FormatSimpleDiff(base, local, label string) string {
	before := strings.Split(Canonicalize(base), "\n")
	after := strings.Split(Canonicalize(local), "\n")
	prefix := 0
	for prefix < len(before) && prefix < len(after) && before[prefix] == after[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(before)-prefix && suffix < len(after)-prefix && before[len(before)-1-suffix] == after[len(after)-1-suffix] {
		suffix++
	}
	start := prefix - 3
	if start < 0 {
		start = 0
	}
	var output strings.Builder
	fmt.Fprintf(&output, "--- baseline/%s\n+++ local/%s\n", label, label)
	for _, line := range before[start:prefix] {
		fmt.Fprintf(&output, " %s\n", line)
	}
	for _, line := range before[prefix : len(before)-suffix] {
		fmt.Fprintf(&output, "-%s\n", line)
	}
	for _, line := range after[prefix : len(after)-suffix] {
		fmt.Fprintf(&output, "+%s\n", line)
	}
	end := len(before) - suffix + 3
	if end > len(before) {
		end = len(before)
	}
	for _, line := range before[len(before)-suffix : end] {
		fmt.Fprintf(&output, " %s\n", line)
	}
	return strings.TrimRight(output.String(), "\n")
}
