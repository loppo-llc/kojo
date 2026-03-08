package agent

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"time"
)

// GroupDM represents a group conversation between agents.
type GroupDM struct {
	ID        string        `json:"id"`
	Name      string        `json:"name"`
	Members   []GroupMember `json:"members"`
	CreatedAt string        `json:"createdAt"`
	UpdatedAt string        `json:"updatedAt"`
}

// GroupMember is a participant in a group DM.
type GroupMember struct {
	AgentID   string `json:"agentId"`
	AgentName string `json:"agentName"`
}

// GroupMessage is a single message in a group DM transcript.
type GroupMessage struct {
	ID        string `json:"id"`
	AgentID   string `json:"agentId"`
	AgentName string `json:"agentName"`
	Content   string `json:"content"`
	Timestamp string `json:"timestamp"`
}

func generateGroupID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "gd_" + hex.EncodeToString(b)
}

func generateGroupMessageID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "gm_" + hex.EncodeToString(b)
}

// groupdmsDir returns the base directory for group DM data.
func groupdmsDir() string {
	return filepath.Join(agentsDir(), "groupdms")
}

// groupDir returns the directory for a specific group.
func groupDir(groupID string) string {
	return filepath.Join(groupdmsDir(), groupID)
}

// appendGroupMessage appends a message to a group's JSONL transcript.
func appendGroupMessage(groupID string, msg *GroupMessage) error {
	dir := groupDir(groupID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return jsonlAppend(filepath.Join(dir, messagesFile), msg)
}

// loadGroupMessages reads messages from a group's JSONL transcript with pagination.
func loadGroupMessages(groupID string, limit int, before string) ([]*GroupMessage, bool, error) {
	path := filepath.Join(groupDir(groupID), messagesFile)
	return jsonlLoadPaginated(path, limit, before, func(m *GroupMessage) string { return m.ID })
}

func newGroupMessage(agentID, agentName, content string) *GroupMessage {
	return &GroupMessage{
		ID:        generateGroupMessageID(),
		AgentID:   agentID,
		AgentName: agentName,
		Content:   content,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
}
