// Package chathistory provides platform-agnostic chat history storage and
// formatting for external messaging platforms (Slack, Discord, etc.).
package chathistory

// HistoryMessage is a platform-agnostic representation of a chat message.
type HistoryMessage struct {
	Platform  string `json:"platform"`  // "slack", "discord", etc.
	ChannelID string `json:"channelId"` // platform channel identifier
	ThreadID  string `json:"threadId"`  // thread identifier (empty = channel-level)
	MessageID string `json:"messageId"` // unique message ID (e.g. Slack ts)
	UserID    string `json:"userId"`
	UserName  string `json:"userName"`
	Text      string `json:"text"`
	Timestamp string `json:"timestamp"` // RFC3339
	IsBot     bool   `json:"isBot"`
}
