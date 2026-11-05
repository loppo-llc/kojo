package agent

import (
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const (
	agyRulesFilePrefix  = "kojo-system-"
	agyRulesFrontmatter = "---\ntrigger: always_on\n---\n\n"
	// agyRuleChunkBytes keeps each file, frontmatter included,
	// well below agy's 24,000-byte per-file cap.
	agyRuleChunkBytes = 23000
)

func agyRulesDir(agentDirPath string) string {
	return filepath.Join(agentDirPath, ".agents", "rules")
}

// chunkAgySystemPrompt splits prompt into pieces of at most
// limit bytes. It cuts on line boundaries where it can, and otherwise on a
// UTF-8 rune boundary, so no chunk ever holds a broken character.
func chunkAgySystemPrompt(prompt string, limit int) []string {
	if prompt == "" {
		return nil
	}
	if limit < utf8.UTFMax {
		limit = utf8.UTFMax
	}
	var chunks []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			chunks = append(chunks, cur.String())
			cur.Reset()
		}
	}
	for _, line := range strings.SplitAfter(prompt, "\n") {
		if line == "" {
			continue
		}
		if cur.Len()+len(line) <= limit {
			cur.WriteString(line)
			continue
		}
		flush()
		for len(line) > limit {
			cut := limit
			for cut > 0 && !utf8.RuneStart(line[cut]) {
				cut--
			}
			if cut == 0 {
				cut = limit
			}
			chunks = append(chunks, line[:cut])
			line = line[cut:]
		}
		cur.WriteString(line)
	}
	flush()
	return chunks
}

// renderAgyRules returns the rule files (by name) that carry
// prompt. Each file is the always_on frontmatter followed by one chunk.
func renderAgyRules(prompt string) map[string]string {
	header := func(i, n int) string {
		return fmt.Sprintf("<!-- Current kojo system prompt, part %d of %d. Replaces previous kojo rule snapshots. Managed by kojo and rewritten every turn; do not edit. -->\n", i, n)
	}
	// Leave room for the frontmatter and the part header.
	budget := agyRuleChunkBytes - len(agyRulesFrontmatter) - len(header(99, 99))
	chunks := chunkAgySystemPrompt(prompt, budget)
	out := make(map[string]string, len(chunks))
	for i, c := range chunks {
		name := fmt.Sprintf("%s%02d.md", agyRulesFilePrefix, i+1)
		out[name] = agyRulesFrontmatter + header(i+1, len(chunks)) + c
	}
	return out
}

// isAgyManagedRuleName reports whether name is a numbered chunk written
// by renderAgyRules (kojo-system-NN.md). Other files sharing the prefix
// are user rules and must survive pruning.
func isAgyManagedRuleName(name string) bool {
	if !strings.HasPrefix(name, agyRulesFilePrefix) || !strings.HasSuffix(name, ".md") {
		return false
	}
	num := strings.TrimSuffix(strings.TrimPrefix(name, agyRulesFilePrefix), ".md")
	if len(num) < 2 {
		return false
	}
	for _, r := range num {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
