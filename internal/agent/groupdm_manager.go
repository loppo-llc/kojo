package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/loppo-llc/kojo/internal/atomicfile"
)

const groupdmsFile = "groups.json"

// notifyTimeout is the maximum time allowed for a notification-triggered chat.
const notifyTimeout = 60 * time.Minute

// defaultNotifyCooldown is the minimum interval between notifications to the same agent
// for the same group. This prevents sequential ping-pong loops.
// Individual groups can override this via GroupDM.Cooldown.
const defaultNotifyCooldown = 50 * time.Second

// notifyState tracks cooldown and deferred notification state per (groupID, agentID).
type notifyState struct {
	lastSent time.Time   // when the last notification was successfully sent
	timer    *time.Timer // pending delivery timer (nil if none)
	gen      uint64      // generation counter; incremented on cancel to invalidate stale timers
	inFlight bool        // true while a delivery is in progress (prevents concurrent sends)

	// deferred notification payload
	agentID    string
	groupID    string
	groupName  string
	sender     string
	msgTime    string // original message timestamp (RFC3339)
	hasPending bool
}

// GroupDMManager manages group DM CRUD, message posting, and notifications.
type GroupDMManager struct {
	mu       sync.Mutex
	groups   map[string]*GroupDM
	agentMgr *Manager
	logger   *slog.Logger
	apiBase  string // base URL for agent-facing API (e.g. "http://127.0.0.1:8080")

	// notify tracks cooldown + deferred notification state per (groupID:agentID).
	notify   map[string]*notifyState
	notifyMu sync.Mutex
}

// NewGroupDMManager creates a new GroupDMManager.
func NewGroupDMManager(agentMgr *Manager, logger *slog.Logger) *GroupDMManager {
	m := &GroupDMManager{
		groups:   make(map[string]*GroupDM),
		agentMgr: agentMgr,
		logger:   logger,
		notify:   make(map[string]*notifyState),
	}
	m.load()
	return m
}

// SetAPIBase sets the base URL for agent-facing API docs in system prompts.
func (m *GroupDMManager) SetAPIBase(base string) {
	m.mu.Lock()
	m.apiBase = base
	m.mu.Unlock()
}

// APIBase returns the current API base URL.
func (m *GroupDMManager) APIBase() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.apiBase
}

// Create creates a new group DM with the given members.
// cooldown is the notification cooldown in seconds (0 = use default).
// style controls the communication style ("efficient" or "expressive"; empty = "efficient").
func (m *GroupDMManager) Create(name string, memberIDs []string, cooldown int, style GroupDMStyle) (*GroupDM, error) {
	if len(memberIDs) < 2 {
		return nil, ErrGroupTooFew
	}

	members, err := m.resolveMembers(memberIDs)
	if err != nil {
		return nil, err
	}
	if len(members) < 2 {
		return nil, ErrGroupTooFew
	}

	if style == "" {
		style = GroupDMStyleEfficient
	}
	if !ValidGroupDMStyles[style] {
		return nil, fmt.Errorf("invalid style: %q (must be %q or %q)", style, GroupDMStyleEfficient, GroupDMStyleExpressive)
	}

	now := time.Now().Format(time.RFC3339)
	g := &GroupDM{
		ID:        generateGroupID(),
		Name:      name,
		Members:   members,
		Cooldown:  clampCooldown(cooldown),
		Style:     style,
		CreatedAt: now,
		UpdatedAt: now,
	}

	if g.Name == "" {
		g.Name = m.defaultGroupName(members)
	}

	// Ensure group directory exists
	if err := os.MkdirAll(groupDir(g.ID), 0o755); err != nil {
		return nil, fmt.Errorf("create group dir: %w", err)
	}

	m.mu.Lock()
	m.groups[g.ID] = g
	m.mu.Unlock()

	m.save()
	m.logger.Info("group DM created", "id", g.ID, "name", g.Name)
	return m.copyGroup(g), nil
}

// CheckMembership verifies that the group exists and the agent is a member.
func (m *GroupDMManager) CheckMembership(groupID, agentID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	g, ok := m.groups[groupID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrGroupNotFound, groupID)
	}
	for _, mem := range g.Members {
		if mem.AgentID == agentID {
			return nil
		}
	}
	return fmt.Errorf("%w: agent %s in group %s", ErrGroupNotMember, agentID, groupID)
}

// Get returns a group DM by ID.
func (m *GroupDMManager) Get(id string) (*GroupDM, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	g, ok := m.groups[id]
	if !ok {
		return nil, false
	}
	return m.copyGroup(g), true
}

// List returns all group DMs.
func (m *GroupDMManager) List() []*GroupDM {
	m.mu.Lock()
	defer m.mu.Unlock()
	list := make([]*GroupDM, 0, len(m.groups))
	for _, g := range m.groups {
		list = append(list, m.copyGroup(g))
	}
	return list
}

// Rename changes the name of a group DM. Only members can rename.
func (m *GroupDMManager) Rename(id, name, callerAgentID string) (*GroupDM, error) {
	if name == "" {
		return nil, errors.New("name must not be empty")
	}

	m.mu.Lock()
	g, ok := m.groups[id]
	if !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrGroupNotFound, id)
	}

	// Verify caller is a member
	if callerAgentID != "" {
		isMember := false
		for _, mem := range g.Members {
			if mem.AgentID == callerAgentID {
				isMember = true
				break
			}
		}
		if !isMember {
			m.mu.Unlock()
			return nil, fmt.Errorf("%w: agent %s in group %s", ErrGroupNotMember, callerAgentID, id)
		}
	}

	oldName := g.Name
	g.Name = name
	g.UpdatedAt = time.Now().Format(time.RFC3339)

	// Resolve caller name and collect recipients
	var callerName string
	var recipients []GroupMember
	for _, mem := range g.Members {
		if mem.AgentID == callerAgentID {
			callerName = mem.AgentName
		} else {
			recipients = append(recipients, mem)
		}
	}
	if callerName == "" {
		callerName = "the owner"
	}
	groupName := g.Name
	cp := m.copyGroup(g)
	m.mu.Unlock()

	m.save()
	m.logger.Info("group DM renamed", "id", id, "oldName", oldName, "name", name)

	// Notify other members about the rename
	for _, r := range recipients {
		go m.notifyRename(r.AgentID, id, groupName, oldName, name, callerName)
	}

	return cp, nil
}

// Delete removes a group DM and its data.
// If notify is true, members are notified about the deletion before the group is removed.
func (m *GroupDMManager) Delete(id string, notify bool) error {
	m.mu.Lock()
	g, ok := m.groups[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrGroupNotFound, id)
	}

	// Collect members for notification before deleting
	var members []GroupMember
	var groupName string
	if notify {
		members = make([]GroupMember, len(g.Members))
		copy(members, g.Members)
		groupName = g.Name
	}

	delete(m.groups, id)
	m.mu.Unlock()

	// Clean up cooldown entries for this group
	m.cleanNotifyKeys(id)

	os.RemoveAll(groupDir(id))
	m.save()
	m.logger.Info("group DM deleted", "id", id, "notify", notify)

	// Notify members after deletion
	if notify {
		for _, mem := range members {
			go m.notifyGroupDeleted(mem.AgentID, id, groupName)
		}
	}

	return nil
}

// sendSystemNotification sends a system message to an agent and drains the response.
// Errors from busy/resetting agents are silently ignored; other errors are logged.
func (m *GroupDMManager) sendSystemNotification(agentID, notification, logContext string) {
	ctx, cancel := context.WithTimeout(context.Background(), notifyTimeout)
	defer cancel()

	events, err := m.agentMgr.Chat(ctx, agentID, notification, "system", nil)
	if err != nil {
		if !errors.Is(err, ErrAgentBusy) && !errors.Is(err, ErrAgentResetting) {
			m.logger.Warn("failed to send notification", "agent", agentID, "context", logContext, "err", err)
		}
		return
	}
	for range events {
	}
}

// notifyGroupDeleted sends a notification about group deletion to a member.
func (m *GroupDMManager) notifyGroupDeleted(agentID, groupID, groupName string) {
	notification := fmt.Sprintf(
		"[Group DM: %s] This group has been deleted.",
		groupName,
	)
	m.sendSystemNotification(agentID, notification, "group_deleted:"+groupID)
}

// PostMessage posts a message to a group and optionally notifies other members.
// If notify is true, other members receive a system notification in their 1:1 chat.
// Set notify=false for messages sent from notification-triggered chats to prevent loops.
func (m *GroupDMManager) PostMessage(ctx context.Context, groupID, agentID, content string, notify bool) (*GroupMessage, error) {
	m.mu.Lock()
	g, ok := m.groups[groupID]
	if !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrGroupNotFound, groupID)
	}

	// Verify sender is a member
	var senderName string
	isMember := false
	for _, mem := range g.Members {
		if mem.AgentID == agentID {
			isMember = true
			senderName = mem.AgentName
			break
		}
	}
	if !isMember {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: agent %s in group %s", ErrGroupNotMember, agentID, groupID)
	}

	// Collect other members for notification
	var recipients []GroupMember
	for _, mem := range g.Members {
		if mem.AgentID != agentID {
			recipients = append(recipients, mem)
		}
	}
	groupName := g.Name
	m.mu.Unlock()

	// Store message
	msg := newGroupMessage(agentID, senderName, content)
	if err := appendGroupMessage(groupID, msg); err != nil {
		return nil, fmt.Errorf("store message: %w", err)
	}

	// Update timestamp after successful write
	m.mu.Lock()
	if g, ok := m.groups[groupID]; ok {
		g.UpdatedAt = time.Now().Format(time.RFC3339)
	}
	m.mu.Unlock()
	m.save()

	// Notify other members asynchronously (unless suppressed to prevent loops)
	if notify {
		msgTime := msg.Timestamp
		for _, r := range recipients {
			go m.notifyAgent(r.AgentID, groupID, groupName, senderName, msgTime)
		}
	}

	return msg, nil
}

// Messages returns paginated messages for a group.
func (m *GroupDMManager) Messages(groupID string, limit int, before string) ([]*GroupMessage, bool, error) {
	m.mu.Lock()
	_, ok := m.groups[groupID]
	m.mu.Unlock()
	if !ok {
		return nil, false, fmt.Errorf("%w: %s", ErrGroupNotFound, groupID)
	}
	return loadGroupMessages(groupID, limit, before)
}

// GroupsForAgent returns all groups that contain the specified agent.
func (m *GroupDMManager) GroupsForAgent(agentID string) []*GroupDM {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []*GroupDM
	for _, g := range m.groups {
		for _, mem := range g.Members {
			if mem.AgentID == agentID {
				result = append(result, m.copyGroup(g))
				break
			}
		}
	}
	return result
}

// maxCooldown is the upper bound for notification cooldown (1 hour).
const maxCooldown = 3600

// groupCooldown returns the effective cooldown duration for a group.
func (m *GroupDMManager) groupCooldown(groupID string) time.Duration {
	m.mu.Lock()
	var cd int
	if g, ok := m.groups[groupID]; ok {
		cd = g.Cooldown
	}
	m.mu.Unlock()
	if cd > 0 {
		return time.Duration(cd) * time.Second
	}
	return defaultNotifyCooldown
}

// groupStyle returns the communication style for a group (defaults to efficient).
func (m *GroupDMManager) groupStyle(groupID string) GroupDMStyle {
	m.mu.Lock()
	defer m.mu.Unlock()
	if g, ok := m.groups[groupID]; ok && g.Style != "" {
		return g.Style
	}
	return GroupDMStyleEfficient
}

// clampCooldown validates and clamps cooldown to [0, maxCooldown].
func clampCooldown(seconds int) int {
	if seconds < 0 {
		return 0
	}
	if seconds > maxCooldown {
		return maxCooldown
	}
	return seconds
}

// SetCooldown updates the notification cooldown for a group (in seconds).
func (m *GroupDMManager) SetCooldown(id string, seconds int) (*GroupDM, error) {
	seconds = clampCooldown(seconds)
	m.mu.Lock()
	g, ok := m.groups[id]
	if !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrGroupNotFound, id)
	}
	g.Cooldown = seconds
	g.UpdatedAt = time.Now().Format(time.RFC3339)
	cp := m.copyGroup(g)
	m.mu.Unlock()
	m.save()
	m.logger.Info("group DM cooldown updated", "id", id, "cooldown", seconds)
	return cp, nil
}

// SetStyle updates the communication style for a group. callerAgentID must be a member.
// An empty callerAgentID skips the membership check (for admin/UI calls).
func (m *GroupDMManager) SetStyle(id string, style GroupDMStyle, callerAgentID string) (*GroupDM, error) {
	if style == "" {
		style = GroupDMStyleEfficient
	}
	if !ValidGroupDMStyles[style] {
		return nil, fmt.Errorf("invalid style: %q (must be %q or %q)", style, GroupDMStyleEfficient, GroupDMStyleExpressive)
	}
	m.mu.Lock()
	g, ok := m.groups[id]
	if !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrGroupNotFound, id)
	}
	if callerAgentID != "" {
		found := false
		for _, mem := range g.Members {
			if mem.AgentID == callerAgentID {
				found = true
				break
			}
		}
		if !found {
			m.mu.Unlock()
			return nil, fmt.Errorf("%w: agent %s in group %s", ErrGroupNotMember, callerAgentID, id)
		}
	}
	g.Style = style
	g.UpdatedAt = time.Now().Format(time.RFC3339)
	cp := m.copyGroup(g)
	m.mu.Unlock()
	m.save()
	m.logger.Info("group DM style updated", "id", id, "style", style)
	return cp, nil
}

// notifyRename sends a lightweight notification about a group rename.
// Unlike message notifications, this does not enforce cooldown or expect a reply.
func (m *GroupDMManager) notifyRename(agentID, groupID, groupName, oldName, newName, callerName string) {
	notification := fmt.Sprintf(
		"[Group DM: %s] Group renamed from %q to %q by %s.",
		groupName, oldName, newName, callerName,
	)
	m.sendSystemNotification(agentID, notification, "rename:"+groupID)
}

// notifyAgent sends a system message to an agent about new group activity.
// The notification only says there's a new message — no untrusted content is injected.
// The agent reads the actual messages via the API.
// Enforces a per-(group, agent) cooldown to prevent ping-pong loops.
func (m *GroupDMManager) notifyAgent(agentID, groupID, groupName, senderName, msgTime string) {
	key := groupID + ":" + agentID
	cooldown := m.groupCooldown(groupID)

	m.notifyMu.Lock()
	ns := m.notify[key]
	if ns == nil {
		ns = &notifyState{}
		m.notify[key] = ns
	}

	// Defer if: cooldown active, delivery in progress, or a pending timer exists
	elapsed := time.Since(ns.lastSent)
	if elapsed < cooldown || ns.inFlight || ns.timer != nil {
		ns.agentID = agentID
		ns.groupID = groupID
		ns.groupName = groupName
		ns.sender = senderName
		ns.msgTime = msgTime
		ns.hasPending = true

		if ns.timer == nil && !ns.inFlight {
			delay := cooldown - elapsed
			if delay < 0 {
				delay = 0
			}
			gen := ns.gen
			ns.timer = time.AfterFunc(delay, func() {
				m.firePending(key, gen)
			})
			m.logger.Debug("notification deferred", "agent", agentID, "group", groupID, "delay", delay)
		} else {
			m.logger.Debug("notification updated (pending)", "agent", agentID, "group", groupID)
		}
		m.notifyMu.Unlock()
		return
	}

	// Reserve delivery slot
	gen := ns.gen
	ns.inFlight = true
	m.notifyMu.Unlock()

	m.deliverNotification(key, gen, agentID, groupID, groupName, senderName, msgTime)
}

// firePending delivers the most recent deferred notification for the key.
// The generation check ensures stale timers (from cancelled/cleaned keys) are no-ops.
func (m *GroupDMManager) firePending(key string, gen uint64) {
	m.notifyMu.Lock()
	ns := m.notify[key]
	if ns == nil || ns.gen != gen || !ns.hasPending {
		if ns != nil {
			ns.timer = nil
		}
		m.notifyMu.Unlock()
		return
	}

	agentID := ns.agentID
	groupID := ns.groupID
	groupName := ns.groupName
	sender := ns.sender
	msgTime := ns.msgTime
	ns.hasPending = false
	ns.timer = nil
	ns.inFlight = true
	m.notifyMu.Unlock()

	m.deliverNotification(key, gen, agentID, groupID, groupName, sender, msgTime)
}

// deliverNotification sends a notification to the agent immediately.
// On transient failure (agent busy), it re-defers for retry after cooldown.
// The gen parameter prevents operating on state that was cleaned up.
func (m *GroupDMManager) deliverNotification(key string, gen uint64, agentID, groupID, groupName, senderName, msgTime string) { //nolint:gocognit
	cooldown := m.groupCooldown(groupID)
	apiBase := m.APIBase()
	curlFlags := "-s"
	if strings.HasPrefix(apiBase, "https://") {
		curlFlags = "-sk"
	}
	style := m.groupStyle(groupID)
	var styleHint string
	switch style {
	case GroupDMStyleExpressive:
		styleHint = "Style: expressive — reply naturally like a human chat."
	default:
		styleHint = "Style: efficient — EXTREME token saving. No greetings, no filler, no acknowledgements. Bare facts only. One-word replies preferred. Do NOT reply if you have nothing substantive to add."
	}
	notification := fmt.Sprintf(
		"[Group DM: %s] New message from %s at %s.\n"+
			"%s\n"+
			"Read: curl %s '%s/api/v1/groupdms/%s/messages?limit=20'\n"+
			"Reply: curl %s -X POST '%s/api/v1/groupdms/%s/messages' -H 'Content-Type: application/json' -d '{\"agentId\":\"%s\",\"content\":\"your reply\"}'",
		groupName, senderName, msgTime,
		styleHint,
		curlFlags, apiBase, groupID,
		curlFlags, apiBase, groupID, agentID,
	)

	ctx, cancel := context.WithTimeout(context.Background(), notifyTimeout)
	defer cancel()

	events, err := m.agentMgr.Chat(ctx, agentID, notification, "system", nil)

	m.notifyMu.Lock()
	ns := m.notify[key]
	if ns == nil || ns.gen != gen {
		// Key was cleaned up while we were delivering — drop silently
		m.notifyMu.Unlock()
		if err == nil {
			for range events {
			}
		}
		return
	}
	ns.inFlight = false

	if err != nil {
		if errors.Is(err, ErrAgentBusy) || errors.Is(err, ErrAgentResetting) {
			// Agent busy — re-defer for retry after cooldown
			if !ns.hasPending {
				ns.agentID = agentID
				ns.groupID = groupID
				ns.groupName = groupName
				ns.sender = senderName
				ns.msgTime = msgTime
				ns.hasPending = true
			}
			if ns.timer == nil {
				ns.timer = time.AfterFunc(cooldown, func() {
					m.firePending(key, gen)
				})
			}
			m.notifyMu.Unlock()
			m.logger.Debug("agent busy, notification re-deferred", "agent", agentID, "group", groupID)
		} else {
			// Non-transient error — release inFlight, fire pending if any arrived during delivery
			if ns.hasPending && ns.timer == nil {
				ns.timer = time.AfterFunc(0, func() {
					m.firePending(key, gen)
				})
			}
			m.notifyMu.Unlock()
			m.logger.Warn("failed to notify agent", "agent", agentID, "group", groupID, "err", err)
		}
		return
	}

	// Success — record cooldown and fire any pending notification that arrived during delivery
	ns.lastSent = time.Now()
	if ns.hasPending && ns.timer == nil {
		ns.timer = time.AfterFunc(cooldown, func() {
			m.firePending(key, gen)
		})
	}
	m.notifyMu.Unlock()

	// Drain events
	for range events {
	}
}

// resolveMembers validates member IDs and resolves their names.
func (m *GroupDMManager) resolveMembers(ids []string) ([]GroupMember, error) {
	seen := make(map[string]bool, len(ids))
	var members []GroupMember
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		a, ok := m.agentMgr.Get(id)
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrAgentNotFound, id)
		}
		members = append(members, GroupMember{AgentID: a.ID, AgentName: a.Name})
	}
	return members, nil
}

func (m *GroupDMManager) defaultGroupName(members []GroupMember) string {
	names := make([]string, len(members))
	for i, mem := range members {
		names[i] = mem.AgentName
	}
	return strings.Join(names, ", ")
}

func (m *GroupDMManager) copyGroup(g *GroupDM) *GroupDM {
	cp := *g
	cp.Members = make([]GroupMember, len(g.Members))
	copy(cp.Members, g.Members)
	return &cp
}

// AddMember adds an agent to an existing group DM. callerAgentID must be a current member.
// Notifies existing members about the new addition.
func (m *GroupDMManager) AddMember(id, newAgentID, callerAgentID string) (*GroupDM, error) {
	// Resolve the new member first (outside lock)
	a, ok := m.agentMgr.Get(newAgentID)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrAgentNotFound, newAgentID)
	}
	newMember := GroupMember{AgentID: a.ID, AgentName: a.Name}

	m.mu.Lock()
	g, ok := m.groups[id]
	if !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrGroupNotFound, id)
	}

	// Verify caller is a member
	var callerName string
	callerOK := false
	for _, mem := range g.Members {
		if mem.AgentID == callerAgentID {
			callerOK = true
			callerName = mem.AgentName
		}
		if mem.AgentID == newAgentID {
			m.mu.Unlock()
			return nil, fmt.Errorf("%w: %s in group %s", ErrGroupAlreadyMember, newAgentID, id)
		}
	}
	if callerAgentID == "" || !callerOK {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: agent %s in group %s", ErrGroupNotMember, callerAgentID, id)
	}

	g.Members = append(g.Members, newMember)
	g.UpdatedAt = time.Now().Format(time.RFC3339)

	// Collect recipients (all members except the new one)
	var recipients []GroupMember
	for _, mem := range g.Members {
		if mem.AgentID != newAgentID {
			recipients = append(recipients, mem)
		}
	}
	groupName := g.Name
	cp := m.copyGroup(g)
	m.mu.Unlock()

	m.save()
	m.logger.Info("member added to group DM", "group", id, "agent", newAgentID)

	// Notify existing members about the addition
	for _, r := range recipients {
		go m.notifyMemberChange(r.AgentID, id, groupName, callerName, newMember.AgentName, "added")
	}
	// Notify the new member that they were added
	go m.notifyMemberChange(newAgentID, id, groupName, callerName, newMember.AgentName, "added_you")

	return cp, nil
}

// LeaveGroup removes an agent from a group DM voluntarily.
// The group is deleted if fewer than 2 members remain.
func (m *GroupDMManager) LeaveGroup(id, agentID string) error {
	m.mu.Lock()
	g, ok := m.groups[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrGroupNotFound, id)
	}

	// Find and remove the member
	var leaverName string
	var filtered []GroupMember
	for _, mem := range g.Members {
		if mem.AgentID == agentID {
			leaverName = mem.AgentName
		} else {
			filtered = append(filtered, mem)
		}
	}
	if leaverName == "" {
		m.mu.Unlock()
		return fmt.Errorf("%w: agent %s in group %s", ErrGroupNotMember, agentID, id)
	}

	deleteGroup := len(filtered) < 2
	// Always collect remaining members for notification (even on group deletion)
	remaining := make([]GroupMember, len(filtered))
	copy(remaining, filtered)
	groupName := g.Name

	if deleteGroup {
		delete(m.groups, id)
	} else {
		g.Members = filtered
		g.UpdatedAt = time.Now().Format(time.RFC3339)
	}
	m.mu.Unlock()

	if deleteGroup {
		os.RemoveAll(groupDir(id))
		m.cleanNotifyKeys(id)
	} else {
		m.cleanNotifyKeys(id + ":" + agentID)
	}

	m.save()
	m.logger.Info("agent left group DM", "group", id, "agent", agentID, "deleted", deleteGroup)

	// Notify remaining members (including the last one when group is dissolved)
	for _, r := range remaining {
		go m.notifyMemberChange(r.AgentID, id, groupName, leaverName, leaverName, "left")
	}

	return nil
}

// notifyMemberChange sends a notification about member addition or departure.
func (m *GroupDMManager) notifyMemberChange(agentID, groupID, groupName, actorName, targetName, action string) {
	var notification string
	switch action {
	case "added":
		notification = fmt.Sprintf("[Group DM: %s] %s added %s to the group.", groupName, actorName, targetName)
	case "added_you":
		notification = fmt.Sprintf("[Group DM: %s] %s added you to the group.", groupName, actorName)
	case "left":
		notification = fmt.Sprintf("[Group DM: %s] %s left the group.", groupName, targetName)
	}
	m.sendSystemNotification(agentID, notification, action+":"+groupID)
}

// RemoveAgent removes an agent from all groups. Groups with fewer than 2 members are deleted.
func (m *GroupDMManager) RemoveAgent(agentID string) {
	m.mu.Lock()
	var toDelete []string
	changed := false
	for id, g := range m.groups {
		origLen := len(g.Members)
		var filtered []GroupMember
		for _, mem := range g.Members {
			if mem.AgentID != agentID {
				filtered = append(filtered, mem)
			}
		}
		if len(filtered) == origLen {
			continue // agent wasn't in this group
		}
		changed = true
		if len(filtered) < 2 {
			toDelete = append(toDelete, id)
		} else {
			g.Members = filtered
			g.UpdatedAt = time.Now().Format(time.RFC3339)
		}
	}
	for _, id := range toDelete {
		delete(m.groups, id)
	}
	m.mu.Unlock()

	for _, id := range toDelete {
		os.RemoveAll(groupDir(id))
		m.cleanNotifyKeys(id)
	}
	m.cleanNotifyKeys(agentID)

	if changed {
		m.save()
	}
}

// cleanNotifyKeys removes cooldown entries matching a group or agent prefix,
// or an exact key match (e.g. "groupID:agentID").
func (m *GroupDMManager) cleanNotifyKeys(prefix string) {
	m.notifyMu.Lock()
	for key, ns := range m.notify {
		if key == prefix || strings.HasPrefix(key, prefix+":") || strings.HasSuffix(key, ":"+prefix) {
			// Bump generation to invalidate any in-flight timer callback
			ns.gen++
			if ns.timer != nil {
				ns.timer.Stop()
			}
			delete(m.notify, key)
		}
	}
	m.notifyMu.Unlock()
}

// --- Persistence ---

func (m *GroupDMManager) save() {
	m.mu.Lock()
	groups := make([]*GroupDM, 0, len(m.groups))
	for _, g := range m.groups {
		groups = append(groups, m.copyGroup(g))
	}
	m.mu.Unlock()

	dir := groupdmsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		m.logger.Warn("failed to create groupdms dir", "err", err)
		return
	}

	if err := atomicfile.WriteJSON(filepath.Join(dir, groupdmsFile), groups, 0o644); err != nil {
		m.logger.Warn("failed to save groups", "err", err)
	}
}

func (m *GroupDMManager) load() {
	path := filepath.Join(groupdmsDir(), groupdmsFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var groups []*GroupDM
	if err := json.Unmarshal(data, &groups); err != nil {
		m.logger.Warn("failed to unmarshal groups", "err", err)
		return
	}
	for _, g := range groups {
		g.CreatedAt = normalizeTimestamp(g.CreatedAt)
		g.UpdatedAt = normalizeTimestamp(g.UpdatedAt)
		// Normalize legacy groups that predate the style field.
		if g.Style == "" || !ValidGroupDMStyles[g.Style] {
			g.Style = GroupDMStyleEfficient
		}
		m.groups[g.ID] = g
	}
}
