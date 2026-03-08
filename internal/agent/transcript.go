package agent

import (
	"os"
	"path/filepath"
)

const messagesFile = "messages.jsonl"

// appendMessage appends a message to the agent's JSONL transcript.
func appendMessage(agentID string, msg *Message) error {
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
	return jsonlLoadPaginated(path, limit, before, func(m *Message) string { return m.ID })
}
