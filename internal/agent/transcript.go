package agent

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

const messagesFile = "messages.jsonl"

// ErrMessageNotFound is returned when a message with the given ID does not exist.
var ErrMessageNotFound = errors.New("message not found")

// transcriptLocks serializes appendMessage / rewriteMessages per agent so that
// concurrent writers can't lose updates (the last rename would otherwise
// replace the file out from under a concurrent append).
var (
	transcriptLocksMu sync.Mutex
	transcriptLocks   = make(map[string]*sync.Mutex)
)

func transcriptLock(agentID string) *sync.Mutex {
	transcriptLocksMu.Lock()
	defer transcriptLocksMu.Unlock()
	mu, ok := transcriptLocks[agentID]
	if !ok {
		mu = &sync.Mutex{}
		transcriptLocks[agentID] = mu
	}
	return mu
}

// appendMessage appends a message to the agent's JSONL transcript.
func appendMessage(agentID string, msg *Message) error {
	mu := transcriptLock(agentID)
	mu.Lock()
	defer mu.Unlock()
	dir := agentDir(agentID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return jsonlAppend(filepath.Join(dir, messagesFile), msg)
}

// loadMessages reads the last N messages from the agent's JSONL transcript.
// If limit <= 0, all messages are returned.
func loadMessages(agentID string, limit int) ([]*Message, error) {
	msgs, _, err := loadMessagesPaginated(agentID, limit, "")
	return msgs, err
}

// loadMessagesPaginated reads messages with cursor-based pagination.
// If before is non-empty, returns the last `limit` messages before that ID.
// Returns the messages and whether there are more older messages.
func loadMessagesPaginated(agentID string, limit int, before string) ([]*Message, bool, error) {
	path := filepath.Join(agentDir(agentID), messagesFile)
	msgs, hasMore, err := jsonlLoadPaginated(path, limit, before, func(m *Message) string { return m.ID })
	for _, m := range msgs {
		m.Timestamp = normalizeTimestamp(m.Timestamp)
	}
	return msgs, hasMore, err
}

// rewriteMessages rewrites the transcript by streaming through each record and
// applying transform. If transform returns (nil, true), the record is dropped.
// If it returns (msg, false), msg replaces the original. Writes to a temp file,
// fsyncs, and atomically renames to avoid partial state on crash.
// Serialized per-agent against appendMessage via transcriptLock.
func rewriteMessages(agentID string, transform func(*Message) (*Message, bool)) (*Message, error) {
	mu := transcriptLock(agentID)
	mu.Lock()
	defer mu.Unlock()

	dir := agentDir(agentID)
	path := filepath.Join(dir, messagesFile)
	src, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrMessageNotFound
		}
		return nil, err
	}
	defer src.Close()

	tmp, err := os.CreateTemp(dir, "messages-*.jsonl.tmp")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	tmpClosed := false
	cleanup := func() {
		if !tmpClosed {
			tmp.Close()
		}
		os.Remove(tmpPath)
	}

	var matched *Message
	w := bufio.NewWriter(tmp)
	scanner := bufio.NewScanner(src)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		var m Message
		if err := json.Unmarshal(line, &m); err != nil {
			// Preserve malformed lines verbatim.
			if _, err := w.Write(append(append([]byte{}, line...), '\n')); err != nil {
				cleanup()
				return nil, err
			}
			continue
		}
		out, drop := transform(&m)
		if out != nil {
			matched = out
		}
		if drop {
			continue
		}
		target := &m
		if out != nil {
			target = out
		}
		data, err := json.Marshal(target)
		if err != nil {
			cleanup()
			return nil, err
		}
		data = append(data, '\n')
		if _, err := w.Write(data); err != nil {
			cleanup()
			return nil, err
		}
	}
	if err := scanner.Err(); err != nil {
		cleanup()
		return nil, err
	}
	if matched == nil {
		cleanup()
		return nil, ErrMessageNotFound
	}
	if err := w.Flush(); err != nil {
		cleanup()
		return nil, err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		tmpClosed = true
		os.Remove(tmpPath)
		return nil, err
	}
	tmpClosed = true
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return nil, err
	}
	// Sync parent dir so the rename is durable across crashes.
	if err := syncDir(dir); err != nil {
		return nil, err
	}
	return matched, nil
}

// syncDir fsyncs the given directory. Returns nil on platforms where
// directory fsync is not meaningful and surfaces the error otherwise.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	syncErr := d.Sync()
	closeErr := d.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

// findRegenerateTarget locates the user message that should drive a
// regeneration rooted at msgID, and computes how many leading messages of
// the transcript to keep.
//
//   - assistant msgID: keeps messages before msgID, returns the nearest
//     preceding user message as the regeneration source.
//   - user msgID: keeps messages up to and including msgID, returns msgID
//     itself as the regeneration source.
//   - system/unknown: ErrInvalidRegenerate.
func findRegenerateTarget(agentID, msgID string) (*Message, int, error) {
	msgs, err := loadMessages(agentID, 0)
	if err != nil {
		return nil, 0, err
	}
	idx := -1
	for i, m := range msgs {
		if m.ID == msgID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil, 0, ErrMessageNotFound
	}
	target := msgs[idx]
	switch target.Role {
	case "assistant":
		for i := idx - 1; i >= 0; i-- {
			if msgs[i].Role == "user" {
				return msgs[i], idx, nil
			}
		}
		return nil, 0, ErrInvalidRegenerate
	case "user":
		return target, idx + 1, nil
	default:
		return nil, 0, ErrInvalidRegenerate
	}
}

// truncateMessagesTo rewrites the transcript to keep only the first
// keepCount messages. Writes to a temp file, fsyncs, and atomically renames.
// Serialized per-agent against appendMessage via transcriptLock.
func truncateMessagesTo(agentID string, keepCount int) error {
	mu := transcriptLock(agentID)
	mu.Lock()
	defer mu.Unlock()

	dir := agentDir(agentID)
	path := filepath.Join(dir, messagesFile)
	src, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			if keepCount == 0 {
				return nil
			}
			return ErrMessageNotFound
		}
		return err
	}
	defer src.Close()

	tmp, err := os.CreateTemp(dir, "messages-*.jsonl.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	tmpClosed := false
	cleanup := func() {
		if !tmpClosed {
			tmp.Close()
		}
		os.Remove(tmpPath)
	}

	w := bufio.NewWriter(tmp)
	scanner := bufio.NewScanner(src)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	count := 0
	for scanner.Scan() && count < keepCount {
		if _, err := w.Write(append(append([]byte{}, scanner.Bytes()...), '\n')); err != nil {
			cleanup()
			return err
		}
		count++
	}
	if err := scanner.Err(); err != nil {
		cleanup()
		return err
	}
	if err := w.Flush(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		tmpClosed = true
		os.Remove(tmpPath)
		return err
	}
	tmpClosed = true
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return syncDir(dir)
}

// updateMessageContent replaces the content of the message with the given ID.
// Returns ErrMessageNotFound if no matching message exists.
func updateMessageContent(agentID, msgID, content string) (*Message, error) {
	return rewriteMessages(agentID, func(m *Message) (*Message, bool) {
		if m.ID != msgID {
			return nil, false
		}
		updated := *m
		updated.Content = content
		updated.Timestamp = normalizeTimestamp(updated.Timestamp)
		return &updated, false
	})
}

// deleteMessage removes the message with the given ID from the transcript.
// Returns ErrMessageNotFound if no matching message exists.
func deleteMessage(agentID, msgID string) error {
	_, err := rewriteMessages(agentID, func(m *Message) (*Message, bool) {
		if m.ID != msgID {
			return nil, false
		}
		copy := *m
		return &copy, true
	})
	return err
}
