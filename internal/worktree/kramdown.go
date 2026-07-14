package worktree

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var blockIDPattern = regexp.MustCompile(`\{:[^}\n]*\bid="(\d{14}-[a-z0-9]{7})"[^}\n]*}`)
var volatileUpdatedAttrPattern = regexp.MustCompile(`\s+updated="[^"]*"`)
var generatedBlockIdentityAttrPattern = regexp.MustCompile(`\s+(?:id|updated)="[^"]*"`)
var ialAttrPattern = regexp.MustCompile(`([A-Za-z0-9_-]+)=("(?:\\.|[^"\\])*")`)

func Canonicalize(content string) string {
	normalized := strings.ReplaceAll(strings.ReplaceAll(content, "\r\n", "\n"), "\r", "\n")
	normalized = strings.TrimRight(normalized, "\n")
	if normalized == "" {
		return ""
	}
	return normalized + "\n"
}

func HashContent(content string) string {
	sum := sha256.Sum256([]byte(Canonicalize(content)))
	return hex.EncodeToString(sum[:])
}

func EquivalentBlockContent(expected, actual string) bool {
	return HashContent(stripVolatileIALAttrs(expected)) == HashContent(stripVolatileIALAttrs(actual))
}

func stripVolatileIALAttrs(content string) string {
	lines := strings.Split(Canonicalize(content), "\n")
	for index, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "{:") && strings.HasSuffix(trimmed, "}") {
			lines[index] = volatileUpdatedAttrPattern.ReplaceAllString(line, "")
		}
	}
	return strings.Join(lines, "\n")
}

func stripGeneratedBlockIdentity(content string) string {
	lines := strings.Split(Canonicalize(content), "\n")
	result := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "{:") && strings.HasSuffix(trimmed, "}") {
			line = generatedBlockIdentityAttrPattern.ReplaceAllString(line, "")
			remaining := strings.TrimSpace(line)
			if remaining == "{:}" || remaining == "{: }" {
				continue
			}
		}
		result = append(result, line)
	}
	return Canonicalize(strings.Join(result, "\n"))
}

func ExtractBlockIDs(content string) []string {
	matches := blockIDPattern.FindAllStringSubmatch(content, -1)
	ids := make([]string, 0, len(matches))
	for _, match := range matches {
		if len(match) == 2 {
			ids = append(ids, match[1])
		}
	}
	return ids
}

func ValidateUniqueBlockIDs(content string) error {
	if duplicates := duplicateIDs(ExtractBlockIDs(content)); len(duplicates) > 0 {
		return fmt.Errorf("duplicate SiYuan block IDs: %s", strings.Join(duplicates, ", "))
	}
	return nil
}

func ValidateReadOnlyBlockAttrs(base, local string) error {
	baseAttrs, err := extractBlockIALAttrs(base)
	if err != nil {
		return fmt.Errorf("parse base block attributes: %w", err)
	}
	localAttrs, err := extractBlockIALAttrs(local)
	if err != nil {
		return fmt.Errorf("parse local block attributes: %w", err)
	}
	for blockID, actual := range localAttrs {
		expected, exists := baseAttrs[blockID]
		if !exists {
			return fmt.Errorf("block %s introduces an unknown SiYuan block ID", blockID)
		}
		if changed := changedAttrNames(expected, actual); len(changed) > 0 {
			return fmt.Errorf("attributes of block %s are read-only; changed: %s", blockID, strings.Join(changed, ", "))
		}
	}
	return nil
}

func extractBlockIALAttrs(content string) (map[string]map[string]string, error) {
	result := map[string]map[string]string{}
	for lineNumber, line := range strings.Split(Canonicalize(content), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "{:") || !strings.HasSuffix(trimmed, "}") {
			continue
		}
		attrs := map[string]string{}
		for _, match := range ialAttrPattern.FindAllStringSubmatch(trimmed, -1) {
			value, err := strconv.Unquote(match[2])
			if err != nil {
				return nil, fmt.Errorf("invalid IAL value at line %d: %w", lineNumber+1, err)
			}
			attrs[match[1]] = value
		}
		blockID := attrs["id"]
		if blockID == "" {
			continue
		}
		if _, exists := result[blockID]; exists {
			return nil, fmt.Errorf("duplicate block IAL %s", blockID)
		}
		result[blockID] = attrs
	}
	return result, nil
}

func changedAttrNames(expected, actual map[string]string) []string {
	names := map[string]bool{}
	for name, expectedValue := range expected {
		if actualValue, exists := actual[name]; !exists || actualValue != expectedValue {
			names[name] = true
		}
	}
	for name, actualValue := range actual {
		if expectedValue, exists := expected[name]; !exists || expectedValue != actualValue {
			names[name] = true
		}
	}
	result := make([]string, 0, len(names))
	for name := range names {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

func duplicateIDs(ids []string) []string {
	seen := map[string]bool{}
	duplicates := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			duplicates[id] = true
		}
		seen[id] = true
	}
	result := make([]string, 0, len(duplicates))
	for id := range duplicates {
		result = append(result, id)
	}
	sort.Strings(result)
	return result
}
