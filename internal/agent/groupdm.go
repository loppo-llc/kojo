package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/loppo-llc/kojo/internal/store"
)

// jsonMarshalAttachments encodes attachments as a json.RawMessage suitable
// for store.GroupDMMessageRecord.Attachments. Callers guard against empty
// input, so an empty slice marshals to a JSON "[]" here.
func jsonMarshalAttachments(atts []MessageAttachment) (json.RawMessage, error) {
	return json.Marshal(atts)
}

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
	Venue GroupDMVenue `json:"venue,omitempty"`
	// MaxHops caps the agent-to-agent notification relay depth for this
	// room. 0 means "use defaultMaxHops". See PostMessage hop semantics.
	MaxHops int `json:"maxHops,omitempty"`
	// Kind is "group" (default) or "dm" (first-class 1:1 room sugar).
	Kind      GroupDMKind `json:"kind,omitempty"`
	CreatedAt string      `json:"createdAt"`
	UpdatedAt string      `json:"updatedAt"`
}

// GroupDMKind partitions rooms into classic groups and 1:1 DMs.
type GroupDMKind string

const (
	GroupDMKindGroup GroupDMKind = "group"
	GroupDMKindDM    GroupDMKind = "dm"
	// GroupDMKindThread is a parallel human↔agent thread room. Unlike a
	// single-agent "dm" it is always created fresh (no member-set dedup) so
	// an agent can have many independent threads at once. Behaves like a
	// single-agent dm for the thread-turn / archive / UI paths.
	GroupDMKindThread GroupDMKind = "thread"
)

// DefaultThreadName is the placeholder title a new thread room carries until
// its first agent reply auto-titles it (see maybeAutoTitleThread).
const DefaultThreadName = "無題のスレッド"

// GroupMember is a participant in a group DM.
//
// AgentName is a denormalized read-only display field. The DB store
// (members_json) does not persist it — it is rebuilt from the agents
// table on every load (and on every Members copy in groupdm_manager).
// Callers must not write AgentName expecting durability.
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
	ID          string              `json:"id"`
	AgentID     string              `json:"agentId"`
	AgentName   string              `json:"agentName"`
	Content     string              `json:"content"`
	Attachments []MessageAttachment `json:"attachments,omitempty"`
	// Hop is the agent-relay depth: 0 for user/system/fresh agent posts,
	// trigger-hop + 1 for posts made from a notification-triggered turn.
	Hop int `json:"hop,omitempty"`
	// Mentions is the list of member ids (agent ids, or "user" for the
	// human operator) referenced via @name in Content, parsed at post time.
	Mentions  []string `json:"mentions,omitempty"`
	Timestamp string   `json:"timestamp"`
	// Usage is the token usage of an agent thread reply (nil otherwise).
	Usage *Usage `json:"usage,omitempty"`
	// Thinking is the extended-thinking text of an agent thread reply
	// ("" otherwise).
	Thinking string `json:"thinking,omitempty"`
	// ToolUses is the tool-call trace of an agent thread reply (nil
	// otherwise).
	ToolUses []ToolUse `json:"toolUses,omitempty"`
}

func generateGroupID() string {
	return generatePrefixedID("gd_")
}

func generateGroupMessageID() string {
	return generatePrefixedID("gm_")
}

// appendGroupMessage inserts a message into the group's groupdm_messages
// table. The store handles seq allocation and member-vs-author validation.
// expectedLatestSeq re-verifies the per-group head inside the store
// transaction (store.ErrStaleHead on mismatch) — the DB-level half of the
// CAS whose fast path is the manager's latestMsgID cache. enforceSeq makes
// the check run even for expectedLatestSeq == 0 ("room must be empty");
// with enforceSeq false a zero seq disables the check (legacy callers).
func appendGroupMessage(groupID string, msg *GroupMessage, expectedLatestSeq int64, enforceSeq bool) error {
	if msg == nil {
		return errors.New("appendGroupMessage: nil message")
	}
	db := getGlobalStore()
	if db == nil {
		return errStoreNotReady
	}
	rec := &store.GroupDMMessageRecord{
		ID:        msg.ID,
		GroupDMID: groupID,
		AgentID:   msg.AgentID,
		Content:   msg.Content,
		Hop:       msg.Hop,
	}
	if len(msg.Mentions) > 0 {
		buf, err := json.Marshal(msg.Mentions)
		if err != nil {
			return fmt.Errorf("appendGroupMessage: marshal mentions: %w", err)
		}
		rec.Mentions = buf
	}
	if len(msg.Attachments) > 0 {
		buf, err := jsonMarshalAttachments(msg.Attachments)
		if err != nil {
			return fmt.Errorf("appendGroupMessage: marshal attachments: %w", err)
		}
		rec.Attachments = buf
	}
	if msg.Usage != nil {
		buf, err := json.Marshal(msg.Usage)
		if err != nil {
			return fmt.Errorf("appendGroupMessage: marshal usage: %w", err)
		}
		rec.Usage = buf
	}
	rec.Thinking = msg.Thinking
	if len(msg.ToolUses) > 0 {
		buf, err := json.Marshal(msg.ToolUses)
		if err != nil {
			return fmt.Errorf("appendGroupMessage: marshal tool_uses: %w", err)
		}
		rec.ToolUses = buf
	}
	ts := parseAgentRFC3339Millis(msg.Timestamp)
	if ts == 0 {
		ts = store.NowMillis()
	}
	ctx, cancel := transcriptCtx()
	defer cancel()
	if _, err := db.AppendGroupDMMessage(ctx, rec, store.GroupDMMessageInsertOptions{
		CreatedAt:                ts,
		UpdatedAt:                ts,
		ExpectedLatestSeq:        expectedLatestSeq,
		RequireExpectedLatestSeq: enforceSeq,
	}); err != nil {
		return err
	}
	if msg.Timestamp == "" {
		msg.Timestamp = millisToRFC3339(ts)
	}
	return nil
}

// resolveGroupMessageSeq maps a message ID to its per-group seq.
// ok=false means the message is not (or no longer) in this group's live
// transcript.
func resolveGroupMessageSeq(groupID, msgID string) (int64, bool, error) {
	db := getGlobalStore()
	if db == nil {
		return 0, false, errStoreNotReady
	}
	ctx, cancel := transcriptCtx()
	defer cancel()
	return groupMessageSeq(ctx, db, groupID, msgID)
}

// loadGroupMessages reads messages for groupID with pagination, plus
// the current head ID derived from the same snapshot.
//
// Head/page consistency: when before == "" the head is taken from the
// page itself (newest entry of the just-fetched recs) so a concurrent
// AppendGroupDMMessage between separate "head" and "list" queries can't
// surface a head-ID that isn't represented in the messages slice.
// When before != "" the page can't include rows newer than the cursor,
// so we issue a separate LatestGroupDMMessageID call — the head is
// purely informational on a paginated request and the brief skew there
// is acceptable.
//
// head is "" when the transcript is empty or the group is missing.
func loadGroupMessages(groupID string, limit int, before string) ([]*GroupMessage, bool, string, error) {
	db := getGlobalStore()
	if db == nil {
		return nil, false, "", errStoreNotReady
	}
	ctx, cancel := transcriptCtx()
	defer cancel()

	var beforeSeq int64
	if before != "" {
		bs, ok, err := groupMessageSeq(ctx, db, groupID, before)
		if err != nil {
			return nil, false, "", fmt.Errorf("resolve before cursor: %w", err)
		}
		if ok {
			beforeSeq = bs
		}
	}

	listOpts := store.GroupDMMessageListOptions{
		BeforeSeq: beforeSeq,
		Order:     "desc",
	}
	if limit > 0 {
		listOpts.Limit = limit + 1
	}
	recs, err := db.ListGroupDMMessages(ctx, groupID, listOpts)
	if err != nil {
		return nil, false, "", err
	}
	hasMore := false
	if limit > 0 && len(recs) > limit {
		hasMore = true
		recs = recs[:limit]
	}

	// Derive head from the same snapshot when we're loading the latest
	// page; otherwise fall back to a separate query. "Latest page" here
	// is beforeSeq==0, which covers both before=="" and before set to a
	// stale ID we couldn't resolve — in both cases recs is the newest
	// transcript window and recs[0] is the head.
	var headID string
	if beforeSeq == 0 {
		if len(recs) > 0 {
			headID = recs[0].ID // recs is desc-ordered, so recs[0] is newest
		}
	} else {
		hid, _, err := db.LatestGroupDMMessageID(ctx, groupID)
		if err != nil {
			return nil, false, "", err
		}
		headID = hid
	}

	if len(recs) == 0 {
		return nil, false, headID, nil
	}
	out := make([]*GroupMessage, len(recs))
	for i, rec := range recs {
		out[len(recs)-1-i] = groupRecordToMessage(rec)
	}
	if err := populateAgentNames(ctx, db, out); err != nil {
		return nil, false, "", err
	}
	return out, hasMore, headID, nil
}

// loadLatestGroupMessageID returns the ID of the newest message in a
// group's transcript. Returns ("", nil) if the group has no messages.
func loadLatestGroupMessageID(groupID string) (string, error) {
	db := getGlobalStore()
	if db == nil {
		return "", errStoreNotReady
	}
	ctx, cancel := transcriptCtx()
	defer cancel()
	id, _, err := db.LatestGroupDMMessageID(ctx, groupID)
	return id, err
}

// loadGroupMessagesAfter returns messages strictly newer than afterID,
// capped to the newest `limit` entries. hasMore is true when older diff
// entries had to be dropped to fit the cap.
//
// If afterID is empty, returns the newest `limit` messages from the
// transcript. If afterID is not found in the transcript, the caller-
// supplied cursor is treated as stale: the function returns the newest
// `limit` messages with hasMore=true so the caller can render "we
// couldn't locate your cursor, here's the latest state."
func loadGroupMessagesAfter(groupID, afterID string, limit int) ([]*GroupMessage, bool, error) {
	db := getGlobalStore()
	if db == nil {
		return nil, false, errStoreNotReady
	}
	ctx, cancel := transcriptCtx()
	defer cancel()

	if afterID == "" {
		listOpts := store.GroupDMMessageListOptions{Order: "desc"}
		if limit > 0 {
			listOpts.Limit = limit + 1
		}
		recs, err := db.ListGroupDMMessages(ctx, groupID, listOpts)
		if err != nil {
			return nil, false, err
		}
		hasMore := false
		if limit > 0 && len(recs) > limit {
			hasMore = true
			recs = recs[:limit]
		}
		out := reverseToOldestFirst(recs)
		if err := populateAgentNames(ctx, db, out); err != nil {
			return nil, false, err
		}
		return out, hasMore, nil
	}

	// Resolve afterID's seq. A stale cursor (the message has been hard-
	// deleted, the agent was wrong) falls back to "newest limit + flag
	// hasMore", matching the legacy file-based loader.
	afterSeq, ok, err := groupMessageSeq(ctx, db, groupID, afterID)
	if err != nil {
		return nil, false, err
	}
	if !ok {
		listOpts := store.GroupDMMessageListOptions{Order: "desc"}
		if limit > 0 {
			listOpts.Limit = limit + 1
		}
		recs, err := db.ListGroupDMMessages(ctx, groupID, listOpts)
		if err != nil {
			return nil, false, err
		}
		hasMore := true
		if limit > 0 && len(recs) > limit {
			recs = recs[:limit]
		}
		out := reverseToOldestFirst(recs)
		if err := populateAgentNames(ctx, db, out); err != nil {
			return nil, false, err
		}
		return out, hasMore, nil
	}

	listOpts := store.GroupDMMessageListOptions{
		SinceSeq: afterSeq,
		Order:    "desc",
	}
	if limit > 0 {
		listOpts.Limit = limit + 1
	}
	recs, err := db.ListGroupDMMessages(ctx, groupID, listOpts)
	if err != nil {
		return nil, false, err
	}
	hasMore := false
	if limit > 0 && len(recs) > limit {
		hasMore = true
		recs = recs[:limit]
	}
	out := reverseToOldestFirst(recs)
	if err := populateAgentNames(ctx, db, out); err != nil {
		return nil, false, err
	}
	return out, hasMore, nil
}

// loadGroupMessagesAfterSeq returns messages strictly newer than afterSeq
// (the server-persisted read cursor), capped to the newest `limit` entries.
// afterSeq<=0 returns the newest `limit` messages (nothing read yet).
// hasMore is true when older diff entries had to be dropped to fit the cap.
func loadGroupMessagesAfterSeq(groupID string, afterSeq int64, limit int) ([]*GroupMessage, bool, error) {
	db := getGlobalStore()
	if db == nil {
		return nil, false, errStoreNotReady
	}
	ctx, cancel := transcriptCtx()
	defer cancel()

	listOpts := store.GroupDMMessageListOptions{Order: "desc"}
	if afterSeq > 0 {
		listOpts.SinceSeq = afterSeq
	}
	if limit > 0 {
		listOpts.Limit = limit + 1
	}
	recs, err := db.ListGroupDMMessages(ctx, groupID, listOpts)
	if err != nil {
		return nil, false, err
	}
	hasMore := false
	if limit > 0 && len(recs) > limit {
		hasMore = true
		recs = recs[:limit]
	}
	out := reverseToOldestFirst(recs)
	if err := populateAgentNames(ctx, db, out); err != nil {
		return nil, false, err
	}
	return out, hasMore, nil
}

// reverseToOldestFirst converts a desc-ordered store result into the
// oldest-first GroupMessage slice the v0 callers expect.
func reverseToOldestFirst(recs []*store.GroupDMMessageRecord) []*GroupMessage {
	if len(recs) == 0 {
		return nil
	}
	out := make([]*GroupMessage, len(recs))
	for i, rec := range recs {
		out[len(recs)-1-i] = groupRecordToMessage(rec)
	}
	return out
}

// groupRecordToMessage converts the store's GroupDMMessageRecord into
// the v0-shaped GroupMessage. AgentName is left blank — populateAgentNames
// fills it in a single batched read.
func groupRecordToMessage(rec *store.GroupDMMessageRecord) *GroupMessage {
	out := &GroupMessage{
		ID:        rec.ID,
		AgentID:   rec.AgentID,
		Content:   rec.Content,
		Hop:       rec.Hop,
		Timestamp: normalizeTimestamp(millisToRFC3339(rec.CreatedAt)),
	}
	if len(rec.Mentions) > 0 && string(rec.Mentions) != "null" {
		var mentions []string
		if err := json.Unmarshal(rec.Mentions, &mentions); err == nil {
			out.Mentions = mentions
		}
	}
	if len(rec.Attachments) > 0 && string(rec.Attachments) != "null" {
		var atts []MessageAttachment
		if err := json.Unmarshal(rec.Attachments, &atts); err == nil {
			out.Attachments = atts
		}
	}
	if len(rec.Usage) > 0 && string(rec.Usage) != "null" {
		var u Usage
		if err := json.Unmarshal(rec.Usage, &u); err == nil {
			out.Usage = &u
		}
	}
	out.Thinking = rec.Thinking
	if len(rec.ToolUses) > 0 && string(rec.ToolUses) != "null" {
		var tu []ToolUse
		if err := json.Unmarshal(rec.ToolUses, &tu); err == nil {
			out.ToolUses = tu
		}
	}
	return out
}

// groupMessageSeq looks up a group message's seq by ID. ok=false means
// "no such message in this group" (deleted, never existed, or belongs
// to a different group). The agentdm-side caller uses this to decide
// between "advance from cursor" and "fall back to newest N + hasMore".
func groupMessageSeq(ctx context.Context, db *store.Store, groupID, msgID string) (int64, bool, error) {
	// We don't have a direct GetGroupDMMessage helper; ListGroupDMMessages
	// with a tight predicate isn't available either, so do a single
	// SELECT through the *sql.DB handle the store exposes via DB().
	row := db.DB().QueryRowContext(ctx,
		`SELECT seq FROM groupdm_messages
		  WHERE id = ? AND groupdm_id = ? AND deleted_at IS NULL`,
		msgID, groupID,
	)
	var seq int64
	err := row.Scan(&seq)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false, nil
		}
		return 0, false, err
	}
	return seq, true, nil
}

// populateAgentNames fills msg.AgentName for every message in the
// supplied slice using one SELECT per unique AgentID. The user-sender
// sentinel and system messages get a fixed display label so the UI
// never renders a blank author.
func populateAgentNames(ctx context.Context, db *store.Store, msgs []*GroupMessage) error {
	if len(msgs) == 0 {
		return nil
	}
	uniq := make(map[string]struct{}, len(msgs))
	for _, m := range msgs {
		if m.AgentID == "" || m.AgentID == store.UserSenderID {
			continue
		}
		uniq[m.AgentID] = struct{}{}
	}
	names := make(map[string]string, len(uniq))
	if len(uniq) > 0 {
		// IN(?,?,...) with one bind per id; agent counts here are bounded
		// by the group's member set (typically <=10) so the query stays
		// tiny even on a transcript with thousands of messages.
		ids := make([]string, 0, len(uniq))
		for id := range uniq {
			ids = append(ids, id)
		}
		placeholders := strings.Repeat("?,", len(ids))
		placeholders = placeholders[:len(placeholders)-1]
		args := make([]any, len(ids))
		for i, id := range ids {
			args[i] = id
		}
		rows, err := db.DB().QueryContext(ctx,
			"SELECT id, name FROM agents WHERE deleted_at IS NULL AND id IN ("+placeholders+")",
			args...,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id, name string
			if err := rows.Scan(&id, &name); err != nil {
				return err
			}
			names[id] = name
		}
		if err := rows.Err(); err != nil {
			return err
		}
	}
	for _, m := range msgs {
		switch {
		case m.AgentID == "":
			m.AgentName = "system"
		case m.AgentID == store.UserSenderID:
			m.AgentName = "user"
		default:
			if n, ok := names[m.AgentID]; ok {
				m.AgentName = n
			}
			// Unknown agent (hard-deleted between transcript write and
			// this read) leaves AgentName blank — the UI surfaces the
			// AgentID so the audit trail stays legible.
		}
	}
	return nil
}

func newGroupMessage(agentID, agentName, content string, attachments []MessageAttachment) *GroupMessage {
	return &GroupMessage{
		ID:          generateGroupMessageID(),
		AgentID:     agentID,
		AgentName:   agentName,
		Content:     content,
		Attachments: attachments,
		Timestamp:   time.Now().Format(time.RFC3339),
	}
}
