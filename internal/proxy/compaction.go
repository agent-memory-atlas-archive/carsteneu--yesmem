package proxy

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ArchiveLearning holds a learning for archive block injection.
type ArchiveLearning struct {
	Category  string
	Content   string
	CreatedAt time.Time
}

type compactionStats struct {
	FileStats    string
	ToolStats    string
	Decisions    string
	PivotTexts   string
	Digests      []string               // fallback timeline (when no flavors available)
	SessionStart time.Time              // for temporal annotation
	SessionEnd   time.Time              // for temporal annotation
	Learnings    []ArchiveLearning      // for metamemory (gotchas, unfinished, etc.)
	Flavors      []ArchiveSessionFlavor // session summaries from extraction
	Commits      []GitCommit            // git commits with hash
}

// extractStatsFromMessages extracts file and tool usage stats from a message range.
func extractStatsFromMessages(messages []any, start, end int) compactionStats {
	toolCounts := make(map[string]int)
	fileCounts := make(map[string]int)

	for i := start; i <= end && i < len(messages); i++ {
		msg, ok := messages[i].(map[string]any)
		if !ok {
			continue
		}

		blocks, ok := msg["content"].([]any)
		if !ok {
			continue
		}

		for _, block := range blocks {
			b, ok := block.(map[string]any)
			if !ok {
				continue
			}

			typ, _ := b["type"].(string)
			if typ == "tool_use" {
				name, _ := b["name"].(string)
				if name != "" {
					toolCounts[name]++
				}
				// Extract file paths from input
				if input, ok := b["input"].(map[string]any); ok {
					for _, key := range []string{"file_path", "path", "command"} {
						if v, ok := input[key].(string); ok && v != "" {
							if key == "command" {
								// Skip commands, they're not file paths
								continue
							}
							// Use basename for brevity
							fileCounts[filepath.Base(v)]++
						}
					}
				}
			}
		}
	}

	return compactionStats{
		FileStats: formatCounts(fileCounts),
		ToolStats: formatCounts(toolCounts),
	}
}

// formatCounts turns a map into "key1(3), key2(1)" sorted by count desc.
func formatCounts(counts map[string]int) string {
	if len(counts) == 0 {
		return ""
	}

	type kv struct {
		key   string
		count int
	}
	var pairs []kv
	for k, v := range counts {
		pairs = append(pairs, kv{k, v})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].count != pairs[j].count {
			return pairs[i].count > pairs[j].count
		}
		return pairs[i].key < pairs[j].key // deterministic secondary sort
	})

	// Top-5 to keep compacted blocks concise (~500 tokens budget)
	limit := 5
	if len(pairs) < limit {
		limit = len(pairs)
	}

	var parts []string
	for _, p := range pairs[:limit] {
		parts = append(parts, fmt.Sprintf("%s(%d)", p.key, p.count))
	}
	if len(pairs) > 5 {
		parts = append(parts, fmt.Sprintf("+%d more", len(pairs)-5))
	}
	return strings.Join(parts, ", ")
}
