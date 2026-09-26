package agent

import (
	"context"
	"fmt"
	"strings"
)

// WebUI thread rooms as a keyed background surface: a thread turn runs on a
// keyed lingering claude session (SessionKey "groupdm:<roomID>"), so a
// run_in_background task started by it survives the turn. Its completion turn
// arrives here and is posted into the same room exactly like a normal thread
// reply (consumeThreadTurn), serialized through the room's thread FIFO.

// threadBackgroundPendingNote is appended to a thread reply whose session
// keeps lingering for still-running background tasks.
func threadBackgroundPendingNote(n int) string {
	return fmt.Sprintf("_バックグラウンド処理 %d件 実行中。完了したらこのスレッドで続きを投稿します_", n)
}

// threadStoppedBackgroundNote is appended to a stopped thread reply whose
// background tasks keep running (a stop ends only the turn).
func threadStoppedBackgroundNote(n int) string {
	return fmt.Sprintf("_バックグラウンド処理 %d件 は継続中です。完了したらこのスレッドで続きを投稿します（止めるにはエージェントに停止を依頼してください）_", n)
}

// threadBackgroundAbandonedNote is the notice posted when a lingering thread
// session ended with background tasks still pending.
func threadBackgroundAbandonedNote(pending int, reason string) string {
	switch reason {
	case KeyedStopRequestedReason:
		return fmt.Sprintf("_バックグラウンド処理 %d件 を停止しました（エージェントの依頼）_", pending)
	case KeyedUserStopReason:
		return fmt.Sprintf("_バックグラウンド処理 %d件 を停止しました（ユーザーの依頼）_", pending)
	}
	msg := fmt.Sprintf("_バックグラウンド処理 %d件 が完了前に終了しました", pending)
	if reason != "" {
		msg += "（" + reason + "）"
	}
	return msg + "。必要なら改めて依頼してください_"
}

// liveAgentThread reports whether groupID is a live thread room of agentID.
func (m *GroupDMManager) liveAgentThread(groupID, agentID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	g, err := m.liveGroupLocked(groupID)
	return err == nil && isThreadRoom(g) && len(g.Members) == 1 && g.Members[0].AgentID == agentID
}

// HandleKeyedBackgroundTurn implements KeyedBackgroundHandler for WebUI thread
// rooms: it waits for the room's thread FIFO, exposes the turn as the room's
// live/stoppable turn, and posts the result daemon-authored.
func (m *GroupDMManager) HandleKeyedBackgroundTurn(agentID, sessionKey string, events <-chan ChatEvent, cancel func()) {
	m.handleKeyedBackgroundTurnCtx(context.Background(), agentID, sessionKey, events, cancel)
}

// handleKeyedBackgroundTurnCtx is HandleKeyedBackgroundTurn with the turn's
// lifecycle context: when parent is cancelled (reset / delete / shutdown) the
// turn is treated like an archive cancel — nothing is posted.
func (m *GroupDMManager) handleKeyedBackgroundTurnCtx(parent context.Context, agentID, sessionKey string, events <-chan ChatEvent, cancel func()) {
	defer func() {
		for range events {
		}
	}()
	abort := func() {
		if cancel != nil {
			cancel()
		}
	}
	groupID, ok := strings.CutPrefix(sessionKey, webUIThreadKeyPrefix)
	if !ok || groupID == "" || !m.liveAgentThread(groupID, agentID) {
		abort()
		return
	}
	// A lingering keyed session only exists on the node running the agent, so
	// files the background turn stages are captured locally.
	m.runKeyedBackgroundTurn(parent, agentID, groupID, events, abort, generateGroupMessageID(), false)
}

// deliverRemoteKeyedBackgroundTurn posts a remote holder's keyed background
// turn into the thread (Hub side). The holder captures response attachments
// for replyMessageID itself, exactly like a remote thread turn.
func (m *GroupDMManager) deliverRemoteKeyedBackgroundTurn(agentID, sessionKey string, open RemoteKeyedTurnOpener) error {
	groupID, ok := strings.CutPrefix(sessionKey, webUIThreadKeyPrefix)
	if !ok || groupID == "" || !m.liveAgentThread(groupID, agentID) {
		return ErrNoKeyedBackgroundSurface
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	replyMessageID := generateGroupMessageID()
	// Attach before waiting for the thread FIFO: the holder's turn is already
	// running and its stream must be drained promptly.
	events, err := open(ctx, groupID, replyMessageID)
	if err != nil {
		return err
	}
	defer func() {
		for range events {
		}
	}()
	m.runKeyedBackgroundTurn(context.Background(), agentID, groupID, events, cancel, replyMessageID, true)
	return nil
}

// runKeyedBackgroundTurn waits for the thread FIFO and posts the background
// turn's result. holderCapture: response attachments are captured by the
// remote holder, so no local stage-dir watcher runs.
func (m *GroupDMManager) runKeyedBackgroundTurn(parent context.Context, agentID, groupID string, events <-chan ChatEvent, abort func(), replyMessageID string, holderCapture bool) {
	reservation := m.threadTurns.Reserve(groupID)
	defer reservation.Release()
	reservation.Wait()
	if !m.liveAgentThread(groupID, agentID) {
		abort()
		return
	}

	if parent.Err() != nil {
		abort()
		return
	}
	ctx, ctxCancel := context.WithCancel(parent)
	defer ctxCancel()
	m.threadCancelMu.Lock()
	m.threadCancels[groupID] = func() {
		ctxCancel()
		abort()
	}
	delete(m.threadStopped, groupID)
	m.threadCancelMu.Unlock()
	defer func() {
		m.threadCancelMu.Lock()
		delete(m.threadCancels, groupID)
		delete(m.threadStopped, groupID)
		m.threadCancelMu.Unlock()
	}()

	var agentModel, agentEffort string
	if a, ok := m.threadAgentInfo(agentID); ok {
		agentModel = a.Model
		agentEffort = a.Effort
	}
	m.startThreadLive(groupID, agentModel, agentEffort)
	defer m.endThreadLive(groupID)

	attachmentStageDir := threadAttachmentStageDir(agentID, groupID)
	var attachmentWatcher *attachWatcher
	if !holderCapture {
		attachmentWatcher = m.agentMgr.watchAndStreamAttachmentsFromDir(ctx, agentID, replyMessageID, attachmentStageDir)
	}
	m.consumeThreadTurn(ctx, events, threadTurnOutput{
		agentID:            agentID,
		groupID:            groupID,
		payload:            "[background task notification]",
		agentModel:         agentModel,
		agentEffort:        agentEffort,
		replyMessageID:     replyMessageID,
		attachmentStageDir: attachmentStageDir,
		attachmentWatcher:  attachmentWatcher,
	})
}

// KeyedBackgroundTasksAbandoned implements KeyedBackgroundHandler: it posts a
// notice into the thread (behind any in-flight reply) when the lingering
// session ended before its background tasks completed.
func (m *GroupDMManager) KeyedBackgroundTasksAbandoned(agentID, sessionKey string, pending int, reason string) {
	groupID, ok := strings.CutPrefix(sessionKey, webUIThreadKeyPrefix)
	if !ok || groupID == "" || pending <= 0 || !m.liveAgentThread(groupID, agentID) {
		return
	}
	reservation := m.threadTurns.Reserve(groupID)
	defer reservation.Release()
	reservation.Wait()
	if _, err := m.postThreadReply(groupID, agentID, threadBackgroundAbandonedNote(pending, reason), "", "", nil, "", nil, false); err != nil {
		m.logger.Warn("failed to post thread background abandoned notice", "group", groupID, "agent", agentID, "err", err)
	}
}
