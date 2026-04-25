package agent

import (
	"os"
	"path/filepath"
	"time"
)

// GroupDM represents a group conversation between agents.
// GroupDMStyle controls the communication style for a group conversation.
// "efficient" (default): concise, token-saving, no pleasantries.
// "expressive": human-like chat with greetings and conversational filler.
type GroupDMStyle string

const (
	GroupDMStyleEfficient  GroupDMStyle = "efficient"
	GroupDMStyleExpressive GroupDMStyle = "expressive"
)

// ValidGroupDMStyles is the set of accepted style values.
var ValidGroupDMStyles = map[GroupDMStyle]bool{
	GroupDMStyleEfficient:  true,
	GroupDMStyleExpressive: true,
}

// NotifyMode controls how a specific member receives group-DM notifications.
//
//   - "realtime" (default): notify as soon as the group-level cooldown allows.
//   - "digest":  collect messages for up to DigestWindow seconds (or the group
//     cooldown, whichever is larger) before delivering a single batched turn.
//   - "muted":   do not notify this member at all. The member can still read
//     messages via the API on their own initiative.
type NotifyMode string

const (
	NotifyRealtime NotifyMode = "realtime"
	NotifyDigest   NotifyMode = "digest"
	NotifyMuted    NotifyMode = "muted"
)

// ValidNotifyModes is the set of accepted notify-mode values.
var ValidNotifyModes = map[NotifyMode]bool{
	NotifyRealtime: true,
	NotifyDigest:   true,
	NotifyMuted:    true,
}

// defaultDigestWindow is the fallback digest window when a member opts into
// "digest" mode without specifying DigestWindow explicitly.
const defaultDigestWindow = 300 // 5 minutes

// maxDigestWindow caps the digest window to 1 hour.
const maxDigestWindow = 3600

// GroupDMVenue is the physical/virtual setting that hosts the conversation.
// Agents use this hint to calibrate speech style: a co-located venue invites
// references to shared surroundings and gestures, while a chat room
// constrains everything to the text channel.
//
//   - "chatroom" (default): closed online chat room. Members are not
//     physically together; the only shared context is what is sent here.
//   - "colocated": same physical space. Members are co-present in real
//     time and may reference ambient cues, gestures, deictic ('this',
//     'over there') language.
type GroupDMVenue string

const (
	GroupDMVenueChatroom  GroupDMVenue = "chatroom"
	GroupDMVenueColocated GroupDMVenue = "colocated"
)

// ValidGroupDMVenues is the set of accepted venue values.
var ValidGroupDMVenues = map[GroupDMVenue]bool{
	GroupDMVenueChatroom:  true,
	GroupDMVenueColocated: true,
}

// defaultGroupDMVenue is what gets stamped onto a group when the field is
// empty (legacy data, callers omitting the parameter, etc.). We default to
// chatroom because that matches the existing token-saving DM design — a
// co-located venue is opt-in.
const defaultGroupDMVenue = GroupDMVenueChatroom

type GroupDM struct {
	ID       string        `json:"id"`
	Name     string        `json:"name"`
	Members  []GroupMember `json:"members"`
	Cooldown int           `json:"cooldown"` // notification cooldown in seconds (0 = use default)
	Style    GroupDMStyle  `json:"style"`    // communication style: "efficient" or "expressive"
	// Venue is the physical setting hint. "chatroom" (default) for a closed
	// online chat, "colocated" when members are co-present in real space.
	Venue     GroupDMVenue `json:"venue,omitempty"`
	CreatedAt string       `json:"createdAt"`
	UpdatedAt string       `json:"updatedAt"`
}

// GroupMember is a participant in a group DM.
type GroupMember struct {
	AgentID   string `json:"agentId"`
	AgentName string `json:"agentName"`
	// NotifyMode is the per-member delivery mode. Empty string is treated as
	// NotifyRealtime on read but omitted from JSON to keep legacy groups small.
	NotifyMode NotifyMode `json:"notifyMode,omitempty"`
	// DigestWindow is the digest-batching window in seconds. Only meaningful
	// when NotifyMode == NotifyDigest. 0 means "use defaultDigestWindow".
	DigestWindow int `json:"digestWindow,omitempty"`
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
	return generatePrefixedID("gd_")
}

func generateGroupMessageID() string {
	return generatePrefixedID("gm_")
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
	msgs, hasMore, err := jsonlLoadPaginated(path, limit, before, func(m *GroupMessage) string { return m.ID })
	for _, m := range msgs {
		m.Timestamp = normalizeTimestamp(m.Timestamp)
	}
	return msgs, hasMore, err
}

func newGroupMessage(agentID, agentName, content string) *GroupMessage {
	return &GroupMessage{
		ID:        generateGroupMessageID(),
		AgentID:   agentID,
		AgentName: agentName,
		Content:   content,
		Timestamp: time.Now().Format(time.RFC3339),
	}
}
