package agent

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// compactionThreshold is the minimum MEMORY.md size (bytes) to trigger compaction.
	compactionThreshold = 4096

	// compactionKeepDays is how many recent diary files to keep (today + N-1 days).
	compactionKeepDays = 1

	// compactionMaxPromptBytes caps the total prompt size to avoid context overflow.
	compactionMaxPromptBytes = 128 * 1024
)

// maybeCompact checks whether an agent's MEMORY.md needs compaction and, if so,
// consolidates it with old diary files via CLI backends.
// Prefers the agent's own tool, then falls back to other available CLIs.
// Runs synchronously — caller is responsible for lifecycle management.
// Never mutates files on failure; uses mtime checks to avoid clobbering concurrent edits.
func maybeCompact(agentID string, tool string, logger *slog.Logger) {
	dir := agentDir(agentID)
	memPath := filepath.Join(dir, "MEMORY.md")

	memInfo, err := os.Stat(memPath)
	if err != nil || memInfo.Size() < compactionThreshold {
		return
	}
	memMtime := memInfo.ModTime()

	memory, err := os.ReadFile(memPath)
	if err != nil {
		logger.Warn("compaction: failed to read MEMORY.md", "agent", agentID, "err", err)
		return
	}

	// Collect old diary files (older than keepDays) with their mtimes
	cutoff := time.Now().AddDate(0, 0, -compactionKeepDays)
	diaries := collectOldDiaries(filepath.Join(dir, "memory"), cutoff)

	prompt := buildCompactionPrompt(string(memory), diaries)

	// Cap prompt size to avoid context overflow
	if len(prompt) > compactionMaxPromptBytes {
		logger.Warn("compaction: prompt too large, skipping",
			"agent", agentID, "promptBytes", len(prompt))
		return
	}

	// Try agent's own CLI tool first, then fall back to others
	compacted, err := generateWithPreferred(tool, prompt)
	if err != nil {
		logger.Warn("compaction: all backends failed", "agent", agentID, "err", err)
		return
	}

	compacted = strings.TrimSpace(compacted)
	if compacted == "" {
		logger.Warn("compaction: LLM returned empty result", "agent", agentID)
		return
	}

	// Check MEMORY.md hasn't been modified since we read it
	curInfo, err := os.Stat(memPath)
	if err != nil || !curInfo.ModTime().Equal(memMtime) {
		logger.Info("compaction: MEMORY.md modified during compaction, aborting", "agent", agentID)
		return
	}

	// Backup before overwriting
	bakPath := memPath + ".bak"
	if err := os.WriteFile(bakPath, memory, 0o644); err != nil {
		logger.Warn("compaction: failed to write backup", "agent", agentID, "err", err)
		return
	}

	// Atomic write: temp file + rename
	tmpFile, err := os.CreateTemp(dir, "MEMORY-*.md.tmp")
	if err != nil {
		logger.Warn("compaction: failed to create temp file", "agent", agentID, "err", err)
		return
	}
	tmpPath := tmpFile.Name()
	if _, err := tmpFile.WriteString(compacted + "\n"); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		logger.Warn("compaction: failed to write temp file", "agent", agentID, "err", err)
		return
	}
	tmpFile.Close()

	if err := os.Rename(tmpPath, memPath); err != nil {
		os.Remove(tmpPath)
		logger.Warn("compaction: failed to rename temp file", "agent", agentID, "err", err)
		return
	}

	// Remove incorporated diary files only if unchanged since we read them
	removed := 0
	for _, d := range diaries {
		curInfo, err := os.Stat(d.path)
		if err != nil {
			continue
		}
		if !curInfo.ModTime().Equal(d.mtime) {
			logger.Debug("compaction: diary modified during compaction, keeping", "path", d.path)
			continue
		}
		if err := os.Remove(d.path); err != nil {
			logger.Warn("compaction: failed to remove diary", "path", d.path, "err", err)
		} else {
			removed++
		}
	}

	logger.Info("memory compacted",
		"agent", agentID,
		"beforeBytes", len(memory),
		"afterBytes", len(compacted),
		"diariesMerged", removed,
	)
}


// diaryEntry holds a diary file's content and metadata for safe compaction.
type diaryEntry struct {
	date    string
	content string
	path    string
	mtime   time.Time
}

// collectOldDiaries reads diary files from the memory directory that are older
// than cutoff. Returns date-sorted entries with mtime for safe deletion.
func collectOldDiaries(memDir string, cutoff time.Time) []diaryEntry {
	entries, err := os.ReadDir(memDir)
	if err != nil {
		return nil
	}

	var diaries []diaryEntry

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		date, ok := parseDiaryDate(e.Name())
		if !ok {
			continue
		}
		if !date.Before(cutoff) {
			continue
		}
		p := filepath.Join(memDir, e.Name())
		info, err := e.Info()
		if err != nil {
			continue
		}
		content, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if len(strings.TrimSpace(string(content))) == 0 {
			continue
		}
		diaries = append(diaries, diaryEntry{
			date:    date.Format("2006-01-02"),
			content: string(content),
			path:    p,
			mtime:   info.ModTime(),
		})
	}

	sort.Slice(diaries, func(i, j int) bool {
		return diaries[i].date < diaries[j].date
	})

	return diaries
}

// parseDiaryDate extracts a date from a diary filename like "2006-01-02.md".
func parseDiaryDate(name string) (time.Time, bool) {
	name = strings.TrimSuffix(name, ".md")
	t, err := time.ParseInLocation("2006-01-02", name, time.Local)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// buildCompactionPrompt constructs the LLM prompt for memory compaction.
func buildCompactionPrompt(memory string, diaries []diaryEntry) string {
	var sb strings.Builder

	sb.WriteString("以下はAIエージェントの長期記憶(MEMORY.md)と古い日記ファイルの内容です。\n")
	sb.WriteString("これらを統合・圧縮して、新しいMEMORY.mdを生成してください。\n\n")

	sb.WriteString("## ルール\n\n")
	sb.WriteString("### 識別子の保持\n")
	sb.WriteString("すべての不透明な識別子を省略・短縮・再構成せず原文のまま保持すること: ")
	sb.WriteString("UUID, ハッシュ, ID, トークン, APIキー, ホスト名, IP, ポート, URL, ファイルパス, メールアドレス, アカウント名, credential ID\n\n")
	sb.WriteString("### 必ず保持する情報\n")
	sb.WriteString("- 決定事項とその理由 (判断の根拠が失われると再現できない)\n")
	sb.WriteString("- 未解決の課題・TODO・未回答の質問\n")
	sb.WriteString("- 制約条件 (技術的制約, ユーザーからの指示, 運用ルール)\n")
	sb.WriteString("- 人間関係・他エージェントとの関係\n")
	sb.WriteString("- ユーザーの好み・性格・行動パターン\n")
	sb.WriteString("- 技術的知見・デバッグで得た教訓\n")
	sb.WriteString("- アカウント情報・認証状態\n\n")
	sb.WriteString("### 削除・圧縮してよい情報\n")
	sb.WriteString("- 完了済みタスクの詳細経緯 (結論だけ残す)\n")
	sb.WriteString("- 一回限りの手順メモ (再利用しないもの)\n")
	sb.WriteString("- 冗長・重複する記述 (同じ情報の繰り返し)\n")
	sb.WriteString("- 日常的な挨拶やログ\n")
	sb.WriteString("- 古いステータス更新 (最新状態だけ残す)\n\n")
	sb.WriteString("### 出力形式\n")
	sb.WriteString("- 日記の中で重要な情報(新しい知見, 永続的な事実)があればMEMORY.mdの適切なセクションに統合する\n")
	sb.WriteString("- マークダウン形式を維持。セクション構造は元のMEMORY.mdに準拠\n")
	sb.WriteString("- 出力は圧縮後のMEMORY.md本文のみ。説明や前置きは不要\n\n")

	sb.WriteString("## 現在のMEMORY.md\n\n")
	sb.WriteString(memory)
	sb.WriteString("\n")

	if len(diaries) > 0 {
		sb.WriteString("\n## 古い日記ファイル（統合後に削除されます）\n\n")
		for _, d := range diaries {
			fmt.Fprintf(&sb, "### %s\n\n%s\n\n", d.date, strings.TrimSpace(d.content))
		}
	}

	return sb.String()
}
