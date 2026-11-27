package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStripPreCompactSections(t *testing.T) {
	body := "## 2026-11-20\n\n" +
		"- 09:00 — agent note one\n" +
		"\n## Pre-compaction summary (12:38)\n\n# Recent Activity\n\nLast summary: x\n\n## 進行中タスク\n- SUMMARY_CANARY\n```\n## Pre-compaction summary (13:00)\n```\n" +
		"- 13:10 — agent note two\n" +
		"\n## Pre-compaction summary (14:27)\n\nSECOND_CANARY\n````\n```\n- 14:30 — fenced in summary\n```\n````\nFENCE_TAIL_CANARY\n```\n```go\n- 14:31 — info string is not a closer\n```\nINFO_TAIL_CANARY\n- item\n    ```\n    - 14:32 — nested fence\n    ```\nNESTED_TAIL_CANARY\n" +
		"\n## Pre-compaction summary (15:00)\n\nTHIRD_CANARY\n" +
		"## 2026-11-21\n- 01:00 — agent note three\n" +
		"```\n## Pre-compaction summary (16:00)\nquoted sample stays\n```\n"
	got := stripPreCompactSections(body)
	for _, gone := range []string{"SUMMARY_CANARY", "SECOND_CANARY", "THIRD_CANARY", "FENCE_TAIL_CANARY", "INFO_TAIL_CANARY", "NESTED_TAIL_CANARY", "nested fence", "info string is not a closer", "fenced in summary", "Recent Activity", "(12:38)", "(14:27)"} {
		if strings.Contains(got, gone) {
			t.Errorf("summary content %q survived:\n%s", gone, got)
		}
	}
	for _, kept := range []string{"agent note one", "agent note two", "## 2026-11-21", "agent note three", "quoted sample stays"} {
		if !strings.Contains(got, kept) {
			t.Errorf("agent content %q dropped:\n%s", kept, got)
		}
	}
	if plain := "## 2026-11-20\n- 09:00 — x\n"; stripPreCompactSections(plain) != plain {
		t.Error("diary without summaries was changed")
	}
}

func TestIndexFiles_SkipsPreCompactSectionsAndReindexesOldFormat(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	id := "ag_index_strip"
	memDir := filepath.Join(agentDir(id), "memory")
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		t.Fatal(err)
	}
	diary := "## 2026-11-20\n- 09:00 — keepword note\n\n## Pre-compaction summary (12:00)\n\ncontaminated dropword\n"
	if err := os.WriteFile(filepath.Join(memDir, "2026-11-20.md"), []byte(diary), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(memDir, recentSummaryFile), []byte("rolling staleword"), 0o644); err != nil {
		t.Fatal(err)
	}
	// filesStale compares against a second-precision stamp; keep the
	// fixture clearly older than it.
	old := time.Now().Add(-time.Hour)
	for _, p := range []string{filepath.Join(memDir, "2026-11-20.md"), filepath.Join(memDir, recentSummaryFile), memDir} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	idx, err := OpenMemoryIndex(id, testLogger(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	if err := idx.IndexFiles(id); err != nil {
		t.Fatal(err)
	}
	if res, _ := idx.Search("dropword", 5); len(res) != 0 {
		t.Errorf("summary section still searchable: %+v", res)
	}
	if res, _ := idx.Search("staleword", 5); len(res) != 0 {
		t.Errorf("legacy recent.md still searchable: %+v", res)
	}
	if res, _ := idx.Search("keepword", 5); len(res) == 0 {
		t.Error("agent note not indexed")
	}
	if idx.filesStale(id) {
		t.Error("fresh index reported stale")
	}
	// An index written before the format marker existed must be redone.
	if _, err := idx.db.Exec("DELETE FROM index_meta WHERE source = 'files_format'"); err != nil {
		t.Fatal(err)
	}
	if !idx.filesStale(id) {
		t.Error("old-format index not reported stale")
	}
}
