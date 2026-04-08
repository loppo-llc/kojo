package chathistory

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// HistoryDir returns the directory for a platform/channel combination.
// Layout: {agentDataDir}/chat_history/{platform}/{channelID}/
func HistoryDir(agentDataDir, platform, channelID string) string {
	return filepath.Join(agentDataDir, "chat_history", platform, channelID)
}

// HistoryFilePath returns the JSONL file path for a conversation.
// Threaded conversations use {threadID}.jsonl; channel-level uses _channel.jsonl.
func HistoryFilePath(agentDataDir, platform, channelID, threadID string) string {
	dir := HistoryDir(agentDataDir, platform, channelID)
	if threadID == "" {
		return filepath.Join(dir, "_channel.jsonl")
	}
	// Sanitize threadID for filesystem safety (Slack ts contains dots)
	safe := strings.ReplaceAll(threadID, "/", "_")
	return filepath.Join(dir, safe+".jsonl")
}

// LoadHistory reads all messages from a JSONL history file.
// Returns nil (not error) if the file does not exist.
func LoadHistory(path string) ([]HistoryMessage, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("open history: %w", err)
	}
	defer f.Close()

	var msgs []HistoryMessage
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024) // up to 1MB per line
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var m HistoryMessage
		if err := json.Unmarshal(line, &m); err != nil {
			continue // skip corrupt lines
		}
		msgs = append(msgs, m)
	}
	return msgs, sc.Err()
}

// AppendMessages appends messages to a JSONL history file, creating
// parent directories as needed.
func AppendMessages(path string, msgs []HistoryMessage) error {
	if len(msgs) == 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create history dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open history file: %w", err)
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	for i := range msgs {
		if err := enc.Encode(&msgs[i]); err != nil {
			return fmt.Errorf("encode message: %w", err)
		}
	}
	return nil
}

// WriteMessages atomically overwrites a history file with the given messages.
// Used for channel-level sliding window history.
// Writes to a temporary file first and renames it into place so that a crash
// mid-write never corrupts the existing file.
func WriteMessages(path string, msgs []HistoryMessage) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create history dir: %w", err)
	}

	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("create temp history file: %w", err)
	}

	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	for i := range msgs {
		if err := enc.Encode(&msgs[i]); err != nil {
			f.Close()
			os.Remove(tmp)
			return fmt.Errorf("encode message: %w", err)
		}
	}

	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("sync history file: %w", err)
	}
	f.Close()

	return os.Rename(tmp, path)
}

// LastMessage returns the last entry in a JSONL file.
// Returns nil if the file does not exist or is empty.
func LastMessage(path string) *HistoryMessage {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var lastLine string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if line := sc.Text(); line != "" {
			lastLine = line
		}
	}
	if lastLine == "" {
		return nil
	}
	var m HistoryMessage
	if err := json.Unmarshal([]byte(lastLine), &m); err != nil {
		return nil
	}
	return &m
}

// HasHistory returns true if a history file exists and is non-empty.
func HasHistory(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Size() > 0
}

// LastPlatformTS returns the messageId of the last entry that has a real
// platform timestamp (e.g. Slack ts "1234567890.123456" — digits and dots
// only). Entries with locally-generated IDs like "1234567890.bot" are skipped.
// Used as the cursor for delta fetching from platform APIs.
func LastPlatformTS(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	var lastReal string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var m HistoryMessage
		if err := json.Unmarshal(line, &m); err != nil {
			continue
		}
		if isNumericTS(m.MessageID) {
			lastReal = m.MessageID
		}
	}
	return lastReal
}

// isNumericTS returns true if id contains only digits and dots
// (e.g. "1712345678.123456"). IDs like "1712345678.bot" return false.
func isNumericTS(id string) bool {
	if id == "" {
		return false
	}
	for _, c := range id {
		if c != '.' && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}
