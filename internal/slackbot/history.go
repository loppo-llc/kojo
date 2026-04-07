package slackbot

import (
	"context"
	"log/slog"
	"sort"
	"time"

	"github.com/loppo-llc/kojo/internal/chathistory"
	"github.com/slack-go/slack"
)

const (
	// channelHistoryLimit is the default number of recent channel messages to fetch.
	channelHistoryLimit = 30
)

// FetchThreadHistory retrieves thread messages from Slack.
//   - First call (no history file): fetches the entire thread including the
//     parent message, and writes the file.
//   - Subsequent calls: uses the last real Slack timestamp in the file as a
//     cursor to fetch only new messages (delta), appending them to the file.
//
// The history file may also contain locally-appended bot response entries
// (with IDs like "1234567890.bot") used by shouldAutoReply. These are
// ignored when determining the delta cursor.
func FetchThreadHistory(ctx context.Context, api *slack.Client, agentDataDir, channelID, threadTS string, resolve UserResolver, logger *slog.Logger) []chathistory.HistoryMessage {
	path := chathistory.HistoryFilePath(agentDataDir, "slack", channelID, threadTS)

	// Determine if this is a first fetch or a delta fetch.
	lastRealTS := chathistory.LastPlatformTS(path)

	params := &slack.GetConversationRepliesParameters{
		ChannelID: channelID,
		Timestamp: threadTS,
		Limit:     200,
	}
	if lastRealTS != "" {
		// Delta fetch: only messages after the last real Slack ts.
		// Slack oldest is inclusive, so we deduplicate below.
		params.Oldest = lastRealTS
	}

	var allSlackMsgs []slack.Message
	for {
		msgs, hasMore, cursor, err := api.GetConversationRepliesContext(ctx, params)
		if err != nil {
			logger.Warn("failed to fetch thread replies", "channel", channelID, "thread", threadTS, "err", err)
			break
		}
		allSlackMsgs = append(allSlackMsgs, msgs...)
		if !hasMore || cursor == "" {
			break
		}
		params.Cursor = cursor
	}

	if len(allSlackMsgs) == 0 {
		// API failed or empty thread — fall back to existing file
		history, _ := chathistory.LoadHistory(path)
		return history
	}

	// Convert and deduplicate (skip the cursor message we already have)
	var newMsgs []chathistory.HistoryMessage
	for _, sm := range allSlackMsgs {
		if sm.Timestamp == lastRealTS {
			continue
		}
		newMsgs = append(newMsgs, slackMsgToHistory(sm, channelID, threadTS, resolve))
	}

	sort.Slice(newMsgs, func(i, j int) bool {
		return newMsgs[i].MessageID < newMsgs[j].MessageID
	})

	if len(newMsgs) > 0 {
		if lastRealTS == "" {
			// First fetch: write the full thread (including parent message)
			if err := chathistory.WriteMessages(path, newMsgs); err != nil {
				logger.Warn("failed to save thread history", "path", path, "err", err)
			}
		} else {
			// Delta fetch: append new messages
			if err := chathistory.AppendMessages(path, newMsgs); err != nil {
				logger.Warn("failed to append thread history", "path", path, "err", err)
			}
		}
	}

	// Load full history from file
	history, err := chathistory.LoadHistory(path)
	if err != nil {
		logger.Warn("failed to load thread history", "path", path, "err", err)
		return newMsgs
	}
	return history
}

// FetchChannelHistory retrieves recent channel messages from Slack.
// This is a sliding window that overwrites the previous channel history file.
func FetchChannelHistory(ctx context.Context, api *slack.Client, agentDataDir, channelID string, limit int, resolve UserResolver, logger *slog.Logger) []chathistory.HistoryMessage {
	if limit <= 0 {
		limit = channelHistoryLimit
	}

	params := &slack.GetConversationHistoryParameters{
		ChannelID: channelID,
		Limit:     limit,
	}

	resp, err := api.GetConversationHistoryContext(ctx, params)
	if err != nil {
		logger.Warn("failed to fetch channel history", "channel", channelID, "err", err)
		return nil
	}

	var msgs []chathistory.HistoryMessage
	for _, sm := range resp.Messages {
		msgs = append(msgs, slackMsgToHistory(sm, channelID, "", resolve))
	}

	// Slack returns newest-first; reverse to chronological order
	sort.Slice(msgs, func(i, j int) bool {
		return msgs[i].MessageID < msgs[j].MessageID
	})

	// Overwrite channel history file (sliding window)
	path := chathistory.HistoryFilePath(agentDataDir, "slack", channelID, "")
	if err := chathistory.WriteMessages(path, msgs); err != nil {
		logger.Warn("failed to save channel history", "path", path, "err", err)
	}

	return msgs
}

// slackMsgToHistory converts a Slack message to a platform-agnostic HistoryMessage.
func slackMsgToHistory(sm slack.Message, channelID, threadID string, resolve UserResolver) chathistory.HistoryMessage {
	userName := sm.User
	if resolve != nil && sm.User != "" {
		userName = resolve(sm.User)
	}
	// Bot messages may use BotID instead of User
	if userName == "" && sm.BotID != "" {
		userName = sm.Username // Slack bot display name
		if userName == "" {
			userName = sm.BotID
		}
	}

	isBot := sm.BotID != "" || sm.SubType == "bot_message"

	// Parse Slack ts to RFC3339
	ts := slackTSToRFC3339(sm.Timestamp)

	// Convert Slack mrkdwn to plain text
	text := SlackToPlain(sm.Text, resolve)

	return chathistory.HistoryMessage{
		Platform:  "slack",
		ChannelID: channelID,
		ThreadID:  threadID,
		MessageID: sm.Timestamp,
		UserID:    sm.User,
		UserName:  userName,
		Text:      text,
		Timestamp: ts,
		IsBot:     isBot,
	}
}

// slackTSToRFC3339 converts a Slack timestamp (e.g. "1712345678.123456") to RFC3339.
func slackTSToRFC3339(ts string) string {
	// Slack ts format: "epoch.microseconds"
	// We only need the epoch part for time.Unix
	var sec int64
	for _, c := range ts {
		if c == '.' {
			break
		}
		sec = sec*10 + int64(c-'0')
	}
	if sec == 0 {
		return ""
	}
	return time.Unix(sec, 0).Format(time.RFC3339)
}
