package chathistory

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// MaxJSONLLineBytes caps the size of a single JSONL line read by LoadHistory,
// LastMessage, LastPlatformTS, and autosummary.loadSessionMessages — all of
// which go through ScanJSONLLines below. Without a cap the per-line
// accumulator buffer (filled across repeated ReadSlice+ErrBufferFull chunks
// when a line exceeds bufio's default buffer) could grow without bound, so a
// corrupted/adversarial extremely-long line could allocate arbitrarily large
// amounts of memory. 10 MiB is far above realistic message payloads (a Slack
// message is < 40 KiB, a Claude transcript turn rarely exceeds 1 MiB), but
// small enough that hitting it almost certainly indicates corruption or
// attack. ScanJSONLLines surfaces ErrLineTooLarge once accumulated bytes
// would cross this threshold and stops reading.
const MaxJSONLLineBytes = 10 << 20

// ErrLineTooLarge is returned by ScanJSONLLines when a single line exceeds
// MaxJSONLLineBytes before a newline is found. Callers that want to be
// resilient (e.g. LastMessage / LastPlatformTS used on a partially-truncated
// channel file) can treat this as "stop, but don't fail"; LoadHistory
// surfaces it so the caller knows the history is unusable.
var ErrLineTooLarge = errors.New("jsonl line exceeds max size")

// ScanJSONLLines reads r line-by-line, invoking onLine for each non-empty
// line. The internal buffer is bounded at MaxJSONLLineBytes — once that many
// bytes accumulate without seeing '\n', ErrLineTooLarge is returned and no
// more data is consumed. Other I/O errors are returned wrapped; io.EOF on a
// trailing line without a final newline is treated as a clean end.
func ScanJSONLLines(r io.Reader, onLine func(line []byte)) error {
	br := bufio.NewReader(r)
	var buf []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if err == nil {
			// Complete line found via the internal buffer. If we've been
			// accumulating in `buf` because earlier ReadSlice calls hit
			// ErrBufferFull, the final tail chunk could push the total over
			// the cap — check here too so onLine never receives a
			// > MaxJSONLLineBytes slice.
			if len(buf) > 0 {
				if len(buf)+len(chunk) > MaxJSONLLineBytes {
					return ErrLineTooLarge
				}
				buf = append(buf, chunk...)
				onLine(buf)
				buf = nil
			} else {
				if len(chunk) > MaxJSONLLineBytes {
					return ErrLineTooLarge
				}
				// Copy because ReadSlice returns a view into the bufio buffer
				// that is invalidated on the next read.
				line := append([]byte(nil), chunk...)
				onLine(line)
			}
			continue
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			// Line is longer than bufio's default buffer (4 KiB); accumulate.
			if len(buf)+len(chunk) > MaxJSONLLineBytes {
				return ErrLineTooLarge
			}
			buf = append(buf, chunk...)
			continue
		}
		if errors.Is(err, io.EOF) {
			// Trailing line without final '\n'.
			if len(chunk) > 0 || len(buf) > 0 {
				buf = append(buf, chunk...)
				if len(buf) > MaxJSONLLineBytes {
					return ErrLineTooLarge
				}
				onLine(buf)
			}
			return nil
		}
		return fmt.Errorf("read jsonl: %w", err)
	}
}

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
	if err := ScanJSONLLines(f, func(line []byte) {
		var m HistoryMessage
		if json.Unmarshal(line, &m) == nil {
			msgs = append(msgs, m)
		}
	}); err != nil {
		return nil, fmt.Errorf("read history: %w", err)
	}
	return msgs, nil
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

	var last *HistoryMessage
	// Ignore ScanJSONLLines errors (incl. ErrLineTooLarge): a corrupted tail
	// shouldn't prevent reporting the last successfully-parsed message. We
	// stop on the first oversize line, but any earlier message is kept.
	_ = ScanJSONLLines(f, func(line []byte) {
		var m HistoryMessage
		if json.Unmarshal(line, &m) == nil {
			last = &m
		}
	})
	return last
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
	// Ignore ScanJSONLLines errors (incl. ErrLineTooLarge) for the same
	// reason as LastMessage: a corrupted tail shouldn't erase the cursor
	// derived from earlier well-formed entries.
	_ = ScanJSONLLines(f, func(line []byte) {
		var m HistoryMessage
		if json.Unmarshal(line, &m) == nil && isNumericTS(m.MessageID) {
			lastReal = m.MessageID
		}
	})
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
